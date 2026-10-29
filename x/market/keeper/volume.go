package keeper

import (
	"fmt"
	"time"

	"cosmossdk.io/collections"
	sdk "github.com/cosmos/cosmos-sdk/types"

	dtypes "pkg.akt.dev/go/node/deployment/v1"
	dvbeta "pkg.akt.dev/go/node/deployment/v1beta4"
	mv1 "pkg.akt.dev/go/node/market/v1"
	types "pkg.akt.dev/go/node/market/v1beta5"

	"pkg.akt.dev/node/v2/x/market/keeper/keys"
)

// VolumeOrdersEnabled reports the storage-market feature flag gating volume-group
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
func (k Keeper) HasAttachments(ctx sdk.Context, id dtypes.GroupID) bool {
	return k.AttachmentCount(ctx, id) > 0
}

// AttachmentCount returns the number of compute leases currently attached to
// the given volume group — an O(max_attachments) prefix count on the
// attachment index, never a lease scan.
func (k Keeper) AttachmentCount(ctx sdk.Context, id dtypes.GroupID) uint32 {
	count := uint32(0)

	k.WithAttachments(ctx, id, func(_ mv1.LeaseID) bool {
		count++
		return false
	})

	return count
}

// WithAttachments iterates the compute leases attached to the given volume
// group in deterministic key order.
func (k Keeper) WithAttachments(ctx sdk.Context, id dtypes.GroupID, fn func(mv1.LeaseID) bool) {
	rng := collections.NewPrefixedPairRange[keys.GroupPartKey, keys.LeasePrimaryKey](keys.GroupIDToPartKey(id))

	err := k.attachments.Walk(ctx, rng, func(key keys.AttachmentKey) (bool, error) {
		return fn(keys.KeyToLeaseID(key.K2())), nil
	})
	if err != nil {
		panic(fmt.Sprintf("WithAttachments iteration failed: %v", err))
	}
}

// setAttachment writes both directions of the attachment index. Called only
// from the keeper lease-create routine.
func (k Keeper) setAttachment(ctx sdk.Context, gid dtypes.GroupID, lid mv1.LeaseID) error {
	gpart := keys.GroupIDToPartKey(gid)
	lpart := keys.LeaseIDToKey(lid)

	if err := k.attachments.Set(ctx, collections.Join(gpart, lpart)); err != nil {
		return fmt.Errorf("failed to set attachment: %w", err)
	}
	if err := k.attachmentsRev.Set(ctx, collections.Join(lpart, gpart)); err != nil {
		return fmt.Errorf("failed to set reverse attachment: %w", err)
	}

	return nil
}

// removeAttachment prunes both directions of the attachment index. Called
// only from the keeper lease-close routine.
func (k Keeper) removeAttachment(ctx sdk.Context, gid dtypes.GroupID, lid mv1.LeaseID) error {
	gpart := keys.GroupIDToPartKey(gid)
	lpart := keys.LeaseIDToKey(lid)

	if err := k.attachments.Remove(ctx, collections.Join(gpart, lpart)); err != nil {
		return fmt.Errorf("failed to remove attachment: %w", err)
	}
	if err := k.attachmentsRev.Remove(ctx, collections.Join(lpart, gpart)); err != nil {
		return fmt.Errorf("failed to remove reverse attachment: %w", err)
	}

	return nil
}

// attachVolumesOnLeaseCreate writes the attachment index entries for a newly
// created lease on an attaching (compute) group and emits EventVolumeAttached
// per volume. A lease on a group without volume references is a no-op.
func (k Keeper) attachVolumesOnLeaseCreate(ctx sdk.Context, lid mv1.LeaseID) error {
	order, found := k.GetOrder(ctx, lid.OrderID())
	if !found {
		return nil
	}

	seen := make(map[dtypes.GroupID]bool)

	for _, ref := range SpecVolumeRefs(order.Spec) {
		gid := ref.GroupID()
		if seen[gid] {
			continue
		}
		seen[gid] = true

		if err := k.setAttachment(ctx, gid, lid); err != nil {
			return err
		}

		err := ctx.EventManager().EmitTypedEvent(
			&mv1.EventVolumeAttached{
				LeaseID: lid,
				Volume:  ref,
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// detachOnLeaseClose prunes both attachment index directions for a closing
// compute lease and emits EventVolumeDetached per detached volume with the
// close reason. Reached from every close path through the keeper close
// routine.
func (k Keeper) detachOnLeaseClose(ctx sdk.Context, lease mv1.Lease, spec dvbeta.GroupSpec, reason mv1.LeaseClosedReason) error {
	lpart := keys.LeaseIDToKey(lease.ID)
	rng := collections.NewPrefixedPairRange[keys.LeasePrimaryKey, keys.GroupPartKey](lpart)

	var gids []dtypes.GroupID
	err := k.attachmentsRev.Walk(ctx, rng, func(key keys.AttachmentRevKey) (bool, error) {
		gids = append(gids, keys.PartKeyToGroupID(key.K2()))
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk reverse attachments: %w", err)
	}

	refs := SpecVolumeRefs(spec)

	for _, gid := range gids {
		if err := k.removeAttachment(ctx, gid, lease.ID); err != nil {
			return err
		}

		// Resolve the full reference (with name) from the group spec; the
		// index carries only the volume group identity.
		ref := dtypes.VolumeRef{Owner: gid.Owner, DSeq: gid.DSeq, GSeq: gid.GSeq}
		for _, r := range refs {
			if r.GroupID().Equals(gid) {
				ref = r
				break
			}
		}

		err := ctx.EventManager().EmitTypedEvent(
			&mv1.EventVolumeDetached{
				LeaseID: lease.ID,
				Volume:  ref,
				Reason:  reason,
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// cascadeDetach force-closes every compute lease attached to the given
// volume group with reason_volume_detach, running the normal compute close
// path for each: payment closed, EventLeaseClosed emitted (which prunes the
// attachment entries and emits EventVolumeDetached), and the compute group
// auto-re-orders so the workload re-lists — its new order will gate to
// wherever the volume lands next. Runs before the volume lease itself
// closes, on every path (reclaim completion, tenant close, escrow cascade).
//
// Invariant: a volume lease never reaches closed while the attachment index
// holds entries for its group — asserted after the cascade. Bounded by
// max_attachments (=1 in v1), so at most one extra close per volume.
func (k Keeper) cascadeDetach(ctx sdk.Context, gid dtypes.GroupID) error {
	var lids []mv1.LeaseID
	k.WithAttachments(ctx, gid, func(lid mv1.LeaseID) bool {
		lids = append(lids, lid)
		return false
	})

	for _, lid := range lids {
		lease, found := k.GetLease(ctx, lid)
		if !found {
			return fmt.Errorf("cascade-detach: attached lease %s not found", lid)
		}

		if err := k.OnLeaseClosed(ctx, lease, mv1.LeaseClosed, mv1.LeaseClosedReasonVolumeDetach); err != nil {
			return err
		}

		if bid, ok := k.GetBid(ctx, lid.BidID()); ok {
			if err := k.OnBidClosed(ctx, bid); err != nil {
				return err
			}
		}

		order, ok := k.GetOrder(ctx, lid.OrderID())
		if ok {
			if err := k.OnOrderClosed(ctx, order); err != nil {
				return err
			}
		}

		if err := k.ekeeper.PaymentClose(ctx, lid.ToEscrowPaymentID()); err != nil {
			ctx.Logger().With("err", err).Info("error closing payment")
		}

		// The compute group survives (separate escrow) and auto-re-orders —
		// the market's spec-of-record copy re-lists the workload.
		if ok {
			if _, err := k.CreateOrder(ctx, lid.GroupID(), order.Spec, order.Reclamation); err != nil {
				return fmt.Errorf("cascade-detach: re-order for group %s: %w", lid.GroupID(), err)
			}
		}
	}

	if k.HasAttachments(ctx, gid) {
		return fmt.Errorf("cascade-detach: attachments remain for volume group %s", gid)
	}

	return nil
}

// volumeLeaseCloseReason substitutes the recorded close reason for a closing
// volume lease: escrow exhaustion is always volume_unfunded (terminal —
// adoption takes over); an explicit volume reason is kept; otherwise the
// reclamation reason under which the window ran wins over the generic
// tenant/provider reason; a close that never declared intent retains.
func volumeLeaseCloseReason(lease mv1.Lease, state mv1.Lease_State, reason mv1.LeaseClosedReason) mv1.LeaseClosedReason {
	if state == mv1.LeaseInsufficientFunds || reason == mv1.LeaseClosedReasonInsufficientFunds {
		return mv1.LeaseClosedReasonVolumeUnfunded
	}

	switch reason {
	case mv1.LeaseClosedReasonVolumeMigrate,
		mv1.LeaseClosedReasonVolumeEvict,
		mv1.LeaseClosedReasonVolumeClosedRetain,
		mv1.LeaseClosedReasonVolumeUnfunded:
		return reason
	}

	if lease.Reclamation != nil && lease.Reclamation.Reason != mv1.LeaseClosedReasonInvalid {
		return lease.Reclamation.Reason
	}

	return mv1.LeaseClosedReasonVolumeClosedRetain
}

// VolumeLease returns the volume group's current lease — active or
// reclaiming — or false when the group holds no live lease.
func (k Keeper) VolumeLease(ctx sdk.Context, id dtypes.GroupID) (mv1.Lease, bool) {
	var lease mv1.Lease
	var found bool

	for _, state := range []types.Order_State{types.OrderActive, types.OrderOpen} {
		k.WithOrdersForGroup(ctx, id, state, func(order types.Order) bool {
			k.WithBidsForOrder(ctx, order.ID, types.BidActive, func(bid types.Bid) bool {
				if l, ok := k.GetLease(ctx, bid.ID.LeaseID()); ok {
					switch l.State {
					case mv1.LeaseActive, mv1.LeaseReclaiming:
						lease = l
						found = true
					}
				}
				return found
			})
			return found
		})
		if found {
			break
		}
	}

	return lease, found
}

// LastClosedVolumeLeaseProvider returns the provider of the volume group's
// most recent closed lease — the retaining provider adoption bids are gated
// to. Pure chain state: the closed lease records in the leases IndexedMap,
// never provider CRDs.
func (k Keeper) LastClosedVolumeLeaseProvider(ctx sdk.Context, id dtypes.GroupID) (string, bool) {
	lease, found := k.lastClosedVolumeLease(ctx, id)
	if !found {
		return "", false
	}

	return lease.ID.Provider, true
}

// lastClosedVolumeLease returns the volume group's most recent closed lease.
// The most recent close wins: later close height, then the later order as
// the total-order tie-break.
func (k Keeper) lastClosedVolumeLease(ctx sdk.Context, id dtypes.GroupID) (mv1.Lease, bool) {
	var last mv1.Lease
	var oseq uint32
	var found bool

	k.WithOrdersForGroup(ctx, id, types.OrderClosed, func(order types.Order) bool {
		for _, bstate := range []types.Bid_State{types.BidClosed, types.BidActive} {
			k.WithBidsForOrder(ctx, order.ID, bstate, func(bid types.Bid) bool {
				lease, ok := k.GetLease(ctx, bid.ID.LeaseID())
				if !ok {
					return false
				}
				switch lease.State {
				case mv1.LeaseClosed, mv1.LeaseInsufficientFunds:
				default:
					return false
				}
				if !found || lease.ClosedOn > last.ClosedOn || (lease.ClosedOn == last.ClosedOn && order.ID.OSeq > oseq) {
					last = lease
					oseq = order.ID.OSeq
					found = true
				}
				return false
			})
		}
		return false
	})

	return last, found
}

// ReclaimVolumeOnClose routes a tenant-initiated close of a volume group
// through the reclamation window — the chain-enforced floor in both close
// directions. It returns true when the close may proceed now: either the
// group holds no live lease, or its lease is reclaiming and the deadline has
// elapsed. An active lease enters LeaseReclaiming with
// deadline = now + max(bid reclamation window, params floor) and the given
// reason, and the close is deferred to a follow-up msg after the deadline.
func (k Keeper) ReclaimVolumeOnClose(ctx sdk.Context, id dtypes.GroupID, reason mv1.LeaseClosedReason) (bool, error) {
	lease, found := k.VolumeLease(ctx, id)
	if !found {
		return true, nil
	}

	switch lease.State {
	case mv1.LeaseReclaiming:
		if ctx.BlockTime().Unix() < lease.Reclamation.Deadline {
			return false, mv1.ErrReclamationWindowNotElapsed
		}
		return true, nil
	default:
	}

	params, err := k.GetParams(ctx)
	if err != nil {
		return false, err
	}

	window := params.MinVolumeReclamationWindow
	if lease.Reclamation != nil && lease.Reclamation.Window > window {
		window = lease.Reclamation.Window
	}

	deadline := ctx.BlockTime().Add(window)

	if lease.Reclamation == nil {
		lease.Reclamation = &mv1.Reclamation{Window: window}
	}
	lease.Reclamation.StartedAt = ctx.BlockHeight()
	lease.Reclamation.Deadline = deadline.Unix()
	lease.Reclamation.Reason = reason
	lease.State = mv1.LeaseReclaiming

	if err := k.SaveLease(ctx, lease); err != nil {
		return false, err
	}

	if err := k.CancelLeaseResize(ctx, lease.ID, mv1.LeaseResizeCancelReasonLeaseClosed); err != nil {
		return false, err
	}

	err = ctx.EventManager().EmitTypedEvent(&mv1.EventLeaseReclaimStarted{
		ID:       lease.ID,
		Reason:   reason,
		Deadline: deadline.Unix(),
	})
	if err != nil {
		return false, err
	}

	return false, nil
}

// MarkVolumeGroupClose records that the reclamation the tenant just started
// on the volume group's lease ends the group alone, as a MsgCloseGroup does,
// not its deployment. A provider that closes the lease when the window ends
// finishes the tenant's close and reads this to leave the deployment open,
// just as the tenant's own completing close would.
//
// Without the mark the close ends the deployment, the common case. The mark
// is not carried in genesis, so after an export and import a provider's
// close also ends the deployment of a volume whose tenant closed only its
// group; a volume deployment holds nothing but that group.
func (k Keeper) MarkVolumeGroupClose(ctx sdk.Context, id dtypes.GroupID) error {
	lease, found := k.VolumeLease(ctx, id)
	if !found {
		return nil
	}

	if err := k.volumeGroupCloses.Set(ctx, keys.LeaseIDToKey(lease.ID)); err != nil {
		return fmt.Errorf("failed to mark volume group close: %w", err)
	}

	return nil
}

// VolumeGroupClose reports whether the tenant's close under way on the volume
// lease ends its group alone; see MarkVolumeGroupClose.
func (k Keeper) VolumeGroupClose(ctx sdk.Context, id mv1.LeaseID) bool {
	has, err := k.volumeGroupCloses.Has(ctx, keys.LeaseIDToKey(id))
	if err != nil {
		panic(fmt.Sprintf("VolumeGroupClose lookup failed: %v", err))
	}

	return has
}

// RebuildAttachmentIndexes derives both attachment index directions from the
// live leases and their orders' group specs. The indexes are a pure function
// of that state — an attachment exists exactly for each live (active or
// reclaiming) lease on a group referencing volumes — so genesis import
// rebuilds them deterministically instead of carrying them in the genesis
// document, satisfying export/import from day one.
func (k Keeper) RebuildAttachmentIndexes(ctx sdk.Context) error {
	var err error

	k.WithLeases(ctx, func(lease mv1.Lease) bool {
		switch lease.State {
		case mv1.LeaseActive, mv1.LeaseReclaiming:
		default:
			return false
		}

		order, found := k.GetOrder(ctx, lease.ID.OrderID())
		if !found {
			return false
		}

		seen := make(map[dtypes.GroupID]bool)
		for _, ref := range SpecVolumeRefs(order.Spec) {
			gid := ref.GroupID()
			if seen[gid] {
				continue
			}
			seen[gid] = true

			if err = k.setAttachment(ctx, gid, lease.ID); err != nil {
				return true
			}
		}

		return false
	})
	if err != nil {
		return fmt.Errorf("RebuildAttachmentIndexes failed: %w", err)
	}

	return nil
}

// LastVolumeLeaseClosedAt returns the block time at which the volume group's
// most recent closed lease was closed — the anchor of the post-close
// retention window — or false when no anchor is recorded.
//
// ClosedAt is zero on leases closed before the v2.2.0 upgrade added the
// field, so those report no-anchor and the readers treat the dead volume as
// expired: the vid is free to re-mint and adoption rejects. That is the
// intended migration semantics for pre-upgrade closes — volume leases only
// exist behind the storage-market feature flag the upgrade ships, so no real
// retention window predates the field and the upgrade needs no backfill.
func (k Keeper) LastVolumeLeaseClosedAt(ctx sdk.Context, id dtypes.GroupID) (time.Time, bool) {
	lease, found := k.lastClosedVolumeLease(ctx, id)
	if !found || lease.ClosedAt == 0 {
		return time.Time{}, false
	}

	return time.Unix(lease.ClosedAt, 0), true
}

// SpecVolumeRefs collects the volume references an attaching (compute) group
// spec mounts, in resource-unit order. Empty for volume groups and for
// compute groups without volumes.
func SpecVolumeRefs(spec dvbeta.GroupSpec) []dtypes.VolumeRef {
	var refs []dtypes.VolumeRef

	for i := range spec.Resources {
		refs = append(refs, spec.Resources[i].Volumes...)
	}

	return refs
}
