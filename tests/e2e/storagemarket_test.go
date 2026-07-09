// Package e2e: AEP-87 decoupled storage market — fast, k8s-free chain e2e
// suite (IMPLEMENTATION.md §8.1).
//
// Unlike the x/deployment and x/market handler tests (which stub the
// cross-module seams), every scenario here drives the REAL deployment,
// market, and escrow message servers wired against the real keepers on the
// testutil/state app fixture — the same composition the chain runs, minus
// consensus: flag gating, the volume create→order→bid→lease happy path, vid
// gates, the attach lifecycle, cascade-detach through reclamation, escrow
// exhaustion settled through the real escrow account, runway events, and
// grant-funded (AuthorizeDeposits) volume deployments.
//
// This file carries no build tag on purpose: it needs no network fixture, so
// it runs in the plain `go test ./...` sweep.
package e2e

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	sdkmath "cosmossdk.io/math"
	cmtabci "github.com/cometbft/cometbft/abci/types"
	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authzkeeper "github.com/cosmos/cosmos-sdk/x/authz/keeper"
	"github.com/cosmos/gogoproto/proto"

	dv1 "pkg.akt.dev/go/node/deployment/v1"
	dvbeta "pkg.akt.dev/go/node/deployment/v1beta5"
	ev1 "pkg.akt.dev/go/node/escrow/v1"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v2beta1"
	ptypes "pkg.akt.dev/go/node/provider/v1beta4"
	attr "pkg.akt.dev/go/node/types/attributes/v1"
	deposit "pkg.akt.dev/go/node/types/deposit/v1"
	rtypes "pkg.akt.dev/go/node/types/resources/v1beta4"
	"pkg.akt.dev/go/sdkutil"

	testutil "pkg.akt.dev/node/v2/testutil/fixtures"
	"pkg.akt.dev/node/v2/testutil/state"
	bmemodule "pkg.akt.dev/node/v2/x/bme"
	dhandler "pkg.akt.dev/node/v2/x/deployment/handler"
	ehandler "pkg.akt.dev/node/v2/x/escrow/handler"
	mhandler "pkg.akt.dev/node/v2/x/market/handler"
)

// ===========================
// Suite plumbing
// ===========================

type smSuite struct {
	*state.TestSuite
	t        *testing.T
	dhandler baseapp.MsgServiceHandler
	mhandler baseapp.MsgServiceHandler
	ehandler baseapp.MsgServiceHandler
}

func setupStorageMarketSuite(t *testing.T) *smSuite {
	ss := state.SetupTestSuite(t)

	s := &smSuite{
		TestSuite: ss,
		t:         t,
		mhandler: mhandler.NewHandler(mhandler.Keepers{
			Escrow:     ss.EscrowKeeper(),
			Audit:      ss.AuditKeeper(),
			Market:     ss.MarketKeeper(),
			Deployment: ss.DeploymentKeeper(),
			Provider:   ss.ProviderKeeper(),
			Bank:       ss.BankKeeper(),
		}),
		dhandler: dhandler.NewHandler(ss.DeploymentKeeper(), ss.MarketKeeper(), ss.EscrowKeeper(), ss.BmeKeeper(), ss.BankKeeper()),
		ehandler: ehandler.NewHandler(ss.EscrowKeeper(), ss.AuthzKeeper(), ss.BankKeeper()),
	}

	// Blanket bank mocks: the escrow flows move real (mocked) coins on
	// every deposit, settle, withdraw, and refund leg.
	s.PrepareMocks(func(ts *state.TestSuite) {
		bk := ts.BankKeeper()
		bk.On("SendCoinsFromAccountToModule", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
		bk.On("SendCoinsFromModuleToAccount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
		bk.On("SendCoinsFromModuleToModule", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
		bk.On("MintCoins", mock.Anything, bmemodule.ModuleName, mock.Anything).Return(nil).Maybe()
		bk.On("BurnCoins", mock.Anything, bmemodule.ModuleName, mock.Anything).Return(nil).Maybe()
	})

	return s
}

// enableVolumeOrders flips the AEP-87 governance feature flag on.
func (s *smSuite) enableVolumeOrders() {
	s.t.Helper()

	params, err := s.MarketKeeper().GetParams(s.Context())
	require.NoError(s.t, err)
	params.VolumeOrdersEnabled = true
	require.NoError(s.t, s.MarketKeeper().SetParams(s.Context(), params))
}

// disableVolumeOrders flips the flag back off (defense-in-depth checks).
func (s *smSuite) disableVolumeOrders() {
	s.t.Helper()

	params, err := s.MarketKeeper().GetParams(s.Context())
	require.NoError(s.t, err)
	params.VolumeOrdersEnabled = false
	require.NoError(s.t, s.MarketKeeper().SetParams(s.Context(), params))
}

// smVolumeProviderAttrs are the storage capabilities matching the canonical
// volume fixture (class beta3, persistent).
func smVolumeProviderAttrs() attr.Attributes {
	return attr.Attributes{
		{Key: "capabilities/storage/1/class", Value: "beta3"},
		{Key: "capabilities/storage/1/persistent", Value: "true"},
	}
}

func (s *smSuite) createProvider(attrs attr.Attributes) sdk.AccAddress {
	s.t.Helper()

	prov := ptypes.Provider{
		Owner:      testutil.AccAddress(s.t).String(),
		HostURI:    "https://storage.example.com:8443",
		Attributes: attrs,
	}
	require.NoError(s.t, s.ProviderKeeper().Create(s.Context(), prov))

	return sdk.MustAccAddressFromBech32(prov.Owner)
}

func (s *smSuite) defaultDeposit() sdk.Coin {
	s.t.Helper()

	amount, err := dvbeta.DefaultParams().MinDepositFor(sdkutil.DenomUact)
	require.NoError(s.t, err)

	return amount
}

// volumeCreateMsg builds the MsgCreateDeployment carrying a single canonical
// volume group with the given vid.
func (s *smSuite) volumeCreateMsg(owner string, vid string, amount sdk.Coin) *dvbeta.MsgCreateDeployment {
	s.t.Helper()

	did := testutil.DeploymentID(s.t)
	did.Owner = owner

	return &dvbeta.MsgCreateDeployment{
		ID:     did,
		Groups: dvbeta.GroupSpecs{testutil.VolumeGroupSpec(s.t, vid)},
		Deposit: deposit.Deposit{
			Amount:  amount,
			Sources: deposit.Sources{deposit.SourceBalance},
		},
	}
}

// createVolumeDeployment drives MsgCreateDeployment through the real
// deployment server and returns the open volume order.
func (s *smSuite) createVolumeDeployment(owner string, vid string, amount sdk.Coin) (mvbeta.Order, *dvbeta.MsgCreateDeployment) {
	s.t.Helper()

	msg := s.volumeCreateMsg(owner, vid, amount)

	res, err := s.dhandler(s.Context(), msg)
	require.NoError(s.t, err)
	require.NotNil(s.t, res)

	order, found := s.MarketKeeper().GetOrder(s.Context(), mv1.OrderID{
		Owner: msg.ID.Owner,
		DSeq:  msg.ID.DSeq,
		GSeq:  1,
		OSeq:  1,
	})
	require.True(s.t, found, "volume order must be created with the deployment")
	require.Equal(s.t, mvbeta.OrderOpen, order.State)

	return order, msg
}

// bidAndLease drives MsgCreateBid + MsgCreateLease on the given order
// through the real market server and returns the lease ID.
func (s *smSuite) bidAndLease(order mvbeta.Order, provider sdk.AccAddress, window *time.Duration) mv1.LeaseID {
	s.t.Helper()

	bidID := mv1.MakeBidID(order.ID, provider)

	_, err := s.mhandler(s.Context(), &mvbeta.MsgCreateBid{
		ID:    bidID,
		Price: order.Price(),
		Deposit: deposit.Deposit{
			Amount:  mvbeta.DefaultBidMinDepositACT,
			Sources: deposit.Sources{deposit.SourceBalance},
		},
		ResourcesOffer:    mvbeta.ResourceOfferFromRU(order.Spec.Resources),
		ReclamationWindow: window,
	})
	require.NoError(s.t, err)

	_, err = s.mhandler(s.Context(), &mvbeta.MsgCreateLease{BidID: bidID})
	require.NoError(s.t, err)

	lid := bidID.LeaseID()
	lease, found := s.MarketKeeper().GetLease(s.Context(), lid)
	require.True(s.t, found)
	require.Equal(s.t, mv1.LeaseActive, lease.State)

	return lid
}

// fullVolumeLease is the message-driven happy path: volume deployment →
// order → bid (with reclamation window) → lease.
func (s *smSuite) fullVolumeLease(owner string, vid string, provider sdk.AccAddress, amount sdk.Coin) (mv1.LeaseID, mvbeta.Order) {
	s.t.Helper()

	order, _ := s.createVolumeDeployment(owner, vid, amount)
	window := 24 * time.Hour
	lid := s.bidAndLease(order, provider, &window)

	return lid, order
}

// smComputeSpec builds a minimal deterministic compute group spec priced at
// 1uact/block mounting the given volume refs. The fixed rate keeps escrow
// runway math exact in the exhaustion scenarios.
func smComputeSpec(t testing.TB, refs ...dv1.VolumeRef) dvbeta.GroupSpec {
	t.Helper()

	cfg := dvbeta.GetValidationConfig()

	return dvbeta.GroupSpec{
		Name:         "compute",
		Requirements: attr.PlacementRequirements{},
		Resources: dvbeta.ResourceUnits{
			{
				Resources: rtypes.Resources{
					ID:     1,
					CPU:    &rtypes.CPU{Units: rtypes.NewResourceValue(uint64(cfg.Unit.Min.CPU))}, // nolint: gosec
					GPU:    &rtypes.GPU{Units: rtypes.NewResourceValue(uint64(cfg.Unit.Min.GPU))}, // nolint: gosec
					Memory: &rtypes.Memory{Quantity: rtypes.NewResourceValue(cfg.Unit.Min.Memory)},
					Storage: rtypes.Volumes{
						rtypes.Storage{Quantity: rtypes.NewResourceValue(cfg.Unit.Min.Storage)},
					},
				},
				Count:   1,
				Price:   sdk.NewDecCoin(sdkutil.DenomUact, sdkmath.NewInt(1)),
				Volumes: refs,
			},
		},
	}
}

// createComputeDeployment drives an attaching compute MsgCreateDeployment
// through the real deployment server and returns the open compute order.
func (s *smSuite) createComputeDeployment(owner string, amount sdk.Coin, refs ...dv1.VolumeRef) (mvbeta.Order, *dvbeta.MsgCreateDeployment) {
	s.t.Helper()

	did := testutil.DeploymentID(s.t)
	did.Owner = owner

	msg := &dvbeta.MsgCreateDeployment{
		ID:     did,
		Groups: dvbeta.GroupSpecs{smComputeSpec(s.t, refs...)},
		Deposit: deposit.Deposit{
			Amount:  amount,
			Sources: deposit.Sources{deposit.SourceBalance},
		},
	}

	res, err := s.dhandler(s.Context(), msg)
	require.NoError(s.t, err)
	require.NotNil(s.t, res)

	order, found := s.MarketKeeper().GetOrder(s.Context(), mv1.OrderID{
		Owner: did.Owner,
		DSeq:  did.DSeq,
		GSeq:  1,
		OSeq:  1,
	})
	require.True(s.t, found)

	return order, msg
}

// smVolumeRef addresses the canonical volume fixture of the given order.
func smVolumeRef(order mvbeta.Order) dv1.VolumeRef {
	return dv1.VolumeRef{
		Owner: order.ID.Owner,
		DSeq:  order.ID.DSeq,
		GSeq:  order.ID.GSeq,
		Name:  "data",
	}
}

func smAttachBidMsg(order mvbeta.Order, provider sdk.AccAddress) *mvbeta.MsgCreateBid {
	return &mvbeta.MsgCreateBid{
		ID:    mv1.MakeBidID(order.ID, provider),
		Price: order.Price(),
		Deposit: deposit.Deposit{
			Amount:  mvbeta.DefaultBidMinDepositACT,
			Sources: deposit.Sources{deposit.SourceBalance},
		},
		ResourcesOffer: mvbeta.ResourceOfferFromRU(order.Spec.Resources),
	}
}

func (s *smSuite) hasOpenOrder(id dv1.GroupID, oseq uint32) bool {
	s.t.Helper()

	found := false
	s.MarketKeeper().WithOrdersForGroup(s.Context(), id, mvbeta.OrderOpen, func(order mvbeta.Order) bool {
		if order.ID.OSeq == oseq {
			found = true
		}
		return found
	})

	return found
}

// smEnsureEvent asserts the expected typed event is among the given ABCI
// events, tolerating untyped (non-proto) events in the stream — the context
// event manager mixes both.
func smEnsureEvent(t *testing.T, events []cmtabci.Event, expected proto.Message) {
	t.Helper()

	for _, e := range events {
		iev, err := sdk.ParseTypedEvent(e)
		if err != nil {
			continue
		}
		if reflect.DeepEqual(iev, expected) {
			return
		}
	}

	t.Errorf("events do not contain %v", expected)
}

// ===========================
// Flag gating
// ===========================

func TestStorageMarketE2E_FlagGating(t *testing.T) {
	s := setupStorageMarketSuite(t)
	owner := testutil.AccAddress(t).String()

	msg := s.volumeCreateMsg(owner, "pg-data", s.defaultDeposit())

	// Volume create is rejected while the governance flag ships off.
	res, err := s.dhandler(s.Context(), msg)
	require.Nil(t, res)
	require.ErrorIs(t, err, dv1.ErrVolumeOrdersDisabled)

	// The param flip frees the very same message.
	s.enableVolumeOrders()

	res, err = s.dhandler(s.Context(), msg)
	require.NoError(t, err)
	require.NotNil(t, res)

	order, found := s.MarketKeeper().GetOrder(s.Context(), mv1.OrderID{
		Owner: msg.ID.Owner, DSeq: msg.ID.DSeq, GSeq: 1, OSeq: 1,
	})
	require.True(t, found)

	// Defense-in-depth: flipping the flag back off gates bids on the
	// already-open volume order too.
	s.disableVolumeOrders()

	provider := s.createProvider(smVolumeProviderAttrs())
	window := 24 * time.Hour
	_, err = s.mhandler(s.Context(), &mvbeta.MsgCreateBid{
		ID:    mv1.MakeBidID(order.ID, provider),
		Price: order.Price(),
		Deposit: deposit.Deposit{
			Amount:  mvbeta.DefaultBidMinDepositACT,
			Sources: deposit.Sources{deposit.SourceBalance},
		},
		ResourcesOffer:    mvbeta.ResourceOfferFromRU(order.Spec.Resources),
		ReclamationWindow: &window,
	})
	require.ErrorIs(t, err, mhandler.ErrVolumeOrdersDisabled)
}

// ===========================
// Happy path: create → order → bid → lease
// ===========================

func TestStorageMarketE2E_VolumeLeaseHappyPath(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()

	owner := testutil.AccAddress(t).String()
	order, msg := s.createVolumeDeployment(owner, "pg-data", s.defaultDeposit())

	// The deployment, its single volume group, and the vid index resolve.
	deployment, found := s.DeploymentKeeper().GetDeployment(s.Context(), msg.ID)
	require.True(t, found)
	require.Equal(t, dv1.DeploymentActive, deployment.State)

	gid, found := s.DeploymentKeeper().GetVolumeByVid(s.Context(), owner, "pg-data")
	require.True(t, found)
	require.Equal(t, order.ID.GroupID(), gid)

	group, found := s.DeploymentKeeper().GetGroup(s.Context(), gid)
	require.True(t, found)
	require.NotNil(t, group.GroupSpec.Volume)
	require.Equal(t, "pg-data", group.GroupSpec.Volume.Vid)

	// The escrow account was funded by the create.
	eacc, err := s.EscrowKeeper().GetAccount(s.Context(), msg.ID.ToEscrowAccountID())
	require.NoError(t, err)
	require.Equal(t, sdkmath.LegacyNewDecFromInt(msg.Deposit.Amount.Amount), eacc.State.Funds[0].Amount)

	// Bid (with the reclamation-floor window) and lease through the real
	// market server.
	provider := s.createProvider(smVolumeProviderAttrs())
	window := 24 * time.Hour
	lid := s.bidAndLease(order, provider, &window)

	// The lease records the provider's retain-through-migration window and
	// its escrow payment streams at the order price.
	lease, found := s.MarketKeeper().GetLease(s.Context(), lid)
	require.True(t, found)
	require.NotNil(t, lease.Reclamation)
	assert.Equal(t, window, lease.Reclamation.Window)

	payment, err := s.EscrowKeeper().GetPayment(s.Context(), lid.ToEscrowPaymentID())
	require.NoError(t, err)
	assert.Equal(t, order.Price(), payment.State.Rate)

	// A volume group holds no attachments of its own.
	require.False(t, s.MarketKeeper().HasAttachments(s.Context(), gid))
}

// ===========================
// vid uniqueness + adoption gates
// ===========================

func TestStorageMarketE2E_VidGates(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()

	owner := testutil.AccAddress(t).String()
	firstOrder, firstMsg := s.createVolumeDeployment(owner, "pg-data", s.defaultDeposit())

	t.Run("same owner same vid rejected while live", func(t *testing.T) {
		msg := s.volumeCreateMsg(owner, "pg-data", s.defaultDeposit())
		_, err := s.dhandler(s.Context(), msg)
		require.ErrorIs(t, err, dv1.ErrVidInUse)
	})

	t.Run("same vid under another owner allowed", func(t *testing.T) {
		other := testutil.AccAddress(t).String()
		msg := s.volumeCreateMsg(other, "pg-data", s.defaultDeposit())
		res, err := s.dhandler(s.Context(), msg)
		require.NoError(t, err)
		require.NotNil(t, res)
	})

	// Terminal close of the first volume (no lease yet, so the close
	// completes immediately) frees the vid.
	_, err := s.dhandler(s.Context(), &dvbeta.MsgCloseDeployment{ID: firstMsg.ID})
	require.NoError(t, err)

	t.Run("adoption rejected without a closed-lease retention anchor", func(t *testing.T) {
		// The first volume died without ever holding a lease, so no
		// provider retains its data: there is no closed lease to anchor a
		// retention window, adoption has nothing to adopt from, and the
		// dead vid behaves as expired rather than retained.
		msg := s.volumeCreateMsg(owner, "pg-data", s.defaultDeposit())
		adoptRef := smVolumeRef(firstOrder)
		msg.Groups[0].Volume.Adopt = &adoptRef

		_, err := s.dhandler(s.Context(), msg)
		require.ErrorIs(t, err, dv1.ErrInvalidVolumeAdoption)
	})

	t.Run("dead vid is re-mintable and the index moves", func(t *testing.T) {
		msg := s.volumeCreateMsg(owner, "pg-data", s.defaultDeposit())
		res, err := s.dhandler(s.Context(), msg)
		require.NoError(t, err)
		require.NotNil(t, res)

		gid, found := s.DeploymentKeeper().GetVolumeByVid(s.Context(), owner, "pg-data")
		require.True(t, found)
		assert.Equal(t, dv1.MakeGroupID(msg.ID, 1), gid)
		assert.NotEqual(t, firstOrder.ID.GroupID(), gid)
	})
}

// ===========================
// Attach lifecycle
// ===========================

func TestStorageMarketE2E_AttachLifecycle(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()

	owner := testutil.AccAddress(t).String()
	colocated := s.createProvider(smVolumeProviderAttrs())
	elsewhere := s.createProvider(nil)

	volLid, volOrder := s.fullVolumeLease(owner, "pg-data", colocated, s.defaultDeposit())
	volGid := volOrder.ID.GroupID()

	computeOrder, _ := s.createComputeDeployment(owner, s.defaultDeposit(), smVolumeRef(volOrder))

	// Colocation: a provider without the volume lease is rejected; the
	// volume's provider is accepted.
	_, err := s.mhandler(s.Context(), smAttachBidMsg(computeOrder, elsewhere))
	require.ErrorIs(t, err, mhandler.ErrVolumeNotColocated)

	_, err = s.mhandler(s.Context(), smAttachBidMsg(computeOrder, colocated))
	require.NoError(t, err)

	bidID := mv1.MakeBidID(computeOrder.ID, colocated)
	res, err := s.mhandler(s.Context(), &mvbeta.MsgCreateLease{BidID: bidID})
	require.NoError(t, err)

	computeLid := bidID.LeaseID()

	// The lease-create routine wrote the attachment index and announced
	// the attachment.
	require.True(t, s.MarketKeeper().HasAttachments(s.Context(), volGid))
	assert.Equal(t, uint32(1), s.MarketKeeper().AttachmentCount(s.Context(), volGid))
	testutil.EnsureEvent(t, res.Events, &mv1.EventVolumeAttached{
		LeaseID: computeLid,
		Volume:  smVolumeRef(volOrder),
	})

	// Closing the compute lease prunes the indexes, announces the detach,
	// and leaves the volume lease untouched.
	res, err = s.mhandler(s.Context(), &mvbeta.MsgCloseLease{ID: computeLid})
	require.NoError(t, err)

	require.False(t, s.MarketKeeper().HasAttachments(s.Context(), volGid))
	testutil.EnsureEvent(t, res.Events, &mv1.EventVolumeDetached{
		LeaseID: computeLid,
		Volume:  smVolumeRef(volOrder),
		Reason:  mv1.LeaseClosedReasonOwner,
	})

	volLease, found := s.MarketKeeper().GetLease(s.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseActive, volLease.State)

	// The compute workload re-lists (separate escrow, group still open).
	assert.True(t, s.hasOpenOrder(computeOrder.ID.GroupID(), 2), "compute group must re-order")
}

// ===========================
// Cascade-detach: volume closed through reclamation
// ===========================

func TestStorageMarketE2E_CascadeDetachThroughReclamation(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(smVolumeProviderAttrs())

	volLid, volOrder := s.fullVolumeLease(owner, "pg-data", provider, s.defaultDeposit())
	volGid := volOrder.ID.GroupID()

	computeOrder, _ := s.createComputeDeployment(owner, s.defaultDeposit(), smVolumeRef(volOrder))
	_, err := s.mhandler(s.Context(), smAttachBidMsg(computeOrder, provider))
	require.NoError(t, err)
	computeBidID := mv1.MakeBidID(computeOrder.ID, provider)
	_, err = s.mhandler(s.Context(), &mvbeta.MsgCreateLease{BidID: computeBidID})
	require.NoError(t, err)
	computeLid := computeBidID.LeaseID()

	// While attached, the tenant cannot close the volume deployment.
	_, err = s.dhandler(s.Context(), &dvbeta.MsgCloseDeployment{ID: volOrder.ID.GroupID().DeploymentID()})
	require.ErrorIs(t, err, dv1.ErrVolumeAttached)

	blockTime := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	ctx := s.Context().WithBlockTime(blockTime)

	// The tenant close of the volume LEASE routes through reclamation: the
	// first close starts the window instead of closing.
	_, err = s.mhandler(ctx, &mvbeta.MsgCloseLease{ID: volLid})
	require.NoError(t, err)

	volLease, found := s.MarketKeeper().GetLease(s.Context(), volLid)
	require.True(t, found)
	require.Equal(t, mv1.LeaseReclaiming, volLease.State)
	require.NotNil(t, volLease.Reclamation)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeMigrate, volLease.Reclamation.Reason)
	assert.Equal(t, blockTime.Add(24*time.Hour).Unix(), volLease.Reclamation.Deadline)

	// The floor holds inside the window.
	early := s.Context().WithBlockTime(blockTime.Add(time.Hour))
	_, err = s.mhandler(early, &mvbeta.MsgCloseLease{ID: volLid})
	require.ErrorIs(t, err, mv1.ErrReclamationWindowNotElapsed)
	require.True(t, s.MarketKeeper().HasAttachments(s.Context(), volGid))

	// Past the deadline the close cascade-detaches, then closes the volume.
	late := s.Context().WithBlockTime(blockTime.Add(25 * time.Hour))
	res, err := s.mhandler(late, &mvbeta.MsgCloseLease{ID: volLid})
	require.NoError(t, err)

	// Invariant: no attachments survive the volume lease close.
	require.False(t, s.MarketKeeper().HasAttachments(s.Context(), volGid))

	// The attached compute lease was force-closed with the detach reason.
	computeLease, found := s.MarketKeeper().GetLease(s.Context(), computeLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseClosed, computeLease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeDetach, computeLease.Reason)
	testutil.EnsureEvent(t, res.Events, &mv1.EventVolumeDetached{
		LeaseID: computeLid,
		Volume:  smVolumeRef(volOrder),
		Reason:  mv1.LeaseClosedReasonVolumeDetach,
	})

	// The volume lease closed under the reclamation reason it declared.
	volLease, found = s.MarketKeeper().GetLease(s.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseClosed, volLease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeMigrate, volLease.Reason)

	// Migration carve-out: both groups re-list.
	assert.True(t, s.hasOpenOrder(volGid, 2), "volume group must re-list for migration")
	assert.True(t, s.hasOpenOrder(computeOrder.ID.GroupID(), 2), "compute group must re-order")
}

// ===========================
// Escrow exhaustion cascade
// ===========================

func TestStorageMarketE2E_EscrowExhaustionCascade(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()

	// Lower the deposit floor so the volume account can be funded with a
	// deposit tiny relative to its 1uact/block lease rate.
	dparams, err := s.DeploymentKeeper().GetParams(s.Context())
	require.NoError(t, err)
	dparams.MinDeposits = sdk.Coins{
		sdk.NewInt64Coin(sdkutil.DenomUakt, 1000),
		sdk.NewInt64Coin(sdkutil.DenomUact, 1000),
	}
	require.NoError(t, s.DeploymentKeeper().SetParams(s.Context(), dparams))

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(smVolumeProviderAttrs())

	tiny := sdk.NewInt64Coin(sdkutil.DenomUact, 1000)
	volLid, volOrder := s.fullVolumeLease(owner, "pg-data", provider, tiny)
	volGid := volOrder.ID.GroupID()
	volDid := volGid.DeploymentID()

	// The attached compute deployment is funded well past the horizon at
	// which the volume account runs dry.
	computeOrder, computeMsg := s.createComputeDeployment(owner, sdk.NewInt64Coin(sdkutil.DenomUact, 500000), smVolumeRef(volOrder))
	_, err = s.mhandler(s.Context(), smAttachBidMsg(computeOrder, provider))
	require.NoError(t, err)
	computeBidID := mv1.MakeBidID(computeOrder.ID, provider)
	_, err = s.mhandler(s.Context(), &mvbeta.MsgCreateLease{BidID: computeBidID})
	require.NoError(t, err)
	computeLid := computeBidID.LeaseID()

	// Past the volume account's runway (1000 blocks at 1uact/block) the
	// settle overdraws it and the escrow cascade fires: payment hooks close
	// the volume lease, which cascade-detaches the compute lease; account
	// hooks close the deployment and its group.
	s.SetBlockHeight(s.Context().BlockHeight() + 5000)
	_, err = s.EscrowKeeper().AccountSettle(s.Context(), volDid.ToEscrowAccountID())
	require.NoError(t, err)

	// The volume lease terminated as unfunded — the diagnosable adoption
	// trigger, never a generic close.
	volLease, found := s.MarketKeeper().GetLease(s.Context(), volLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseInsufficientFunds, volLease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeUnfunded, volLease.Reason)

	// The attached compute lease was force-closed with the detach reason
	// and its group re-ordered (separate escrow, it survives).
	computeLease, found := s.MarketKeeper().GetLease(s.Context(), computeLid)
	require.True(t, found)
	assert.Equal(t, mv1.LeaseClosed, computeLease.State)
	assert.Equal(t, mv1.LeaseClosedReasonVolumeDetach, computeLease.Reason)
	require.False(t, s.MarketKeeper().HasAttachments(s.Context(), volGid))
	assert.True(t, s.hasOpenOrder(computeOrder.ID.GroupID(), 2), "compute group must re-order")

	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &mv1.EventVolumeDetached{
		LeaseID: computeLid,
		Volume:  smVolumeRef(volOrder),
		Reason:  mv1.LeaseClosedReasonVolumeDetach,
	})

	// The volume deployment closed terminally: group insufficient_funds,
	// no re-order — adoption takes over from here.
	deployment, found := s.DeploymentKeeper().GetDeployment(s.Context(), volDid)
	require.True(t, found)
	assert.Equal(t, dv1.DeploymentClosed, deployment.State)

	group, found := s.DeploymentKeeper().GetGroup(s.Context(), volGid)
	require.True(t, found)
	assert.Equal(t, dvbeta.GroupInsufficientFunds, group.State)
	assert.False(t, s.hasOpenOrder(volGid, 2), "terminal close must not re-order")

	// The healthy compute account was untouched by the cascade.
	eacc, err := s.EscrowKeeper().GetAccount(s.Context(), computeMsg.ID.ToEscrowAccountID())
	require.NoError(t, err)
	assert.True(t, eacc.State.Funds[0].Amount.IsPositive(), "compute escrow must stay funded")
}

// ===========================
// Runway events at settles
// ===========================

func TestStorageMarketE2E_RunwayEvents(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()

	owner := testutil.AccAddress(t).String()
	provider := s.createProvider(smVolumeProviderAttrs())

	amount := s.defaultDeposit() // 500000uact against a 1uact/block rate
	_, volOrder := s.fullVolumeLease(owner, "pg-data", provider, amount)
	accID := volOrder.ID.GroupID().DeploymentID().ToEscrowAccountID()

	h0 := s.Context().BlockHeight()

	// First settle: 1000 blocks in, 1000uact drained.
	s.SetBlockHeight(h0 + 1000)
	od, err := s.EscrowKeeper().AccountSettle(s.Context(), accID)
	require.NoError(t, err)
	require.False(t, od)

	funds := amount.Amount.Int64() - 1000
	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &ev1.EventAccountRunway{
		ID:                       accID,
		ProjectedDepletionHeight: (h0 + 1000) + funds,
	})

	// Second settle: the projection tracks the drained funds and the new
	// height — the tenant can recompute its top-up deadline every settle.
	s.SetBlockHeight(h0 + 3000)
	od, err = s.EscrowKeeper().AccountSettle(s.Context(), accID)
	require.NoError(t, err)
	require.False(t, od)

	funds = amount.Amount.Int64() - 3000
	smEnsureEvent(t, s.Context().EventManager().ABCIEvents(), &ev1.EventAccountRunway{
		ID:                       accID,
		ProjectedDepletionHeight: (h0 + 3000) + funds,
	})
}

// ===========================
// Grant-funded volume deployment (AuthorizeDeposits)
// ===========================

func TestStorageMarketE2E_GrantFundedVolume(t *testing.T) {
	s := setupStorageMarketSuite(t)
	s.enableVolumeOrders()

	ownerAddr := testutil.AccAddress(t)
	granter := testutil.AccAddress(t)
	amount := s.defaultDeposit()

	// The granter authorized deployment deposits for the owner. Volume
	// deployments ride MsgCreateDeployment unchanged, so the existing
	// DepositAuthorization must fund them with zero new switch entries.
	msgTypeURL := (&ev1.DepositAuthorization{}).MsgTypeURL()
	s.AuthzKeeper().
		On("GetGranteeGrantsByMsgType", mock.Anything, ownerAddr, msgTypeURL, mock.Anything).
		Run(func(args mock.Arguments) {
			onGrant := args.Get(3).(authzkeeper.OnGrantFn)
			authorization := &ev1.DepositAuthorization{
				Scopes:      ev1.DepositAuthorizationScopes{ev1.DepositScopeDeployment},
				SpendLimits: sdk.Coins{amount},
			}
			_ = onGrant(context.TODO(), granter, authorization, &time.Time{})
		}).
		Once()
	s.AuthzKeeper().
		On("SaveGrant", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()
	s.AuthzKeeper().
		On("GetAuthorization", mock.Anything, mock.Anything, mock.Anything, msgTypeURL).
		Return(nil, nil).Maybe()

	msg := s.volumeCreateMsg(ownerAddr.String(), "pg-data", amount)
	msg.Deposit.Sources = deposit.Sources{deposit.SourceGrant}

	res, err := s.dhandler(s.Context(), msg)
	require.NoError(t, err)
	require.NotNil(t, res)

	// The escrow account exists, fully funded by the granter.
	eacc, err := s.EscrowKeeper().GetAccount(s.Context(), msg.ID.ToEscrowAccountID())
	require.NoError(t, err)
	require.Len(t, eacc.State.Deposits, 1)
	assert.Equal(t, granter.String(), eacc.State.Deposits[0].Owner)
	assert.Equal(t, deposit.SourceGrant, eacc.State.Deposits[0].Source)
	assert.Equal(t, sdkmath.LegacyNewDecFromInt(amount.Amount), eacc.State.Funds[0].Amount)

	// The grant-funded volume proceeds through bid and lease like any
	// other — the deposit source is invisible to the market.
	order, found := s.MarketKeeper().GetOrder(s.Context(), mv1.OrderID{
		Owner: msg.ID.Owner, DSeq: msg.ID.DSeq, GSeq: 1, OSeq: 1,
	})
	require.True(t, found)

	provider := s.createProvider(smVolumeProviderAttrs())
	window := 24 * time.Hour
	lid := s.bidAndLease(order, provider, &window)

	payment, err := s.EscrowKeeper().GetPayment(s.Context(), lid.ToEscrowPaymentID())
	require.NoError(t, err)
	assert.Equal(t, order.Price(), payment.State.Rate)
}
