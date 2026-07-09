package v2_2_0

import (
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkmodule "github.com/cosmos/cosmos-sdk/types/module"
	"github.com/cosmos/gogoproto/proto"

	dvbeta "pkg.akt.dev/go/node/deployment/v1beta5"

	utypes "pkg.akt.dev/node/v2/upgrades/types"
	dkeys "pkg.akt.dev/node/v2/x/deployment/keeper/keys"
)

type deploymentMigrations struct {
	utypes.Migrator
}

func newDeploymentMigration(m utypes.Migrator) utypes.Migration {
	return deploymentMigrations{Migrator: m}
}

func (m deploymentMigrations) GetHandler() sdkmodule.MigrationHandler {
	return m.handler
}

// handler migrates deployment from version 8 to 9: an explicit re-encode of
// the groups store from deployment/v1beta4 to deployment/v1beta5 (AEP-87).
// v1beta5 is a pure nullable-additive superset of v1beta4 — the added volume
// fields are absent on the wire for every pre-upgrade record — so decoding
// the stored bytes with the v1beta5 type and re-serializing is lossless and,
// by proto3 rules, byte-identical. The re-encode is still written out
// explicitly rather than relying on accidental wire compatibility.
//
// Only the groups store carries a re-versioned proto type: deployment
// records are deployment/v1 (unchanged) and the params item is re-written by
// the upgrade handler when it seeds the AEP-87 volume params. The new vid
// index and replica backref counters (0x13 family) are initialized —
// necessarily empty — by the upgrade handler after migrations run.
func (m deploymentMigrations) handler(ctx sdk.Context) error {
	store := ctx.KVStore(m.StoreKey())

	return utypes.MigrateValue(store, m.Codec(), dkeys.GroupPrefix, nil, nil, migrateGroup)
}

func migrateGroup(fromBz []byte, cdc codec.BinaryCodec) proto.Message {
	var group dvbeta.Group
	cdc.MustUnmarshal(fromBz, &group)

	return &group
}
