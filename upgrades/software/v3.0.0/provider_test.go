package v3_0_0

import (
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
	"github.com/stretchr/testify/require"

	types "pkg.akt.dev/go/node/provider/v1beta4"
	"pkg.akt.dev/go/testutil"

	utypes "pkg.akt.dev/node/v3/upgrades/types"
	"pkg.akt.dev/node/v3/x/provider/keeper"
)

func TestProviderMigrationBackfillsRegistrationsAtUpgradeBlockTime(t *testing.T) {
	ctx, kpr := setupProviderMigrationKeeper(t)
	ctx = ctx.WithBlockTime(time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC))
	store := ctx.KVStore(kpr.StoreKey())

	provider := testutil.Provider(t)
	owner, err := sdk.AccAddressFromBech32(provider.Owner)
	require.NoError(t, err)
	store.Set(keeper.ProviderKey(owner), kpr.Codec().MustMarshal(&provider))

	migration := newProviderMigration(utypes.NewMigrator(kpr.Codec(), kpr.StoreKey()))
	require.NoError(t, migration.GetHandler()(ctx))

	registration, found := kpr.GetRegistration(ctx, owner)
	require.True(t, found)
	require.Equal(t, types.ProviderRegistration{
		Owner:        provider.Owner,
		RegisteredAt: ctx.BlockTime(),
	}, registration)
}

func TestProviderMigrationKeepsExistingRegistrations(t *testing.T) {
	ctx, kpr := setupProviderMigrationKeeper(t)
	ctx = ctx.WithBlockTime(time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC))
	store := ctx.KVStore(kpr.StoreKey())

	provider := testutil.Provider(t)
	owner, err := sdk.AccAddressFromBech32(provider.Owner)
	require.NoError(t, err)
	store.Set(keeper.ProviderKey(owner), kpr.Codec().MustMarshal(&provider))

	existing := types.ProviderRegistration{
		Owner:        provider.Owner,
		RegisteredAt: ctx.BlockTime().Add(-24 * time.Hour),
	}
	require.NoError(t, kpr.SetRegistration(ctx, existing))

	migration := newProviderMigration(utypes.NewMigrator(kpr.Codec(), kpr.StoreKey()))
	require.NoError(t, migration.GetHandler()(ctx))

	registration, found := kpr.GetRegistration(ctx, owner)
	require.True(t, found)
	require.Equal(t, existing, registration)
}

func TestProviderMigrationBackfillsMissingRegistrationsOnly(t *testing.T) {
	ctx, kpr := setupProviderMigrationKeeper(t)
	ctx = ctx.WithBlockTime(time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC))
	store := ctx.KVStore(kpr.StoreKey())

	providerA := testutil.Provider(t)
	providerB := testutil.Provider(t)
	ownerA, err := sdk.AccAddressFromBech32(providerA.Owner)
	require.NoError(t, err)
	ownerB, err := sdk.AccAddressFromBech32(providerB.Owner)
	require.NoError(t, err)
	store.Set(keeper.ProviderKey(ownerA), kpr.Codec().MustMarshal(&providerA))
	store.Set(keeper.ProviderKey(ownerB), kpr.Codec().MustMarshal(&providerB))

	existing := types.ProviderRegistration{
		Owner:        providerA.Owner,
		RegisteredAt: ctx.BlockTime().Add(-24 * time.Hour),
	}
	require.NoError(t, kpr.SetRegistration(ctx, existing))

	migration := newProviderMigration(utypes.NewMigrator(kpr.Codec(), kpr.StoreKey()))
	require.NoError(t, migration.GetHandler()(ctx))

	registrationA, found := kpr.GetRegistration(ctx, ownerA)
	require.True(t, found)
	require.Equal(t, existing, registrationA)

	registrationB, found := kpr.GetRegistration(ctx, ownerB)
	require.True(t, found)
	require.Equal(t, types.ProviderRegistration{
		Owner:        providerB.Owner,
		RegisteredAt: ctx.BlockTime(),
	}, registrationB)
}

func setupProviderMigrationKeeper(t testing.TB) (sdk.Context, keeper.IKeeper) {
	t.Helper()

	cfg := testutilmod.MakeTestEncodingConfig()
	key := storetypes.NewKVStoreKey(types.StoreKey)
	db := dbm.NewMemDB()

	ms := store.NewCommitMultiStore(db, log.NewNopLogger(), storemetrics.NewNoOpMetrics())
	ms.MountStoreWithDB(key, storetypes.StoreTypeIAVL, db)
	require.NoError(t, ms.LoadLatestVersion())

	ctx := sdk.NewContext(ms, tmproto.Header{}, false, testutil.Logger(t))
	return ctx, keeper.NewKeeper(cfg.Codec, key, "")
}
