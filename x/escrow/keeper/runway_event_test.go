package keeper_test

import (
	"testing"

	sdkmath "cosmossdk.io/math"
	abci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	escrowid "pkg.akt.dev/go/node/escrow/id/v1"
	"pkg.akt.dev/go/node/escrow/module"
	etypes "pkg.akt.dev/go/node/escrow/types/v1"
	ev1 "pkg.akt.dev/go/node/escrow/v1"
	"pkg.akt.dev/go/testutil"

	"pkg.akt.dev/node/v2/testutil/state"
	bmemodule "pkg.akt.dev/node/v2/x/bme"
)

// runwayEvents extracts every EventAccountRunway emitted on the context.
func runwayEvents(t *testing.T, ctx sdk.Context) []ev1.EventAccountRunway {
	t.Helper()

	evName := proto.MessageName(&ev1.EventAccountRunway{})

	var out []ev1.EventAccountRunway
	for _, ev := range ctx.EventManager().Events() {
		if ev.Type != evName {
			continue
		}

		msg, err := sdk.ParseTypedEvent(abci.Event(ev))
		require.NoError(t, err)

		out = append(out, *msg.(*ev1.EventAccountRunway))
	}

	return out
}

func setupRunwayAccount(t *testing.T, ssuite *state.TestSuite, deposit int64, rates ...int64) escrowid.Account {
	t.Helper()

	ctx := ssuite.Context()
	ekeeper := ssuite.EscrowKeeper()

	lid := testutil.LeaseID(t)
	did := lid.DeploymentID()
	aid := did.ToEscrowAccountID()

	aowner := testutil.AccAddress(t)
	amt := testutil.ACTCoin(t, deposit)

	ssuite.MockBMEForDeposit(aowner, amt)
	require.NoError(t, ekeeper.AccountCreate(ctx, aid, aowner, []etypes.Depositor{{
		Owner:   aowner.String(),
		Height:  ctx.BlockHeight(),
		Balance: sdk.NewDecCoinFromCoin(amt),
	}}))

	for idx, rate := range rates {
		plid := lid
		plid.GSeq = uint32(idx + 1) // nolint: gosec
		powner := testutil.AccAddress(t)
		require.NoError(t, ekeeper.PaymentCreate(
			ctx,
			plid.ToEscrowPaymentID(),
			powner,
			sdk.NewDecCoinFromCoin(sdk.NewCoin("uact", sdkmath.NewInt(rate))),
		))
	}

	return aid
}

// Test_AccountSettle_EmitsRunwayEvent asserts that settling an account with
// open payments emits EventAccountRunway with
// projected_depletion_height = current + floor(funds / sum of open rates).
func Test_AccountSettle_EmitsRunwayEvent(t *testing.T) {
	ssuite := state.SetupTestSuite(t)
	// deposit 1000, rates 10 + 20 = 30 per block
	aid := setupRunwayAccount(t, ssuite, 1000, 10, 20)

	ctx := ssuite.Context().WithBlockHeight(ssuite.Context().BlockHeight() + 10)

	od, err := ssuite.EscrowKeeper().AccountSettle(ctx, aid)
	require.NoError(t, err)
	require.False(t, od)

	// 10 blocks x 30 transferred -> 700 left; floor(700 / 30) = 23
	events := runwayEvents(t, ctx)
	require.Len(t, events, 1)
	require.Equal(t, aid, events[0].ID)
	require.Equal(t, ctx.BlockHeight()+23, events[0].ProjectedDepletionHeight)
}

// Test_AccountSettle_RunwayZeroWhenNoFullBlock asserts the event reports 0
// when the remaining funds do not cover a single full block.
func Test_AccountSettle_RunwayZeroWhenNoFullBlock(t *testing.T) {
	ssuite := state.SetupTestSuite(t)
	// deposit 1000, rate 30 per block
	aid := setupRunwayAccount(t, ssuite, 1000, 30)

	// 33 blocks x 30 = 990 transferred -> 10 left < 30
	ctx := ssuite.Context().WithBlockHeight(ssuite.Context().BlockHeight() + 33)

	od, err := ssuite.EscrowKeeper().AccountSettle(ctx, aid)
	require.NoError(t, err)
	require.False(t, od)

	events := runwayEvents(t, ctx)
	require.Len(t, events, 1)
	require.Equal(t, aid, events[0].ID)
	require.Equal(t, int64(0), events[0].ProjectedDepletionHeight)
}

// Test_AccountSettle_NoRunwayEventWhenOverdrawn asserts that a settle that
// overdraws the account leaves no open payments and therefore emits no
// runway event.
func Test_AccountSettle_NoRunwayEventWhenOverdrawn(t *testing.T) {
	ssuite := state.SetupTestSuite(t)
	bkeeper := ssuite.BankKeeper()
	// deposit 100, rate 30 per block: 10 blocks owe 300 -> overdrawn
	aid := setupRunwayAccount(t, ssuite, 100, 30)

	bkeeper.
		On("SendCoinsFromModuleToModule", mock.Anything, module.ModuleName, mock.MatchedBy(func(dest string) bool {
			return dest == bmemodule.ModuleName || dest == distrtypes.ModuleName
		}), mock.Anything).
		Return(nil).Maybe()
	bkeeper.
		On("SendCoinsFromModuleToModule", mock.Anything, bmemodule.ModuleName, mock.Anything, mock.Anything).
		Return(nil).Maybe()
	bkeeper.
		On("MintCoins", mock.Anything, bmemodule.ModuleName, mock.Anything).
		Return(nil).Maybe()
	bkeeper.
		On("BurnCoins", mock.Anything, bmemodule.ModuleName, mock.Anything).
		Return(nil).Maybe()
	bkeeper.
		On("SendCoinsFromModuleToAccount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()

	ctx := ssuite.Context().WithBlockHeight(ssuite.Context().BlockHeight() + 10)

	od, err := ssuite.EscrowKeeper().AccountSettle(ctx, aid)
	require.NoError(t, err)
	require.True(t, od)

	require.Empty(t, runwayEvents(t, ctx))
}
