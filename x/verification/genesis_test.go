package verification

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"

	vtypes "pkg.akt.dev/go/node/verification/v1"
	"pkg.akt.dev/go/testutil"

	"pkg.akt.dev/node/v2/x/verification/keeper"
	moduletypes "pkg.akt.dev/node/v2/x/verification/types"
)

func TestGenesisRoundTrip(t *testing.T) {
	ctx, k := setupGenesisKeeper(t)
	provider := testutil.AccAddress(t)
	auditor := testutil.AccAddress(t)
	auditorB := testutil.AccAddress(t)
	params := keeper.DefaultParams()

	auditorARecord := genesisAuditor(auditor)
	auditorARecord.Status = vtypes.AuditorStatusFrozen
	auditorARecord.BondStatus = vtypes.BondStatusFrozen
	auditorARecord.DiscrepancyCount = 1
	auditorBRecord := genesisAuditor(auditorB)
	auditorBRecord.Status = vtypes.AuditorStatusFrozen
	auditorBRecord.BondStatus = vtypes.BondStatusFrozen
	auditorBRecord.DiscrepancyCount = 1

	attestationA := genesisAttestation(provider, auditor, 7, vtypes.TierVerified, params.MinFeeL2)
	attestationA.Status = vtypes.AttestationStatusVoided
	attestationA.VoidedReason = vtypes.VoidedReasonDiscrepancy
	attestationA.DepositStatus = vtypes.DepositStatusPendingDiscrepancy
	attestationB := genesisAttestation(provider, auditorB, 8, vtypes.TierEstablished, params.MinFeeL3)
	attestationB.Status = vtypes.AttestationStatusVoided
	attestationB.VoidedReason = vtypes.VoidedReasonDiscrepancy
	attestationB.DepositStatus = vtypes.DepositStatusPendingDiscrepancy

	state := &vtypes.GenesisState{
		Params:             params,
		Auditors:           []vtypes.AuditorRecord{auditorARecord, auditorBRecord},
		Attestations:       []vtypes.AttestationRecord{attestationA, attestationB},
		Discrepancies:      []vtypes.DiscrepancyEvent{genesisDiscrepancy(provider, auditor, auditorB)},
		ProviderBonds:      []vtypes.ProviderBondRecord{genesisProviderBond(provider)},
		ProviderSnapshots:  []vtypes.ProviderSnapshotRecord{genesisProviderSnapshot(provider)},
		NextDiscrepancyID:  12,
		AuditEscrows:       []vtypes.AuditEscrowRecord{genesisAuditEscrow(provider, auditor, attestationA), genesisAuditEscrow(provider, auditorB, attestationB)},
		NextAuditEscrowID:  9,
		VerificationGraces: []vtypes.ProviderVerificationGraceRecord{genesisGrace(provider)},
		NextGraceRecordID:  10,
	}

	InitGenesis(ctx, k, state)
	exported := ExportGenesis(ctx, k)

	require.Equal(t, state.Params, exported.Params)
	require.Equal(t, state.NextDiscrepancyID, exported.NextDiscrepancyID)
	require.Equal(t, state.NextAuditEscrowID, exported.NextAuditEscrowID)
	require.Equal(t, state.NextGraceRecordID, exported.NextGraceRecordID)
	require.ElementsMatch(t, state.Auditors, exported.Auditors)
	require.ElementsMatch(t, state.Attestations, exported.Attestations)
	require.ElementsMatch(t, state.Discrepancies, exported.Discrepancies)
	require.ElementsMatch(t, state.ProviderBonds, exported.ProviderBonds)
	require.ElementsMatch(t, state.ProviderSnapshots, exported.ProviderSnapshots)
	require.ElementsMatch(t, state.AuditEscrows, exported.AuditEscrows)
	require.ElementsMatch(t, state.VerificationGraces, exported.VerificationGraces)
}

func TestDefaultGenesisState(t *testing.T) {
	state := DefaultGenesisState()

	require.Equal(t, keeper.DefaultParams(), state.Params)
	require.Equal(t, uint64(1), state.NextDiscrepancyID)
	require.Equal(t, uint64(1), state.NextAuditEscrowID)
	require.Equal(t, uint64(1), state.NextGraceRecordID)
}

func TestInitGenesisPersistsVerificationModuleAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		lazy sdk.ModuleAccountI
	}{
		{name: "lazy getter", lazy: authtypes.NewEmptyModuleAccount(moduletypes.ModuleName)},
		{name: "missing account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := &recordingAccountKeeper{lazy: tc.lazy}
			ctx, k := setupGenesisKeeper(t, keeper.WithAccountKeeper(accounts))
			cfg := testutilmod.MakeTestEncodingConfig()
			state := GetGenesisStateFromAppState(cfg.Codec, map[string]json.RawMessage{})

			InitGenesis(ctx, k, state)

			require.Equal(t, 1, accounts.getCalls)
			require.Equal(t, 1, accounts.setCalls)
			require.NotNil(t, accounts.persisted)
			require.Equal(t, authtypes.NewModuleAddress(moduletypes.ModuleName), accounts.persisted.GetAddress())
		})
	}
}

func TestValidateGenesisRejectsInvalidParamsAndRelationships(t *testing.T) {
	provider := testutil.AccAddress(t)
	auditor := testutil.AccAddress(t)
	params := keeper.DefaultParams()
	attestation := genesisAttestation(provider, auditor, 7, vtypes.TierVerified, params.MinFeeL2)
	escrow := genesisAuditEscrow(provider, auditor, attestation)

	t.Run("nil state", func(t *testing.T) {
		require.Error(t, ValidateGenesis(nil))
	})
	t.Run("invalid params", func(t *testing.T) {
		state := DefaultGenesisState()
		state.Params.TtlL1 = 0
		require.Error(t, ValidateGenesis(state))
	})
	t.Run("missing auditor", func(t *testing.T) {
		state := DefaultGenesisState()
		state.Attestations = []vtypes.AttestationRecord{attestation}
		state.AuditEscrows = []vtypes.AuditEscrowRecord{escrow}
		state.NextAuditEscrowID = 8
		require.Error(t, ValidateGenesis(state))
	})
	t.Run("missing audit escrow", func(t *testing.T) {
		state := DefaultGenesisState()
		state.Auditors = []vtypes.AuditorRecord{genesisAuditor(auditor)}
		state.Attestations = []vtypes.AttestationRecord{attestation}
		require.Error(t, ValidateGenesis(state))
	})
	t.Run("missing grace discrepancy", func(t *testing.T) {
		state := DefaultGenesisState()
		state.VerificationGraces = []vtypes.ProviderVerificationGraceRecord{genesisGrace(provider)}
		state.NextGraceRecordID = 10
		require.Error(t, ValidateGenesis(state))
	})
	t.Run("next id overlaps records", func(t *testing.T) {
		state := DefaultGenesisState()
		state.AuditEscrows = []vtypes.AuditEscrowRecord{escrow}
		state.NextAuditEscrowID = escrow.ID
		require.Error(t, ValidateGenesis(state))
	})
}

func TestInitGenesisRebuildsAuditorAndProviderUnbondingQueues(t *testing.T) {
	bank := &genesisBank{}
	ctx, k := setupGenesisKeeper(t, keeper.WithBankKeeper(bank))
	auditor := testutil.AccAddress(t)
	provider := testutil.AccAddress(t)
	completion := ctx.BlockTime()

	auditorRecord := genesisAuditor(auditor)
	auditorRecord.Status = vtypes.AuditorStatusResigned
	auditorRecord.RenewalDeadline = time.Time{}
	auditorRecord.BondStatus = vtypes.BondStatusUnbonding
	auditorRecord.BondUnbondingCompletionTime = &completion
	providerAmount := sdk.NewInt64Coin("uakt", 50000000)

	state := DefaultGenesisState()
	state.Auditors = []vtypes.AuditorRecord{auditorRecord}
	state.ProviderBonds = []vtypes.ProviderBondRecord{{
		Provider:     provider.String(),
		BondedAmount: sdk.NewInt64Coin("uakt", 0),
		UnbondingEntries: []vtypes.UnbondingEntry{{
			Amount:         providerAmount,
			CompletionTime: completion,
		}},
	}}

	InitGenesis(ctx, k, state)
	require.NoError(t, k.EndBlocker(ctx))

	gotAuditor, found := k.GetAuditor(ctx, auditor)
	require.True(t, found)
	require.Equal(t, vtypes.BondStatusNotBonded, gotAuditor.BondStatus)
	require.True(t, gotAuditor.BondAmount.IsZero())
	gotProvider, found := k.GetProviderBond(ctx, provider)
	require.True(t, found)
	require.Empty(t, gotProvider.UnbondingEntries)
	require.Equal(t, []genesisTransfer{
		{address: auditor.String(), amount: auditorRecord.BondAmount},
		{address: provider.String(), amount: providerAmount},
	}, bank.moduleToAccount)
}

func setupGenesisKeeper(t testing.TB, opts ...keeper.Option) (sdk.Context, keeper.Keeper) {
	t.Helper()

	cfg := testutilmod.MakeTestEncodingConfig()
	key := storetypes.NewKVStoreKey(moduletypes.StoreKey)
	db := dbm.NewMemDB()

	ms := store.NewCommitMultiStore(db, log.NewNopLogger(), storemetrics.NewNoOpMetrics())
	ms.MountStoreWithDB(key, storetypes.StoreTypeIAVL, db)

	err := ms.LoadLatestVersion()
	require.NoError(t, err)

	ctx := sdk.NewContext(ms, tmproto.Header{Time: genesisNow()}, false, testutil.Logger(t))
	return ctx, keeper.NewKeeper(cfg.Codec, key, opts...)
}

type recordingAccountKeeper struct {
	lazy      sdk.ModuleAccountI
	persisted sdk.ModuleAccountI
	getCalls  int
	setCalls  int
}

func (k *recordingAccountKeeper) GetModuleAccount(context.Context, string) sdk.ModuleAccountI {
	k.getCalls++
	if k.persisted != nil {
		return k.persisted
	}
	return k.lazy
}

func (k *recordingAccountKeeper) SetModuleAccount(_ context.Context, account sdk.ModuleAccountI) {
	k.setCalls++
	k.persisted = account
}

func genesisAuditor(auditor sdk.AccAddress) vtypes.AuditorRecord {
	return vtypes.AuditorRecord{
		Address:            auditor.String(),
		Status:             vtypes.AuditorStatusActive,
		MaxAttestationTier: vtypes.TierTrusted,
		BondAmount:         sdk.NewInt64Coin("uakt", 100000000000),
		BondStatus:         vtypes.BondStatusBonded,
		RegisteredAt:       genesisNow(),
		RenewalDeadline:    genesisNow().Add(365 * 24 * time.Hour),
	}
}

func genesisAttestation(
	provider sdk.AccAddress,
	auditor sdk.AccAddress,
	escrowID uint64,
	tier vtypes.VerificationTier,
	fee sdk.Coin,
) vtypes.AttestationRecord {
	return vtypes.AttestationRecord{
		Provider:      provider.String(),
		Auditor:       auditor.String(),
		Tier:          tier,
		EvidenceHash:  genesisHash("evidence"),
		Fee:           fee,
		FeeStatus:     vtypes.FeeStatusEscrowed,
		CreatedAt:     genesisNow(),
		ExpiresAt:     genesisNow().Add(90 * 24 * time.Hour),
		Status:        vtypes.AttestationStatusValid,
		Deposit:       sdk.NewInt64Coin("uakt", 100000000),
		DepositStatus: vtypes.DepositStatusEscrowed,
		AuditEscrowID: escrowID,
	}
}

func genesisProviderBond(provider sdk.AccAddress) vtypes.ProviderBondRecord {
	return vtypes.ProviderBondRecord{
		Provider:     provider.String(),
		BondedAmount: sdk.NewInt64Coin("uakt", 200000000),
	}
}

func genesisProviderSnapshot(provider sdk.AccAddress) vtypes.ProviderSnapshotRecord {
	return vtypes.ProviderSnapshotRecord{
		Provider:           provider.String(),
		SnapshotHash:       genesisHash("snapshot"),
		ResourceSummary:    vtypes.ResourceSummary{TotalGPUs: 1, TotalVCPUs: 2},
		PostedAt:           genesisNow(),
		SnapshotTimestamp:  genesisNow(),
		ComplianceDeadline: genesisNow().Add(24 * time.Hour),
	}
}

func genesisAuditEscrow(provider, auditor sdk.AccAddress, attestation vtypes.AttestationRecord) vtypes.AuditEscrowRecord {
	consumedAt := genesisNow()
	return vtypes.AuditEscrowRecord{
		ID:                    attestation.AuditEscrowID,
		Provider:              provider.String(),
		ConsumedByAuditor:     auditor.String(),
		RequestedTier:         attestation.Tier,
		Fee:                   attestation.Fee,
		FeeStatus:             vtypes.FeeStatusEscrowed,
		ProviderDeposit:       sdk.NewInt64Coin("uakt", 100000000),
		ProviderDepositStatus: vtypes.ProviderDepositStatusEscrowed,
		Status:                vtypes.AuditEscrowStatusConsumed,
		OpenedAt:              genesisNow(),
		ConsumedAt:            &consumedAt,
		ExpiresAt:             genesisNow().Add(24 * time.Hour),
	}
}

func genesisGrace(provider sdk.AccAddress) vtypes.ProviderVerificationGraceRecord {
	return vtypes.ProviderVerificationGraceRecord{
		ID:                   9,
		Provider:             provider.String(),
		PreservedTier:        vtypes.TierVerified,
		SourceDiscrepancyIDs: []uint64{11},
		StartedAt:            genesisNow(),
		ExpiresAt:            genesisNow().Add(14 * 24 * time.Hour),
		Status:               vtypes.VerificationGraceStatusActive,
	}
}

func genesisDiscrepancy(provider, auditorA, auditorB sdk.AccAddress) vtypes.DiscrepancyEvent {
	return vtypes.DiscrepancyEvent{
		ID:               11,
		Provider:         provider.String(),
		AuditorA:         auditorA.String(),
		AuditorATier:     vtypes.TierVerified,
		AuditorB:         auditorB.String(),
		AuditorBTier:     vtypes.TierEstablished,
		Timestamp:        genesisNow(),
		ResolutionStatus: vtypes.DiscrepancyStatusPending,
	}
}

func genesisNow() time.Time {
	return time.Unix(1700000000, 0).UTC()
}

func genesisHash(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

type genesisBank struct {
	moduleToAccount []genesisTransfer
}

type genesisTransfer struct {
	address string
	amount  sdk.Coin
}

func (b *genesisBank) GetBalance(context.Context, sdk.AccAddress, string) sdk.Coin {
	return sdk.NewInt64Coin("uakt", 0)
}

func (b *genesisBank) SendCoinsFromAccountToModule(context.Context, sdk.AccAddress, string, sdk.Coins) error {
	return nil
}

func (b *genesisBank) SendCoinsFromModuleToAccount(_ context.Context, _ string, address sdk.AccAddress, amount sdk.Coins) error {
	b.moduleToAccount = append(b.moduleToAccount, genesisTransfer{
		address: address.String(),
		amount:  amount[0],
	})
	return nil
}
