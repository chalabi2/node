package keeper

import (
	"errors"
	"fmt"

	"cosmossdk.io/collections"
	sdk "github.com/cosmos/cosmos-sdk/types"

	dtypes "pkg.akt.dev/go/node/deployment/v1"
	mv1 "pkg.akt.dev/go/node/market/v1"
	types "pkg.akt.dev/go/node/market/v2beta1"

	"pkg.akt.dev/node/v2/x/market/keeper/keys"
)

// LeaseResizeStore returns the pending lease resize store, for genesis and
// queries.
func (k Keeper) LeaseResizeStore() collections.Map[keys.DeploymentPartKey, types.LeaseResize] {
	return k.leaseResizes
}

// GetLeaseResize returns the deployment's pending lease resize, if any.
func (k Keeper) GetLeaseResize(ctx sdk.Context, id dtypes.DeploymentID) (types.LeaseResize, bool) {
	resize, err := k.leaseResizes.Get(ctx, keys.DeploymentIDToPartKey(id))
	if err != nil {
		return types.LeaseResize{}, false
	}

	return resize, true
}

// SetLeaseResize stores a pending lease resize, replacing any earlier one for
// the same deployment.
func (k Keeper) SetLeaseResize(ctx sdk.Context, resize types.LeaseResize) error {
	if err := k.leaseResizes.Set(ctx, keys.DeploymentIDToPartKey(resize.LeaseID.DeploymentID()), resize); err != nil {
		return fmt.Errorf("failed to save lease resize: %w", err)
	}

	return nil
}

// RemoveLeaseResize drops the deployment's pending resize without an event,
// for a resize that was applied.
func (k Keeper) RemoveLeaseResize(ctx sdk.Context, id dtypes.DeploymentID) error {
	err := k.leaseResizes.Remove(ctx, keys.DeploymentIDToPartKey(id))
	if err != nil && !errors.Is(err, collections.ErrNotFound) {
		return fmt.Errorf("failed to remove lease resize: %w", err)
	}

	return nil
}

// CancelLeaseResize drops the pending resize of the given lease, if there is
// one, and tells the tenant and provider why.
func (k Keeper) CancelLeaseResize(ctx sdk.Context, id mv1.LeaseID, reason mv1.LeaseResizeCancelReason) error {
	resize, found := k.GetLeaseResize(ctx, id.DeploymentID())
	if !found || !resize.LeaseID.Equals(id) {
		return nil
	}

	if err := k.RemoveLeaseResize(ctx, id.DeploymentID()); err != nil {
		return err
	}

	return ctx.EventManager().EmitTypedEvent(&mv1.EventLeaseResizeCanceled{
		ID:     id,
		Reason: reason,
	})
}

// WithLeaseResizes iterates all pending lease resizes in key order until fn
// returns true.
func (k Keeper) WithLeaseResizes(ctx sdk.Context, fn func(types.LeaseResize) bool) {
	err := k.leaseResizes.Walk(ctx, nil, func(_ keys.DeploymentPartKey, resize types.LeaseResize) (bool, error) {
		return fn(resize), nil
	})
	if err != nil {
		panic(fmt.Sprintf("WithLeaseResizes iteration failed: %v", err))
	}
}
