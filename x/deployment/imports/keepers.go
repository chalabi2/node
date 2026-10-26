package imports

import (
	"context"
	"time"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	authzkeeper "github.com/cosmos/cosmos-sdk/x/authz/keeper"
	bmetypes "pkg.akt.dev/go/node/bme/v1"
	dv1 "pkg.akt.dev/go/node/deployment/v1"
	dvbeta "pkg.akt.dev/go/node/deployment/v1beta4"
	escrowid "pkg.akt.dev/go/node/escrow/id/v1"
	etypes "pkg.akt.dev/go/node/escrow/types/v1"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v1beta5"
)

// MarketKeeper is the subset of the market keeper needed for denom migration
// and the AEP-87 volume gates.
type MarketKeeper interface {
	CreateOrder(ctx sdk.Context, id dv1.GroupID, spec dvbeta.GroupSpec, reclamation *dv1.DeploymentReclamation) (mvbeta.Order, error)
	GetParams(ctx sdk.Context) (mvbeta.Params, error)
	OnGroupClosed(ctx sdk.Context, id dv1.GroupID, state dvbeta.Group_State) error
	WithOrdersForGroup(ctx sdk.Context, id dv1.GroupID, state mvbeta.Order_State, fn func(mvbeta.Order) bool)
	WithBidsForOrder(ctx sdk.Context, id mv1.OrderID, state mvbeta.Bid_State, fn func(mvbeta.Bid) bool)
	LeaseForOrder(ctx sdk.Context, bs mvbeta.Bid_State, oid mv1.OrderID) (mv1.Lease, bool)
	SaveOrder(ctx sdk.Context, order mvbeta.Order) error
	SaveBid(ctx sdk.Context, bid mvbeta.Bid) error
	SaveLease(ctx sdk.Context, lease mv1.Lease) error
	// GetLeaseResize returns the deployment's pending lease resize, if any.
	GetLeaseResize(ctx sdk.Context, id dv1.DeploymentID) (mvbeta.LeaseResize, bool)

	// AEP-87 narrow volume additions.
	//
	// VolumeOrdersEnabled reports the market-module feature flag gating
	// volume-group creation.
	VolumeOrdersEnabled(ctx sdk.Context) bool
	// HasAttachments reports whether the market attachment index holds
	// entries for the given volume group: an attached volume must not be
	// closable by the tenant.
	HasAttachments(ctx sdk.Context, id dv1.GroupID) bool
	// ReclaimVolumeOnClose routes a tenant-initiated close of a volume
	// group through the reclamation window. It returns true when the close
	// may proceed now; otherwise the volume lease has entered (or is still
	// inside) LeaseReclaiming and the close completes at msg time after
	// the deadline.
	ReclaimVolumeOnClose(ctx sdk.Context, id dv1.GroupID, reason mv1.LeaseClosedReason) (bool, error)
	// LastVolumeLeaseClosedAt returns the block time at which the volume
	// group's most recent closed lease was closed — the anchor of the
	// post-close retention window — or false when no closed lease is
	// recorded.
	LastVolumeLeaseClosedAt(ctx sdk.Context, id dv1.GroupID) (time.Time, bool)
}

// EscrowKeeper is the subset of the escrow keeper needed for denom migration.
type EscrowKeeper interface {
	AccountCreate(ctx sdk.Context, id escrowid.Account, owner sdk.AccAddress, deposits []etypes.Depositor) error
	AccountDeposit(ctx sdk.Context, id escrowid.Account, deposits []etypes.Depositor) error
	AccountClose(ctx sdk.Context, id escrowid.Account) error
	AuthorizeDeposits(sctx sdk.Context, msg sdk.Msg) ([]etypes.Depositor, error)

	GetAccount(ctx sdk.Context, id escrowid.Account) (etypes.Account, error)
	SaveAccountRaw(ctx sdk.Context, obj etypes.Account) error
	GetAccountPayments(ctx sdk.Context, id escrowid.Account, states []etypes.State) []etypes.Payment
	SavePaymentRaw(ctx sdk.Context, obj etypes.Payment) error
}

// AuthzKeeper is the subset of the authz keeper needed for denom migration.
type AuthzKeeper interface {
	DeleteGrant(ctx context.Context, grantee sdk.AccAddress, granter sdk.AccAddress, msgType string) error
	GetAuthorization(ctx context.Context, grantee sdk.AccAddress, granter sdk.AccAddress, msgType string) (authz.Authorization, *time.Time)
	SaveGrant(ctx context.Context, grantee sdk.AccAddress, granter sdk.AccAddress, authorization authz.Authorization, expiration *time.Time) error
	GetGranteeGrantsByMsgType(ctx context.Context, grantee sdk.AccAddress, msgType string, onGrant authzkeeper.OnGrantFn)
}

type BankKeeper interface {
	GetBalance(ctx context.Context, addr sdk.AccAddress, denom string) sdk.Coin
	SendCoinsFromModuleToModule(ctx context.Context, senderModule, recipientModule string, amt sdk.Coins) error
	MintCoins(ctx context.Context, moduleName string, amt sdk.Coins) error
	BurnCoins(ctx context.Context, moduleName string, amt sdk.Coins) error
}

type OracleKeeper interface {
	GetAggregatedPrice(ctx sdk.Context, denom string) (sdkmath.LegacyDec, error)
}

type BMEKeeper interface {
	GetParams(sdk.Context) (bmetypes.Params, error)
	GetMintStatus(sdk.Context) (bmetypes.MintStatus, error)
}

type DeploymentKeeper interface {
	SaveGroup(ctx sdk.Context, group dvbeta.Group) error
	GetGroups(ctx sdk.Context, id dv1.DeploymentID) (dvbeta.Groups, error)
}
