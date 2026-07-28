package handler

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	atypes "pkg.akt.dev/go/node/audit/v1"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v1beta5"
	ptypes "pkg.akt.dev/go/node/provider/v1beta4"
	vtypes "pkg.akt.dev/go/node/verification/v1"

	verificationtypes "pkg.akt.dev/node/v3/x/verification/types"
)

func (ms msgServer) bidFilter(ctx sdk.Context, msg *mvbeta.MsgCreateBid, order mvbeta.Order, provider sdk.AccAddress, prov ptypes.Provider) error {
	providerAttributes, _ := ms.keepers.Audit.GetProviderAttributes(ctx, provider)
	providerAttributes = append([]atypes.AuditedProvider{{
		Owner:      msg.ID.Provider,
		Attributes: prov.Attributes,
	}}, providerAttributes...)

	if !order.MatchRequirements(providerAttributes) {
		return mv1.ErrAttributeMismatch
	}
	if !order.MatchResourcesRequirements(prov.Attributes) {
		return mv1.ErrCapabilitiesMismatch
	}

	return ms.verificationBidFilter(ctx, provider, order.Spec.Requirements.Verification)
}

func (ms msgServer) verificationBidFilter(ctx sdk.Context, provider sdk.AccAddress, requirement *vtypes.VerificationRequirement) error {
	if ms.keepers.Verification == nil ||
		!ms.keepers.Verification.IsVerificationActive(ctx) ||
		requirement == nil ||
		requirement.MinTier == vtypes.TierUnspecified {
		return nil
	}

	if vtypes.TierAtLeast(requirement.MinTier, vtypes.TierVerified) &&
		!ms.keepers.Verification.IsProviderSnapshotCompliant(ctx, provider) {
		return verificationtypes.ErrProviderSnapshotSuspended
	}

	tier := ms.keepers.Verification.GetProviderBestTier(ctx, provider)
	if graceTier, found := ms.keepers.Verification.GetProviderGraceTier(ctx, provider); found &&
		vtypes.TierBetter(graceTier, tier) {
		tier = graceTier
	}
	if !vtypes.TierAtLeast(tier, requirement.MinTier) {
		return verificationtypes.ErrInsufficientVerificationTier
	}

	for _, capability := range requirement.RequiredCapabilities {
		if !ms.keepers.Verification.ProviderHasCapability(ctx, provider, capability) {
			return verificationtypes.ErrMissingCapability
		}
	}

	if requirement.MinAuditorCount > 0 &&
		ms.keepers.Verification.CountQualifiedAuditors(ctx, provider, requirement.MinTier) < requirement.MinAuditorCount {
		return verificationtypes.ErrInsufficientAuditorCount
	}

	if len(requirement.RequiredAuditors) == 0 {
		return nil
	}
	attestations, _ := ms.keepers.Verification.GetProviderValidAttestations(ctx, provider)
	if !requiredAuditorsSatisfied(attestations, requirement) {
		return verificationtypes.ErrRequiredAuditorNotFound
	}
	return nil
}

func requiredAuditorsSatisfied(attestations []vtypes.AttestationRecord, requirement *vtypes.VerificationRequirement) bool {
	qualified := make(map[string]struct{}, len(attestations))
	for _, attestation := range attestations {
		if vtypes.TierAtLeast(attestation.Tier, requirement.MinTier) {
			qualified[attestation.Auditor] = struct{}{}
		}
	}

	if requirement.AuditorMode == vtypes.AuditorSelectionModeAll {
		for _, auditor := range requirement.RequiredAuditors {
			if _, found := qualified[auditor]; !found {
				return false
			}
		}
		return true
	}

	for _, auditor := range requirement.RequiredAuditors {
		if _, found := qualified[auditor]; found {
			return true
		}
	}
	return false
}
