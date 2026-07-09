package v2_2_0

import (
	"testing"
	"time"

	"cosmossdk.io/collections"
	storetypes "cosmossdk.io/store/types"
	sdktestutil "github.com/cosmos/cosmos-sdk/testutil"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/runtime"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	dv1 "pkg.akt.dev/go/node/deployment/v1"
	dvbeta4 "pkg.akt.dev/go/node/deployment/v1beta4"
	dvbeta "pkg.akt.dev/go/node/deployment/v1beta5"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta5 "pkg.akt.dev/go/node/market/v1beta5"
	mvbeta "pkg.akt.dev/go/node/market/v2beta1"
	"pkg.akt.dev/go/testutil"

	utypes "pkg.akt.dev/node/v2/upgrades/types"
	dkeys "pkg.akt.dev/node/v2/x/deployment/keeper/keys"
	mkeys "pkg.akt.dev/node/v2/x/market/keeper/keys"
)

// The v2.2.0 migrations re-encode stores written with the pre-upgrade proto
// versions (deployment/v1beta4, market/v1beta5) into the AEP-87 versions
// (deployment/v1beta5, market/v2beta1). These tests write fixtures encoded
// with the OLD generated types — exactly the bytes a pre-upgrade keeper
// stores — run the migration handlers, and assert the records decode
// losslessly with the new types, with the added volume fields default-empty
// and the raw bytes unchanged (the nullable-additive wire-compat contract).

func setupMigrationTest(t *testing.T, storeKey string) (sdk.Context, *storetypes.KVStoreKey, codec.BinaryCodec) {
	t.Helper()

	skey := storetypes.NewKVStoreKey(storeKey)
	tctx := sdktestutil.DefaultContextWithDB(t, skey, storetypes.NewTransientStoreKey("transient_test"))
	cdc := codec.NewProtoCodec(codectypes.NewInterfaceRegistry())

	return tctx.Ctx, skey, cdc
}

// storeSnapshot returns a copy of every key/value pair under the prefix.
func storeSnapshot(t *testing.T, ctx sdk.Context, skey storetypes.StoreKey, prefix []byte) map[string][]byte {
	t.Helper()

	snap := make(map[string][]byte)

	iter := storetypes.KVStorePrefixIterator(ctx.KVStore(skey), prefix)
	defer func() {
		require.NoError(t, iter.Close())
	}()

	for ; iter.Valid(); iter.Next() {
		snap[string(iter.Key())] = append([]byte(nil), iter.Value()...)
	}

	return snap
}

func TestDeploymentMigrationReencodesGroups(t *testing.T) {
	ctx, skey, cdc := setupMigrationTest(t, dv1.StoreKey)

	// Write v1beta4-encoded groups through an old-typed collections map:
	// the same prefix, key codec, and value encoding the pre-upgrade
	// keeper uses.
	sb := collections.NewSchemaBuilder(runtime.NewKVStoreService(skey))
	oldGroups := collections.NewMap(sb, collections.NewPrefix(dkeys.GroupPrefix), "groups", dkeys.GroupPrimaryKeyCodec, codec.CollValue[dvbeta4.Group](cdc))
	_, err := sb.Build()
	require.NoError(t, err)

	fixtures := make([]dvbeta4.Group, 0, 4)
	did := testutil.DeploymentID(t)
	for gseq := uint32(1); gseq <= 4; gseq++ {
		group := testutil.DeploymentGroup(t, did, gseq)
		require.NoError(t, oldGroups.Set(ctx, dkeys.GroupIDToKey(group.ID), group))
		fixtures = append(fixtures, group)
	}

	before := storeSnapshot(t, ctx, skey, dkeys.GroupPrefix)
	require.Len(t, before, len(fixtures))

	mig := newDeploymentMigration(utypes.NewMigrator(cdc, skey))
	require.NoError(t, mig.GetHandler()(ctx))

	// Nullable-additive contract: the re-encode is byte-identical.
	require.Equal(t, before, storeSnapshot(t, ctx, skey, dkeys.GroupPrefix))

	// The records decode losslessly with the v1beta5 type.
	sb = collections.NewSchemaBuilder(runtime.NewKVStoreService(skey))
	newGroups := collections.NewMap(sb, collections.NewPrefix(dkeys.GroupPrefix), "groups", dkeys.GroupPrimaryKeyCodec, codec.CollValue[dvbeta.Group](cdc))
	_, err = sb.Build()
	require.NoError(t, err)

	for _, old := range fixtures {
		group, err := newGroups.Get(ctx, dkeys.GroupIDToKey(old.ID))
		require.NoError(t, err)

		require.Equal(t, old.ID, group.ID)
		require.Equal(t, old.State.String(), group.State.String())
		require.Equal(t, old.GroupSpec.Name, group.GroupSpec.Name)
		require.Equal(t, old.GroupSpec.Requirements, group.GroupSpec.Requirements)
		require.Len(t, group.GroupSpec.Resources, len(old.GroupSpec.Resources))
		for i, res := range old.GroupSpec.Resources {
			require.Equal(t, res.Resources, group.GroupSpec.Resources[i].Resources)
			require.Equal(t, res.Count, group.GroupSpec.Resources[i].Count)
			require.Equal(t, res.Price, group.GroupSpec.Resources[i].Price)
		}

		// The AEP-87 additions decode default-empty.
		require.Nil(t, group.GroupSpec.Volume)
		for _, res := range group.GroupSpec.Resources {
			require.Nil(t, res.Volumes)
		}
	}
}

func TestMarketMigrationReencodesOrdersAndBids(t *testing.T) {
	ctx, skey, cdc := setupMigrationTest(t, mv1.StoreKey)

	// Write v1beta5-encoded orders and bids through old-typed collections
	// maps at the keeper's prefixes.
	sb := collections.NewSchemaBuilder(runtime.NewKVStoreService(skey))
	oldOrders := collections.NewMap(sb, collections.NewPrefix(mkeys.OrderPrefixNew), "orders", mkeys.OrderPrimaryKeyCodec, codec.CollValue[mvbeta5.Order](cdc))
	oldBids := collections.NewMap(sb, collections.NewPrefix(mkeys.BidPrefixNew), "bids", mkeys.BidPrimaryKeyCodec, codec.CollValue[mvbeta5.Bid](cdc))
	_, err := sb.Build()
	require.NoError(t, err)

	reclamationWindow := 48 * time.Hour

	oldOrder := mvbeta5.Order{
		ID:        testutil.OrderID(t),
		State:     mvbeta5.OrderOpen,
		Spec:      testutil.GroupSpec(t),
		CreatedAt: 100,
	}
	require.NoError(t, oldOrders.Set(ctx, mkeys.OrderIDToKey(oldOrder.ID), oldOrder))

	oldBid := mvbeta5.Bid{
		ID:        testutil.BidID(t),
		State:     mvbeta5.BidOpen,
		Price:     testutil.AkashDecCoinRandom(t),
		CreatedAt: 101,
		ResourcesOffer: mvbeta5.ResourcesOffer{
			{
				Resources: testutil.Resources(t)[0].Resources,
				Count:     1,
			},
		},
		ReclamationWindow: &reclamationWindow,
	}
	require.NoError(t, oldBids.Set(ctx, mkeys.BidIDToKey(oldBid.ID), oldBid))

	beforeOrders := storeSnapshot(t, ctx, skey, mkeys.OrderPrefixNew)
	beforeBids := storeSnapshot(t, ctx, skey, mkeys.BidPrefixNew)
	require.Len(t, beforeOrders, 1)
	require.Len(t, beforeBids, 1)

	mig := newMarketMigration(utypes.NewMigrator(cdc, skey))
	require.NoError(t, mig.GetHandler()(ctx))

	// Nullable-additive contract: the re-encode is byte-identical.
	require.Equal(t, beforeOrders, storeSnapshot(t, ctx, skey, mkeys.OrderPrefixNew))
	require.Equal(t, beforeBids, storeSnapshot(t, ctx, skey, mkeys.BidPrefixNew))

	// The records decode losslessly with the v2beta1 types.
	sb = collections.NewSchemaBuilder(runtime.NewKVStoreService(skey))
	newOrders := collections.NewMap(sb, collections.NewPrefix(mkeys.OrderPrefixNew), "orders", mkeys.OrderPrimaryKeyCodec, codec.CollValue[mvbeta.Order](cdc))
	newBids := collections.NewMap(sb, collections.NewPrefix(mkeys.BidPrefixNew), "bids", mkeys.BidPrimaryKeyCodec, codec.CollValue[mvbeta.Bid](cdc))
	_, err = sb.Build()
	require.NoError(t, err)

	order, err := newOrders.Get(ctx, mkeys.OrderIDToKey(oldOrder.ID))
	require.NoError(t, err)
	require.Equal(t, oldOrder.ID, order.ID)
	require.Equal(t, oldOrder.State.String(), order.State.String())
	require.Equal(t, oldOrder.CreatedAt, order.CreatedAt)
	require.Equal(t, oldOrder.Spec.Name, order.Spec.Name)
	require.Equal(t, oldOrder.Spec.Requirements, order.Spec.Requirements)
	require.Len(t, order.Spec.Resources, len(oldOrder.Spec.Resources))
	require.Nil(t, order.Spec.Volume)

	bid, err := newBids.Get(ctx, mkeys.BidIDToKey(oldBid.ID))
	require.NoError(t, err)
	require.Equal(t, oldBid.ID, bid.ID)
	require.Equal(t, oldBid.State.String(), bid.State.String())
	require.Equal(t, oldBid.Price, bid.Price)
	require.Equal(t, oldBid.CreatedAt, bid.CreatedAt)
	require.Len(t, bid.ResourcesOffer, len(oldBid.ResourcesOffer))
	require.Equal(t, oldBid.ResourcesOffer[0].Resources, bid.ResourcesOffer[0].Resources)
	require.Equal(t, oldBid.ResourcesOffer[0].Count, bid.ResourcesOffer[0].Count)
	require.NotNil(t, bid.ReclamationWindow)
	require.Equal(t, reclamationWindow, *bid.ReclamationWindow)
}
