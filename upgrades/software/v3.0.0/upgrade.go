// Package v3_0_0
// nolint revive
package v3_0_0

import (
	"context"
	"fmt"

	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"
	upgradetypes "cosmossdk.io/x/upgrade/types"
	"github.com/cosmos/cosmos-sdk/types/module"

	apptypes "pkg.akt.dev/node/v3/app/types"
	utypes "pkg.akt.dev/node/v3/upgrades/types"
	verificationtypes "pkg.akt.dev/node/v3/x/verification/types"
)

const (
	UpgradeName = "v3.0.0"
)

type upgrade struct {
	*apptypes.App
	log log.Logger
}

var _ utypes.IUpgrade = (*upgrade)(nil)

func initUpgrade(log log.Logger, app *apptypes.App) (utypes.IUpgrade, error) {
	up := &upgrade{
		App: app,
		log: log.With("module", fmt.Sprintf("upgrade/%s", UpgradeName)),
	}

	return up, nil
}

func (up *upgrade) StoreLoader() *storetypes.StoreUpgrades {
	return &storetypes.StoreUpgrades{
		Added:   []string{verificationtypes.StoreKey},
		Deleted: []string{},
	}
}

func (up *upgrade) UpgradeHandler() upgradetypes.UpgradeHandler {
	return func(ctx context.Context, _ upgradetypes.Plan, fromVM module.VersionMap) (module.VersionMap, error) {
		return up.MM.RunMigrations(ctx, up.Configurator, fromVM)
	}
}
