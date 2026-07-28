package keeper

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	vtypes "pkg.akt.dev/go/node/verification/v1"
)

func (k *keeper) IsVerificationActive(ctx sdk.Context) bool {
	return k.GetParams(ctx).VerificationModuleActive
}

func (k *keeper) GetProviderValidAttestations(ctx sdk.Context, provider sdk.Address) ([]vtypes.AttestationRecord, bool) {
	if provider == nil {
		return nil, false
	}

	records := make([]vtypes.AttestationRecord, 0)
	k.WithProviderAttestations(ctx, sdk.AccAddress(provider.Bytes()), vtypes.AttestationStatusValid, func(record vtypes.AttestationRecord) bool {
		if record.ExpiresAt.After(ctx.BlockTime()) {
			records = append(records, record)
		}
		return false
	})
	return records, len(records) != 0
}

func (k *keeper) IsProviderSnapshotCompliant(ctx sdk.Context, provider sdk.Address) bool {
	if provider == nil {
		return false
	}

	record, found := k.GetProviderSnapshot(ctx, sdk.AccAddress(provider.Bytes()))
	return found && !record.Suspended && record.ComplianceDeadline.After(ctx.BlockTime())
}

func (k *keeper) GetProviderBestTier(ctx sdk.Context, provider sdk.Address) vtypes.VerificationTier {
	records, _ := k.GetProviderValidAttestations(ctx, provider)
	tier := vtypes.TierUnspecified
	for _, record := range records {
		if vtypes.TierBetter(record.Tier, tier) {
			tier = record.Tier
		}
	}
	return tier
}

func (k *keeper) GetProviderGraceTier(ctx sdk.Context, provider sdk.Address) (vtypes.VerificationTier, bool) {
	if provider == nil {
		return vtypes.TierUnspecified, false
	}

	var active vtypes.ProviderVerificationGraceRecord
	found := false
	k.WithProviderVerificationGraces(ctx, sdk.AccAddress(provider.Bytes()), func(record vtypes.ProviderVerificationGraceRecord) bool {
		if record.Status != vtypes.VerificationGraceStatusActive || !record.ExpiresAt.After(ctx.BlockTime()) {
			return false
		}
		if !found || record.StartedAt.After(active.StartedAt) || (record.StartedAt.Equal(active.StartedAt) && record.ID > active.ID) {
			active = record
			found = true
		}
		return false
	})
	return active.PreservedTier, found
}

func (k *keeper) ProviderHasCapability(ctx sdk.Context, provider sdk.Address, capability vtypes.CapabilityFlag) bool {
	if capability == vtypes.CapabilityUnspecified {
		return false
	}

	records, _ := k.GetProviderValidAttestations(ctx, provider)
	for _, record := range records {
		for _, available := range record.Capabilities {
			if available == capability {
				return true
			}
		}
	}
	return false
}

func (k *keeper) CountQualifiedAuditors(ctx sdk.Context, provider sdk.Address, minTier vtypes.VerificationTier) uint32 {
	records, _ := k.GetProviderValidAttestations(ctx, provider)
	auditors := make(map[string]struct{}, len(records))
	for _, record := range records {
		if record.Auditor != "" && vtypes.TierAtLeast(record.Tier, minTier) {
			auditors[record.Auditor] = struct{}{}
		}
	}
	return uint32(len(auditors))
}
