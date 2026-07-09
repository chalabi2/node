package v2_2_0

import (
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkmodule "github.com/cosmos/cosmos-sdk/types/module"
	"github.com/cosmos/gogoproto/proto"

	mvbeta "pkg.akt.dev/go/node/market/v2beta1"

	utypes "pkg.akt.dev/node/v2/upgrades/types"
	mkeys "pkg.akt.dev/node/v2/x/market/keeper/keys"
)

type marketMigrations struct {
	utypes.Migrator
}

func newMarketMigration(m utypes.Migrator) utypes.Migration {
	return marketMigrations{Migrator: m}
}

func (m marketMigrations) GetHandler() sdkmodule.MigrationHandler {
	return m.handler
}

// handler migrates market from version 9 to 10: an explicit re-encode of the
// orders and bids stores from market/v1beta5 to market/v2beta1 (AEP-87).
// v2beta1 is a pure nullable-additive superset of v1beta5 (the embedded
// group spec moves to deployment/v1beta5, itself a superset of v1beta4), so
// decoding the stored bytes with the v2beta1 types and re-serializing is
// lossless and, by proto3 rules, byte-identical. The re-encode is still
// written out explicitly rather than relying on accidental wire
// compatibility.
//
// Leases are market/v1 (unchanged) and the params item is re-written by the
// upgrade handler when it seeds the AEP-87 volume params. The new attachment
// indexes (0x15/0x16) are initialized — necessarily empty — by the upgrade
// handler after migrations run.
func (m marketMigrations) handler(ctx sdk.Context) error {
	store := ctx.KVStore(m.StoreKey())
	cdc := m.Codec()

	if err := utypes.MigrateValue(store, cdc, mkeys.OrderPrefixNew, nil, nil, migrateOrder); err != nil {
		return err
	}

	return utypes.MigrateValue(store, cdc, mkeys.BidPrefixNew, nil, nil, migrateBid)
}

func migrateOrder(fromBz []byte, cdc codec.BinaryCodec) proto.Message {
	var order mvbeta.Order
	cdc.MustUnmarshal(fromBz, &order)

	return &order
}

func migrateBid(fromBz []byte, cdc codec.BinaryCodec) proto.Message {
	var bid mvbeta.Bid
	cdc.MustUnmarshal(fromBz, &bid)

	return &bid
}
