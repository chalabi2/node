package keeper_test

import (
	"context"
	"testing"
	"time"

	"cosmossdk.io/log"
	"cosmossdk.io/store"
	storemetrics "cosmossdk.io/store/metrics"
	storetypes "cosmossdk.io/store/types"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	dbm "github.com/cosmos/cosmos-db"
	sdk "github.com/cosmos/cosmos-sdk/types"
	testutilmod "github.com/cosmos/cosmos-sdk/types/module/testutil"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	"github.com/stretchr/testify/require"

	mv1 "pkg.akt.dev/go/node/market/v1"
	ptypes "pkg.akt.dev/go/node/provider/v1beta4"
	vtypes "pkg.akt.dev/go/node/verification/v1"
	"pkg.akt.dev/go/testutil"

	verificationhandler "pkg.akt.dev/node/v2/x/verification/handler"
	verificationkeeper "pkg.akt.dev/node/v2/x/verification/keeper"
	moduletypes "pkg.akt.dev/node/v2/x/verification/types"
)

const updateParamsAuthority = "gov"

func TestMsgUpdateParamsRejectsInvalidAuthority(t *testing.T) {
	ctx, k, server := setupUpdateParamsMsgServer(t)
	params := verificationkeeper.DefaultParams()
	params.VerificationModuleActive = true

	res, err := server.UpdateParams(sdk.WrapSDKContext(ctx), &vtypes.MsgUpdateParams{
		Authority: "not-gov",
		Params:    params,
	})

	require.ErrorIs(t, err, govtypes.ErrInvalidSigner)
	require.Nil(t, res)
	require.Equal(t, verificationkeeper.DefaultParams(), k.GetParams(ctx))
}

func TestMsgUpdateParamsRejectsInvalidAuthorityBeforeParamsValidation(t *testing.T) {
	ctx, k, server := setupUpdateParamsMsgServer(t)
	params := verificationkeeper.DefaultParams()
	params.BondL1 = sdk.NewInt64Coin("uakt", 0)

	res, err := server.UpdateParams(sdk.WrapSDKContext(ctx), &vtypes.MsgUpdateParams{
		Authority: "",
		Params:    params,
	})

	require.ErrorIs(t, err, govtypes.ErrInvalidSigner)
	require.NotErrorIs(t, err, moduletypes.ErrInvalidReason)
	require.Nil(t, res)
	require.Equal(t, verificationkeeper.DefaultParams(), k.GetParams(ctx))
}

func TestMsgUpdateParamsPersistsValidUpdate(t *testing.T) {
	ctx, k, server := setupUpdateParamsMsgServer(t)
	params := verificationkeeper.DefaultParams()
	params.VerificationModuleActive = true
	params.MinFeeL1 = sdk.NewInt64Coin("uakt", 11000000)

	res, err := server.UpdateParams(sdk.WrapSDKContext(ctx), &vtypes.MsgUpdateParams{
		Authority: updateParamsAuthority,
		Params:    params,
	})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, params, k.GetParams(ctx))
}

func TestMsgUpdateParamsAllowsZeroProviderAuditDeposit(t *testing.T) {
	ctx, k, server := setupUpdateParamsMsgServer(t)
	params := verificationkeeper.DefaultParams()
	params.ProviderAuditDeposit = sdk.NewInt64Coin("uakt", 0)

	res, err := server.UpdateParams(sdk.WrapSDKContext(ctx), &vtypes.MsgUpdateParams{
		Authority: updateParamsAuthority,
		Params:    params,
	})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, params, k.GetParams(ctx))
}

func TestMsgUpdateParamsRejectsInvalidParams(t *testing.T) {
	ctx, k, server := setupUpdateParamsMsgServer(t)
	params := verificationkeeper.DefaultParams()
	params.BondL1 = sdk.NewInt64Coin("uakt", 0)

	res, err := server.UpdateParams(sdk.WrapSDKContext(ctx), &vtypes.MsgUpdateParams{
		Authority: updateParamsAuthority,
		Params:    params,
	})

	require.ErrorIs(t, err, moduletypes.ErrInvalidReason)
	require.ErrorContains(t, err, "params.bond_l1")
	require.Nil(t, res)
	require.Equal(t, verificationkeeper.DefaultParams(), k.GetParams(ctx))
}

func setupUpdateParamsMsgServer(t testing.TB) (sdk.Context, verificationkeeper.Keeper, vtypes.MsgServer) {
	t.Helper()

	cfg := testutilmod.MakeTestEncodingConfig()
	key := storetypes.NewKVStoreKey(moduletypes.StoreKey)
	db := dbm.NewMemDB()

	ms := store.NewCommitMultiStore(db, log.NewNopLogger(), storemetrics.NewNoOpMetrics())
	ms.MountStoreWithDB(key, storetypes.StoreTypeIAVL, db)

	err := ms.LoadLatestVersion()
	require.NoError(t, err)

	ctx := sdk.NewContext(
		ms,
		tmproto.Header{Time: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)},
		false,
		testutil.Logger(t),
	)
	k := verificationkeeper.NewKeeper(cfg.Codec, key, updateParamsDependencies())

	return ctx, k, verificationhandler.NewMsgServerImpl(k)
}

func updateParamsDependencies() verificationkeeper.Dependencies {
	bank := updateParamsBank{}
	return verificationkeeper.Dependencies{
		Authority:    updateParamsAuthority,
		Account:      &updateParamsAccountKeeper{},
		Bank:         bank,
		Distribution: bank,
		Provider:     updateParamsProviderKeeper{},
		Market:       updateParamsMarketKeeper{},
	}
}

type updateParamsAccountKeeper struct {
	account sdk.ModuleAccountI
}

func (k *updateParamsAccountKeeper) GetModuleAccount(context.Context, string) sdk.ModuleAccountI {
	return k.account
}

func (k *updateParamsAccountKeeper) SetModuleAccount(_ context.Context, account sdk.ModuleAccountI) {
	k.account = account
}

type updateParamsBank struct{}

func (updateParamsBank) GetBalance(_ context.Context, _ sdk.AccAddress, denom string) sdk.Coin {
	return sdk.NewInt64Coin(denom, 0)
}

func (updateParamsBank) SendCoinsFromAccountToModule(context.Context, sdk.AccAddress, string, sdk.Coins) error {
	return nil
}

func (updateParamsBank) SendCoinsFromModuleToAccount(context.Context, string, sdk.AccAddress, sdk.Coins) error {
	return nil
}

func (updateParamsBank) FundCommunityPool(context.Context, sdk.Coins, sdk.AccAddress) error {
	return nil
}

type updateParamsProviderKeeper struct{}

func (updateParamsProviderKeeper) Get(_ sdk.Context, id sdk.Address) (ptypes.Provider, bool) {
	return ptypes.Provider{Owner: id.String()}, true
}

func (updateParamsProviderKeeper) GetRegistrationTime(ctx sdk.Context, _ sdk.Address) (time.Time, bool) {
	return ctx.BlockTime(), true
}

type updateParamsMarketKeeper struct{}

func (updateParamsMarketKeeper) GetProviderLeaseStats(sdk.Context, sdk.Address, time.Time) (uint64, map[mv1.LeaseClosedReason]uint64, bool) {
	return 0, nil, true
}
