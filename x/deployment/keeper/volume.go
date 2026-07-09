package keeper

import (
	"errors"
	"fmt"

	"cosmossdk.io/collections"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"pkg.akt.dev/go/node/deployment/v1"
	types "pkg.akt.dev/go/node/deployment/v1beta5"

	"pkg.akt.dev/node/v2/x/deployment/keeper/keys"
)

// GetVolumeByVid resolves a (owner, vid) pair to the volume group currently
// holding the data-continuity label. The entry survives group close so the
// vid keeps resolving to the dead-but-retained volume for adoption; a fresh
// create or an adoption overwrites it to the new group.
func (k Keeper) GetVolumeByVid(ctx sdk.Context, owner, vid string) (v1.GroupID, bool) {
	id, err := k.volumesByVid.Get(ctx, keys.VolumeVidToKey(owner, vid))
	if err != nil {
		if !errors.Is(err, collections.ErrNotFound) {
			ctx.Logger().Error("unexpected error getting volume by vid", "owner", owner, "vid", vid, "err", err)
		}
		return v1.GroupID{}, false
	}
	return id, true
}

// GetVolumeReplicaCount returns the number of live replica volumes whose
// replica_of backref points at the given volume group.
func (k Keeper) GetVolumeReplicaCount(ctx sdk.Context, id v1.GroupID) uint64 {
	count, err := k.volumeReplicaCounts.Get(ctx, keys.GroupIDToKey(id))
	if err != nil {
		if !errors.Is(err, collections.ErrNotFound) {
			ctx.Logger().Error("unexpected error getting volume replica count", "id", id, "err", err)
		}
		return 0
	}
	return count
}

// onVolumeGroupCreated maintains the volume indexes when a volume group is
// written by Create: the vid index entry is (over)written to the new group —
// the handler-side vid gate has already established the previous holder is
// dead and either expired or explicitly adopted — and a replica_of backref
// bumps the target's counter. An adoption additionally emits
// EventVolumeAdopted.
func (k Keeper) onVolumeGroupCreated(ctx sdk.Context, group types.Group) error {
	vol := group.GroupSpec.Volume
	if vol == nil {
		return nil
	}

	if err := k.volumesByVid.Set(ctx, keys.VolumeVidToKey(group.ID.Owner, vol.Vid), group.ID); err != nil {
		return fmt.Errorf("failed to index volume vid: %w", err)
	}

	if vol.ReplicaOf != nil {
		if err := k.incVolumeReplicaCount(ctx, vol.ReplicaOf.GroupID()); err != nil {
			return err
		}
	}

	if vol.Adopt != nil {
		err := ctx.EventManager().EmitTypedEvent(
			&v1.EventVolumeAdopted{
				ID:      group.ID,
				Adopted: *vol.Adopt,
				Vid:     vol.Vid,
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// onVolumeGroupClosed maintains the volume indexes when a group leaves the
// live set: a replica volume releases its backref against the replication
// target. The vid index entry is deliberately kept — it points at the
// dead-but-retained volume until adoption or an expired-window re-create
// overwrites it.
func (k Keeper) onVolumeGroupClosed(ctx sdk.Context, group types.Group) error {
	vol := group.GroupSpec.Volume
	if vol == nil || vol.ReplicaOf == nil {
		return nil
	}

	// group carries the pre-transition state; only a group leaving the live
	// set releases its backref, so a re-close of an already-terminal group
	// can never double-decrement.
	if !volumeGroupLive(group) {
		return nil
	}

	return k.decVolumeReplicaCount(ctx, vol.ReplicaOf.GroupID())
}

// RebuildVolumeIndexes derives the vid index and the replica backref
// counters from the groups store. Both indexes are pure functions of the
// groups, so genesis import rebuilds them instead of carrying them in the
// genesis document. Deterministic regardless of iteration order: the vid
// entry goes to the outranking group per volumeVidEntryOutranks, and the
// counters are commutative sums over live replicas.
func (k Keeper) RebuildVolumeIndexes(ctx sdk.Context) error {
	err := k.groups.Walk(ctx, nil, func(_ keys.GroupPrimaryKey, group types.Group) (bool, error) {
		vol := group.GroupSpec.Volume
		if vol == nil {
			return false, nil
		}

		key := keys.VolumeVidToKey(group.ID.Owner, vol.Vid)

		write := true
		if existing, err := k.volumesByVid.Get(ctx, key); err == nil {
			if current, ok := k.GetGroup(ctx, existing); ok {
				write = volumeVidEntryOutranks(group, current)
			}
		} else if !errors.Is(err, collections.ErrNotFound) {
			return true, err
		}

		if write {
			if err := k.volumesByVid.Set(ctx, key, group.ID); err != nil {
				return true, err
			}
		}

		if vol.ReplicaOf != nil && volumeGroupLive(group) {
			if err := k.incVolumeReplicaCount(ctx, vol.ReplicaOf.GroupID()); err != nil {
				return true, err
			}
		}

		return false, nil
	})
	if err != nil {
		return fmt.Errorf("RebuildVolumeIndexes failed: %w", err)
	}

	return nil
}

func (k Keeper) incVolumeReplicaCount(ctx sdk.Context, id v1.GroupID) error {
	pk := keys.GroupIDToKey(id)

	count, err := k.volumeReplicaCounts.Get(ctx, pk)
	if err != nil && !errors.Is(err, collections.ErrNotFound) {
		return fmt.Errorf("failed to read volume replica count: %w", err)
	}

	if err := k.volumeReplicaCounts.Set(ctx, pk, count+1); err != nil {
		return fmt.Errorf("failed to increment volume replica count: %w", err)
	}

	return nil
}

func (k Keeper) decVolumeReplicaCount(ctx sdk.Context, id v1.GroupID) error {
	pk := keys.GroupIDToKey(id)

	count, err := k.volumeReplicaCounts.Get(ctx, pk)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to read volume replica count: %w", err)
	}

	if count <= 1 {
		if err := k.volumeReplicaCounts.Remove(ctx, pk); err != nil {
			return fmt.Errorf("failed to remove volume replica count: %w", err)
		}
		return nil
	}

	if err := k.volumeReplicaCounts.Set(ctx, pk, count-1); err != nil {
		return fmt.Errorf("failed to decrement volume replica count: %w", err)
	}

	return nil
}

// volumeGroupLive reports whether the group occupies its vid: open and
// paused groups are live; closed and insufficient_funds are terminal.
func volumeGroupLive(group types.Group) bool {
	return group.State == types.GroupOpen || group.State == types.GroupPaused
}

// volumeVidEntryOutranks reports whether candidate should replace current as
// the vid index entry during a deterministic rebuild: the live group wins
// (per-owner uniqueness guarantees at most one), then the later-created,
// with the group ID as the final total-order tie-break.
func volumeVidEntryOutranks(candidate, current types.Group) bool {
	if cl, xl := volumeGroupLive(candidate), volumeGroupLive(current); cl != xl {
		return cl
	}
	if candidate.CreatedAt != current.CreatedAt {
		return candidate.CreatedAt > current.CreatedAt
	}
	if candidate.ID.DSeq != current.ID.DSeq {
		return candidate.ID.DSeq > current.ID.DSeq
	}
	return candidate.ID.GSeq > current.ID.GSeq
}
