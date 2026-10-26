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
	dvbeta "pkg.akt.dev/go/node/deployment/v1beta5"
	ev1 "pkg.akt.dev/go/node/escrow/v1"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v2beta1"
	rtypes "pkg.akt.dev/go/node/types/resources/v1beta4"
	"pkg.akt.dev/go/node/types/unit"
	"pkg.akt.dev/go/sdkutil"

	testutil "pkg.akt.dev/node/v2/testutil/fixtures"
)

func (s *smSuite) enableLeaseResize() {
	s.t.Helper()

	params, err := s.MarketKeeper().GetParams(s.Context())
	require.NoError(s.t, err)
	params.LeaseResizeEnabled = true
	require.NoError(s.t, s.MarketKeeper().SetParams(s.Context(), params))
}

// computeLease leases a 1uact/block compute deployment to the provider.
func (s *smSuite) computeLease(owner string, provider sdk.AccAddress, refs ...dv1.VolumeRef) (mv1.LeaseID, mvbeta.Order) {
	s.t.Helper()

	order, _ := s.createComputeDeployment(owner, s.defaultDeposit(), refs...)
	lid := s.bidAndLease(order, provider, nil)

	return lid, order
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
