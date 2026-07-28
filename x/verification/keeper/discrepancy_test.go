package keeper

import (
	"testing"

	"github.com/stretchr/testify/require"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"

	vtypes "pkg.akt.dev/go/node/verification/v1"
	"pkg.akt.dev/go/testutil"

	moduletypes "pkg.akt.dev/node/v2/x/verification/types"
)

func TestSubmitAttestationCreatesDiscrepancyAndGrace(t *testing.T) {
	ctx, k := setupStoreKeeper(t)
	provider := testutil.AccAddress(t)
	auditorA := testutil.AccAddress(t)
	auditorB := testutil.AccAddress(t)
	params := k.GetParams(ctx)

	require.NoError(t, k.SetAuditor(ctx, auditorRecord(auditorA)))
	require.NoError(t, k.SetAuditor(ctx, auditorRecord(auditorB)))
	require.NoError(t, k.SetProviderSnapshot(ctx, providerSnapshotRecord(provider)))
	providerBond := providerBondRecord(provider)
	providerBond.BondedAmount = sdk.NewInt64Coin(bondDenom, 1000000000000)
	require.NoError(t, k.SetProviderBond(ctx, providerBond))

	existing := attestationRecord(provider, auditorA)
	existing.Tier = vtypes.TierIdentified
	existing.Fee = params.MinFeeL1
	require.NoError(t, k.SetAttestation(ctx, existing))

	escrow := openAuditEscrowRecord(ctx, provider, 1, params)
	escrow.RequestedTier = vtypes.TierEstablished
	escrow.Fee = params.MinFeeL3
	require.NoError(t, k.SetAuditEscrow(ctx, escrow))

	err := k.SubmitAttestation(
		ctx,
		provider,
		auditorB,
		vtypes.TierEstablished,
		nil,
		testHash(),
		params.MinFeeL3,
		params.AttestationDeposit,
		1,
	)
	require.NoError(t, err)

	gotA, found := k.GetAttestation(ctx, provider, auditorA)
	require.True(t, found)
	require.Equal(t, vtypes.AttestationStatusVoided, gotA.Status)
	require.Equal(t, vtypes.VoidedReasonDiscrepancy, gotA.VoidedReason)
	require.Equal(t, vtypes.FeeStatusEscrowed, gotA.FeeStatus)
	require.Equal(t, vtypes.DepositStatusPendingDiscrepancy, gotA.DepositStatus)

	gotB, found := k.GetAttestation(ctx, provider, auditorB)
	require.True(t, found)
	require.Equal(t, vtypes.AttestationStatusVoided, gotB.Status)
	require.Equal(t, vtypes.VoidedReasonDiscrepancy, gotB.VoidedReason)
	require.Equal(t, vtypes.DepositStatusPendingDiscrepancy, gotB.DepositStatus)

	discrepancy, found := k.GetDiscrepancy(ctx, 1)
	require.True(t, found)
	require.Equal(t, provider.String(), discrepancy.Provider)
	require.Equal(t, auditorA.String(), discrepancy.AuditorA)
	require.Equal(t, vtypes.TierIdentified, discrepancy.AuditorATier)
	require.Equal(t, auditorB.String(), discrepancy.AuditorB)
	require.Equal(t, vtypes.TierEstablished, discrepancy.AuditorBTier)
	require.Equal(t, vtypes.DiscrepancyStatusPending, discrepancy.ResolutionStatus)
	require.Equal(t, uint64(1), discrepancy.GraceRecordID)
	require.Equal(t, uint64(2), k.GetNextDiscrepancyID(ctx))

	grace, found := k.GetProviderVerificationGrace(ctx, provider)
	require.True(t, found)
	require.Equal(t, uint64(1), grace.ID)
	require.Equal(t, vtypes.TierIdentified, grace.PreservedTier)
	require.Equal(t, []uint64{1}, grace.SourceDiscrepancyIDs)
	require.Equal(t, vtypes.VerificationGraceStatusActive, grace.Status)
	require.Equal(t, uint64(2), k.GetNextGraceRecordID(ctx))

	auditorARecord, found := k.GetAuditor(ctx, auditorA)
	require.True(t, found)
	require.Equal(t, vtypes.BondStatusFrozen, auditorARecord.BondStatus)
	require.Equal(t, uint64(1), auditorARecord.DiscrepancyCount)

	auditorBRecord, found := k.GetAuditor(ctx, auditorB)
	require.True(t, found)
	require.Equal(t, vtypes.BondStatusFrozen, auditorBRecord.BondStatus)
	require.Equal(t, uint64(1), auditorBRecord.DiscrepancyCount)

	events := ctx.EventManager().Events().ToABCIEvents()
	testutil.EnsureEvent(t, events, &vtypes.EventDiscrepancyDetected{
		DiscrepancyID: 1,
		Provider:      provider.String(),
		AuditorA:      auditorA.String(),
		TierA:         vtypes.TierIdentified,
		AuditorB:      auditorB.String(),
		TierB:         vtypes.TierEstablished,
	})
	testutil.EnsureEvent(t, events, &vtypes.EventVerificationGraceStarted{
		GraceRecordID: 1,
		Provider:      provider.String(),
		PreservedTier: vtypes.TierIdentified,
	})
}

func TestSubmitAttestationDoesNotCreateDiscrepancyAtThreshold(t *testing.T) {
	ctx, k := setupStoreKeeper(t)
	provider := testutil.AccAddress(t)
	auditorA := testutil.AccAddress(t)
	auditorB := testutil.AccAddress(t)
	params := k.GetParams(ctx)

	require.NoError(t, k.SetAuditor(ctx, auditorRecord(auditorA)))
	require.NoError(t, k.SetAuditor(ctx, auditorRecord(auditorB)))
	require.NoError(t, k.SetProviderSnapshot(ctx, providerSnapshotRecord(provider)))
	require.NoError(t, k.SetProviderBond(ctx, providerBondRecord(provider)))

	existing := attestationRecord(provider, auditorA)
	existing.Tier = vtypes.TierIdentified
	existing.Fee = params.MinFeeL1
	require.NoError(t, k.SetAttestation(ctx, existing))

	escrow := openAuditEscrowRecord(ctx, provider, 1, params)
	escrow.RequestedTier = vtypes.TierVerified
	escrow.Fee = params.MinFeeL2
	require.NoError(t, k.SetAuditEscrow(ctx, escrow))

	err := k.SubmitAttestation(
		ctx,
		provider,
		auditorB,
		vtypes.TierVerified,
		nil,
		testHash(),
		params.MinFeeL2,
		params.AttestationDeposit,
		1,
	)
	require.NoError(t, err)

	gotB, found := k.GetAttestation(ctx, provider, auditorB)
	require.True(t, found)
	require.Equal(t, vtypes.AttestationStatusValid, gotB.Status)

	_, found = k.GetDiscrepancy(ctx, 1)
	require.False(t, found)
	_, found = k.GetProviderVerificationGrace(ctx, provider)
	require.False(t, found)
}

func TestSubmitAttestationRejectsFrozenAuditor(t *testing.T) {
	ctx, k := setupStoreKeeper(t)
	provider := testutil.AccAddress(t)
	auditor := testutil.AccAddress(t)
	params := k.GetParams(ctx)

	record := auditorRecord(auditor)
	record.BondStatus = vtypes.BondStatusFrozen
	require.NoError(t, k.SetAuditor(ctx, record))

	err := k.SubmitAttestation(
		ctx,
		provider,
		auditor,
		vtypes.TierIdentified,
		nil,
		testHash(),
		params.MinFeeL1,
		params.AttestationDeposit,
		1,
	)
	require.ErrorIs(t, err, moduletypes.ErrAuditorFrozen)
}

func TestResolveDiscrepancySettlesAttestationsAndAuditorBonds(t *testing.T) {
	bank := &recordingBank{}
	ctx, k := setupStoreKeeperWithOptions(t, WithAuthority("gov"), WithBankKeeper(bank), WithDistributionKeeper(bank))
	provider := testutil.AccAddress(t)
	auditorA := testutil.AccAddress(t)
	auditorB := testutil.AccAddress(t)
	params := k.GetParams(ctx)

	setupPendingDiscrepancy(t, ctx, k, provider, auditorA, auditorB)

	err := k.ResolveDiscrepancy(
		ctx,
		"gov",
		1,
		auditorA.String(),
		false,
		true,
		vtypes.DiscrepancyResolutionReasonAuditorACorrect,
		vtypes.FaultAttributionAuditorFault,
		testHash(),
	)
	require.NoError(t, err)

	discrepancy, found := k.GetDiscrepancy(ctx, 1)
	require.True(t, found)
	require.Equal(t, vtypes.DiscrepancyStatusResolved, discrepancy.ResolutionStatus)
	require.Equal(t, vtypes.DiscrepancyResolutionReasonAuditorACorrect, discrepancy.ResolutionReason)
	require.Equal(t, vtypes.FaultAttributionAuditorFault, discrepancy.FaultAttribution)
	require.Equal(t, testHash(), discrepancy.ResolutionEvidenceHash)

	attestationA, found := k.GetAttestation(ctx, provider, auditorA)
	require.True(t, found)
	require.Equal(t, vtypes.FeeStatusReleasedToAuditor, attestationA.FeeStatus)
	require.Equal(t, vtypes.DepositStatusReturnedToAuditor, attestationA.DepositStatus)
	require.Equal(t, vtypes.FaultAttributionNoFault, attestationA.FaultAttribution)

	attestationB, found := k.GetAttestation(ctx, provider, auditorB)
	require.True(t, found)
	require.Equal(t, vtypes.FeeStatusReturnedToProvider, attestationB.FeeStatus)
	require.Equal(t, vtypes.DepositStatusSlashed, attestationB.DepositStatus)
	require.Equal(t, vtypes.FaultAttributionAuditorFault, attestationB.FaultAttribution)

	escrowA, found := k.GetAuditEscrow(ctx, attestationA.AuditEscrowID)
	require.True(t, found)
	require.Equal(t, vtypes.AuditEscrowStatusSettled, escrowA.Status)
	require.Equal(t, vtypes.FeeStatusReleasedToAuditor, escrowA.FeeStatus)
	require.Equal(t, vtypes.ProviderDepositStatusReturnedToProvider, escrowA.ProviderDepositStatus)
	require.Equal(t, vtypes.AuditEscrowSettlementReasonNoFault, escrowA.SettlementReason)
	require.Equal(t, vtypes.FaultAttributionNoFault, escrowA.FaultAttribution)

	escrowB, found := k.GetAuditEscrow(ctx, attestationB.AuditEscrowID)
	require.True(t, found)
	require.Equal(t, vtypes.AuditEscrowStatusSettled, escrowB.Status)
	require.Equal(t, vtypes.FeeStatusReturnedToProvider, escrowB.FeeStatus)
	require.Equal(t, vtypes.ProviderDepositStatusReturnedToProvider, escrowB.ProviderDepositStatus)
	require.Equal(t, vtypes.AuditEscrowSettlementReasonNoFault, escrowB.SettlementReason)
	require.Equal(t, vtypes.FaultAttributionNoFault, escrowB.FaultAttribution)

	auditorARecord, found := k.GetAuditor(ctx, auditorA)
	require.True(t, found)
	require.Equal(t, vtypes.BondStatusBonded, auditorARecord.BondStatus)
	require.Equal(t, params.BondL4, auditorARecord.BondAmount)

	auditorBRecord, found := k.GetAuditor(ctx, auditorB)
	require.True(t, found)
	require.Equal(t, vtypes.BondStatusNotBonded, auditorBRecord.BondStatus)
	require.True(t, auditorBRecord.BondAmount.Amount.IsZero())

	testutil.EnsureEvent(t, ctx.EventManager().Events().ToABCIEvents(), &vtypes.EventDiscrepancyResolved{
		DiscrepancyID:     1,
		VindicatedAuditor: auditorA.String(),
		Reason:            vtypes.DiscrepancyResolutionReasonAuditorACorrect,
		FaultAttribution:  vtypes.FaultAttributionAuditorFault,
	})

	require.Equal(t, []bankTransfer{
		{to: auditorA, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.MinFeeL1)},
		{to: auditorA, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.AttestationDeposit)},
		{to: provider, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.ProviderAuditDeposit)},
		{to: provider, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.MinFeeL3)},
		{to: provider, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.ProviderAuditDeposit)},
	}, bank.moduleToAccount)
	require.Equal(t, []bankTransfer{
		{from: authtypes.NewModuleAddress(moduletypes.ModuleName), amt: sdk.NewCoins(params.AttestationDeposit)},
		{from: authtypes.NewModuleAddress(moduletypes.ModuleName), amt: sdk.NewCoins(params.BondL4)},
	}, bank.communityPool)
}

func TestResolveMultiAuditorDiscrepanciesSettlesSharedAttestationOnce(t *testing.T) {
	bank := &recordingBank{}
	ctx, k := setupStoreKeeperWithOptions(t, WithAuthority("gov"), WithBankKeeper(bank), WithDistributionKeeper(bank))
	provider := testutil.AccAddress(t)
	auditorA := testutil.AccAddress(t)
	auditorB := testutil.AccAddress(t)
	sharedAuditor := testutil.AccAddress(t)
	params := k.GetParams(ctx)

	setupMultiAuditorDiscrepancies(t, ctx, k, provider, auditorA, auditorB, sharedAuditor)

	sharedRecord, found := k.GetAuditor(ctx, sharedAuditor)
	require.True(t, found)
	require.Equal(t, uint64(2), sharedRecord.DiscrepancyCount)

	first, found := k.GetDiscrepancy(ctx, 1)
	require.True(t, found)
	second, found := k.GetDiscrepancy(ctx, 2)
	require.True(t, found)

	require.NoError(t, k.ResolveDiscrepancy(
		ctx,
		"gov",
		first.ID,
		first.AuditorA,
		false,
		true,
		vtypes.DiscrepancyResolutionReasonAuditorACorrect,
		vtypes.FaultAttributionAuditorFault,
		testHash(),
	))

	_, broken := DiscrepancyConsistencyInvariant(k)(ctx)
	require.False(t, broken)

	require.NoError(t, k.ResolveDiscrepancy(
		ctx,
		"gov",
		second.ID,
		second.AuditorA,
		false,
		true,
		vtypes.DiscrepancyResolutionReasonAuditorACorrect,
		vtypes.FaultAttributionAuditorFault,
		testHash(),
	))

	first, found = k.GetDiscrepancy(ctx, first.ID)
	require.True(t, found)
	require.Equal(t, vtypes.DiscrepancyStatusResolved, first.ResolutionStatus)
	second, found = k.GetDiscrepancy(ctx, second.ID)
	require.True(t, found)
	require.Equal(t, vtypes.DiscrepancyStatusResolved, second.ResolutionStatus)

	sharedRecord, found = k.GetAuditor(ctx, sharedAuditor)
	require.True(t, found)
	require.Equal(t, uint64(2), sharedRecord.DiscrepancyCount)
	require.Equal(t, vtypes.BondStatusNotBonded, sharedRecord.BondStatus)
	require.True(t, sharedRecord.BondAmount.IsZero())

	sharedAttestation, found := k.GetAttestation(ctx, provider, sharedAuditor)
	require.True(t, found)
	require.Equal(t, vtypes.FeeStatusReturnedToProvider, sharedAttestation.FeeStatus)
	require.Equal(t, vtypes.DepositStatusSlashed, sharedAttestation.DepositStatus)
	sharedEscrow, found := k.GetAuditEscrow(ctx, sharedAttestation.AuditEscrowID)
	require.True(t, found)
	require.Equal(t, vtypes.AuditEscrowStatusSettled, sharedEscrow.Status)

	_, broken = DiscrepancyConsistencyInvariant(k)(ctx)
	require.False(t, broken)
	_, broken = AuditEscrowInvariant(k)(ctx)
	require.False(t, broken)

	require.Len(t, bank.moduleToAccount, 8)
	require.Len(t, bank.communityPool, 2)
	require.Equal(t, []bankTransfer{
		{from: authtypes.NewModuleAddress(moduletypes.ModuleName), amt: sdk.NewCoins(params.AttestationDeposit)},
		{from: authtypes.NewModuleAddress(moduletypes.ModuleName), amt: sdk.NewCoins(params.BondL4)},
	}, bank.communityPool)
}

func TestResolveDiscrepancySlashesProviderDepositsOnProviderFault(t *testing.T) {
	bank := &recordingBank{}
	ctx, k := setupStoreKeeperWithOptions(t, WithAuthority("gov"), WithBankKeeper(bank), WithDistributionKeeper(bank))
	provider := testutil.AccAddress(t)
	auditorA := testutil.AccAddress(t)
	auditorB := testutil.AccAddress(t)
	params := k.GetParams(ctx)

	setupPendingDiscrepancy(t, ctx, k, provider, auditorA, auditorB)

	err := k.ResolveDiscrepancy(
		ctx,
		"gov",
		1,
		"",
		false,
		false,
		vtypes.DiscrepancyResolutionReasonProviderFault,
		vtypes.FaultAttributionProviderFault,
		testHash(),
	)
	require.NoError(t, err)

	attestationA, found := k.GetAttestation(ctx, provider, auditorA)
	require.True(t, found)
	require.Equal(t, vtypes.FeeStatusReleasedToAuditor, attestationA.FeeStatus)
	require.Equal(t, vtypes.DepositStatusReturnedToAuditor, attestationA.DepositStatus)
	require.Equal(t, vtypes.FaultAttributionProviderFault, attestationA.FaultAttribution)

	attestationB, found := k.GetAttestation(ctx, provider, auditorB)
	require.True(t, found)
	require.Equal(t, vtypes.FeeStatusReleasedToAuditor, attestationB.FeeStatus)
	require.Equal(t, vtypes.DepositStatusReturnedToAuditor, attestationB.DepositStatus)
	require.Equal(t, vtypes.FaultAttributionProviderFault, attestationB.FaultAttribution)

	escrowA, found := k.GetAuditEscrow(ctx, attestationA.AuditEscrowID)
	require.True(t, found)
	require.Equal(t, vtypes.AuditEscrowStatusSettled, escrowA.Status)
	require.Equal(t, vtypes.FeeStatusReleasedToAuditor, escrowA.FeeStatus)
	require.Equal(t, vtypes.ProviderDepositStatusSlashed, escrowA.ProviderDepositStatus)
	require.Equal(t, vtypes.AuditEscrowSettlementReasonProviderFault, escrowA.SettlementReason)
	require.Equal(t, vtypes.FaultAttributionProviderFault, escrowA.FaultAttribution)

	escrowB, found := k.GetAuditEscrow(ctx, attestationB.AuditEscrowID)
	require.True(t, found)
	require.Equal(t, vtypes.AuditEscrowStatusSettled, escrowB.Status)
	require.Equal(t, vtypes.FeeStatusReleasedToAuditor, escrowB.FeeStatus)
	require.Equal(t, vtypes.ProviderDepositStatusSlashed, escrowB.ProviderDepositStatus)
	require.Equal(t, vtypes.AuditEscrowSettlementReasonProviderFault, escrowB.SettlementReason)
	require.Equal(t, vtypes.FaultAttributionProviderFault, escrowB.FaultAttribution)

	require.Equal(t, []bankTransfer{
		{to: auditorA, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.MinFeeL1)},
		{to: auditorA, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.AttestationDeposit)},
		{to: auditorB, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.MinFeeL3)},
		{to: auditorB, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.AttestationDeposit)},
	}, bank.moduleToAccount)
	require.Equal(t, []bankTransfer{
		{from: authtypes.NewModuleAddress(moduletypes.ModuleName), amt: sdk.NewCoins(params.ProviderAuditDeposit)},
		{from: authtypes.NewModuleAddress(moduletypes.ModuleName), amt: sdk.NewCoins(params.ProviderAuditDeposit)},
	}, bank.communityPool)
}

func TestEndBlockerTimesOutPendingDiscrepancy(t *testing.T) {
	bank := &recordingBank{}
	ctx, k := setupStoreKeeperWithOptions(t, WithBankKeeper(bank), WithDistributionKeeper(bank))
	provider := testutil.AccAddress(t)
	auditorA := testutil.AccAddress(t)
	auditorB := testutil.AccAddress(t)
	params := k.GetParams(ctx)

	setupPendingDiscrepancy(t, ctx, k, provider, auditorA, auditorB)
	ctx = ctx.WithBlockTime(ctx.BlockTime().Add(params.DiscrepancyResolutionTimeout))

	require.NoError(t, k.EndBlocker(ctx))

	discrepancy, found := k.GetDiscrepancy(ctx, 1)
	require.True(t, found)
	require.Equal(t, vtypes.DiscrepancyStatusTimedOut, discrepancy.ResolutionStatus)
	require.Equal(t, vtypes.FaultAttributionInconclusive, discrepancy.FaultAttribution)

	attestationA, found := k.GetAttestation(ctx, provider, auditorA)
	require.True(t, found)
	require.Equal(t, vtypes.FeeStatusReturnedToProvider, attestationA.FeeStatus)
	require.Equal(t, vtypes.DepositStatusSlashed, attestationA.DepositStatus)
	require.Equal(t, vtypes.FaultAttributionInconclusive, attestationA.FaultAttribution)

	attestationB, found := k.GetAttestation(ctx, provider, auditorB)
	require.True(t, found)
	require.Equal(t, vtypes.FeeStatusReturnedToProvider, attestationB.FeeStatus)
	require.Equal(t, vtypes.DepositStatusSlashed, attestationB.DepositStatus)
	require.Equal(t, vtypes.FaultAttributionInconclusive, attestationB.FaultAttribution)

	escrowA, found := k.GetAuditEscrow(ctx, attestationA.AuditEscrowID)
	require.True(t, found)
	require.Equal(t, vtypes.AuditEscrowStatusSettled, escrowA.Status)
	require.Equal(t, vtypes.FeeStatusReturnedToProvider, escrowA.FeeStatus)
	require.Equal(t, vtypes.ProviderDepositStatusReturnedToProvider, escrowA.ProviderDepositStatus)
	require.Equal(t, vtypes.AuditEscrowSettlementReasonNoFault, escrowA.SettlementReason)
	require.Equal(t, vtypes.FaultAttributionNoFault, escrowA.FaultAttribution)

	escrowB, found := k.GetAuditEscrow(ctx, attestationB.AuditEscrowID)
	require.True(t, found)
	require.Equal(t, vtypes.AuditEscrowStatusSettled, escrowB.Status)
	require.Equal(t, vtypes.FeeStatusReturnedToProvider, escrowB.FeeStatus)
	require.Equal(t, vtypes.ProviderDepositStatusReturnedToProvider, escrowB.ProviderDepositStatus)
	require.Equal(t, vtypes.AuditEscrowSettlementReasonNoFault, escrowB.SettlementReason)
	require.Equal(t, vtypes.FaultAttributionNoFault, escrowB.FaultAttribution)

	auditorARecord, found := k.GetAuditor(ctx, auditorA)
	require.True(t, found)
	require.Equal(t, vtypes.BondStatusBonded, auditorARecord.BondStatus)

	auditorBRecord, found := k.GetAuditor(ctx, auditorB)
	require.True(t, found)
	require.Equal(t, vtypes.BondStatusBonded, auditorBRecord.BondStatus)

	testutil.EnsureEvent(t, ctx.EventManager().Events().ToABCIEvents(), &vtypes.EventDiscrepancyTimedOut{
		DiscrepancyID: 1,
		AuditorA:      auditorA.String(),
		AuditorB:      auditorB.String(),
	})

	require.Equal(t, []bankTransfer{
		{to: provider, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.MinFeeL1)},
		{to: provider, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.ProviderAuditDeposit)},
		{to: provider, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.MinFeeL3)},
		{to: provider, module: moduletypes.ModuleName, amt: sdk.NewCoins(params.ProviderAuditDeposit)},
	}, bank.moduleToAccount)
	require.Equal(t, []bankTransfer{
		{from: authtypes.NewModuleAddress(moduletypes.ModuleName), amt: sdk.NewCoins(params.AttestationDeposit)},
		{from: authtypes.NewModuleAddress(moduletypes.ModuleName), amt: sdk.NewCoins(params.AttestationDeposit)},
	}, bank.communityPool)
}

func setupPendingDiscrepancy(t testing.TB, ctx sdk.Context, k Keeper, provider, auditorA, auditorB sdk.AccAddress) {
	t.Helper()
	params := k.GetParams(ctx)

	auditorARecord := auditorRecord(auditorA)
	auditorARecord.BondStatus = vtypes.BondStatusFrozen
	auditorARecord.DiscrepancyCount = 1
	require.NoError(t, k.SetAuditor(ctx, auditorARecord))

	auditorBRecord := auditorRecord(auditorB)
	auditorBRecord.BondStatus = vtypes.BondStatusFrozen
	auditorBRecord.DiscrepancyCount = 1
	require.NoError(t, k.SetAuditor(ctx, auditorBRecord))

	attestationA := attestationRecord(provider, auditorA)
	attestationA.AuditEscrowID = 1
	attestationA.Tier = vtypes.TierIdentified
	attestationA.Fee = params.MinFeeL1
	attestationA.Status = vtypes.AttestationStatusVoided
	attestationA.VoidedReason = vtypes.VoidedReasonDiscrepancy
	attestationA.DepositStatus = vtypes.DepositStatusPendingDiscrepancy
	require.NoError(t, k.SetAttestation(ctx, attestationA))
	require.NoError(t, k.SetAuditEscrow(ctx, consumedAuditEscrowRecord(
		ctx,
		provider,
		auditorA,
		attestationA.AuditEscrowID,
		attestationA.Tier,
		attestationA.Fee,
		params,
	)))

	attestationB := attestationRecord(provider, auditorB)
	attestationB.AuditEscrowID = 2
	attestationB.Tier = vtypes.TierEstablished
	attestationB.Fee = params.MinFeeL3
	attestationB.Status = vtypes.AttestationStatusVoided
	attestationB.VoidedReason = vtypes.VoidedReasonDiscrepancy
	attestationB.DepositStatus = vtypes.DepositStatusPendingDiscrepancy
	require.NoError(t, k.SetAttestation(ctx, attestationB))
	require.NoError(t, k.SetAuditEscrow(ctx, consumedAuditEscrowRecord(
		ctx,
		provider,
		auditorB,
		attestationB.AuditEscrowID,
		attestationB.Tier,
		attestationB.Fee,
		params,
	)))

	k.SetDiscrepancy(ctx, vtypes.DiscrepancyEvent{
		ID:               1,
		Provider:         provider.String(),
		AuditorA:         auditorA.String(),
		AuditorATier:     vtypes.TierIdentified,
		AuditorB:         auditorB.String(),
		AuditorBTier:     vtypes.TierEstablished,
		Timestamp:        ctx.BlockTime(),
		ResolutionStatus: vtypes.DiscrepancyStatusPending,
	})
}

func setupMultiAuditorDiscrepancies(
	t testing.TB,
	ctx sdk.Context,
	k Keeper,
	provider sdk.AccAddress,
	auditorA sdk.AccAddress,
	auditorB sdk.AccAddress,
	sharedAuditor sdk.AccAddress,
) {
	t.Helper()
	params := k.GetParams(ctx)

	require.NoError(t, k.SetAuditor(ctx, auditorRecord(auditorA)))
	require.NoError(t, k.SetAuditor(ctx, auditorRecord(auditorB)))
	require.NoError(t, k.SetAuditor(ctx, auditorRecord(sharedAuditor)))
	require.NoError(t, k.SetProviderSnapshot(ctx, providerSnapshotRecord(provider)))
	providerBond := providerBondRecord(provider)
	providerBond.BondedAmount = sdk.NewInt64Coin(bondDenom, 1000000000000)
	require.NoError(t, k.SetProviderBond(ctx, providerBond))

	for id, auditor := range []sdk.AccAddress{auditorA, auditorB} {
		attestation := attestationRecord(provider, auditor)
		attestation.AuditEscrowID = uint64(id + 1)
		attestation.Tier = vtypes.TierIdentified
		attestation.Fee = params.MinFeeL1
		require.NoError(t, k.SetAttestation(ctx, attestation))
		require.NoError(t, k.SetAuditEscrow(ctx, consumedAuditEscrowRecord(
			ctx,
			provider,
			auditor,
			attestation.AuditEscrowID,
			attestation.Tier,
			attestation.Fee,
			params,
		)))
	}

	escrow := openAuditEscrowRecord(ctx, provider, 3, params)
	escrow.RequestedTier = vtypes.TierEstablished
	escrow.Fee = params.MinFeeL3
	require.NoError(t, k.SetAuditEscrow(ctx, escrow))
	require.NoError(t, k.SubmitAttestation(
		ctx,
		provider,
		sharedAuditor,
		vtypes.TierEstablished,
		nil,
		testHash(),
		params.MinFeeL3,
		params.AttestationDeposit,
		escrow.ID,
	))
}
