package e2e

// In-place lease resize: the tenant requests new resources for an active
// lease, the provider accepts at a price within the request's ceiling, and
// the same lease keeps running with its group, order, lease, bid, escrow rate
// and manifest hash updated in one message. Driven through the real deployment,
// market and escrow servers, like the storage-market suite it reuses.

import (
	"crypto/rand"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	dv1 "pkg.akt.dev/go/node/deployment/v1"
	dvbeta "pkg.akt.dev/go/node/deployment/v1beta4"
	ev1 "pkg.akt.dev/go/node/escrow/v1"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v1beta5"
	rtypes "pkg.akt.dev/go/node/types/resources/v1beta4"
	"pkg.akt.dev/go/node/types/unit"
	"pkg.akt.dev/go/sdkutil"
	"pkg.akt.dev/go/testutil"
)

func (s *smSuite) enableLeaseResize() {
	s.t.Helper()

	params, err := s.MarketKeeper().GetParams(s.Context())
	require.NoError(s.t, err)
	params.LeaseResizeEnabled = true
	require.NoError(s.t, s.MarketKeeper().SetParams(s.Context(), params))
}

// computeLease leases a 1uact/block compute deployment to the provider.
func (s *smSuite) computeLease(owner string, provider sdk.AccAddress) (mv1.LeaseID, mvbeta.Order) {
	s.t.Helper()

	order, _ := s.createComputeDeployment(owner, s.defaultDeposit())
	lid := s.bidAndLease(order, provider, nil)

	return lid, order
}

// leaseVolume creates the volume deployment msg describes and leases it to
// the provider with a 24h reclamation window.
func (s *smSuite) leaseVolume(msg *dvbeta.MsgCreateDeployment, provider sdk.AccAddress) (mv1.LeaseID, mvbeta.Order) {
	s.t.Helper()

	_, err := s.dhandler(s.Context(), msg)
	require.NoError(s.t, err)

	order, found := s.MarketKeeper().GetOrder(s.Context(), mv1.MakeOrderID(dv1.MakeGroupID(msg.ID, 1), 1))
	require.True(s.t, found)

	window := 24 * time.Hour

	return s.bidAndLease(order, provider, &window), order
}

// grownCompute quadruples CPU and doubles memory of the single unit and
// raises its price ceiling to 3uact/block.
func grownCompute(spec dvbeta.GroupSpec) dvbeta.ResourceUnits {
	rus := spec.Resources.Dup()
	rus[0].CPU.Units = rtypes.NewResourceValue(rus[0].CPU.Units.Value() * 4)
	rus[0].Memory.Quantity = rtypes.NewResourceValue(rus[0].Memory.Quantity.Value() * 2)
	rus[0].Price = sdk.NewDecCoin(sdkutil.DenomUact, sdkmath.NewInt(3))

	return rus
}

func uact(amount int64) sdk.DecCoin {
	return sdk.NewDecCoin(sdkutil.DenomUact, sdkmath.NewInt(amount))
}

func newManifestHash(t *testing.T) []byte {
	t.Helper()

	hash := make([]byte, dv1.HashLength)
	_, err := rand.Read(hash)
	require.NoError(t, err)

	return hash
}

// pendingDigest is the digest of the lease's pending resize, which an accept
// or a reject must name.
func (s *smSuite) pendingDigest(lid mv1.LeaseID) []byte {
	s.t.Helper()

	pending, found := s.MarketKeeper().GetLeaseResize(s.Context(), lid.DeploymentID())
	require.True(s.t, found)

	return pending.Digest
}

func TestLeaseResizeE2E_ComputeHappyPath(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(nil)
	lid, order := s.computeLease(owner, provider)
	accID := lid.DeploymentID().ToEscrowAccountID()

	h0 := s.Context().BlockHeight()
	start := s.Context().BlockTime()
	deposit := s.defaultDeposit().Amount.Int64()

	resources := grownCompute(order.Spec)
	hash := newManifestHash(t)

	s.SetBlockHeight(h0 + 100)
	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, hash))
	require.NoError(t, err)

	params, err := s.MarketKeeper().GetParams(s.Context())
	require.NoError(t, err)

	pending, found := s.MarketKeeper().GetLeaseResize(s.Context(), lid.DeploymentID())
	require.True(t, found)
	require.Equal(t, hash, pending.Hash)
	require.Len(t, pending.Digest, mvbeta.LeaseResizeDigestSize)

	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &mv1.EventLeaseResizeRequested{
		ID:        lid,
		MaxPrice:  uact(3),
		ExpiresAt: start.Add(params.LeaseResizeTTL).Unix(),
		Digest:    pending.Digest,
	})

	// nothing about the lease changes until the provider accepts
	lease, _ := s.MarketKeeper().GetLease(s.Context(), lid)
	require.Equal(t, uact(1), lease.Price)

	s.SetBlockHeight(h0 + 150)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(2), pending.Digest))
	require.NoError(t, err)

	lease, _ = s.MarketKeeper().GetLease(s.Context(), lid)
	require.Equal(t, mv1.LeaseActive, lease.State)
	require.Equal(t, uact(2), lease.Price)

	bid, _ := s.MarketKeeper().GetBid(s.Context(), lid.BidID())
	require.Equal(t, uact(2), bid.Price)

	group, _ := s.DeploymentKeeper().GetGroup(s.Context(), lid.GroupID())
	require.True(t, resourcesEqual(group.GroupSpec.Resources, resources))

	resizedOrder, _ := s.MarketKeeper().GetOrder(s.Context(), lid.OrderID())
	require.True(t, resourcesEqual(resizedOrder.Spec.Resources, resources))
	require.Equal(t, uact(3), resizedOrder.Price(), "the order ceiling follows the new resources")

	deployment, _ := s.DeploymentKeeper().GetDeployment(s.Context(), lid.DeploymentID())
	require.Equal(t, hash, deployment.Hash)

	_, found = s.MarketKeeper().GetLeaseResize(s.Context(), lid.DeploymentID())
	require.False(t, found)

	events := s.Context().EventManager().ABCIEvents()
	smEnsureEvent(t, events, &mv1.EventLeaseResized{ID: lid, Price: uact(2), PrevPrice: uact(1), Digest: pending.Digest})
	smEnsureEvent(t, events, &dv1.EventDeploymentUpdated{ID: lid.DeploymentID(), Hash: hash})

	// 150 blocks were charged at the old 1uact rate before the switch
	funds := deposit - 150
	smEnsureEvent(t, events, &ev1.EventAccountRunway{
		ID:                       accID,
		ProjectedDepletionHeight: (h0 + 150) + funds/2,
	})

	payment, err := s.EscrowKeeper().GetPayment(s.Context(), lid.ToEscrowPaymentID())
	require.NoError(t, err)
	require.Equal(t, uact(2), payment.State.Rate)

	// and every block after it at the new 2uact rate
	s.SetBlockHeight(h0 + 250)
	od, err := s.EscrowKeeper().AccountSettle(s.Context(), accID)
	require.NoError(t, err)
	require.False(t, od)

	acc, err := s.EscrowKeeper().GetAccount(s.Context(), accID)
	require.NoError(t, err)
	require.Equal(t, sdkmath.LegacyNewDec(funds-200), acc.State.Funds[0].Amount)
}

func resourcesEqual(a, b dvbeta.ResourceUnits) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if !a[i].Equal(&b[i]) {
			return false
		}
	}

	return true
}

func TestLeaseResizeE2E_Gates(t *testing.T) {
	s := setupStorageMarketSuite(t)

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(nil)
	lid, order := s.computeLease(owner, provider)
	resources := grownCompute(order.Spec)

	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, newManifestHash(t)))
	require.ErrorIs(t, err, mv1.ErrLeaseResizeDisabled)

	s.enableLeaseResize()

	gpu := order.Spec.Resources.Dup()
	gpu[0].GPU = &rtypes.GPU{Units: rtypes.NewResourceValue(1)}
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, gpu, newManifestHash(t)))
	require.ErrorIs(t, err, dv1.ErrInvalidResize)

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, newManifestHash(t)))
	require.NoError(t, err)

	// one pending resize per deployment
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, newManifestHash(t)))
	require.ErrorIs(t, err, mv1.ErrLeaseResizePending)

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(4), s.pendingDigest(lid)))
	require.ErrorIs(t, err, mv1.ErrLeaseResizePriceTooHigh)

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, sdk.NewDecCoin(sdkutil.DenomUakt, sdkmath.NewInt(1)), s.pendingDigest(lid)))
	require.ErrorIs(t, err, mv1.ErrInvalidLeaseResize)

	other := lid
	other.Provider = testutil.AccAddress(t).String()
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(other, uact(2), s.pendingDigest(lid)))
	require.ErrorIs(t, err, mv1.ErrLeaseResizeNotFound)

	// past its ttl the request can no longer be accepted, and a new one
	// replaces it
	params, err := s.MarketKeeper().GetParams(s.Context())
	require.NoError(t, err)
	s.SetBlockTime(s.Context().BlockTime().Add(params.LeaseResizeTTL + time.Second))

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(2), s.pendingDigest(lid)))
	require.ErrorIs(t, err, mv1.ErrLeaseResizeExpired)

	hash := newManifestHash(t)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, hash))
	require.NoError(t, err)

	pending, found := s.MarketKeeper().GetLeaseResize(s.Context(), lid.DeploymentID())
	require.True(t, found)
	require.Equal(t, hash, pending.Hash)

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(3), pending.Digest))
	require.NoError(t, err)
}

func TestLeaseResizeE2E_UpdateWaitsForPendingResize(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(nil)
	lid, order := s.computeLease(owner, provider)

	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grownCompute(order.Spec), newManifestHash(t)))
	require.NoError(t, err)

	// the accept would set the deployment's hash to the request's, undoing
	// an update made in between
	update := &dvbeta.MsgUpdateDeployment{ID: lid.DeploymentID(), Hash: newManifestHash(t)}
	_, err = s.dhandler(s.Context(), update)
	require.ErrorIs(t, err, mv1.ErrLeaseResizePending)

	// an expired request no longer holds updates back
	params, err := s.MarketKeeper().GetParams(s.Context())
	require.NoError(t, err)
	s.SetBlockTime(s.Context().BlockTime().Add(params.LeaseResizeTTL + time.Second))

	_, err = s.dhandler(s.Context(), update)
	require.NoError(t, err)

	deployment, found := s.DeploymentKeeper().GetDeployment(s.Context(), lid.DeploymentID())
	require.True(t, found)
	require.Equal(t, update.Hash, deployment.Hash)
}

func TestLeaseResizeE2E_RejectCancelClose(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(nil)
	lid, order := s.computeLease(owner, provider)
	resources := grownCompute(order.Spec)

	request := func() {
		t.Helper()
		_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, newManifestHash(t)))
		require.NoError(t, err)
	}

	pending := func() bool {
		_, found := s.MarketKeeper().GetLeaseResize(s.Context(), lid.DeploymentID())
		return found
	}

	request()
	digest := s.pendingDigest(lid)
	_, err := s.mhandler(s.Context(), mvbeta.NewMsgRejectLeaseResize(lid, mv1.LeaseResizeRejectCapacity, digest))
	require.NoError(t, err)
	require.False(t, pending())
	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &mv1.EventLeaseResizeCanceled{
		ID:     lid,
		Reason: mv1.LeaseResizeRejectCapacity,
		Digest: digest,
	})

	request()
	digest = s.pendingDigest(lid)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgCancelLeaseResize(lid))
	require.NoError(t, err)
	require.False(t, pending())
	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &mv1.EventLeaseResizeCanceled{
		ID:     lid,
		Reason: mv1.LeaseResizeCancelReasonOwner,
		Digest: digest,
	})

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgCancelLeaseResize(lid))
	require.ErrorIs(t, err, mv1.ErrLeaseResizeNotFound)

	// a lease that closes takes its pending resize with it
	request()
	digest = s.pendingDigest(lid)
	_, err = s.mhandler(s.Context(), &mvbeta.MsgCloseLease{ID: lid, Reason: mv1.LeaseClosedReasonOwner})
	require.NoError(t, err)
	require.False(t, pending())
	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &mv1.EventLeaseResizeCanceled{
		ID:     lid,
		Reason: mv1.LeaseResizeCancelReasonLeaseClosed,
		Digest: digest,
	})

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, newManifestHash(t)))
	require.ErrorIs(t, err, mv1.ErrLeaseNotActive)
}

func TestLeaseResizeE2E_VolumeGrow(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(smVolumeProviderAttrs())
	lid, volOrder := s.fullVolumeLease(owner, "pg-data", provider, s.defaultDeposit())

	grow := func(size uint64) dvbeta.ResourceUnits {
		rus := volOrder.Spec.Resources.Dup()
		rus[0].Storage[0].Quantity = rtypes.NewResourceValue(size)
		rus[0].Price = uact(2)
		return rus
	}

	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grow(50*unit.Gi), newManifestHash(t)))
	require.ErrorIs(t, err, mv1.ErrInvalidLeaseResize, "a volume's size is not part of the manifest")

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grow(5*unit.Gi), nil))
	require.ErrorIs(t, err, dv1.ErrInvalidResize, "volumes only grow")

	dparams, err := s.DeploymentKeeper().GetParams(s.Context())
	require.NoError(t, err)
	dparams.MaxVolumeSize = 20 * unit.Gi
	require.NoError(t, s.DeploymentKeeper().SetParams(s.Context(), dparams))

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grow(50*unit.Gi), nil))
	require.ErrorIs(t, err, mv1.ErrInvalidLeaseResize, "the governance size cap still applies")

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grow(20*unit.Gi), nil))
	require.NoError(t, err)

	hashBefore, _ := s.DeploymentKeeper().GetDeployment(s.Context(), lid.DeploymentID())

	digest := s.pendingDigest(lid)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(2), digest))
	require.NoError(t, err)

	group, _ := s.DeploymentKeeper().GetGroup(s.Context(), lid.GroupID())
	require.Equal(t, uint64(20*unit.Gi), group.GroupSpec.Resources[0].Storage[0].Quantity.Value())
	require.NotNil(t, group.GroupSpec.Volume, "the volume policy carries over")

	order, _ := s.MarketKeeper().GetOrder(s.Context(), lid.OrderID())
	require.Equal(t, uint64(20*unit.Gi), order.Spec.Resources[0].Storage[0].Quantity.Value())

	hashAfter, _ := s.DeploymentKeeper().GetDeployment(s.Context(), lid.DeploymentID())
	require.Equal(t, hashBefore.Hash, hashAfter.Hash)

	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &mv1.EventLeaseResized{ID: lid, Price: uact(2), PrevPrice: uact(1), Digest: digest})
}

func TestLeaseResizeE2E_AttachedComputeKeepsAttachment(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(smVolumeProviderAttrs())
	_, volOrder := s.fullVolumeLease(owner, "pg-data", provider, s.defaultDeposit())

	computeOrder, _ := s.createComputeDeployment(owner, s.defaultDeposit(), smVolumeRef(volOrder))
	_, err := s.mhandler(s.Context(), smAttachBidMsg(computeOrder, provider))
	require.NoError(t, err)

	lid := mv1.MakeBidID(computeOrder.ID, provider).LeaseID()
	_, err = s.mhandler(s.Context(), &mvbeta.MsgCreateLease{BidID: lid.BidID()})
	require.NoError(t, err)
	require.True(t, s.MarketKeeper().HasAttachments(s.Context(), volOrder.ID.GroupID()))

	resources := computeOrder.Spec.Resources.Dup()
	resources[0].Memory.Quantity = rtypes.NewResourceValue(resources[0].Memory.Quantity.Value() * 2)

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, newManifestHash(t)))
	require.NoError(t, err)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(1), s.pendingDigest(lid)))
	require.NoError(t, err)

	require.True(t, s.MarketKeeper().HasAttachments(s.Context(), volOrder.ID.GroupID()))

	order, _ := s.MarketKeeper().GetOrder(s.Context(), lid.OrderID())
	require.Equal(t, computeOrder.Spec.Resources[0].Volumes, order.Spec.Resources[0].Volumes,
		"cascade-detach reads the order copy, so it must keep the volume refs")
}

func TestLeaseResizeE2E_AnswerNamesItsRequest(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(nil)
	lid, order := s.computeLease(owner, provider)

	// the provider works out its answer to the first request...
	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grownCompute(order.Spec), newManifestHash(t)))
	require.NoError(t, err)
	first := s.pendingDigest(lid)

	// ...while the tenant replaces it, in the same block, with a bigger one
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgCancelLeaseResize(lid))
	require.NoError(t, err)

	bigger := grownCompute(order.Spec)
	bigger[0].CPU.Units = rtypes.NewResourceValue(bigger[0].CPU.Units.Value() * 2)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, bigger, newManifestHash(t)))
	require.NoError(t, err)
	second := s.pendingDigest(lid)
	require.NotEqual(t, first, second)

	// an answer decided on the first request applies to neither
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(2), first))
	require.ErrorIs(t, err, mv1.ErrLeaseResizeChanged)

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgRejectLeaseResize(lid, mv1.LeaseResizeRejectPrice, first))
	require.ErrorIs(t, err, mv1.ErrLeaseResizeChanged)

	group, _ := s.DeploymentKeeper().GetGroup(s.Context(), lid.GroupID())
	require.True(t, resourcesEqual(group.GroupSpec.Resources, order.Spec.Resources), "nothing was applied")

	// the replacement is answered on its own
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(3), second))
	require.NoError(t, err)

	group, _ = s.DeploymentKeeper().GetGroup(s.Context(), lid.GroupID())
	require.True(t, resourcesEqual(group.GroupSpec.Resources, bigger))
}

func TestLeaseResizeE2E_AcceptAfterSameBlockWithdraw(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(nil)
	lid, order := s.computeLease(owner, provider)

	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grownCompute(order.Spec), newManifestHash(t)))
	require.NoError(t, err)

	// the withdraw settles the account at this height, so the accept below
	// finds nothing due and must use the stored payment
	s.SetBlockHeight(s.Context().BlockHeight() + 10)
	_, err = s.mhandler(s.Context(), &mvbeta.MsgWithdrawLease{ID: lid})
	require.NoError(t, err)

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(2), s.pendingDigest(lid)))
	require.NoError(t, err)

	payment, err := s.EscrowKeeper().GetPayment(s.Context(), lid.ToEscrowPaymentID())
	require.NoError(t, err)
	require.Equal(t, uact(2), payment.State.Rate)
}

// grownVolume is a volume order's units with its 10Gi of storage grown to
// 20Gi.
func grownVolume(order mvbeta.Order) dvbeta.ResourceUnits {
	rus := order.Spec.Resources.Dup()
	rus[0].Storage[0].Quantity = rtypes.NewResourceValue(20 * unit.Gi)

	return rus
}

// closeThroughReclamation closes the deployment the way a tenant does when
// its lease offered a reclamation window: the first close starts the window,
// the second completes the close once it has passed.
func (s *smSuite) closeThroughReclamation(did dv1.DeploymentID) {
	s.t.Helper()

	_, err := s.dhandler(s.Context(), &dvbeta.MsgCloseDeployment{ID: did})
	require.NoError(s.t, err)

	s.SetBlockTime(s.Context().BlockTime().Add(25 * time.Hour))
	_, err = s.dhandler(s.Context(), &dvbeta.MsgCloseDeployment{ID: did})
	require.NoError(s.t, err)
}

func TestLeaseResizeE2E_AdoptGrownVolume(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(smVolumeProviderAttrs())
	lid, volOrder := s.fullVolumeLease(owner, "pg-data", provider, s.defaultDeposit())

	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grownVolume(volOrder), nil))
	require.NoError(t, err)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(1), s.pendingDigest(lid)))
	require.NoError(t, err)

	s.closeThroughReclamation(lid.DeploymentID())
	adoptRef := smVolumeRef(volOrder)

	// the SDL the volume was first deployed with asks for 10Gi; the 20Gi of
	// retained data does not fit it
	msg := s.volumeCreateMsg(owner, "pg-data", s.defaultDeposit())
	msg.Groups[0].Volume.Adopt = &adoptRef
	_, err = s.dhandler(s.Context(), msg)
	require.ErrorIs(t, err, dv1.ErrInvalidVolumeAdoption)

	msg = s.volumeCreateMsg(owner, "pg-data", s.defaultDeposit())
	msg.Groups[0].Volume.Adopt = &adoptRef
	msg.Groups[0].Resources[0].Storage[0].Quantity = rtypes.NewResourceValue(20 * unit.Gi)
	_, err = s.dhandler(s.Context(), msg)
	require.NoError(t, err)
}

func TestLeaseResizeE2E_ReplicatingVolumeKeepsItsSize(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	window := 24 * time.Hour

	lease := func(msg *dvbeta.MsgCreateDeployment, provider sdk.AccAddress) (mv1.LeaseID, mvbeta.Order) {
		t.Helper()

		_, err := s.dhandler(s.Context(), msg)
		require.NoError(t, err)

		order, found := s.MarketKeeper().GetOrder(s.Context(), mv1.OrderID{Owner: owner, DSeq: msg.ID.DSeq, GSeq: 1, OSeq: 1})
		require.True(t, found)

		return s.bidAndLease(order, provider, &window), order
	}

	srcMsg := s.volumeCreateMsg(owner, "pg-data", s.defaultDeposit())
	srcMsg.Groups[0].Volume.MaxReplicas = 1
	srcLid, srcOrder := lease(srcMsg, s.createProvider(smVolumeProviderAttrs()))

	// a replica on another provider, as when migrating the volume
	repMsg := s.volumeCreateMsg(owner, "pg-data-r1", s.defaultDeposit())
	srcRef := smVolumeRef(srcOrder)
	repMsg.Groups[0].Volume.ReplicaOf = &srcRef
	repLid, repOrder := lease(repMsg, s.createProvider(smVolumeProviderAttrs()))

	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(srcLid, grownVolume(srcOrder), nil))
	require.ErrorIs(t, err, mv1.ErrInvalidLeaseResize, "the source of a live replica keeps its size")

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(repLid, grownVolume(repOrder), nil))
	require.ErrorIs(t, err, mv1.ErrInvalidLeaseResize, "a replica of a live source keeps its size")

	// once the replica is gone the source is a plain volume again
	s.closeThroughReclamation(repLid.DeploymentID())

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(srcLid, grownVolume(srcOrder), nil))
	require.NoError(t, err)
}

func TestLeaseResizeE2E_AcceptRechecksVolumeRules(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()

	volumeSize := func(lid mv1.LeaseID) uint64 {
		group, found := s.DeploymentKeeper().GetGroup(s.Context(), lid.GroupID())
		require.True(t, found)

		return group.GroupSpec.Resources[0].Storage[0].Quantity.Value()
	}

	t.Run("a replica created after the request blocks the accept", func(t *testing.T) {
		srcMsg := s.volumeCreateMsg(owner, "pg-data", s.defaultDeposit())
		srcMsg.Groups[0].Volume.MaxReplicas = 1
		srcLid, srcOrder := s.leaseVolume(srcMsg, s.createProvider(smVolumeProviderAttrs()))

		_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(srcLid, grownVolume(srcOrder), nil))
		require.NoError(t, err)
		digest := s.pendingDigest(srcLid)

		// the replica is created between the request and the accept
		repMsg := s.volumeCreateMsg(owner, "pg-data-r1", s.defaultDeposit())
		srcRef := smVolumeRef(srcOrder)
		repMsg.Groups[0].Volume.ReplicaOf = &srcRef
		_, err = s.dhandler(s.Context(), repMsg)
		require.NoError(t, err)

		_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(srcLid, uact(1), digest))
		require.ErrorIs(t, err, mv1.ErrInvalidLeaseResize)
		require.Equal(t, uint64(10*unit.Gi), volumeSize(srcLid), "nothing was applied")

		// with the replica gone the same request can be accepted
		_, err = s.dhandler(s.Context(), &dvbeta.MsgCloseDeployment{ID: repMsg.ID})
		require.NoError(t, err)

		_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(srcLid, uact(1), digest))
		require.NoError(t, err)
		require.Equal(t, uint64(20*unit.Gi), volumeSize(srcLid))
	})

	t.Run("a size cap lowered after the request blocks the accept", func(t *testing.T) {
		lid, order := s.leaseVolume(s.volumeCreateMsg(owner, "media", s.defaultDeposit()), s.createProvider(smVolumeProviderAttrs()))

		_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grownVolume(order), nil))
		require.NoError(t, err)
		digest := s.pendingDigest(lid)

		dparams, err := s.DeploymentKeeper().GetParams(s.Context())
		require.NoError(t, err)

		capped := dparams
		capped.MaxVolumeSize = 15 * unit.Gi
		require.NoError(t, s.DeploymentKeeper().SetParams(s.Context(), capped))

		_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(1), digest))
		require.ErrorIs(t, err, mv1.ErrInvalidLeaseResize)
		require.Equal(t, uint64(10*unit.Gi), volumeSize(lid), "nothing was applied")

		require.NoError(t, s.DeploymentKeeper().SetParams(s.Context(), dparams))

		_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(1), digest))
		require.NoError(t, err)
		require.Equal(t, uint64(20*unit.Gi), volumeSize(lid))
	})
}

func TestLeaseResizeE2E_ReclaimClearsPendingResize(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(nil)

	order, _ := s.createComputeDeployment(owner, s.defaultDeposit())
	window := 24 * time.Hour
	lid := s.bidAndLease(order, provider, &window)

	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grownCompute(order.Spec), newManifestHash(t)))
	require.NoError(t, err)
	digest := s.pendingDigest(lid)

	// the provider starts reclaiming the lease instead of answering
	_, err = s.mhandler(s.Context(), &mvbeta.MsgLeaseStartReclaim{
		ID:     lid,
		Reason: mv1.LeaseClosedReasonDecommissioned,
	})
	require.NoError(t, err)

	_, found := s.MarketKeeper().GetLeaseResize(s.Context(), lid.DeploymentID())
	require.False(t, found)
	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &mv1.EventLeaseResizeCanceled{
		ID:     lid,
		Reason: mv1.LeaseResizeCancelReasonLeaseClosed,
		Digest: digest,
	})

	// the request is gone, and a lease in reclamation takes no new one
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(2), digest))
	require.ErrorIs(t, err, mv1.ErrLeaseResizeNotFound)

	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, grownCompute(order.Spec), newManifestHash(t)))
	require.ErrorIs(t, err, mv1.ErrLeaseNotActive)

	group, _ := s.DeploymentKeeper().GetGroup(s.Context(), lid.GroupID())
	require.True(t, resourcesEqual(group.GroupSpec.Resources, order.Spec.Resources), "nothing was applied")

	lease, _ := s.MarketKeeper().GetLease(s.Context(), lid)
	require.Equal(t, uact(1), lease.Price)
}

func TestLeaseResizeE2E_ComputeShrink(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(nil)

	// a lease four times the minimum unit, at 4uact per block
	small := smComputeSpec(t).Resources
	spec := smComputeSpec(t)
	spec.Resources[0].CPU.Units = rtypes.NewResourceValue(small[0].CPU.Units.Value() * 4)
	spec.Resources[0].Memory.Quantity = rtypes.NewResourceValue(small[0].Memory.Quantity.Value() * 4)
	spec.Resources[0].Price = uact(4)

	order, _ := s.createComputeDeploymentWithSpec(owner, s.defaultDeposit(), spec)
	lid := s.bidAndLease(order, provider, nil)
	accID := lid.DeploymentID().ToEscrowAccountID()

	h0 := s.Context().BlockHeight()
	deposit := s.defaultDeposit().Amount.Int64()

	// back down to the minimum unit, with a ceiling of 2uact per block
	resources := order.Spec.Resources.Dup()
	resources[0].CPU.Units = small[0].CPU.Units
	resources[0].Memory.Quantity = small[0].Memory.Quantity
	resources[0].Price = uact(2)
	hash := newManifestHash(t)

	s.SetBlockHeight(h0 + 100)
	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, hash))
	require.NoError(t, err)

	pending, found := s.MarketKeeper().GetLeaseResize(s.Context(), lid.DeploymentID())
	require.True(t, found)
	require.Equal(t, uact(2), pending.MaxPrice)

	s.SetBlockHeight(h0 + 150)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(1), pending.Digest))
	require.NoError(t, err)

	lease, _ := s.MarketKeeper().GetLease(s.Context(), lid)
	require.Equal(t, mv1.LeaseActive, lease.State)
	require.Equal(t, uact(1), lease.Price)

	bid, _ := s.MarketKeeper().GetBid(s.Context(), lid.BidID())
	require.Equal(t, uact(1), bid.Price)

	group, _ := s.DeploymentKeeper().GetGroup(s.Context(), lid.GroupID())
	require.True(t, resourcesEqual(group.GroupSpec.Resources, resources))

	resizedOrder, _ := s.MarketKeeper().GetOrder(s.Context(), lid.OrderID())
	require.True(t, resourcesEqual(resizedOrder.Spec.Resources, resources))
	require.Equal(t, uact(2), resizedOrder.Price(), "the order ceiling follows the new resources")

	deployment, _ := s.DeploymentKeeper().GetDeployment(s.Context(), lid.DeploymentID())
	require.Equal(t, hash, deployment.Hash)

	events := s.Context().EventManager().ABCIEvents()
	smEnsureEvent(t, events, &mv1.EventLeaseResized{ID: lid, Price: uact(1), PrevPrice: uact(4), Digest: pending.Digest})

	// the 150 blocks before the accept were charged at the old 4uact
	funds := deposit - 150*4

	acc, err := s.EscrowKeeper().GetAccount(s.Context(), accID)
	require.NoError(t, err)
	require.Equal(t, h0+150, acc.State.SettledAt)
	require.Equal(t, sdkmath.LegacyNewDec(funds), acc.State.Funds[0].Amount)

	smEnsureEvent(t, events, &ev1.EventAccountRunway{
		ID:                       accID,
		ProjectedDepletionHeight: (h0 + 150) + funds,
	})

	payment, err := s.EscrowKeeper().GetPayment(s.Context(), lid.ToEscrowPaymentID())
	require.NoError(t, err)
	require.Equal(t, uact(1), payment.State.Rate)
	require.Equal(t, sdkmath.LegacyNewDec(150*4), payment.State.Balance.Amount)

	// and every block after it at the new 1uact; the provider's withdraw
	// settles those and pays out exactly what the lease earned
	s.SetBlockHeight(h0 + 250)
	_, err = s.mhandler(s.Context(), &mvbeta.MsgWithdrawLease{ID: lid})
	require.NoError(t, err)

	acc, err = s.EscrowKeeper().GetAccount(s.Context(), accID)
	require.NoError(t, err)
	require.Equal(t, sdkmath.LegacyNewDec(funds-100), acc.State.Funds[0].Amount)

	payment, err = s.EscrowKeeper().GetPayment(s.Context(), lid.ToEscrowPaymentID())
	require.NoError(t, err)
	require.Equal(t, sdk.NewInt64Coin(sdkutil.DenomUact, 150*4+100), payment.State.Withdrawn)
	require.True(t, payment.State.Balance.Amount.IsZero())
}

func TestLeaseResizeE2E_BucketGrow(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()
	s.enableLeaseResize()

	owner := testutil.AccAddress(t).String()

	msg := s.volumeCreateMsg(owner, "media", s.defaultDeposit())
	msg.Groups[0].Volume.Access = dv1.VolumeAccessBucket
	msg.Groups[0].Volume.MaxAttachments = 0

	lid, order := s.leaseVolume(msg, s.createProvider(smVolumeProviderAttrs()))
	accID := lid.DeploymentID().ToEscrowAccountID()

	h0 := s.Context().BlockHeight()
	deposit := s.defaultDeposit().Amount.Int64()

	resources := grownVolume(order)
	resources[0].Price = uact(2)

	// a bucket's size is not part of a manifest either
	_, err := s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, newManifestHash(t)))
	require.ErrorIs(t, err, mv1.ErrInvalidLeaseResize)

	s.SetBlockHeight(h0 + 10)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, resources, nil))
	require.NoError(t, err)

	hashBefore, _ := s.DeploymentKeeper().GetDeployment(s.Context(), lid.DeploymentID())
	digest := s.pendingDigest(lid)

	s.SetBlockHeight(h0 + 20)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgAcceptLeaseResize(lid, uact(2), digest))
	require.NoError(t, err)

	// the bucket's quota grows and it stays a bucket
	group, _ := s.DeploymentKeeper().GetGroup(s.Context(), lid.GroupID())
	require.Equal(t, uint64(20*unit.Gi), group.GroupSpec.Resources[0].Storage[0].Quantity.Value())
	require.Equal(t, order.Spec.Volume, group.GroupSpec.Volume, "the volume policy carries over")
	require.True(t, group.GroupSpec.Volume.IsBucket())

	resizedOrder, _ := s.MarketKeeper().GetOrder(s.Context(), lid.OrderID())
	require.Equal(t, uint64(20*unit.Gi), resizedOrder.Spec.Resources[0].Storage[0].Quantity.Value())
	require.True(t, resizedOrder.Spec.Volume.IsBucket())

	hashAfter, _ := s.DeploymentKeeper().GetDeployment(s.Context(), lid.DeploymentID())
	require.Equal(t, hashBefore.Hash, hashAfter.Hash)

	lease, _ := s.MarketKeeper().GetLease(s.Context(), lid)
	require.Equal(t, uact(2), lease.Price)

	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &mv1.EventLeaseResized{ID: lid, Price: uact(2), PrevPrice: uact(1), Digest: digest})

	// 20 blocks at 1uact before the accept, 10 at 2uact after it
	s.SetBlockHeight(h0 + 30)
	od, err := s.EscrowKeeper().AccountSettle(s.Context(), accID)
	require.NoError(t, err)
	require.False(t, od)

	acc, err := s.EscrowKeeper().GetAccount(s.Context(), accID)
	require.NoError(t, err)
	require.Equal(t, sdkmath.LegacyNewDec(deposit-20-20), acc.State.Funds[0].Amount)

	// like any volume, a bucket never shrinks
	shrink := resizedOrder.Spec.Resources.Dup()
	shrink[0].Storage[0].Quantity = rtypes.NewResourceValue(10 * unit.Gi)
	_, err = s.mhandler(s.Context(), mvbeta.NewMsgResizeLease(lid, shrink, nil))
	require.ErrorIs(t, err, dv1.ErrInvalidResize)
}
