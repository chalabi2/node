package keeper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	sdk "github.com/cosmos/cosmos-sdk/types"

	vtypes "pkg.akt.dev/go/node/verification/v1"
	"pkg.akt.dev/go/testutil"
)

func TestVerificationFactsIgnoreExpiredAttestations(t *testing.T) {
	ctx, k := setupStoreKeeper(t)
	provider := testutil.AccAddress(t)
	activeAuditor := testutil.AccAddress(t)
	expiredAuditor := testutil.AccAddress(t)

	active := attestationRecord(provider, activeAuditor)
	active.Tier = vtypes.TierVerified
	active.Capabilities = []vtypes.CapabilityFlag{vtypes.CapabilityTEEHardwareAttestation}
	active.ExpiresAt = ctx.BlockTime().Add(time.Hour)
	require.NoError(t, k.SetAttestation(ctx, active))

	expired := attestationRecord(provider, expiredAuditor)
	expired.Tier = vtypes.TierTrusted
	expired.Capabilities = []vtypes.CapabilityFlag{vtypes.CapabilityBareMetal}
	expired.ExpiresAt = ctx.BlockTime()
	require.NoError(t, k.SetAttestation(ctx, expired))

	attestations, found := k.GetProviderValidAttestations(ctx, sdk.Address(provider))
	require.True(t, found)
	require.Equal(t, []vtypes.AttestationRecord{active}, attestations)
	require.Equal(t, vtypes.TierVerified, k.GetProviderBestTier(ctx, sdk.Address(provider)))
	require.True(t, k.ProviderHasCapability(ctx, sdk.Address(provider), vtypes.CapabilityTEEHardwareAttestation))
	require.False(t, k.ProviderHasCapability(ctx, sdk.Address(provider), vtypes.CapabilityBareMetal))
	require.Equal(t, uint32(1), k.CountQualifiedAuditors(ctx, sdk.Address(provider), vtypes.TierVerified))
	require.Equal(t, uint32(0), k.CountQualifiedAuditors(ctx, sdk.Address(provider), vtypes.TierTrusted))
}

func TestVerificationFactsExposeActiveGraceAsReplacementTier(t *testing.T) {
	ctx, k := setupStoreKeeper(t)
	provider := testutil.AccAddress(t)
	auditor := testutil.AccAddress(t)

	attestation := attestationRecord(provider, auditor)
	attestation.Tier = vtypes.TierTrusted
	attestation.ExpiresAt = ctx.BlockTime().Add(time.Hour)
	require.NoError(t, k.SetAttestation(ctx, attestation))

	expiredGrace := graceRecord(provider)
	expiredGrace.ID = 1
	expiredGrace.PreservedTier = vtypes.TierEstablished
	expiredGrace.ExpiresAt = ctx.BlockTime()
	require.NoError(t, k.SetProviderVerificationGrace(ctx, expiredGrace))

	activeGrace := graceRecord(provider)
	activeGrace.ID = 2
	activeGrace.PreservedTier = vtypes.TierIdentified
	activeGrace.ExpiresAt = ctx.BlockTime().Add(time.Hour)
	require.NoError(t, k.SetProviderVerificationGrace(ctx, activeGrace))

	bestTier := k.GetProviderBestTier(ctx, sdk.Address(provider))
	graceTier, found := k.GetProviderGraceTier(ctx, sdk.Address(provider))
	require.True(t, found)
	require.Equal(t, vtypes.TierTrusted, bestTier)
	require.Equal(t, vtypes.TierIdentified, graceTier)

	effectiveTier := bestTier
	if found {
		effectiveTier = graceTier
	}
	require.Equal(t, vtypes.TierIdentified, effectiveTier)
}

func TestVerificationFactsSnapshotAndActivation(t *testing.T) {
	ctx, k := setupStoreKeeper(t)
	provider := testutil.AccAddress(t)

	require.False(t, k.IsVerificationActive(ctx))
	require.False(t, k.IsProviderSnapshotCompliant(ctx, sdk.Address(provider)))

	params := k.GetParams(ctx)
	params.VerificationModuleActive = true
	k.SetParams(ctx, params)
	require.True(t, k.IsVerificationActive(ctx))

	snapshot := providerSnapshotRecord(provider)
	require.NoError(t, k.SetProviderSnapshot(ctx, snapshot))
	require.True(t, k.IsProviderSnapshotCompliant(ctx, sdk.Address(provider)))

	snapshot.ComplianceDeadline = ctx.BlockTime()
	require.NoError(t, k.SetProviderSnapshot(ctx, snapshot))
	require.False(t, k.IsProviderSnapshotCompliant(ctx, sdk.Address(provider)))

	snapshot.ComplianceDeadline = ctx.BlockTime().Add(time.Hour)
	snapshot.Suspended = true
	require.NoError(t, k.SetProviderSnapshot(ctx, snapshot))
	require.False(t, k.IsProviderSnapshotCompliant(ctx, sdk.Address(provider)))
}
