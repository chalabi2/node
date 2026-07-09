package handler_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sdk "github.com/cosmos/cosmos-sdk/types"

	dv1 "pkg.akt.dev/go/node/deployment/v1"
	dtypes "pkg.akt.dev/go/node/deployment/v1beta5"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v2beta1"
	attr "pkg.akt.dev/go/node/types/attributes/v1"
	deposit "pkg.akt.dev/go/node/types/deposit/v1"
	ptypes "pkg.akt.dev/go/node/provider/v1beta4"
	testutil "pkg.akt.dev/node/v2/testutil/fixtures"

	market "pkg.akt.dev/node/v2/x/market"
	"pkg.akt.dev/node/v2/x/market/handler"
)

// ===========================
// Helpers
// ===========================

// volumeProviderAttrs are the storage capabilities matching the canonical
// volume fixture (class beta3, persistent).
func volumeProviderAttrs() attr.Attributes {
	return attr.Attributes{
		{Key: "capabilities/storage/1/class", Value: "beta3"},
		{Key: "capabilities/storage/1/persistent", Value: "true"},
	}
}

// enableVolumeOrders flips the AEP-87 feature flag on for the test.
func (st *testSuite) enableVolumeOrders() mvbeta.Params {
	st.t.Helper()

	params, err := st.MarketKeeper().GetParams(st.Context())
	require.NoError(st.t, err)

	params.VolumeOrdersEnabled = true
	require.NoError(st.t, st.MarketKeeper().SetParams(st.Context(), params))

	return params
}

// createVolumeOrder creates a volume deployment/group and its first order.
func (st *testSuite) createVolumeOrder(vid string) (mvbeta.Order, dtypes.GroupSpec) {
	st.t.Helper()

	deployment := testutil.Deployment(st.t)
	group := testutil.VolumeDeploymentGroup(st.t, deployment.ID, vid)

	err := st.DeploymentKeeper().Create(st.Context(), deployment, []dtypes.Group{group})
	require.NoError(st.t, err)

	order, err := st.MarketKeeper().CreateOrder(st.Context(), group.ID, group.GroupSpec, nil)
	require.NoError(st.t, err)

	return order, group.GroupSpec
}

// createVolumeLease creates a full volume lease held by the given provider
// address through the keeper (bypassing the handler gates), mirroring what a
// won volume bid records: the lease carries the bid's reclamation window.
func (st *testSuite) createVolumeLease(vid string, provider sdk.AccAddress, window time.Duration) (mv1.LeaseID, mvbeta.Bid, mvbeta.Order, dtypes.GroupSpec) {
	st.t.Helper()

	order, gspec := st.createVolumeOrder(vid)

	bidID := mv1.MakeBidID(order.ID, provider)
	bid, err := st.MarketKeeper().CreateBid(st.Context(), bidID, order.Price(), mvbeta.ResourceOfferFromRU(gspec.Resources), &window)
	require.NoError(st.t, err)

	err = st.MarketKeeper().CreateLease(st.Context(), bid)
	require.NoError(st.t, err)

	lease, found := st.MarketKeeper().GetLease(st.Context(), bid.ID.LeaseID())
	require.True(st.t, found)
	lease.Reclamation = &mv1.Reclamation{Window: window}
	require.NoError(st.t, st.MarketKeeper().SaveLease(st.Context(), lease))

	st.MarketKeeper().OnBidMatched(st.Context(), bid)
	st.MarketKeeper().OnOrderMatched(st.Context(), order)

	return bid.ID.LeaseID(), bid, order, gspec
}

// volumeRefFor addresses the canonical volume fixture of the given order.
func volumeRefFor(order mvbeta.Order) dv1.VolumeRef {
	return dv1.VolumeRef{
		Owner: order.ID.Owner,
		DSeq:  order.ID.DSeq,
		GSeq:  order.ID.GSeq,
		Name:  "data",
	}
}

// createComputeOrderWithRefs creates a compute deployment/group owned by the
// given tenant whose resources mount the given volume references.
func (st *testSuite) createComputeOrderWithRefs(owner string, refs ...dv1.VolumeRef) (mvbeta.Order, dtypes.GroupSpec) {
	st.t.Helper()

	deployment := testutil.Deployment(st.t)
	deployment.ID.Owner = owner
	group := testutil.DeploymentGroup(st.t, deployment.ID, 0)
	group.GroupSpec.Resources = testutil.Resources(st.t, testutil.WithDenom("uact"))
	group.GroupSpec.Resources[0].Volumes = refs
	// Any provider may match; the colocation contract is what is under test.
	group.GroupSpec.Requirements = attr.PlacementRequirements{}

	err := st.DeploymentKeeper().Create(st.Context(), deployment, []dtypes.Group{group})
	require.NoError(st.t, err)

	order, err := st.MarketKeeper().CreateOrder(st.Context(), group.ID, group.GroupSpec, nil)
	require.NoError(st.t, err)

	return order, group.GroupSpec
}

// attachCompute creates an attaching compute lease through the keeper: the
// lease-create routine writes the attachment index.
func (st *testSuite) attachCompute(volOrder mvbeta.Order, provider sdk.AccAddress) (mv1.LeaseID, mvbeta.Bid, mvbeta.Order) {
	st.t.Helper()

	order, gspec := st.createComputeOrderWithRefs(volOrder.ID.Owner, volumeRefFor(volOrder))

	bid, err := st.MarketKeeper().CreateBid(st.Context(), mv1.MakeBidID(order.ID, provider), order.Price(), mvbeta.ResourceOfferFromRU(gspec.Resources), nil)
	require.NoError(st.t, err)

	err = st.MarketKeeper().CreateLease(st.Context(), bid)
	require.NoError(st.t, err)

	st.MarketKeeper().OnBidMatched(st.Context(), bid)
	st.MarketKeeper().OnOrderMatched(st.Context(), order)

	return bid.ID.LeaseID(), bid, order
}

// volumeBidMsg builds a handler-path bid on a volume order.
func volumeBidMsg(order mvbeta.Order, gspec dtypes.GroupSpec, provider sdk.AccAddress, window *time.Duration) *mvbeta.MsgCreateBid {
	return &mvbeta.MsgCreateBid{
		ID:    mv1.MakeBidID(order.ID, provider),
		Price: order.Price(),
		Deposit: deposit.Deposit{
			Amount:  mvbeta.DefaultBidMinDepositACT,
			Sources: deposit.Sources{deposit.SourceBalance},
		},
		ResourcesOffer:    mvbeta.ResourceOfferFromRU(gspec.Resources),
		ReclamationWindow: window,
	}
}

func (st *testSuite) newVolumeProvider() ptypes.Provider {
	st.t.Helper()
	return st.createProvider(volumeProviderAttrs())
}

func (st *testSuite) hasOpenOrder(id dv1.GroupID, oseq uint32) bool {
	st.t.Helper()

	found := false
	st.MarketKeeper().WithOrdersForGroup(st.Context(), id, mvbeta.OrderOpen, func(order mvbeta.Order) bool {
		if order.ID.OSeq == oseq {
			found = true
		}
		return found
	})

	return found
}

// ===========================
// CreateBid: volume-group gates
// ===========================

func TestCreateBidVolume_FlagDisabled(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)

	// The order exists (created through the keeper) but the flag is off.
	order, gspec := suite.createVolumeOrder("pgdata")
	prov := suite.newVolumeProvider()
	providerAddr := sdk.MustAccAddressFromBech32(prov.Owner)

	window := 24 * time.Hour
	_, err := suite.handler(suite.Context(), volumeBidMsg(order, gspec, providerAddr, &window))
	require.ErrorIs(t, err, handler.ErrVolumeOrdersDisabled)
}

func TestCreateBidVolume_ReclamationFloor(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	order, gspec := suite.createVolumeOrder("pgdata")
	prov := suite.newVolumeProvider()
	providerAddr := sdk.MustAccAddressFromBech32(prov.Owner)

	// No window at all.
	_, err := suite.handler(suite.Context(), volumeBidMsg(order, gspec, providerAddr, nil))
	require.ErrorIs(t, err, handler.ErrVolumeReclamationWindowTooShort)

	// Above the generic minimum but below the volume floor (24h).
	short := 12 * time.Hour
	_, err = suite.handler(suite.Context(), volumeBidMsg(order, gspec, providerAddr, &short))
	require.ErrorIs(t, err, handler.ErrVolumeReclamationWindowTooShort)

	// At the floor.
	ok := 24 * time.Hour
	res, err := suite.handler(suite.Context(), volumeBidMsg(order, gspec, providerAddr, &ok))
	require.NoError(t, err)
	require.NotNil(t, res)
}

func TestCreateBidVolume_OfferQuantityMismatch(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	order, gspec := suite.createVolumeOrder("pgdata")
	prov := suite.newVolumeProvider()
	providerAddr := sdk.MustAccAddressFromBech32(prov.Owner)

	window := 24 * time.Hour
	msg := volumeBidMsg(order, gspec, providerAddr, &window)

	// Offer half the ordered storage: MatchGSpec does not check quantities,
	// the volume gate must.
	half := msg.ResourcesOffer[0].Resources.Storage[0].Quantity.Value() / 2
	msg.ResourcesOffer[0].Resources.Storage[0].Quantity = msg.ResourcesOffer[0].Resources.Storage[0].Quantity.Dup()
	msg.ResourcesOffer[0].Resources.Storage[0].Quantity.Val = msg.ResourcesOffer[0].Resources.Storage[0].Quantity.Val.SubRaw(int64(half)) // nolint: gosec

	_, err := suite.handler(suite.Context(), msg)
	require.ErrorIs(t, err, handler.ErrVolumeOfferMismatch)
}

func TestCreateBidVolume_AdoptionProviderGate(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	retainer := suite.newVolumeProvider()
	retainerAddr := sdk.MustAccAddressFromBech32(retainer.Owner)
	stranger := suite.newVolumeProvider()
	strangerAddr := sdk.MustAccAddressFromBech32(stranger.Owner)

	// Dead volume: full lease with the retaining provider, then closed.
	_, _, deadOrder, _ := suite.createVolumeLease("pgdata", retainerAddr, 24*time.Hour)
	err := suite.MarketKeeper().OnGroupClosed(suite.Context(), deadOrder.ID.GroupID(), dtypes.GroupClosed)
	require.NoError(t, err)

	// Adoption order: a new volume group whose policy adopts the dead one.
	deployment := testutil.Deployment(suite.t)
	deployment.ID.Owner = deadOrder.ID.Owner
	group := testutil.VolumeDeploymentGroup(suite.t, deployment.ID, "pgdata")
	adoptRef := volumeRefFor(deadOrder)
	group.GroupSpec.Volume.Adopt = &adoptRef

	require.NoError(t, suite.DeploymentKeeper().Create(suite.Context(), deployment, []dtypes.Group{group}))
	adoptOrder, err := suite.MarketKeeper().CreateOrder(suite.Context(), group.ID, group.GroupSpec, nil)
	require.NoError(t, err)

	window := 24 * time.Hour

	// A bid from any provider but the retainer is rejected.
	_, err = suite.handler(suite.Context(), volumeBidMsg(adoptOrder, group.GroupSpec, strangerAddr, &window))
	require.ErrorIs(t, err, handler.ErrVolumeAdoptionProvider)

	// The retaining provider passes the gate.
	res, err := suite.handler(suite.Context(), volumeBidMsg(adoptOrder, group.GroupSpec, retainerAddr, &window))
	require.NoError(t, err)
	require.NotNil(t, res)
}

// ===========================
// CreateBid: attaching-group gates (colocation)
// ===========================

// attachBidMsg builds a handler-path bid on an attaching compute order.
func attachBidMsg(order mvbeta.Order, gspec dtypes.GroupSpec, provider sdk.AccAddress) *mvbeta.MsgCreateBid {
	return &mvbeta.MsgCreateBid{
		ID:    mv1.MakeBidID(order.ID, provider),
		Price: order.Price(),
		Deposit: deposit.Deposit{
			Amount:  mvbeta.DefaultBidMinDepositACT,
			Sources: deposit.Sources{deposit.SourceBalance},
		},
		ResourcesOffer: mvbeta.ResourceOfferFromRU(gspec.Resources),
	}
}

func TestCreateBidAttach_Colocation(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	colocated := suite.createProvider(nil)
	colocatedAddr := sdk.MustAccAddressFromBech32(colocated.Owner)
	elsewhere := suite.createProvider(nil)
	elsewhereAddr := sdk.MustAccAddressFromBech32(elsewhere.Owner)

	_, _, volOrder, _ := suite.createVolumeLease("pgdata", colocatedAddr, 24*time.Hour)

	order, gspec := suite.createComputeOrderWithRefs(volOrder.ID.Owner, volumeRefFor(volOrder))

	// A provider without the volume lease fails the colocation contract.
	_, err := suite.handler(suite.Context(), attachBidMsg(order, gspec, elsewhereAddr))
	require.ErrorIs(t, err, handler.ErrVolumeNotColocated)

	// The volume's provider passes.
	res, err := suite.handler(suite.Context(), attachBidMsg(order, gspec, colocatedAddr))
	require.NoError(t, err)
	require.NotNil(t, res)
}

func TestCreateBidAttach_RefInvalid(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	prov := suite.createProvider(nil)
	providerAddr := sdk.MustAccAddressFromBech32(prov.Owner)

	_, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)

	t.Run("owner mismatch", func(t *testing.T) {
		ref := volumeRefFor(volOrder)
		ref.Owner = testutil.AccAddress(t).String()
		order, gspec := suite.createComputeOrderWithRefs(volOrder.ID.Owner, ref)
		_, err := suite.handler(suite.Context(), attachBidMsg(order, gspec, providerAddr))
		require.ErrorIs(t, err, handler.ErrVolumeRefInvalid)
	})

	t.Run("group not found", func(t *testing.T) {
		ref := volumeRefFor(volOrder)
		ref.DSeq++
		order, gspec := suite.createComputeOrderWithRefs(volOrder.ID.Owner, ref)
		_, err := suite.handler(suite.Context(), attachBidMsg(order, gspec, providerAddr))
		require.ErrorIs(t, err, handler.ErrVolumeRefInvalid)
	})

	t.Run("name mismatch", func(t *testing.T) {
		ref := volumeRefFor(volOrder)
		ref.Name = "nonsuch"
		order, gspec := suite.createComputeOrderWithRefs(volOrder.ID.Owner, ref)
		_, err := suite.handler(suite.Context(), attachBidMsg(order, gspec, providerAddr))
		require.ErrorIs(t, err, handler.ErrVolumeRefInvalid)
	})

	t.Run("not a volume group", func(t *testing.T) {
		computeOrder, _ := suite.createComputeOrderWithRefs(volOrder.ID.Owner)
		ref := dv1.VolumeRef{
			Owner: computeOrder.ID.Owner,
			DSeq:  computeOrder.ID.DSeq,
			GSeq:  computeOrder.ID.GSeq,
			Name:  "data",
		}
		order, gspec := suite.createComputeOrderWithRefs(volOrder.ID.Owner, ref)
		_, err := suite.handler(suite.Context(), attachBidMsg(order, gspec, providerAddr))
		require.ErrorIs(t, err, handler.ErrVolumeRefInvalid)
	})
}

func TestCreateBidAttach_AttachmentLimit(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	prov := suite.createProvider(nil)
	providerAddr := sdk.MustAccAddressFromBech32(prov.Owner)

	_, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)

	// First attach occupies the volume's single slot (max_attachments=1).
	suite.attachCompute(volOrder, providerAddr)

	order, gspec := suite.createComputeOrderWithRefs(volOrder.ID.Owner, volumeRefFor(volOrder))
	_, err := suite.handler(suite.Context(), attachBidMsg(order, gspec, providerAddr))
	require.ErrorIs(t, err, handler.ErrVolumeAttachmentLimit)
}

// ===========================
// CreateLease: re-verify (bid-to-lease race)
// ===========================

func TestCreateLeaseAttach_ReVerify(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	prov := suite.createProvider(nil)
	providerAddr := sdk.MustAccAddressFromBech32(prov.Owner)

	_, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)

	order, gspec := suite.createComputeOrderWithRefs(volOrder.ID.Owner, volumeRefFor(volOrder))
	bid, err := suite.MarketKeeper().CreateBid(suite.Context(), mv1.MakeBidID(order.ID, providerAddr), order.Price(), mvbeta.ResourceOfferFromRU(gspec.Resources), nil)
	require.NoError(t, err)
	suite.setupEscrowAccount(bid, order)

	// The volume lease closes between bid and lease.
	require.NoError(t, suite.MarketKeeper().OnGroupClosed(suite.Context(), volOrder.ID.GroupID(), dtypes.GroupClosed))

	_, err = suite.handler(suite.Context(), &mvbeta.MsgCreateLease{BidID: bid.ID})
	require.ErrorIs(t, err, handler.ErrVolumeNotColocated)
}

func TestCreateLeaseAttach_WritesIndexAndEvent(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	prov := suite.createProvider(nil)
	providerAddr := sdk.MustAccAddressFromBech32(prov.Owner)

	_, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()

	order, gspec := suite.createComputeOrderWithRefs(volOrder.ID.Owner, volumeRefFor(volOrder))
	bid, err := suite.MarketKeeper().CreateBid(suite.Context(), mv1.MakeBidID(order.ID, providerAddr), order.Price(), mvbeta.ResourceOfferFromRU(gspec.Resources), nil)
	require.NoError(t, err)
	suite.setupEscrowAccount(bid, order)

	require.False(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))

	res, err := suite.handler(suite.Context(), &mvbeta.MsgCreateLease{BidID: bid.ID})
	require.NoError(t, err)
	require.NotNil(t, res)

	require.True(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))
	assert.Equal(t, uint32(1), suite.MarketKeeper().AttachmentCount(suite.Context(), volGid))

	var attached []mv1.LeaseID
	suite.MarketKeeper().WithAttachments(suite.Context(), volGid, func(lid mv1.LeaseID) bool {
		attached = append(attached, lid)
		return false
	})
	require.Len(t, attached, 1)
	assert.Equal(t, bid.ID.LeaseID(), attached[0])

	testutil.EnsureEvent(t, res.Events, &mv1.EventVolumeAttached{
		LeaseID: bid.ID.LeaseID(),
		Volume:  volumeRefFor(volOrder),
	})
}

// ===========================
// Attachment index maintenance across close paths
// ===========================

func TestAttachmentIndex_TenantCloseLease(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	volLid, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()

	lid, bid, order := suite.attachCompute(volOrder, providerAddr)
	suite.setupLeaseEscrow(bid, order)

	require.True(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))

	res, err := suite.handler(suite.Context(), &mvbeta.MsgCloseLease{ID: lid})
	require.NoError(t, err)

	require.False(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))

	testutil.EnsureEvent(t, res.Events, &mv1.EventVolumeDetached{
		LeaseID: lid,
		Volume:  volumeRefFor(volOrder),
		Reason:  mv1.LeaseClosedReasonOwner,
	})

	// The volume lease is untouched by compute churn.
	volLease, found := suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseActive, volLease.State)
}

func TestAttachmentIndex_ProviderCloseBid(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	_, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()

	_, bid, order := suite.attachCompute(volOrder, providerAddr)
	suite.setupLeaseEscrow(bid, order)

	require.True(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))

	reason := mv1.LeaseClosedReason(10001)
	_, err := suite.handler(suite.Context(), &mvbeta.MsgCloseBid{ID: bid.ID, Reason: reason})
	require.NoError(t, err)

	require.False(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))
}

func TestAttachmentIndex_ComputeEscrowExhaustion(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	volLid, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()

	lid, _, order := suite.attachCompute(volOrder, providerAddr)

	// The compute deployment's escrow runs dry: the escrow cascade reaches
	// the market keeper via OnGroupClosed.
	err := suite.MarketKeeper().OnGroupClosed(suite.Context(), order.ID.GroupID(), dtypes.GroupInsufficientFunds)
	require.NoError(t, err)

	require.False(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))

	lease, found := suite.MarketKeeper().GetLease(suite.Context(), lid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseInsufficientFunds, lease.State)

	// The volume lease is untouched.
	volLease, found := suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseActive, volLease.State)
}

// ===========================
// Cascade-detach
// ===========================

func TestCascadeDetach_ReclaimCompletion(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	volLid, volBid, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()
	suite.setupLeaseEscrow(volBid, volOrder)

	lid, _, computeOrder := suite.attachCompute(volOrder, providerAddr)

	blockTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	suite.SetBlockHeight(100)
	ctx := suite.Context().WithBlockTime(blockTime)

	// Provider starts eviction.
	_, err := suite.handler(ctx, &mvbeta.MsgLeaseStartReclaim{
		ID:     volLid,
		Reason: mv1.LeaseClosedReasonVolumeEvict,
	})
	require.NoError(t, err)

	// The window is a floor for the provider direction too.
	early := suite.Context().WithBlockTime(blockTime.Add(time.Hour))
	_, err = suite.handler(early, &mvbeta.MsgCloseBid{ID: volBid.ID, Reason: mv1.LeaseClosedReasonVolumeEvict})
	require.ErrorIs(t, err, mv1.ErrReclamationWindowNotElapsed)
	require.True(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))

	// Past the deadline the close cascade-detaches.
	late := suite.Context().WithBlockTime(blockTime.Add(25 * time.Hour))
	res, err := suite.handler(late, &mvbeta.MsgCloseBid{ID: volBid.ID, Reason: mv1.LeaseClosedReasonVolumeEvict})
	require.NoError(t, err)

	// Invariant: no attachments survive the volume lease close.
	require.False(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))

	// The compute lease was force-closed with the detach reason.
	computeLease, found := suite.MarketKeeper().GetLease(suite.Context(), lid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseClosed, computeLease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeDetach, computeLease.Reason)

	testutil.EnsureEvent(t, res.Events, &mv1.EventVolumeDetached{
		LeaseID: lid,
		Volume:  volumeRefFor(volOrder),
		Reason:  mv1.LeaseClosedReasonVolumeDetach,
	})

	// The volume lease closed with the eviction reason.
	volLease, found := suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseClosed, volLease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeEvict, volLease.Reason)

	// Carve-out: both groups re-list — the compute workload re-orders and
	// the volume's re-order IS the migration.
	assert.True(t, suite.hasOpenOrder(computeOrder.ID.GroupID(), 2), "compute group must re-order")
	assert.True(t, suite.hasOpenOrder(volGid, 2), "volume group must re-list for migration")
}

func TestCascadeDetach_EscrowExhaustion(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	volLid, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()

	lid, _, computeOrder := suite.attachCompute(volOrder, providerAddr)

	// The volume deployment's escrow runs dry: the cascade cannot be
	// blocked and cannot wait for a reclaim window.
	err := suite.MarketKeeper().OnGroupClosed(suite.Context(), volGid, dtypes.GroupInsufficientFunds)
	require.NoError(t, err)

	require.False(t, suite.MarketKeeper().HasAttachments(suite.Context(), volGid))

	// The attached compute lease closed with a distinct, diagnosable
	// reason and its group re-ordered (separate escrow, it survives).
	computeLease, found := suite.MarketKeeper().GetLease(suite.Context(), lid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseClosed, computeLease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeDetach, computeLease.Reason)
	assert.True(t, suite.hasOpenOrder(computeOrder.ID.GroupID(), 2), "compute group must re-order")

	// The volume lease closed terminally: unfunded, no re-order —
	// adoption takes over.
	volLease, found := suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseInsufficientFunds, volLease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeUnfunded, volLease.Reason)
	assert.False(t, suite.hasOpenOrder(volGid, 2), "terminal close must not re-order")
}

// ===========================
// Tenant close routes through reclamation
// ===========================

func TestVolumeTenantCloseLease_RoutesThroughReclamation(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	volLid, volBid, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()
	suite.setupLeaseEscrow(volBid, volOrder)

	blockTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	suite.SetBlockHeight(100)
	ctx := suite.Context().WithBlockTime(blockTime)

	// First close does not close: the lease enters reclaiming.
	res, err := suite.handler(ctx, &mvbeta.MsgCloseLease{ID: volLid})
	require.NoError(t, err)
	require.NotNil(t, res)

	lease, found := suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	require.Equal(t, mv1.LeaseReclaiming, lease.State)
	require.NotNil(t, lease.Reclamation)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeMigrate, lease.Reclamation.Reason)
	assert.Equal(t, blockTime.Add(24*time.Hour).Unix(), lease.Reclamation.Deadline)

	// Inside the window the close is refused — the floor holds against the
	// tenant direction too.
	early := suite.Context().WithBlockTime(blockTime.Add(time.Hour))
	_, err = suite.handler(early, &mvbeta.MsgCloseLease{ID: volLid})
	require.ErrorIs(t, err, mv1.ErrReclamationWindowNotElapsed)

	// After the deadline the close completes as a migration.
	late := suite.Context().WithBlockTime(blockTime.Add(25 * time.Hour))
	_, err = suite.handler(late, &mvbeta.MsgCloseLease{ID: volLid})
	require.NoError(t, err)

	lease, found = suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseClosed, lease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeMigrate, lease.Reason)

	// Carve-out: the still-open group re-lists.
	assert.True(t, suite.hasOpenOrder(volGid, 2), "volume group must re-list for migration")
}

func TestVolumeTenantCloseLease_FloorRaisesShortWindow(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	// A window below the floor (possible only by bypassing the bid gate)
	// is raised to the governance floor on the tenant close.
	providerAddr := testutil.AccAddress(t)
	volLid, _, _, _ := suite.createVolumeLease("pgdata", providerAddr, time.Hour)

	blockTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ctx := suite.Context().WithBlockTime(blockTime)

	_, err := suite.handler(ctx, &mvbeta.MsgCloseLease{ID: volLid})
	require.NoError(t, err)

	lease, found := suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	require.Equal(t, mv1.LeaseReclaiming, lease.State)
	assert.Equal(t, blockTime.Add(24*time.Hour).Unix(), lease.Reclamation.Deadline)
}

func TestCloseDeployment_VolumeRoutesThroughReclamation(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	volLid, volBid, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()
	suite.setupLeaseEscrow(volBid, volOrder)

	did := volGid.DeploymentID()

	blockTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	suite.SetBlockHeight(100)
	ctx := suite.Context().WithBlockTime(blockTime)

	// First close starts reclamation and leaves the deployment active.
	_, err := suite.dhandler(ctx, &dtypes.MsgCloseDeployment{ID: did})
	require.NoError(t, err)

	deployment, found := suite.DeploymentKeeper().GetDeployment(suite.Context(), did)
	require.True(t, found)
	assert.Equal(t, dv1.DeploymentActive, deployment.State)

	lease, found := suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	require.Equal(t, mv1.LeaseReclaiming, lease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeClosedRetain, lease.Reclamation.Reason)

	// Inside the window the close is refused.
	early := suite.Context().WithBlockTime(blockTime.Add(time.Hour))
	_, err = suite.dhandler(early, &dtypes.MsgCloseDeployment{ID: did})
	require.ErrorIs(t, err, mv1.ErrReclamationWindowNotElapsed)

	// After the deadline the deployment closes; the volume lease carries
	// the substituted retain reason and no re-order fires.
	late := suite.Context().WithBlockTime(blockTime.Add(25 * time.Hour))
	_, err = suite.dhandler(late, &dtypes.MsgCloseDeployment{ID: did})
	require.NoError(t, err)

	deployment, found = suite.DeploymentKeeper().GetDeployment(suite.Context(), did)
	require.True(t, found)
	assert.Equal(t, dv1.DeploymentClosed, deployment.State)

	lease, found = suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseClosed, lease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeClosedRetain, lease.Reason)

	assert.False(t, suite.hasOpenOrder(volGid, 2), "terminal close must not re-order")
}

func TestCloseDeployment_VolumeAttachedRejected(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	_, volBid, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	suite.setupLeaseEscrow(volBid, volOrder)

	suite.attachCompute(volOrder, providerAddr)

	_, err := suite.dhandler(suite.Context(), &dtypes.MsgCloseDeployment{ID: volOrder.ID.GroupID().DeploymentID()})
	require.Error(t, err)
	require.Contains(t, err.Error(), "volume has attached leases")
}

func TestPauseGroup_VolumeRoutesThroughReclamation(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	volLid, volBid, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()
	suite.setupLeaseEscrow(volBid, volOrder)

	blockTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	suite.SetBlockHeight(100)
	ctx := suite.Context().WithBlockTime(blockTime)

	// First pause starts reclamation and leaves the group open — a pause
	// closes the volume lease just like a close, so the window applies.
	_, err := suite.dhandler(ctx, &dtypes.MsgPauseGroup{ID: volGid})
	require.NoError(t, err)

	group, found := suite.DeploymentKeeper().GetGroup(suite.Context(), volGid)
	require.True(t, found)
	assert.Equal(t, dtypes.GroupOpen, group.State)

	lease, found := suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	require.Equal(t, mv1.LeaseReclaiming, lease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeClosedRetain, lease.Reclamation.Reason)

	// Inside the window the pause is refused.
	early := suite.Context().WithBlockTime(blockTime.Add(time.Hour))
	_, err = suite.dhandler(early, &dtypes.MsgPauseGroup{ID: volGid})
	require.ErrorIs(t, err, mv1.ErrReclamationWindowNotElapsed)

	// After the deadline the pause completes: the group pauses and the
	// lease closes with the substituted retain reason, anchoring retention
	// at this block's wall clock.
	lateTime := blockTime.Add(25 * time.Hour)
	late := suite.Context().WithBlockTime(lateTime)
	_, err = suite.dhandler(late, &dtypes.MsgPauseGroup{ID: volGid})
	require.NoError(t, err)

	group, found = suite.DeploymentKeeper().GetGroup(suite.Context(), volGid)
	require.True(t, found)
	assert.Equal(t, dtypes.GroupPaused, group.State)

	lease, found = suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseClosed, lease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeClosedRetain, lease.Reason)
	assert.Equal(t, lateTime.Unix(), lease.ClosedAt)

	// The pause-close is the retention anchor the vid gate and adoption
	// validation read.
	closedAt, ok := suite.MarketKeeper().LastVolumeLeaseClosedAt(suite.Context(), volGid)
	require.True(t, ok)
	assert.Equal(t, lateTime.Unix(), closedAt.Unix())
}

func TestPauseGroup_VolumeAttachedRejected(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	_, volBid, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	suite.setupLeaseEscrow(volBid, volOrder)

	suite.attachCompute(volOrder, providerAddr)

	_, err := suite.dhandler(suite.Context(), &dtypes.MsgPauseGroup{ID: volOrder.ID.GroupID()})
	require.Error(t, err)
	require.Contains(t, err.Error(), "volume has attached leases")
}

// ===========================
// Retention anchor
// ===========================

func TestLastVolumeLeaseClosedAt(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	volLid, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()

	// No closed lease yet: no anchor.
	_, ok := suite.MarketKeeper().LastVolumeLeaseClosedAt(suite.Context(), volGid)
	require.False(t, ok)

	// The close funnel stamps the wall-clock anchor.
	closeTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ctx := suite.Context().WithBlockTime(closeTime)

	lease, found := suite.MarketKeeper().GetLease(ctx, volLid)
	require.True(t, found)
	require.NoError(t, suite.MarketKeeper().OnLeaseClosed(ctx, lease, mv1.LeaseClosed, mv1.LeaseClosedReasonVolumeClosedRetain))

	order, found := suite.MarketKeeper().GetOrder(ctx, volLid.OrderID())
	require.True(t, found)
	require.NoError(t, suite.MarketKeeper().OnOrderClosed(ctx, order))

	closedAt, ok := suite.MarketKeeper().LastVolumeLeaseClosedAt(suite.Context(), volGid)
	require.True(t, ok)
	assert.Equal(t, closeTime.Unix(), closedAt.Unix())

	provider, ok := suite.MarketKeeper().LastClosedVolumeLeaseProvider(suite.Context(), volGid)
	require.True(t, ok)
	assert.Equal(t, providerAddr.String(), provider)

	// Migration semantics: closed_at is zero on every lease closed before
	// the v2.2.0 upgrade added the field. A zero anchor reports not-found —
	// the dead volume behaves as expired — while the adoption provider gate
	// (which needs no wall clock) still resolves, so the upgrade needs no
	// backfill.
	lease, found = suite.MarketKeeper().GetLease(suite.Context(), volLid)
	require.True(t, found)
	lease.ClosedAt = 0
	require.NoError(t, suite.MarketKeeper().SaveLease(suite.Context(), lease))

	_, ok = suite.MarketKeeper().LastVolumeLeaseClosedAt(suite.Context(), volGid)
	require.False(t, ok)

	provider, ok = suite.MarketKeeper().LastClosedVolumeLeaseProvider(suite.Context(), volGid)
	require.True(t, ok)
	assert.Equal(t, providerAddr.String(), provider)
}

// ===========================
// Genesis export/import
// ===========================

func TestAttachmentIndex_GenesisRebuild(t *testing.T) {
	suite := setupTestSuite(t)
	prepareBlanketMocks(suite)
	suite.enableVolumeOrders()

	providerAddr := testutil.AccAddress(t)
	_, _, volOrder, _ := suite.createVolumeLease("pgdata", providerAddr, 24*time.Hour)
	volGid := volOrder.ID.GroupID()

	lid, _, _ := suite.attachCompute(volOrder, providerAddr)

	exported := market.ExportGenesis(suite.Context(), suite.MarketKeeper())

	// A fresh store rebuilds the attachment indexes from the leases and
	// their orders' specs — they are never carried in the document.
	fresh := setupTestSuite(t)
	market.InitGenesis(fresh.Context(), fresh.MarketKeeper(), exported)

	require.True(t, fresh.MarketKeeper().HasAttachments(fresh.Context(), volGid))
	assert.Equal(t, uint32(1), fresh.MarketKeeper().AttachmentCount(fresh.Context(), volGid))

	var attached []mv1.LeaseID
	fresh.MarketKeeper().WithAttachments(fresh.Context(), volGid, func(l mv1.LeaseID) bool {
		attached = append(attached, l)
		return false
	})
	require.Len(t, attached, 1)
	assert.Equal(t, lid, attached[0])
}
