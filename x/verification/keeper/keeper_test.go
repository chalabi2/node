package keeper

import (
	"testing"

	storetypes "cosmossdk.io/store/types"
	"github.com/stretchr/testify/require"

	vtypes "pkg.akt.dev/go/node/verification/v1"

	moduletypes "pkg.akt.dev/node/v3/x/verification/types"
)

func TestKeeperShell(t *testing.T) {
	key := storetypes.NewKVStoreKey(moduletypes.StoreKey)
	k := NewKeeper(nil, key, defaultTestDependencies())

	require.Equal(t, key, k.StoreKey())
	require.Nil(t, k.Codec())
	require.NotNil(t, k.NewQuerier())

	got, err := k.Settle(SettlementInput{
		Path:             SettlementPathAttestationExpired,
		FaultAttribution: vtypes.FaultAttributionNoFault,
	})
	require.NoError(t, err)
	require.Equal(t, SettlementResult{
		FeeStatus:             vtypes.FeeStatusReleasedToAuditor,
		DepositStatus:         vtypes.DepositStatusReturnedToAuditor,
		ProviderDepositStatus: vtypes.ProviderDepositStatusReturnedToProvider,
	}, got)
}

func TestNewKeeperRequiresDependencies(t *testing.T) {
	key := storetypes.NewKVStoreKey(moduletypes.StoreKey)
	tests := []struct {
		name    string
		message string
		mutate  func(*Dependencies)
	}{
		{
			name:    "authority",
			message: "verification: authority is required",
			mutate:  func(deps *Dependencies) { deps.Authority = "" },
		},
		{
			name:    "account keeper",
			message: "verification: account keeper is required",
			mutate:  func(deps *Dependencies) { deps.Account = nil },
		},
		{
			name:    "bank keeper",
			message: "verification: bank keeper is required",
			mutate:  func(deps *Dependencies) { deps.Bank = nil },
		},
		{
			name:    "distribution keeper",
			message: "verification: distribution keeper is required",
			mutate:  func(deps *Dependencies) { deps.Distribution = nil },
		},
		{
			name:    "provider keeper",
			message: "verification: provider keeper is required",
			mutate:  func(deps *Dependencies) { deps.Provider = nil },
		},
		{
			name:    "market keeper",
			message: "verification: market stats keeper is required",
			mutate:  func(deps *Dependencies) { deps.Market = nil },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := defaultTestDependencies()
			test.mutate(&deps)
			require.PanicsWithValue(t, test.message, func() {
				NewKeeper(nil, key, deps)
			})
		})
	}
}
