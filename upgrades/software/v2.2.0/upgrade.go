// Package v2_2_0
// nolint revive
package v2_2_0

import (
	"context"
	"fmt"

	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"
	upgradetypes "cosmossdk.io/x/upgrade/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"

	dvbeta "pkg.akt.dev/go/node/deployment/v1beta4"
	mvbeta "pkg.akt.dev/go/node/market/v1beta5"

	apptypes "pkg.akt.dev/node/v2/app/types"
	utypes "pkg.akt.dev/node/v2/upgrades/types"
)

const (
	UpgradeName = "v2.2.0"
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

// StoreLoader adds no store keys: the AEP-87 volume indexes live under fresh
// prefixes of the existing deployment and market stores.
func (up *upgrade) StoreLoader() *storetypes.StoreUpgrades {
	return &storetypes.StoreUpgrades{
		Added:   nil,
		Deleted: nil,
	}
}

func (up *upgrade) UpgradeHandler() upgradetypes.UpgradeHandler {
	return func(ctx context.Context, plan upgradetypes.Plan, fromVM module.VersionMap) (module.VersionMap, error) {
		sctx := sdk.UnwrapSDKContext(ctx)

		toVM, err := up.MM.RunMigrations(ctx, up.Configurator, fromVM)
		if err != nil {
			return toVM, err
		}

		// Seed the AEP-87 deployment volume params. The stored params
		// decode with the volume fields zero; MaxVolumeSize == 0 marks
		// them unseeded (Params.Validate rejects it).
		dparams, err := up.Keepers.Akash.Deployment.GetParams(sctx)
		if err != nil {
			return toVM, fmt.Errorf("failed to get deployment params: %w", err)
		}

		if dparams.MaxVolumeSize == 0 {
			dparams.MaxVolumeSize = dvbeta.DefaultMaxVolumeSize
			dparams.MaxVolumeRetention = dvbeta.DefaultMaxVolumeRetention
			dparams.MaxVolumeReplicas = dvbeta.DefaultMaxVolumeReplicas
			if err = up.Keepers.Akash.Deployment.SetParams(sctx, dparams); err != nil {
				return toVM, fmt.Errorf("failed to set deployment params: %w", err)
			}
		}

		// Seed the AEP-87 market volume params and the lease resize params.
		// Both features ship dark: VolumeOrdersEnabled and LeaseResizeEnabled
		// stay false until governance flips them.
		mparams, err := up.Keepers.Akash.Market.GetParams(sctx)
		if err != nil {
			return toVM, fmt.Errorf("failed to get market params: %w", err)
		}

		seeded := false

		if mparams.MinVolumeReclamationWindow == 0 {
			mparams.VolumeOrdersEnabled = mvbeta.DefaultVolumeOrdersEnabled
			mparams.MinVolumeReclamationWindow = mvbeta.DefaultMinVolumeReclamationWindow
			seeded = true
		}

		if mparams.LeaseResizeTTL == 0 {
			mparams.LeaseResizeEnabled = mvbeta.DefaultLeaseResizeEnabled
			mparams.LeaseResizeTTL = mvbeta.DefaultLeaseResizeTTL
			seeded = true
		}

		if seeded {
			if err = up.Keepers.Akash.Market.SetParams(sctx, mparams); err != nil {
				return toVM, fmt.Errorf("failed to set market params: %w", err)
			}
		}

		// Initialize the AEP-87 volume indexes (deployment 0x13 vid index
		// and replica backref counters, market 0x15/0x16 attachment
		// indexes). No volume groups can exist before this upgrade, so
		// both rebuilds write nothing; running them makes the empty
		// initial state explicit and mirrors genesis import.
		if err = up.Keepers.Akash.Deployment.RebuildVolumeIndexes(sctx); err != nil {
			return toVM, fmt.Errorf("failed to rebuild deployment volume indexes: %w", err)
		}

		if err = up.Keepers.Akash.Market.RebuildAttachmentIndexes(sctx); err != nil {
			return toVM, fmt.Errorf("failed to rebuild market attachment indexes: %w", err)
		}

		return toVM, nil
	}
}
