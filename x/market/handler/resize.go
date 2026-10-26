package handler

import (
	"bytes"
	"context"

	cerrors "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"

	dtypes "pkg.akt.dev/go/node/deployment/v1"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v2beta1"
)

// ResizeLease records a tenant's request to change the resources of an
// active lease in place. Nothing about the lease changes until its provider
// accepts.
func (ms msgServer) ResizeLease(goCtx context.Context, msg *mvbeta.MsgResizeLease) (*mvbeta.MsgResizeLeaseResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	params, err := ms.keepers.Market.GetParams(ctx)
	if err != nil {
		return nil, err
	}

	if !params.LeaseResizeEnabled {
		return nil, mv1.ErrLeaseResizeDisabled
	}

	lease, found := ms.keepers.Market.GetLease(ctx, msg.ID)
	if !found {
		return nil, mv1.ErrUnknownLease
	}

	if lease.State != mv1.LeaseActive {
		return nil, mv1.ErrLeaseNotActive
	}

	group, found := ms.keepers.Deployment.GetGroup(ctx, msg.ID.GroupID())
	if !found {
		return nil, dtypes.ErrGroupNotFound
	}

	next, err := group.GroupSpec.Resize(msg.Resources)
	if err != nil {
		return nil, err
	}

	if next.Volume != nil {
		if len(msg.Hash) != 0 {
			return nil, cerrors.Wrap(mv1.ErrInvalidLeaseResize, "a volume's size is not part of the manifest; send no hash")
		}

		dparams, err := ms.keepers.Deployment.GetParams(ctx)
		if err != nil {
			return nil, err
		}

		if err := next.ValidateVolumeBounds(dparams); err != nil {
			return nil, cerrors.Wrap(mv1.ErrInvalidLeaseResize, err.Error())
		}
	}

	if pending, found := ms.keepers.Market.GetLeaseResize(ctx, msg.ID.DeploymentID()); found && !pending.Expired(ctx.BlockTime()) {
		return nil, mv1.ErrLeaseResizePending
	}

	resize := mvbeta.LeaseResize{
		LeaseID:     msg.ID,
		Resources:   next.Resources,
		Hash:        msg.Hash,
		MaxPrice:    next.Price(),
		RequestedAt: ctx.BlockHeight(),
		ExpiresAt:   ctx.BlockTime().Add(params.LeaseResizeTTL).Unix(),
	}

	if err := ms.keepers.Market.SetLeaseResize(ctx, resize); err != nil {
		return nil, err
	}

	if err := ctx.EventManager().EmitTypedEvent(&mv1.EventLeaseResizeRequested{
		ID:        msg.ID,
		MaxPrice:  resize.MaxPrice,
		ExpiresAt: resize.ExpiresAt,
	}); err != nil {
		return nil, err
	}

	return &mvbeta.MsgResizeLeaseResponse{}, nil
}

// AcceptLeaseResize applies a pending resize at the provider's price. Every
// copy of the group's resources and of the lease's price changes in the same
// message: the group and the order spec, the lease and bid price, the escrow
// payment rate and, when the request carries one, the manifest hash.
func (ms msgServer) AcceptLeaseResize(goCtx context.Context, msg *mvbeta.MsgAcceptLeaseResize) (*mvbeta.MsgAcceptLeaseResizeResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	resize, found := ms.keepers.Market.GetLeaseResize(ctx, msg.ID.DeploymentID())
	if !found || !resize.LeaseID.Equals(msg.ID) {
		return nil, mv1.ErrLeaseResizeNotFound
	}

	if resize.Expired(ctx.BlockTime()) {
		return nil, mv1.ErrLeaseResizeExpired
	}

	lease, found := ms.keepers.Market.GetLease(ctx, msg.ID)
	if !found {
		return nil, mv1.ErrUnknownLease
	}

	if lease.State != mv1.LeaseActive {
		return nil, mv1.ErrLeaseNotActive
	}

	// IsLT below panics on mismatched denoms
	if msg.Price.Denom != lease.Price.Denom || msg.Price.Denom != resize.MaxPrice.Denom {
		return nil, cerrors.Wrapf(mv1.ErrInvalidLeaseResize, "price denom %s does not match the lease's %s", msg.Price.Denom, lease.Price.Denom)
	}

	if resize.MaxPrice.IsLT(msg.Price) {
		return nil, mv1.ErrLeaseResizePriceTooHigh
	}

	group, found := ms.keepers.Deployment.GetGroup(ctx, msg.ID.GroupID())
	if !found {
		return nil, dtypes.ErrGroupNotFound
	}

	next, err := group.GroupSpec.Resize(resize.Resources)
	if err != nil {
		return nil, err
	}

	order, found := ms.keepers.Market.GetOrder(ctx, msg.ID.OrderID())
	if !found {
		return nil, mv1.ErrOrderNotFound
	}

	bid, found := ms.keepers.Market.GetBid(ctx, msg.ID.BidID())
	if !found {
		return nil, mv1.ErrUnknownBid
	}

	if err := ms.keepers.Escrow.PaymentUpdateRate(ctx, msg.ID.ToEscrowPaymentID(), msg.Price); err != nil {
		return nil, err
	}

	group.GroupSpec = next
	if err := ms.keepers.Deployment.SaveGroup(ctx, group); err != nil {
		return nil, err
	}

	// cascade-detach and the attachment rebuild read the order's copy
	order.Spec = next.Dup()
	if err := ms.keepers.Market.SaveOrder(ctx, order); err != nil {
		return nil, err
	}

	prevPrice := lease.Price

	lease.Price = msg.Price
	if err := ms.keepers.Market.SaveLease(ctx, lease); err != nil {
		return nil, err
	}

	// the lease ID is the bid ID, so the active bid carries the lease price
	bid.Price = msg.Price
	if err := ms.keepers.Market.SaveBid(ctx, bid); err != nil {
		return nil, err
	}

	if len(resize.Hash) != 0 {
		deployment, found := ms.keepers.Deployment.GetDeployment(ctx, msg.ID.DeploymentID())
		if !found {
			return nil, dtypes.ErrDeploymentNotFound
		}

		if !bytes.Equal(deployment.Hash, resize.Hash) {
			deployment.Hash = resize.Hash
			if err := ms.keepers.Deployment.UpdateDeployment(ctx, deployment); err != nil {
				return nil, err
			}
		}
	}

	if err := ms.keepers.Market.RemoveLeaseResize(ctx, msg.ID.DeploymentID()); err != nil {
		return nil, err
	}

	if err := ctx.EventManager().EmitTypedEvent(&mv1.EventLeaseResized{
		ID:        msg.ID,
		Price:     msg.Price,
		PrevPrice: prevPrice,
	}); err != nil {
		return nil, err
	}

	return &mvbeta.MsgAcceptLeaseResizeResponse{}, nil
}

// RejectLeaseResize drops a pending resize the provider declines.
func (ms msgServer) RejectLeaseResize(goCtx context.Context, msg *mvbeta.MsgRejectLeaseResize) (*mvbeta.MsgRejectLeaseResizeResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	resize, found := ms.keepers.Market.GetLeaseResize(ctx, msg.ID.DeploymentID())
	if !found || !resize.LeaseID.Equals(msg.ID) {
		return nil, mv1.ErrLeaseResizeNotFound
	}

	if err := ms.keepers.Market.CancelLeaseResize(ctx, msg.ID, msg.Reason); err != nil {
		return nil, err
	}

	return &mvbeta.MsgRejectLeaseResizeResponse{}, nil
}

// CancelLeaseResize drops a pending resize the tenant withdraws.
func (ms msgServer) CancelLeaseResize(goCtx context.Context, msg *mvbeta.MsgCancelLeaseResize) (*mvbeta.MsgCancelLeaseResizeResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	resize, found := ms.keepers.Market.GetLeaseResize(ctx, msg.ID.DeploymentID())
	if !found || !resize.LeaseID.Equals(msg.ID) {
		return nil, mv1.ErrLeaseResizeNotFound
	}

	if err := ms.keepers.Market.CancelLeaseResize(ctx, msg.ID, mv1.LeaseResizeCancelReasonOwner); err != nil {
		return nil, err
	}

	return &mvbeta.MsgCancelLeaseResizeResponse{}, nil
}
