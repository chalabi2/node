package handler

import (
	"context"
	"fmt"

	"github.com/cosmos/cosmos-sdk/telemetry"
	sdk "github.com/cosmos/cosmos-sdk/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	atypes "pkg.akt.dev/go/node/audit/v1"
	dv1 "pkg.akt.dev/go/node/deployment/v1"
	dbeta "pkg.akt.dev/go/node/deployment/v1beta4"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v1beta5"
	ptypes "pkg.akt.dev/go/node/provider/v1beta4"
)

type msgServer struct {
	keepers Keepers
}

// NewServer returns an implementation of the market MsgServer interface
// for the provided Keeper.
func NewServer(k Keepers) mvbeta.MsgServer {
	return &msgServer{keepers: k}
}

var _ mvbeta.MsgServer = msgServer{}

func (ms msgServer) CreateBid(goCtx context.Context, msg *mvbeta.MsgCreateBid) (*mvbeta.MsgCreateBidResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	params, err := ms.keepers.Market.GetParams(ctx)
	if err != nil {
		return nil, err
	}

	minDeposit := params.BidMinDeposits.AmountOf(msg.Deposit.Amount.Denom)
	if minDeposit.IsZero() {
		return nil, fmt.Errorf("%w: unsupported denom %s", mv1.ErrInvalidDeposit, msg.Deposit.Amount.Denom)
	}
	if minDeposit.GT(msg.Deposit.Amount.Amount) {
		return nil, fmt.Errorf("%w: minimum:%v received:%v", mv1.ErrInvalidDeposit, sdk.NewCoin(msg.Deposit.Amount.Denom, minDeposit), msg.Deposit)
	}

	if ms.keepers.Market.BidCountForOrder(ctx, msg.ID.OrderID()) > params.OrderMaxBids {
		return nil, fmt.Errorf("%w: too many existing bids (%v)", mv1.ErrInvalidBid, params.OrderMaxBids)
	}

	if msg.ID.BSeq != 0 {
		return nil, mv1.ErrInvalidBid
	}

	order, found := ms.keepers.Market.GetOrder(ctx, msg.ID.OrderID())
	if !found {
		return nil, mv1.ErrOrderNotFound
	}

	if err := order.ValidateCanBid(); err != nil {
		return nil, err
	}

	if !msg.Price.IsValid() {
		return nil, mv1.ErrBidInvalidPrice
	}

	if order.Price().IsLT(msg.Price) {
		return nil, mv1.ErrBidInvalidPrice
	}

	if !msg.ResourcesOffer.MatchGSpec(order.Spec) {
		return nil, mv1.ErrCapabilitiesMismatch
	}

	provider, err := sdk.AccAddressFromBech32(msg.ID.Provider)
	if err != nil {
		return nil, mv1.ErrEmptyProvider
	}

	var prov ptypes.Provider
	if prov, found = ms.keepers.Provider.Get(ctx, provider); !found {
		return nil, mv1.ErrUnknownProvider
	}

	provAttr, _ := ms.keepers.Audit.GetProviderAttributes(ctx, provider)

	provAttr = append([]atypes.AuditedProvider{{
		Owner:      msg.ID.Provider,
		Attributes: prov.Attributes,
	}}, provAttr...)

	if !order.MatchRequirements(provAttr) {
		return nil, mv1.ErrAttributeMismatch
	}

	if !order.MatchResourcesRequirements(prov.Attributes) {
		return nil, mv1.ErrCapabilitiesMismatch
	}

	deposits, err := ms.keepers.Escrow.AuthorizeDeposits(ctx, msg)
	if err != nil {
		return nil, err
	}

	// Reclamation validation
	if order.RequiresReclamation() {
		if msg.ReclamationWindow == nil {
			return nil, mv1.ErrReclamationRequired
		}
		if *msg.ReclamationWindow < order.Reclamation.MinWindow {
			return nil, mv1.ErrReclamationWindowTooShort
		}
	}

	if msg.ReclamationWindow != nil {
		if *msg.ReclamationWindow < params.MinReclamationWindow {
			return nil, mv1.ErrReclamationWindowInvalid
		}
		if *msg.ReclamationWindow > params.MaxReclamationWindow {
			return nil, mv1.ErrReclamationWindowInvalid
		}
	}

	// storage-market volume gates: a volume-group bid commits to the order's volume
	// policy, the reclamation floor and exact storage quantities; an
	// attaching-group bid is chain-gated to the volume's provider
	// (colocation).
	if order.Spec.Volume != nil {
		if err := ms.validateVolumeBid(ctx, params, order, msg); err != nil {
			return nil, err
		}
	} else if msg.VolumePolicy != nil {
		return nil, ErrVolumePolicyNotAccepted.Wrap("not a volume order")
	} else if err := ms.validateAttachRefs(ctx, order.Spec, order.ID.Owner, msg.ID.Provider); err != nil {
		return nil, err
	}

	bid, err := ms.keepers.Market.CreateBid(ctx, msg.ID, msg.Price, msg.ResourcesOffer, msg.ReclamationWindow)
	if err != nil {
		return nil, err
	}

	// create an escrow account for this bid
	err = ms.keepers.Escrow.AccountCreate(ctx, bid.ID.ToEscrowAccountID(), provider, deposits)
	if err != nil {
		return &mvbeta.MsgCreateBidResponse{}, err
	}

	telemetry.IncrCounter(1.0, "akash.bids")
	return &mvbeta.MsgCreateBidResponse{}, nil
}

func (ms msgServer) CloseBid(goCtx context.Context, msg *mvbeta.MsgCloseBid) (*mvbeta.MsgCloseBidResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	bid, found := ms.keepers.Market.GetBid(ctx, msg.ID)
	if !found {
		return nil, mv1.ErrUnknownBid
	}

	order, found := ms.keepers.Market.GetOrder(ctx, msg.ID.OrderID())
	if !found {
		return nil, mv1.ErrUnknownOrderForBid
	}

	if bid.State == mvbeta.BidOpen {
		_ = ms.keepers.Market.OnBidClosed(ctx, bid)
		return &mvbeta.MsgCloseBidResponse{}, nil
	}

	lease, found := ms.keepers.Market.GetLease(ctx, mv1.LeaseID(msg.ID))
	if !found {
		return nil, mv1.ErrUnknownLeaseForBid
	}

	// Reclamation-aware lease state check
	switch lease.State {
	case mv1.LeaseActive:
		if lease.Reclamation != nil {
			return nil, mv1.ErrReclamationNotStarted
		}
	case mv1.LeaseReclaiming:
		if ctx.BlockTime().Unix() < lease.Reclamation.Deadline {
			return nil, mv1.ErrReclamationWindowNotElapsed
		}
	default:
		return nil, mv1.ErrLeaseNotActive
	}

	if bid.State != mvbeta.BidActive {
		return nil, mv1.ErrBidNotActive
	}

	// storage-market carve-out: a volume lease closes under the reason its
	// reclamation ran with, whatever reason the close carries, and never
	// pauses its group. A tenant's move and every provider reclaim keep the
	// group open and re-list it: the auto-re-order IS the move. A tenant's
	// close of the volume ends here as the tenant's completing close would
	// end it; adoption takes over from there.
	reason := msg.Reason
	reorder := false
	closing := false
	groupOnly := false

	if order.Spec.Volume != nil {
		reason = volumeReclaimReason(lease)
		closing = reason == mv1.LeaseClosedReasonVolumeClosedRetain
		reorder = !closing
		// read before the lease close drops it
		groupOnly = closing && ms.keepers.Market.VolumeGroupClose(ctx, lease.ID)
	} else if err := ms.keepers.Deployment.OnBidClosed(ctx, order.ID.GroupID()); err != nil {
		return nil, err
	}

	_ = ms.keepers.Market.OnLeaseClosed(ctx, lease, mv1.LeaseClosed, reason)
	_ = ms.keepers.Market.OnBidClosed(ctx, bid)
	_ = ms.keepers.Market.OnOrderClosed(ctx, order)

	_ = ms.keepers.Escrow.PaymentClose(ctx, lease.ID.ToEscrowPaymentID())

	if reorder {
		group, found := ms.keepers.Deployment.GetGroup(ctx, order.ID.GroupID())
		if found && group.State == dbeta.GroupOpen {
			if _, err := ms.keepers.Market.CreateOrder(ctx, group.ID, group.GroupSpec, order.Reclamation); err != nil {
				return &mvbeta.MsgCloseBidResponse{}, err
			}
		}
	}

	if closing {
		if err := ms.finishVolumeClose(ctx, order.ID.GroupID(), groupOnly); err != nil {
			return &mvbeta.MsgCloseBidResponse{}, err
		}
	}

	telemetry.IncrCounter(1.0, "akash.order_closed")

	return &mvbeta.MsgCloseBidResponse{}, nil
}

// finishVolumeClose ends a volume whose lease its provider closed when the
// window of the tenant's close ran out, exactly as the tenant's completing
// close would have: the group closes, and with it the deployment and its
// escrow account, unless the tenant closed the group alone. Otherwise a
// tenant who never sends that close would leave the vid held by a live group
// long after the provider released the data, and the volume unadoptable.
func (ms msgServer) finishVolumeClose(ctx sdk.Context, gid dv1.GroupID, groupOnly bool) error {
	if !groupOnly {
		deployment, found := ms.keepers.Deployment.GetDeployment(ctx, gid.DeploymentID())
		if !found || deployment.State != dv1.DeploymentActive {
			return nil
		}

		// the escrow hooks close the deployment and its group
		return ms.keepers.Escrow.AccountClose(ctx, deployment.ID.ToEscrowAccountID())
	}

	group, found := ms.keepers.Deployment.GetGroup(ctx, gid)
	if !found || group.ValidateClosable() != nil {
		return nil
	}

	if err := ms.keepers.Deployment.OnCloseGroup(ctx, group, dbeta.GroupClosed); err != nil {
		return err
	}

	return ms.keepers.Market.OnGroupClosed(ctx, gid, dbeta.GroupClosed)
}

func (ms msgServer) WithdrawLease(goCtx context.Context, msg *mvbeta.MsgWithdrawLease) (*mvbeta.MsgWithdrawLeaseResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	_, found := ms.keepers.Market.GetLease(ctx, msg.ID)
	if !found {
		return nil, mv1.ErrUnknownLease
	}

	if err := ms.keepers.Escrow.PaymentWithdraw(ctx, msg.ID.ToEscrowPaymentID()); err != nil {
		return &mvbeta.MsgWithdrawLeaseResponse{}, err
	}

	return &mvbeta.MsgWithdrawLeaseResponse{}, nil
}

func (ms msgServer) CreateLease(goCtx context.Context, msg *mvbeta.MsgCreateLease) (*mvbeta.MsgCreateLeaseResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	bid, found := ms.keepers.Market.GetBid(ctx, msg.BidID)
	if !found {
		return &mvbeta.MsgCreateLeaseResponse{}, mv1.ErrBidNotFound
	}

	if bid.State != mvbeta.BidOpen {
		return &mvbeta.MsgCreateLeaseResponse{}, mv1.ErrBidNotOpen
	}

	order, found := ms.keepers.Market.GetOrder(ctx, msg.BidID.OrderID())
	if !found {
		return &mvbeta.MsgCreateLeaseResponse{}, mv1.ErrOrderNotFound
	}

	if order.State != mvbeta.OrderOpen {
		return &mvbeta.MsgCreateLeaseResponse{}, mv1.ErrOrderNotOpen
	}

	group, found := ms.keepers.Deployment.GetGroup(ctx, order.ID.GroupID())
	if !found {
		return &mvbeta.MsgCreateLeaseResponse{}, mv1.ErrGroupNotFound
	}

	if group.State != dbeta.GroupOpen {
		return &mvbeta.MsgCreateLeaseResponse{}, mv1.ErrGroupNotOpen
	}

	// Storage market: re-verify the colocation contract and attachment bound for
	// attaching groups — state may have moved between bid and lease. The
	// keeper lease-create routine then writes the attachment index and
	// emits EventVolumeAttached.
	if err := ms.validateAttachRefs(ctx, order.Spec, order.ID.Owner, msg.BidID.Provider); err != nil {
		return &mvbeta.MsgCreateLeaseResponse{}, err
	}

	provider, err := sdk.AccAddressFromBech32(msg.BidID.Provider)
	if err != nil {
		return &mvbeta.MsgCreateLeaseResponse{}, err
	}

	paymentRate := bid.Price

	err = ms.keepers.Escrow.PaymentCreate(ctx, msg.BidID.LeaseID().ToEscrowPaymentID(), provider, paymentRate)
	if err != nil {
		return &mvbeta.MsgCreateLeaseResponse{}, err
	}

	err = ms.keepers.Market.CreateLease(ctx, bid)
	if err != nil {
		return &mvbeta.MsgCreateLeaseResponse{}, err
	}

	if bid.ReclamationWindow != nil {
		lease, found := ms.keepers.Market.GetLease(ctx, bid.ID.LeaseID())
		if !found {
			return &mvbeta.MsgCreateLeaseResponse{}, mv1.ErrLeaseNotFound
		}
		lease.Reclamation = &mv1.Reclamation{
			Window: *bid.ReclamationWindow,
		}
		if err = ms.keepers.Market.SaveLease(ctx, lease); err != nil {
			return &mvbeta.MsgCreateLeaseResponse{}, err
		}
	}

	ms.keepers.Market.OnOrderMatched(ctx, order)
	ms.keepers.Market.OnBidMatched(ctx, bid)

	// close losing bids
	ms.keepers.Market.WithBidsForOrder(ctx, msg.BidID.OrderID(), mvbeta.BidOpen, func(cbid mvbeta.Bid) bool {
		ms.keepers.Market.OnBidLost(ctx, cbid)

		if err = ms.keepers.Escrow.AccountClose(ctx, cbid.ID.ToEscrowAccountID()); err != nil {
			return true
		}
		return false
	})

	return &mvbeta.MsgCreateLeaseResponse{}, nil
}

func (ms msgServer) CloseLease(goCtx context.Context, msg *mvbeta.MsgCloseLease) (*mvbeta.MsgCloseLeaseResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	order, found := ms.keepers.Market.GetOrder(ctx, msg.ID.OrderID())
	if !found {
		return nil, mv1.ErrOrderNotFound
	}

	if order.State != mvbeta.OrderActive {
		return &mvbeta.MsgCloseLeaseResponse{}, mv1.ErrOrderClosed
	}

	bid, found := ms.keepers.Market.GetBid(ctx, msg.ID.BidID())
	if !found {
		return &mvbeta.MsgCloseLeaseResponse{}, mv1.ErrBidNotFound
	}
	if bid.State != mvbeta.BidActive {
		return &mvbeta.MsgCloseLeaseResponse{}, mv1.ErrBidNotActive
	}

	lease, found := ms.keepers.Market.GetLease(ctx, msg.ID)
	if !found {
		return &mvbeta.MsgCloseLeaseResponse{}, mv1.ErrLeaseNotFound
	}
	if lease.State != mv1.LeaseActive && lease.State != mv1.LeaseReclaiming {
		return &mvbeta.MsgCloseLeaseResponse{}, mv1.ErrOrderClosed
	}

	// Storage market: a tenant close of a volume lease routes through the
	// reclamation window — the chain-enforced floor in both close
	// directions. The first close enters LeaseReclaiming as a migration
	// re-listing; the close completes at msg time after the deadline.
	if order.Spec.Volume != nil {
		proceed, err := ms.keepers.Market.ReclaimVolumeOnClose(ctx, msg.ID.GroupID(), mv1.LeaseClosedReasonVolumeMigrate)
		if err != nil {
			return nil, err
		}
		if !proceed {
			return &mvbeta.MsgCloseLeaseResponse{}, nil
		}
	}

	_ = ms.keepers.Market.OnLeaseClosed(ctx, lease, mv1.LeaseClosed, mv1.LeaseClosedReasonOwner)
	_ = ms.keepers.Market.OnBidClosed(ctx, bid)
	_ = ms.keepers.Market.OnOrderClosed(ctx, order)

	err := ms.keepers.Escrow.PaymentClose(ctx, lease.ID.ToEscrowPaymentID())
	if err != nil {
		return &mvbeta.MsgCloseLeaseResponse{}, err
	}

	group, err := ms.keepers.Deployment.OnLeaseClosed(ctx, msg.ID.GroupID())
	if err != nil {
		return &mvbeta.MsgCloseLeaseResponse{}, err
	}

	if group.State != dbeta.GroupOpen {
		return &mvbeta.MsgCloseLeaseResponse{}, nil
	}

	if _, err := ms.keepers.Market.CreateOrder(ctx, group.ID, group.GroupSpec, order.Reclamation); err != nil {
		return &mvbeta.MsgCloseLeaseResponse{}, err
	}

	return &mvbeta.MsgCloseLeaseResponse{}, nil
}

func (ms msgServer) LeaseStartReclaim(goCtx context.Context, msg *mvbeta.MsgLeaseStartReclaim) (*mvbeta.MsgLeaseStartReclaimResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	lease, found := ms.keepers.Market.GetLease(ctx, msg.ID)
	if !found {
		return nil, mv1.ErrUnknownLease
	}

	if lease.State != mv1.LeaseActive {
		return nil, mv1.ErrLeaseNotActive
	}

	if lease.Reclamation == nil {
		return nil, mv1.ErrLeaseNotReclamable
	}

	if lease.Reclamation.StartedAt != 0 {
		return nil, mv1.ErrLeaseAlreadyReclaiming
	}

	// A provider that gives up a volume lease evicts the volume: the group
	// re-lists when the window ends and the next provider copies the data
	// from this one. The reclaim is recorded and announced as volume_evict
	// whatever reason it carries, since storage operators start the export
	// on that reason and a move's destination looks for its source by it.
	reason := msg.Reason
	if order, found := ms.keepers.Market.GetOrder(ctx, msg.ID.OrderID()); found && order.Spec.Volume != nil {
		reason = mv1.LeaseClosedReasonVolumeEvict
	}

	blockTime := ctx.BlockTime()
	deadline := blockTime.Add(lease.Reclamation.Window)

	lease.Reclamation.StartedAt = ctx.BlockHeight()
	lease.Reclamation.Deadline = deadline.Unix()
	lease.Reclamation.Reason = reason
	lease.State = mv1.LeaseReclaiming

	if err := ms.keepers.Market.SaveLease(ctx, lease); err != nil {
		return nil, err
	}

	if err := ms.keepers.Market.CancelLeaseResize(ctx, lease.ID, mv1.LeaseResizeCancelReasonLeaseClosed); err != nil {
		return nil, err
	}

	if err := ctx.EventManager().EmitTypedEvent(&mv1.EventLeaseReclaimStarted{
		ID:       lease.ID,
		Reason:   reason,
		Deadline: deadline.Unix(),
	}); err != nil {
		return nil, err
	}

	return &mvbeta.MsgLeaseStartReclaimResponse{}, nil
}

// volumeReclaimReason is the reason a volume lease in reclamation closes
// under when its provider closes it: a tenant's move or close keeps the
// reason the tenant started the window with, and anything else is the
// provider's eviction.
func volumeReclaimReason(lease mv1.Lease) mv1.LeaseClosedReason {
	if lease.Reclamation != nil {
		switch lease.Reclamation.Reason {
		case mv1.LeaseClosedReasonVolumeMigrate, mv1.LeaseClosedReasonVolumeClosedRetain:
			return lease.Reclamation.Reason
		}
	}

	return mv1.LeaseClosedReasonVolumeEvict
}

// validateVolumeBid runs the storage-market gates for a bid on a volume-group order:
// the feature flag (defense-in-depth; the order cannot exist without it), the
// governance floor on the offered reclamation window — the provider's
// on-chain retain-through-migration commitment — exact storage-quantity
// equality against the order, and the adoption-provider gate.
func (ms msgServer) validateVolumeBid(ctx sdk.Context, params mvbeta.Params, order mvbeta.Order, msg *mvbeta.MsgCreateBid) error {
	if !params.VolumeOrdersEnabled {
		return ErrVolumeOrdersDisabled
	}

	// The bid must carry the order's volume policy. Provider software that
	// predates volumes never sets it, so it cannot win an order whose
	// retention, replication and adoption duties it would not honor.
	if msg.VolumePolicy == nil || !msg.VolumePolicy.Equal(order.Spec.Volume) {
		return ErrVolumePolicyNotAccepted
	}

	if msg.ReclamationWindow == nil || *msg.ReclamationWindow < params.MinVolumeReclamationWindow {
		return ErrVolumeReclamationWindowTooShort.Wrapf("minimum %s", params.MinVolumeReclamationWindow)
	}

	// The offered storage quantities must EQUAL the order's per-name
	// quantities — a volume is never partially provisioned. This closes
	// the MatchGSpec quantity-boundary gap for volume orders only; compute
	// offers keep today's semantics.
	want := make(map[string]uint64)
	for i := range order.Spec.Resources {
		for _, storage := range order.Spec.Resources[i].Storage {
			want[storage.Name] = storage.Quantity.Value()
		}
	}

	got := make(map[string]uint64)
	for _, ro := range msg.ResourcesOffer {
		for _, storage := range ro.Resources.Storage {
			got[storage.Name] = storage.Quantity.Value()
		}
	}

	if len(got) != len(want) {
		return ErrVolumeOfferMismatch
	}
	for name, quantity := range want {
		if offered, exists := got[name]; !exists || offered != quantity {
			return ErrVolumeOfferMismatch.Wrapf("storage %q", name)
		}
	}

	// Adoption orders are gated to the provider retaining the dead
	// volume's data: the provider of its most recent closed lease — pure
	// chain state, never provider CRDs.
	if adopt := order.Spec.Volume.Adopt; adopt != nil {
		provider, found := ms.keepers.Market.LastClosedVolumeLeaseProvider(ctx, adopt.GroupID())
		if !found || provider != msg.ID.Provider {
			return ErrVolumeAdoptionProvider.Wrapf("volume %s", adopt)
		}
	}

	return nil
}

// validateAttachRefs runs the storage-market colocation contract for a bid or lease
// on an attaching (compute) group: every referenced volume must resolve to a
// volume group of the same tenant, hold an ACTIVE lease with this provider,
// and have attachment headroom (an O(1) prefix count, not a lease scan).
// It reads current state on every call, so it survives the auto-re-order
// loop and volume migration for free: after migration, attach bids
// automatically gate to the new provider. CreateLease re-runs it to close
// the bid-to-lease race. A group without volume references is a no-op.
func (ms msgServer) validateAttachRefs(ctx sdk.Context, spec dbeta.GroupSpec, owner, provider string) error {
	for i := range spec.Resources {
		for _, ref := range spec.Resources[i].Volumes {
			if ref.Owner != owner {
				return ErrVolumeRefInvalid.Wrapf("volume %s: owner mismatch", ref)
			}

			gid := ref.GroupID()

			group, found := ms.keepers.Deployment.GetGroup(ctx, gid)
			if !found {
				return ErrVolumeRefInvalid.Wrapf("volume %s: group not found", ref)
			}

			vol := group.GroupSpec.Volume
			if vol == nil {
				return ErrVolumeRefInvalid.Wrapf("volume %s: not a volume group", ref)
			}

			if vol.IsBucket() {
				return ErrVolumeNotAttachable.Wrapf("volume %s", ref)
			}

			if res := group.GroupSpec.Resources; len(res) != 1 || len(res[0].Storage) != 1 || res[0].Storage[0].Name != ref.Name {
				return ErrVolumeRefInvalid.Wrapf("volume %s: name mismatch", ref)
			}

			lease, found := ms.keepers.Market.VolumeLease(ctx, gid)
			if !found || lease.State != mv1.LeaseActive || lease.ID.Provider != provider {
				return ErrVolumeNotColocated.Wrapf("volume %s", ref)
			}

			if ms.keepers.Market.AttachmentCount(ctx, gid) >= vol.MaxAttachments {
				return ErrVolumeAttachmentLimit.Wrapf("volume %s: max_attachments %d", ref, vol.MaxAttachments)
			}
		}
	}

	return nil
}

func (ms msgServer) UpdateParams(goCtx context.Context, req *mvbeta.MsgUpdateParams) (*mvbeta.MsgUpdateParamsResponse, error) {
	if ms.keepers.Market.GetAuthority() != req.Authority {
		return nil, govtypes.ErrInvalidSigner.Wrapf("invalid authority; expected %s, got %s", ms.keepers.Market.GetAuthority(), req.Authority)
	}

	ctx := sdk.UnwrapSDKContext(goCtx)
	if err := ms.keepers.Market.SetParams(ctx, req.Params); err != nil {
		return nil, err
	}

	return &mvbeta.MsgUpdateParamsResponse{}, nil
}
