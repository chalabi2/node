package v3_0_0

import (
	"testing"

	"cosmossdk.io/log"
	sdkmodule "github.com/cosmos/cosmos-sdk/types/module"
	"github.com/stretchr/testify/require"

	mv1 "pkg.akt.dev/go/node/market/v1"
	ptypes "pkg.akt.dev/go/node/provider/v1beta4"

	utypes "pkg.akt.dev/node/v3/upgrades/types"
	verificationtypes "pkg.akt.dev/node/v3/x/verification/types"
)

func TestStoreLoaderAddsVerificationStore(t *testing.T) {
	storeUpgrades := (&upgrade{}).StoreLoader()

	require.NotNil(t, storeUpgrades)
	require.ElementsMatch(t, []string{verificationtypes.StoreKey}, storeUpgrades.Added)
	require.Empty(t, storeUpgrades.Deleted)
}

func TestUpgradeRegistersVerificationStoreAndMigrations(t *testing.T) {
	upgrades := utypes.GetUpgradesList()
	require.Contains(t, upgrades, UpgradeName)

	up, err := upgrades[UpgradeName](log.NewNopLogger(), nil)
	require.NoError(t, err)
	require.Contains(t, up.StoreLoader().Added, verificationtypes.StoreKey)

	var providerVersions []uint64
	utypes.ModuleMigrations(ptypes.ModuleName, nil, func(_ string, version uint64, _ sdkmodule.MigrationHandler) {
		providerVersions = append(providerVersions, version)
	})
	require.ElementsMatch(t, []uint64{3}, providerVersions)

	var marketVersions []uint64
	utypes.ModuleMigrations(mv1.ModuleName, nil, func(_ string, version uint64, _ sdkmodule.MigrationHandler) {
		marketVersions = append(marketVersions, version)
	})
	require.ElementsMatch(t, []uint64{8, 9}, marketVersions)
}
