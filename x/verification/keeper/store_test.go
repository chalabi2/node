package keeper

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	"cosmossdk.io/store"
	storemetrics "cosmossdk.io/store/metrics"
	storetypes "cosmossdk.io/store/types"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	dbm "github.com/cosmos/cosmos-db"
	sdk "github.com/cosmos/cosmos-sdk/types"
	testutilmod "github.com/cosmos/cosmos-sdk/types/module/testutil"

	mv1 "pkg.akt.dev/go/node/market/v1"
	ptypes "pkg.akt.dev/go/node/provider/v1beta4"
	vtypes "pkg.akt.dev/go/node/verification/v1"
	"pkg.akt.dev/go/testutil"

	moduletypes "pkg.akt.dev/node/v2/x/verification/types"
)

func TestStoreRecordsAndIndexes(t *testing.T) {
	ctx, k := setupStoreKeeper(t)
	provider := testutil.AccAddress(t)
	auditor := testutil.AccAddress(t)

	require.NoError(t, k.SetAuditor(ctx, auditorRecord(auditor)))
	require.NoError(t, k.SetAttestation(ctx, attestationRecord(provider, auditor)))
	require.NoError(t, k.SetProviderBond(ctx, providerBondRecord(provider)))
	require.NoError(t, k.SetProviderSnapshot(ctx, providerSnapshotRecord(provider)))
	require.NoError(t, k.SetAuditEscrow(ctx, auditEscrowRecord(provider, auditor)))
	require.NoError(t, k.SetProviderVerificationGrace(ctx, graceRecord(provider)))
	k.SetDiscrepancy(ctx, discrepancyRecord(provider, auditor, testutil.AccAddress(t)))

	gotAuditor, found := k.GetAuditor(ctx, auditor)
	require.True(t, found)
	require.Equal(t, auditor.String(), gotAuditor.Address)

	gotAttestation, found := k.GetAttestation(ctx, provider, auditor)
	require.True(t, found)
	require.Equal(t, vtypes.TierVerified, gotAttestation.Tier)

	var byAuditor []vtypes.AttestationRecord
	k.WithAuditorAttestations(ctx, auditor, func(record vtypes.AttestationRecord) bool {
		byAuditor = append(byAuditor, record)
		return false
	})
	require.Equal(t, []vtypes.AttestationRecord{gotAttestation}, byAuditor)

	var escrows []vtypes.AuditEscrowRecord
	k.WithProviderAuditEscrows(ctx, provider, vtypes.AuditEscrowStatusOpen, func(record vtypes.AuditEscrowRecord) bool {
		escrows = append(escrows, record)
		return false
	})
	require.Len(t, escrows, 1)
	require.Equal(t, uint64(7), escrows[0].ID)

	grace, found := k.GetProviderVerificationGrace(ctx, provider)
	require.True(t, found)
	require.Equal(t, uint64(9), grace.ID)

	discrepancy, found := k.GetDiscrepancy(ctx, 11)
	require.True(t, found)
	require.Equal(t, provider.String(), discrepancy.Provider)
}

func TestRequiredProviderBondUsesSpecUnitConversions(t *testing.T) {
	resources := vtypes.ResourceSummary{
		TotalGPUs:      2,
		TotalVCPUs:     4,
		TotalMemoryMB:  2048,
		TotalStorageMB: 1048576,
	}

	got := RequiredProviderBond(DefaultParams(), vtypes.TierVerified, resources)
	require.Equal(t, sdk.NewInt64Coin(bondDenom, 104500000), got)
}

func TestRequiredProviderBondHandlesMaximumResourceSummary(t *testing.T) {
	require.NotPanics(t, func() {
		got := RequiredProviderBond(DefaultParams(), vtypes.TierTrusted, vtypes.ResourceSummary{
			TotalGPUs:      math.MaxUint32,
			TotalVCPUs:     math.MaxUint32,
			TotalMemoryMB:  math.MaxUint64,
			TotalStorageMB: math.MaxUint64,
		})
		require.True(t, got.IsValid())
		require.True(t, got.IsPositive())
	})
}

func TestGetProviderVerificationGraceReturnsLatestRecord(t *testing.T) {
	ctx, k := setupStoreKeeper(t)
	provider := testutil.AccAddress(t)

	first := graceRecord(provider)
	first.ID = 1
	first.StartedAt = ctx.BlockTime()
	require.NoError(t, k.SetProviderVerificationGrace(ctx, first))

	second := graceRecord(provider)
	second.ID = 2
	second.StartedAt = ctx.BlockTime().Add(time.Second)
	require.NoError(t, k.SetProviderVerificationGrace(ctx, second))

	got, found := k.GetProviderVerificationGrace(ctx, provider)
	require.True(t, found)
	require.Equal(t, second.ID, got.ID)
}

func setupStoreKeeper(t testing.TB) (sdk.Context, Keeper) {
	return setupStoreKeeperWithOptions(t)
}

func setupStoreKeeperWithOptions(t testing.TB, opts ...Option) (sdk.Context, Keeper) {
	t.Helper()

	cfg := testutilmod.MakeTestEncodingConfig()
	key := storetypes.NewKVStoreKey(moduletypes.StoreKey)
	db := dbm.NewMemDB()

	ms := store.NewCommitMultiStore(db, log.NewNopLogger(), storemetrics.NewNoOpMetrics())
	ms.MountStoreWithDB(key, storetypes.StoreTypeIAVL, db)

	err := ms.LoadLatestVersion()
	require.NoError(t, err)

	ctx := sdk.NewContext(ms, tmproto.Header{Time: now()}, false, testutil.Logger(t))
	return ctx, NewKeeper(cfg.Codec, key, defaultTestDependencies(), opts...)
}

func defaultTestDependencies() Dependencies {
	bank := &recordingBank{}
	return Dependencies{
		Authority:    "gov",
		Account:      &testAccountKeeper{},
		Bank:         bank,
		Distribution: bank,
		Provider:     testProviderKeeper{},
		Market:       testMarketStatsKeeper{},
	}
}

type testAccountKeeper struct {
	account sdk.ModuleAccountI
}

func (k *testAccountKeeper) GetModuleAccount(context.Context, string) sdk.ModuleAccountI {
	return k.account
}

func (k *testAccountKeeper) SetModuleAccount(_ context.Context, account sdk.ModuleAccountI) {
	k.account = account
}

type testProviderKeeper struct{}

func (testProviderKeeper) Get(_ sdk.Context, id sdk.Address) (ptypes.Provider, bool) {
	return ptypes.Provider{Owner: id.String()}, true
}

func (testProviderKeeper) GetRegistrationTime(ctx sdk.Context, _ sdk.Address) (time.Time, bool) {
	return ctx.BlockTime().Add(-100 * 365 * 24 * time.Hour), true
}

type testMarketStatsKeeper struct{}

func (testMarketStatsKeeper) GetProviderLeaseStats(sdk.Context, sdk.Address) (uint64, map[mv1.LeaseClosedReason]uint64, bool) {
	return 10000, nil, true
}

var _ AccountKeeper = (*testAccountKeeper)(nil)
var _ BankKeeper = (*recordingBank)(nil)
var _ DistributionKeeper = (*recordingBank)(nil)
var _ ProviderKeeper = testProviderKeeper{}
var _ MarketStatsKeeper = testMarketStatsKeeper{}

func auditorRecord(auditor sdk.AccAddress) vtypes.AuditorRecord {
	return vtypes.AuditorRecord{
		Address:            auditor.String(),
		Status:             vtypes.AuditorStatusActive,
		MaxAttestationTier: vtypes.TierTrusted,
		BondAmount:         sdk.NewInt64Coin(bondDenom, 100000000000),
		BondStatus:         vtypes.BondStatusBonded,
		RegisteredAt:       now(),
		RenewalDeadline:    now().Add(365 * 24 * time.Hour),
	}
}

func attestationRecord(provider, auditor sdk.AccAddress) vtypes.AttestationRecord {
	return vtypes.AttestationRecord{
		Provider:         provider.String(),
		Auditor:          auditor.String(),
		Tier:             vtypes.TierVerified,
		EvidenceHash:     []byte("evidence"),
		Fee:              sdk.NewInt64Coin(bondDenom, 10000000),
		FeeStatus:        vtypes.FeeStatusEscrowed,
		CreatedAt:        now(),
		ExpiresAt:        now().Add(90 * 24 * time.Hour),
		Status:           vtypes.AttestationStatusValid,
		Deposit:          sdk.NewInt64Coin(bondDenom, 100000000),
		DepositStatus:    vtypes.DepositStatusEscrowed,
		AuditEscrowID:    7,
		FaultAttribution: vtypes.FaultAttributionNoFault,
	}
}

func providerBondRecord(provider sdk.AccAddress) vtypes.ProviderBondRecord {
	return vtypes.ProviderBondRecord{
		Provider:     provider.String(),
		BondedAmount: sdk.NewInt64Coin(bondDenom, 200000000),
	}
}

func providerSnapshotRecord(provider sdk.AccAddress) vtypes.ProviderSnapshotRecord {
	return vtypes.ProviderSnapshotRecord{
		Provider:     provider.String(),
		SnapshotHash: []byte("snapshot"),
		ResourceSummary: vtypes.ResourceSummary{
			TotalGPUs:      2,
			TotalVCPUs:     4,
			TotalMemoryMB:  2048,
			TotalStorageMB: 1048576,
		},
		PostedAt:           now(),
		SnapshotTimestamp:  now(),
		ComplianceDeadline: now().Add(24 * time.Hour),
		Suspended:          false,
	}
}

func auditEscrowRecord(provider, auditor sdk.AccAddress) vtypes.AuditEscrowRecord {
	return vtypes.AuditEscrowRecord{
		ID:                    7,
		Provider:              provider.String(),
		ConsumedByAuditor:     auditor.String(),
		RequestedTier:         vtypes.TierVerified,
		Fee:                   sdk.NewInt64Coin(bondDenom, 10000000),
		FeeStatus:             vtypes.FeeStatusEscrowed,
		ProviderDeposit:       sdk.NewInt64Coin(bondDenom, 100000000),
		ProviderDepositStatus: vtypes.ProviderDepositStatusEscrowed,
		Status:                vtypes.AuditEscrowStatusOpen,
		OpenedAt:              now(),
		ExpiresAt:             now().Add(24 * time.Hour),
	}
}

func graceRecord(provider sdk.AccAddress) vtypes.ProviderVerificationGraceRecord {
	return vtypes.ProviderVerificationGraceRecord{
		ID:                   9,
		Provider:             provider.String(),
		PreservedTier:        vtypes.TierVerified,
		SourceDiscrepancyIDs: []uint64{11},
		StartedAt:            now(),
		ExpiresAt:            now().Add(14 * 24 * time.Hour),
		Status:               vtypes.VerificationGraceStatusActive,
	}
}

func discrepancyRecord(provider, auditorA, auditorB sdk.AccAddress) vtypes.DiscrepancyEvent {
	return vtypes.DiscrepancyEvent{
		ID:               11,
		Provider:         provider.String(),
		AuditorA:         auditorA.String(),
		AuditorATier:     vtypes.TierVerified,
		AuditorB:         auditorB.String(),
		AuditorBTier:     vtypes.TierEstablished,
		Timestamp:        now(),
		ResolutionStatus: vtypes.DiscrepancyStatusPending,
	}
}

func now() time.Time {
	return time.Unix(1700000000, 0).UTC()
}
