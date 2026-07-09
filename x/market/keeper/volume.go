package keeper

import (
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"

	dtypes "pkg.akt.dev/go/node/deployment/v1"
)

// VolumeOrdersEnabled reports the AEP-87 feature flag gating volume-group
// creation and volume bids.
func (k Keeper) VolumeOrdersEnabled(ctx sdk.Context) bool {
	params, err := k.GetParams(ctx)
	if err != nil {
		return false
	}

	return params.VolumeOrdersEnabled
}

// HasAttachments reports whether the attachment index holds entries for the
// given volume group.
//
// TODO(aep-87): the real read lands with the x/market attachment index
// (prefixes 0x15/0x16). Until that index exists no attachment can have been
// written, so reporting none is correct, not merely a stub.
func (k Keeper) HasAttachments(_ sdk.Context, _ dtypes.GroupID) bool {
	return false
}

// LastVolumeLeaseClosedAt returns the block time at which the volume group's
// most recent closed lease was closed — the anchor of the post-close
// retention window — or false when no closed lease is recorded.
//
// TODO(aep-87): the real read lands with the x/market volume close routine;
// it additionally needs an additive closed_at wall-clock field on the
// market/v1 Lease record (chain-sdk) — Lease currently carries only the
// closed_on block height, which cannot anchor a duration-typed retention
// window. Until then no retention anchor exists, so adoption validation
// rejects and the vid gate treats dead volumes as expired.
func (k Keeper) LastVolumeLeaseClosedAt(_ sdk.Context, _ dtypes.GroupID) (time.Time, bool) {
	return time.Time{}, false
}
