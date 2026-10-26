package handler

import (
	"bytes"
	"context"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	"pkg.akt.dev/go/sdkutil"

	v1 "pkg.akt.dev/go/node/deployment/v1"
	types "pkg.akt.dev/go/node/deployment/v1beta4"
	mv1 "pkg.akt.dev/go/node/market/v1"

	dimports "pkg.akt.dev/node/v2/x/deployment/imports"
	"pkg.akt.dev/node/v2/x/deployment/keeper"
)

var _ types.MsgServer = msgServer{}

type msgServer struct {
	deployment keeper.IKeeper
	market     dimports.MarketKeeper
	escrow     dimports.EscrowKeeper
	bme        dimports.BMEKeeper
	bank       dimports.BankKeeper
}

// NewServer returns an implementation of the deployment MsgServer interface
// for the provided Keeper.
func NewServer(k keeper.IKeeper, mkeeper dimports.MarketKeeper, ekeeper dimports.EscrowKeeper, bme dimports.BMEKeeper, bankKeeper dimports.BankKeeper) types.MsgServer {
	return &msgServer{
		deployment: k,
		market:     mkeeper,
		escrow:     ekeeper,
		bme:        bme,
		bank:       bankKeeper,
	}
}

func (ms msgServer) CreateDeployment(goCtx context.Context, msg *types.MsgCreateDeployment) (*types.MsgCreateDeploymentResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	did := msg.ID

	if _, found := ms.deployment.GetDeployment(ctx, did); found {
		return nil, v1.ErrDeploymentExists
	}

	params, err := ms.deployment.GetParams(ctx)
	if err != nil {
		return nil, err
	}

	if err := params.ValidateDeposit(msg.Deposit.Amount); err != nil {
		return nil, err
	}

	// AKT deposits are only allowed via AccountDeposit (existing deployment)
	if msg.Deposit.Amount.Denom == sdkutil.DenomUakt {
		return nil, v1.ErrInvalidDeposit
	}

	// Validate reclamation window against market module params
	if msg.Reclamation != nil {
		marketParams, err := ms.market.GetParams(ctx)
		if err != nil {
			return nil, err
		}
		if msg.Reclamation.MinWindow < 0 {
			return nil, v1.ErrInvalidReclamation.Wrap("min_window must be >= 0")
		}
		if msg.Reclamation.MinWindow < marketParams.MinReclamationWindow {
			return nil, v1.ErrInvalidReclamation.Wrap("min_window below governance minimum")
		}
		if msg.Reclamation.MinWindow > marketParams.MaxReclamationWindow {
			return nil, v1.ErrInvalidReclamation.Wrap("min_window above governance maximum")
		}
	}

	deployment := v1.Deployment{
		ID:          did,
		State:       v1.DeploymentActive,
		Hash:        msg.Hash,
		CreatedAt:   ctx.BlockHeight(),
		Reclamation: msg.Reclamation,
	}

	if err := types.ValidateDeploymentGroups(msg.Groups); err != nil {
		return nil, v1.ErrInvalidGroups.Wrap(err.Error())
	}

	if msg.Groups[0].Price().Denom != sdkutil.DenomUact {
		return nil, v1.ErrInvalidPrice.Wrapf("unsupported denomination %s", msg.Groups[0].Price().Denom)
	}

	// Volume groups run the AEP-87 stateful gates. ValidateDeploymentGroups
	// above already enforced the single-group invariant, so the volume
	// group, when present, is the only group.
	if msg.Groups[0].Volume != nil {
		if err := ms.validateVolumeCreate(ctx, msg, params); err != nil {
			return nil, err
		}
	}

	deposits, err := ms.escrow.AuthorizeDeposits(ctx, msg)
	if err != nil {
		return nil, err
	}

	groups := make([]types.Group, 0, len(msg.Groups))

	for idx, spec := range msg.Groups {
		groups = append(groups, types.Group{
			ID:        v1.MakeGroupID(deployment.ID, uint32(idx+1)), // nolint gosec
			State:     types.GroupOpen,
			GroupSpec: spec,
			CreatedAt: ctx.BlockHeight(),
		})
	}

	if err := ms.deployment.Create(ctx, deployment, groups); err != nil {
		return nil, fmt.Errorf("%w: %s", v1.ErrInternal, err.Error())
	}

	// create orders
	for _, group := range groups {
		if _, err := ms.market.CreateOrder(ctx, group.ID, group.GroupSpec, msg.Reclamation); err != nil {
			return &types.MsgCreateDeploymentResponse{}, err
		}
	}

	owner, _ := sdk.AccAddressFromBech32(did.Owner)
	if err := ms.escrow.AccountCreate(ctx, deployment.ID.ToEscrowAccountID(), owner, deposits); err != nil {
		return &types.MsgCreateDeploymentResponse{}, err
	}

	return &types.MsgCreateDeploymentResponse{}, nil
}

func (ms msgServer) UpdateDeployment(goCtx context.Context, msg *types.MsgUpdateDeployment) (*types.MsgUpdateDeploymentResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	deployment, found := ms.deployment.GetDeployment(ctx, msg.ID)
	if !found {
		return nil, v1.ErrDeploymentNotFound
	}

	// If the deployment is not active, do not allow it to be updated
	if deployment.State != v1.DeploymentActive {
		return &types.MsgUpdateDeploymentResponse{}, v1.ErrDeploymentClosed
	}

	// If the version is not identical do not allow the update, there is nothing to change in this transaction
	if bytes.Equal(msg.Hash, deployment.Hash) {
		return &types.MsgUpdateDeploymentResponse{}, v1.ErrInvalidHash
	}

	// A pending lease resize sets the deployment's hash when it is accepted,
	// so an update made before then would be undone by the accept.
	if pending, found := ms.market.GetLeaseResize(ctx, msg.ID); found && !pending.Expired(ctx.BlockTime()) {
		return &types.MsgUpdateDeploymentResponse{}, mv1.ErrLeaseResizePending
	}

	deployment.Hash = msg.Hash

	if err := ms.deployment.UpdateDeployment(ctx, deployment); err != nil {
		return &types.MsgUpdateDeploymentResponse{}, fmt.Errorf("%w: %s", v1.ErrInternal, err.Error())
	}

	return &types.MsgUpdateDeploymentResponse{}, nil
}

func (ms msgServer) CloseDeployment(goCtx context.Context, msg *types.MsgCloseDeployment) (*types.MsgCloseDeploymentResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	deployment, found := ms.deployment.GetDeployment(ctx, msg.ID)
	if !found {
		return &types.MsgCloseDeploymentResponse{}, v1.ErrDeploymentNotFound
	}

	if deployment.State != v1.DeploymentActive {
		return &types.MsgCloseDeploymentResponse{}, v1.ErrDeploymentClosed
	}

	// A volume group with attached compute leases cannot be closed by the
	// tenant; compute closes first. (The escrow cascade cannot be blocked;
	// it cascade-detaches in the market keeper instead.)
	groups, err := ms.deployment.GetGroups(ctx, msg.ID)
	if err != nil {
		return &types.MsgCloseDeploymentResponse{}, err
	}
	for _, group := range groups {
		if group.GroupSpec.Volume == nil {
			continue
		}

		if ms.market.HasAttachments(ctx, group.ID) {
			return &types.MsgCloseDeploymentResponse{}, v1.ErrVolumeAttached.Wrapf("volume group %s", group.ID)
		}

		// A tenant volume close routes through the reclamation window: the
		// first close puts the volume lease into LeaseReclaiming and leaves
		// the deployment active; the close completes at msg time after the
		// deadline has elapsed.
		proceed, err := ms.market.ReclaimVolumeOnClose(ctx, group.ID, mv1.LeaseClosedReasonVolumeClosedRetain)
		if err != nil {
			return &types.MsgCloseDeploymentResponse{}, err
		}
		if !proceed {
			return &types.MsgCloseDeploymentResponse{}, nil
		}
	}

	if err := ms.escrow.AccountClose(ctx, deployment.ID.ToEscrowAccountID()); err != nil {
		return &types.MsgCloseDeploymentResponse{}, err
	}

	// Update state via escrow hooks.
	return &types.MsgCloseDeploymentResponse{}, nil
}

func (ms msgServer) CloseGroup(goCtx context.Context, msg *types.MsgCloseGroup) (*types.MsgCloseGroupResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	group, found := ms.deployment.GetGroup(ctx, msg.ID)
	if !found {
		return nil, v1.ErrGroupNotFound
	}

	// if Group already closed; return the validation error
	err := group.ValidateClosable()
	if err != nil {
		return nil, err
	}

	// A volume group with attached compute leases cannot be closed by the
	// tenant; compute closes first.
	if group.GroupSpec.Volume != nil {
		if ms.market.HasAttachments(ctx, group.ID) {
			return nil, v1.ErrVolumeAttached.Wrapf("volume group %s", group.ID)
		}

		// A tenant volume close routes through the reclamation window; see
		// CloseDeployment.
		proceed, err := ms.market.ReclaimVolumeOnClose(ctx, group.ID, mv1.LeaseClosedReasonVolumeClosedRetain)
		if err != nil {
			return nil, err
		}
		if !proceed {
			return &types.MsgCloseGroupResponse{}, nil
		}
	}

	// Update the Group's state
	err = ms.deployment.OnCloseGroup(ctx, group, types.GroupClosed)
	if err != nil {
		return nil, err
	}
	_ = ms.market.OnGroupClosed(ctx, group.ID, types.GroupClosed)

	return &types.MsgCloseGroupResponse{}, nil
}

func (ms msgServer) PauseGroup(goCtx context.Context, msg *types.MsgPauseGroup) (*types.MsgPauseGroupResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	group, found := ms.deployment.GetGroup(ctx, msg.ID)
	if !found {
		return nil, v1.ErrGroupNotFound
	}

	// if Group already closed; return the validation error
	err := group.ValidatePausable()
	if err != nil {
		return nil, err
	}

	// Pausing a volume group closes its lease just like a close does, so it
	// runs the same AEP-87 gates: attached compute detaches first, and no
	// volume lease terminates without its reclamation window — the first
	// pause starts the window (the group stays open), the pause completes at
	// msg time after the deadline. The paused group stays live for the vid
	// gate (no re-mint, no adoption), and the retention window anchors on
	// this pause-close: the provider's retain obligation runs from when its
	// lease closed, whether the group is later closed or restarted.
	if group.GroupSpec.Volume != nil {
		if ms.market.HasAttachments(ctx, group.ID) {
			return nil, v1.ErrVolumeAttached.Wrapf("volume group %s", group.ID)
		}

		proceed, err := ms.market.ReclaimVolumeOnClose(ctx, group.ID, mv1.LeaseClosedReasonVolumeClosedRetain)
		if err != nil {
			return nil, err
		}
		if !proceed {
			return &types.MsgPauseGroupResponse{}, nil
		}
	}

	// Update the Group's state
	err = ms.deployment.OnPauseGroup(ctx, group)
	if err != nil {
		return nil, err
	}
	_ = ms.market.OnGroupClosed(ctx, group.ID, types.GroupPaused)

	return &types.MsgPauseGroupResponse{}, nil
}

func (ms msgServer) StartGroup(goCtx context.Context, msg *types.MsgStartGroup) (*types.MsgStartGroupResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	group, found := ms.deployment.GetGroup(ctx, msg.ID)
	if !found {
		return &types.MsgStartGroupResponse{}, v1.ErrGroupNotFound
	}

	err := group.ValidateStartable()
	if err != nil {
		return &types.MsgStartGroupResponse{}, err
	}

	err = ms.deployment.OnStartGroup(ctx, group)
	if err != nil {
		return &types.MsgStartGroupResponse{}, err
	}
	deployment, found := ms.deployment.GetDeployment(ctx, msg.ID.DeploymentID())
	if !found {
		return &types.MsgStartGroupResponse{}, v1.ErrDeploymentNotFound
	}

	if _, err := ms.market.CreateOrder(ctx, group.ID, group.GroupSpec, deployment.Reclamation); err != nil {
		return &types.MsgStartGroupResponse{}, err
	}

	return &types.MsgStartGroupResponse{}, nil
}

// validateVolumeCreate runs the AEP-87 stateful gates for creating a volume
// group: the market feature flag, the params-bounded volume limits, the vid
// gate, and the adoption / replica reference validation. Stateless shape
// validation already ran in ValidateDeploymentGroups.
func (ms msgServer) validateVolumeCreate(ctx sdk.Context, msg *types.MsgCreateDeployment, params types.Params) error {
	gspec := msg.Groups[0]
	vol := gspec.Volume

	// 1. Feature flag: volume orders are governance-gated.
	if !ms.market.VolumeOrdersEnabled(ctx) {
		return v1.ErrVolumeOrdersDisabled
	}

	// Governance-bounded limits (size, retention, replicas) live in module
	// params, so they are enforced here rather than in stateless validation.
	if err := gspec.ValidateVolumeBounds(params); err != nil {
		return err
	}

	// 2. vid gate: the data-continuity label must not be minted over. A
	// live holder rejects; a dead holder inside its retention window
	// rejects unless this create adopts exactly that volume; an expired or
	// absent holder frees the label.
	if gid, found := ms.deployment.GetVolumeByVid(ctx, msg.ID.Owner, vol.Vid); found {
		if prev, ok := ms.deployment.GetGroup(ctx, gid); ok {
			if groupLive(prev) {
				return v1.ErrVidInUse.Wrapf("vid %q held by volume group %s", vol.Vid, gid)
			}

			if closedAt, ok := ms.market.LastVolumeLeaseClosedAt(ctx, gid); ok {
				retainedUntil := closedAt.Add(prev.GroupSpec.Volume.Retention)
				retained := ctx.BlockTime().Before(retainedUntil)
				adopted := vol.Adopt != nil && vol.Adopt.GroupID().Equals(gid)
				if retained && !adopted {
					return v1.ErrVidRetained.Wrapf("vid %q retained by dead volume group %s until %s", vol.Vid, gid, retainedUntil)
				}
			}
		}
	}

	// 3. Adoption: the reference must name a dead volume of the same owner
	// and vid, still inside its retention window.
	if adopt := vol.Adopt; adopt != nil {
		target, ok := ms.deployment.GetGroup(ctx, adopt.GroupID())
		if !ok {
			return v1.ErrInvalidVolumeAdoption.Wrapf("volume group %s not found", adopt.GroupID())
		}
		tvol := target.GroupSpec.Volume
		if tvol == nil {
			return v1.ErrInvalidVolumeAdoption.Wrapf("group %s is not a volume group", adopt.GroupID())
		}
		if adopt.Owner != msg.ID.Owner {
			return v1.ErrInvalidVolumeAdoption.Wrap("owner mismatch")
		}
		if groupLive(target) {
			return v1.ErrInvalidVolumeAdoption.Wrapf("volume group %s is not closed", adopt.GroupID())
		}
		if tvol.Vid != vol.Vid {
			return v1.ErrInvalidVolumeAdoption.Wrapf("vid mismatch: %q != %q", vol.Vid, tvol.Vid)
		}
		if name := volumeStorageName(target); name != adopt.Name {
			return v1.ErrInvalidVolumeAdoption.Wrapf("volume name mismatch: %q != %q", adopt.Name, name)
		}
		// a lease resize may have grown the volume, and its data does not
		// fit a smaller one
		if size, held := volumeSize(gspec), volumeSize(target.GroupSpec); size < held {
			return v1.ErrInvalidVolumeAdoption.Wrapf("size %d is below the adopted volume's %d", size, held)
		}
		closedAt, ok := ms.market.LastVolumeLeaseClosedAt(ctx, adopt.GroupID())
		if !ok {
			return v1.ErrInvalidVolumeAdoption.Wrapf("volume group %s has no closed lease to adopt from", adopt.GroupID())
		}
		if !ctx.BlockTime().Before(closedAt.Add(tvol.Retention)) {
			return v1.ErrInvalidVolumeAdoption.Wrapf("retention window expired at %s", closedAt.Add(tvol.Retention))
		}
	}

	// 4. Replica: the reference must name a live volume of the same owner
	// with backref headroom.
	if rep := vol.ReplicaOf; rep != nil {
		target, ok := ms.deployment.GetGroup(ctx, rep.GroupID())
		if !ok {
			return v1.ErrInvalidVolumeReplica.Wrapf("volume group %s not found", rep.GroupID())
		}
		tvol := target.GroupSpec.Volume
		if tvol == nil {
			return v1.ErrInvalidVolumeReplica.Wrapf("group %s is not a volume group", rep.GroupID())
		}
		if rep.Owner != msg.ID.Owner {
			return v1.ErrInvalidVolumeReplica.Wrap("owner mismatch")
		}
		if !groupLive(target) {
			return v1.ErrInvalidVolumeReplica.Wrapf("volume group %s is not live", rep.GroupID())
		}
		if name := volumeStorageName(target); name != rep.Name {
			return v1.ErrInvalidVolumeReplica.Wrapf("volume name mismatch: %q != %q", rep.Name, name)
		}
		if size, src := volumeSize(gspec), volumeSize(target.GroupSpec); size < src {
			return v1.ErrInvalidVolumeReplica.Wrapf("size %d is below the source volume's %d", size, src)
		}
		if count := ms.deployment.GetVolumeReplicaCount(ctx, rep.GroupID()); count >= uint64(tvol.MaxReplicas) {
			return v1.ErrInvalidVolumeReplica.Wrapf("volume group %s max_replicas (%d) reached", rep.GroupID(), tvol.MaxReplicas)
		}
	}

	return nil
}

// groupLive reports whether the group occupies its vid: open and paused
// groups are live; closed and insufficient_funds are terminal.
func groupLive(group types.Group) bool {
	return group.State == types.GroupOpen || group.State == types.GroupPaused
}

// volumeStorageName returns the Storage.Name of the volume group's single
// storage entry — the name a VolumeRef addresses.
func volumeStorageName(group types.Group) string {
	res := group.GroupSpec.Resources
	if len(res) != 1 || len(res[0].Storage) != 1 {
		return ""
	}
	return res[0].Storage[0].Name
}

// volumeSize returns the size of the volume group's single storage entry.
func volumeSize(spec types.GroupSpec) uint64 {
	res := spec.Resources
	if len(res) != 1 || len(res[0].Storage) != 1 {
		return 0
	}
	return res[0].Storage[0].Quantity.Value()
}

func (ms msgServer) UpdateParams(goCtx context.Context, req *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
	if ms.deployment.GetAuthority() != req.Authority {
		return nil, govtypes.ErrInvalidSigner.Wrapf("invalid authority; expected %s, got %s", ms.deployment.GetAuthority(), req.Authority)
	}

	ctx := sdk.UnwrapSDKContext(goCtx)
	if err := ms.deployment.SetParams(ctx, req.Params); err != nil {
		return nil, err
	}

	return &types.MsgUpdateParamsResponse{}, nil
}
