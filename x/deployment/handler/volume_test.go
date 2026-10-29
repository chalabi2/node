package handler_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"

	"pkg.akt.dev/go/node/deployment/v1"
	dvbeta "pkg.akt.dev/go/node/deployment/v1beta4"
	mv1 "pkg.akt.dev/go/node/market/v1"
	deposit "pkg.akt.dev/go/node/types/deposit/v1"
	rtypes "pkg.akt.dev/go/node/types/resources/v1beta4"
	"pkg.akt.dev/go/node/types/unit"
	"pkg.akt.dev/go/testutil"

	"pkg.akt.dev/node/v2/x/deployment/handler"
	mkeeper "pkg.akt.dev/node/v2/x/market/keeper"
)

// volumeMarket wraps the real market keeper, overriding the storage-market narrow
// methods the market module implements in a later stage so the handler
// gates can be exercised now.
type volumeMarket struct {
	mkeeper.IKeeper
	enabled       bool
	attached      map[v1.GroupID]bool
	leaseClosedAt map[v1.GroupID]time.Time
}

func (m *volumeMarket) VolumeOrdersEnabled(_ sdk.Context) bool {
	return m.enabled
}

func (m *volumeMarket) HasAttachments(_ sdk.Context, id v1.GroupID) bool {
	return m.attached[id]
}

func (m *volumeMarket) LastVolumeLeaseClosedAt(_ sdk.Context, id v1.GroupID) (time.Time, bool) {
	t, ok := m.leaseClosedAt[id]
	return t, ok
}

type volumeTestSuite struct {
	*testSuite
	market   *volumeMarket
	vhandler baseapp.MsgServiceHandler
	owner    string
}

func setupVolumeTestSuite(t *testing.T) *volumeTestSuite {
	suite := setupTestSuite(t)

	market := &volumeMarket{
		IKeeper:       suite.mkeeper,
		enabled:       true,
		attached:      make(map[v1.GroupID]bool),
		leaseClosedAt: make(map[v1.GroupID]time.Time),
	}

	return &volumeTestSuite{
		testSuite: suite,
		market:    market,
		vhandler:  handler.NewHandler(suite.dkeeper, market, suite.EscrowKeeper(), suite.BmeKeeper(), suite.BankKeeper()),
		owner:     testutil.AccAddress(t).String(),
	}
}

// createVolumeMsg builds a MsgCreateDeployment carrying a single volume
// group with the given vid, owned by the suite owner.
func (vs *volumeTestSuite) createVolumeMsg(t *testing.T, vid string) *dvbeta.MsgCreateDeployment {
	t.Helper()

	did := testutil.DeploymentID(t)
	did.Owner = vs.owner

	return &dvbeta.MsgCreateDeployment{
		ID:     did,
		Groups: dvbeta.GroupSpecs{testutil.VolumeGroupSpec(t, vid)},
		Deposit: deposit.Deposit{
			Amount:  vs.defaultDeposit,
			Sources: deposit.Sources{deposit.SourceBalance},
		},
	}
}

func volumeGroupID(msg *dvbeta.MsgCreateDeployment) v1.GroupID {
	return v1.MakeGroupID(msg.ID, 1)
}

func volumeRef(msg *dvbeta.MsgCreateDeployment) *v1.VolumeRef {
	return &v1.VolumeRef{
		Owner: msg.ID.Owner,
		DSeq:  msg.ID.DSeq,
		GSeq:  1,
		Name:  "data",
	}
}

func TestVolumeCreateFlagGated(t *testing.T) {
	vs := setupVolumeTestSuite(t)
	vs.market.enabled = false

	msg := vs.createVolumeMsg(t, "pg-data")

	res, err := vs.vhandler(vs.ctx, msg)
	require.Nil(t, res)
	require.True(t, errors.Is(err, v1.ErrVolumeOrdersDisabled))

	// flipping the flag frees the same message
	vs.market.enabled = true

	res, err = vs.vhandler(vs.ctx, msg)
	require.NoError(t, err)
	require.NotNil(t, res)
}

func TestVolumeCreateSingleGroupInvariant(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	msg := vs.createVolumeMsg(t, "pg-data")
	compute, groups := vs.createDeployment()
	_ = compute
	msg.Groups = append(msg.Groups, groups[0].GroupSpec)

	res, err := vs.vhandler(vs.ctx, msg)
	require.Nil(t, res)
	require.True(t, errors.Is(err, v1.ErrInvalidGroups))
}

func TestVolumeVidUniqueness(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	first := vs.createVolumeMsg(t, "pg-data")
	res, err := vs.vhandler(vs.ctx, first)
	require.NoError(t, err)
	require.NotNil(t, res)

	t.Run("same owner same vid rejected while live", func(t *testing.T) {
		second := vs.createVolumeMsg(t, "pg-data")
		res, err := vs.vhandler(vs.ctx, second)
		require.Nil(t, res)
		require.True(t, errors.Is(err, v1.ErrVidInUse))
	})

	t.Run("same vid under another owner allowed", func(t *testing.T) {
		other := vs.createVolumeMsg(t, "pg-data")
		other.ID.Owner = testutil.AccAddress(t).String()
		res, err := vs.vhandler(vs.ctx, other)
		require.NoError(t, err)
		require.NotNil(t, res)
	})
}

func TestVolumeVidRetainedAndAdoption(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	dead := vs.createVolumeMsg(t, "pg-data")
	res, err := vs.vhandler(vs.ctx, dead)
	require.NoError(t, err)
	require.NotNil(t, res)

	// close it; the group goes terminal through the escrow cascade
	res, err = vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: dead.ID})
	require.NoError(t, err)
	require.NotNil(t, res)

	group, found := vs.dkeeper.GetGroup(vs.ctx, volumeGroupID(dead))
	require.True(t, found)
	require.Equal(t, dvbeta.GroupClosed, group.State)

	// anchor the retention window at the current block time; the fixture
	// retention is 24h
	closedAt := vs.ctx.BlockTime()
	vs.market.leaseClosedAt[volumeGroupID(dead)] = closedAt

	t.Run("re-create inside retention window rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		res, err := vs.vhandler(vs.ctx, msg)
		require.Nil(t, res)
		require.True(t, errors.Is(err, v1.ErrVidRetained))
	})

	t.Run("adoption that changes the volume's access rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		msg.Groups[0].Volume.Adopt = volumeRef(dead)
		msg.Groups[0].Volume.Access = v1.VolumeAccessBucket
		msg.Groups[0].Volume.MaxAttachments = 0

		res, err := vs.vhandler(vs.ctx, msg)
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrInvalidVolumeAdoption)
		require.ErrorContains(t, err, "access mismatch")
	})

	t.Run("adoption naming the retained volume passes", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		msg.Groups[0].Volume.Adopt = volumeRef(dead)

		res, err := vs.vhandler(vs.ctx, msg)
		require.NoError(t, err)
		require.NotNil(t, res)

		testutil.EnsureEvent(t, res.Events, &v1.EventVolumeAdopted{
			ID:      volumeGroupID(msg),
			Adopted: *msg.Groups[0].Volume.Adopt,
			Vid:     "pg-data",
		})

		// the vid re-resolves to the adopting group
		gid, ok := vs.dkeeper.GetVolumeByVid(vs.ctx, vs.owner, "pg-data")
		require.True(t, ok)
		require.Equal(t, volumeGroupID(msg), gid)
	})
}

func TestVolumeVidExpiredWindow(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	dead := vs.createVolumeMsg(t, "pg-data")
	_, err := vs.vhandler(vs.ctx, dead)
	require.NoError(t, err)

	_, err = vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: dead.ID})
	require.NoError(t, err)

	closedAt := vs.ctx.BlockTime()
	vs.market.leaseClosedAt[volumeGroupID(dead)] = closedAt

	// past the 24h fixture retention window
	expired := vs.ctx.WithBlockTime(closedAt.Add(24*time.Hour + time.Second))

	t.Run("adoption after expiry rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		msg.Groups[0].Volume.Adopt = volumeRef(dead)

		res, err := vs.vhandler(expired, msg)
		require.Nil(t, res)
		require.True(t, errors.Is(err, v1.ErrInvalidVolumeAdoption))
	})

	t.Run("fresh create after expiry allowed", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		res, err := vs.vhandler(expired, msg)
		require.NoError(t, err)
		require.NotNil(t, res)
	})
}

// unleasedAdoption creates a volume under vid, closes it with a lease closed
// at the suite's block time, then creates an adoption of it and closes that
// before it gets a lease. The adoption ends up holding the vid's index entry.
func (vs *volumeTestSuite) unleasedAdoption(t *testing.T, vid string) (*dvbeta.MsgCreateDeployment, *dvbeta.MsgCreateDeployment) {
	t.Helper()

	retained := vs.createVolumeMsg(t, vid)
	_, err := vs.vhandler(vs.ctx, retained)
	require.NoError(t, err)
	_, err = vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: retained.ID})
	require.NoError(t, err)

	vs.market.leaseClosedAt[volumeGroupID(retained)] = vs.ctx.BlockTime()

	adoption := vs.createVolumeMsg(t, vid)
	adoption.Groups[0].Volume.Adopt = volumeRef(retained)
	_, err = vs.vhandler(vs.ctx, adoption)
	require.NoError(t, err)
	_, err = vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: adoption.ID})
	require.NoError(t, err)

	gid, ok := vs.dkeeper.GetVolumeByVid(vs.ctx, vs.owner, vid)
	require.True(t, ok)
	require.Equal(t, volumeGroupID(adoption), gid)

	return retained, adoption
}

func TestVolumeVidHeldThroughUnleasedAdoption(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	// the adoption never got a lease, so it never took the data over: the
	// volume it adopted still holds the vid through its 24h fixture window
	retained, adoption := vs.unleasedAdoption(t, "pg-data")

	t.Run("fresh create rejected", func(t *testing.T) {
		res, err := vs.vhandler(vs.ctx, vs.createVolumeMsg(t, "pg-data"))
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrVidRetained)
		require.ErrorContains(t, err, volumeGroupID(retained).String())
	})

	t.Run("adopting the unleased adoption rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		msg.Groups[0].Volume.Adopt = volumeRef(adoption)

		res, err := vs.vhandler(vs.ctx, msg)
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrVidRetained)
	})

	t.Run("adopting the retained volume passes", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		msg.Groups[0].Volume.Adopt = volumeRef(retained)

		res, err := vs.vhandler(vs.ctx, msg)
		require.NoError(t, err)
		require.NotNil(t, res)

		gid, ok := vs.dkeeper.GetVolumeByVid(vs.ctx, vs.owner, "pg-data")
		require.True(t, ok)
		require.Equal(t, volumeGroupID(msg), gid)
	})
}

func TestVolumeVidFreedOnceUnleasedAdoptionTargetExpires(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	retained, _ := vs.unleasedAdoption(t, "pg-data")
	expired := vs.ctx.WithBlockTime(vs.market.leaseClosedAt[volumeGroupID(retained)].Add(24*time.Hour + time.Second))

	t.Run("adopting the expired volume rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		msg.Groups[0].Volume.Adopt = volumeRef(retained)

		res, err := vs.vhandler(expired, msg)
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrInvalidVolumeAdoption)
	})

	t.Run("fresh create allowed", func(t *testing.T) {
		res, err := vs.vhandler(expired, vs.createVolumeMsg(t, "pg-data"))
		require.NoError(t, err)
		require.NotNil(t, res)
	})
}

func TestVolumeAdoptionOfTakenOverVolumeRejected(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	first := vs.createVolumeMsg(t, "pg-data")
	_, err := vs.vhandler(vs.ctx, first)
	require.NoError(t, err)
	_, err = vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: first.ID})
	require.NoError(t, err)

	closedAt := vs.ctx.BlockTime()
	vs.market.leaseClosedAt[volumeGroupID(first)] = closedAt

	// this adoption got a lease, so the data became its own, kept for an
	// hour after that lease closed
	adoption := vs.createVolumeMsg(t, "pg-data")
	adoption.Groups[0].Volume.Adopt = volumeRef(first)
	adoption.Groups[0].Volume.Retention = time.Hour
	_, err = vs.vhandler(vs.ctx, adoption)
	require.NoError(t, err)
	_, err = vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: adoption.ID})
	require.NoError(t, err)

	vs.market.leaseClosedAt[volumeGroupID(adoption)] = closedAt

	t.Run("adopting the first volume while the adoption retains rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		msg.Groups[0].Volume.Adopt = volumeRef(first)

		res, err := vs.vhandler(vs.ctx.WithBlockTime(closedAt.Add(30*time.Minute)), msg)
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrVidRetained)
		require.ErrorContains(t, err, volumeGroupID(adoption).String())
	})

	// the adoption's hour is over, while the first volume's own 24h window
	// is still open
	later := vs.ctx.WithBlockTime(closedAt.Add(2 * time.Hour))

	t.Run("adopting the first volume after the adoption's window rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-data")
		msg.Groups[0].Volume.Adopt = volumeRef(first)

		res, err := vs.vhandler(later, msg)
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrInvalidVolumeAdoption)
		require.ErrorContains(t, err, "no longer holds")
	})

	t.Run("fresh create allowed", func(t *testing.T) {
		res, err := vs.vhandler(later, vs.createVolumeMsg(t, "pg-data"))
		require.NoError(t, err)
		require.NotNil(t, res)
	})
}

func TestVolumeAdoptionInvalidTargets(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	live := vs.createVolumeMsg(t, "pg-data")
	_, err := vs.vhandler(vs.ctx, live)
	require.NoError(t, err)

	t.Run("adopting a live volume rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-other")
		msg.Groups[0].Volume.Adopt = volumeRef(live)

		res, err := vs.vhandler(vs.ctx, msg)
		require.Nil(t, res)
		// the target is live, so both the vid gate and adoption reject;
		// adoption validation trips on the vid mismatch/live state
		require.True(t, errors.Is(err, v1.ErrInvalidVolumeAdoption))
	})

	t.Run("adopting a missing volume rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-missing")
		ref := volumeRef(msg)
		ref.DSeq++ // no such group
		msg.Groups[0].Volume.Adopt = ref

		res, err := vs.vhandler(vs.ctx, msg)
		require.Nil(t, res)
		require.True(t, errors.Is(err, v1.ErrInvalidVolumeAdoption))
	})

	t.Run("adoption without a closed lease rejected", func(t *testing.T) {
		dead := vs.createVolumeMsg(t, "pg-dead")
		_, err := vs.vhandler(vs.ctx, dead)
		require.NoError(t, err)
		_, err = vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: dead.ID})
		require.NoError(t, err)

		// no leaseClosedAt entry: nothing to adopt from
		msg := vs.createVolumeMsg(t, "pg-dead")
		msg.Groups[0].Volume.Adopt = volumeRef(dead)

		res, err := vs.vhandler(vs.ctx, msg)
		require.Nil(t, res)
		require.True(t, errors.Is(err, v1.ErrInvalidVolumeAdoption))
	})
}

func TestVolumeReplicaOfBucketRejected(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	bucket := vs.createVolumeMsg(t, "media")
	bucket.Groups[0].Volume.Access = v1.VolumeAccessBucket
	bucket.Groups[0].Volume.MaxAttachments = 0
	_, err := vs.vhandler(vs.ctx, bucket)
	require.NoError(t, err)

	replica := vs.createVolumeMsg(t, "media-r1")
	replica.Groups[0].Volume.ReplicaOf = volumeRef(bucket)
	res, err := vs.vhandler(vs.ctx, replica)

	require.Nil(t, res)
	require.ErrorIs(t, err, v1.ErrInvalidVolumeReplica)
	require.ErrorContains(t, err, "is a bucket")
}

func TestVolumeReplicaBounds(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	target := vs.createVolumeMsg(t, "pg-data")
	target.Groups[0].Volume.MaxReplicas = 1
	_, err := vs.vhandler(vs.ctx, target)
	require.NoError(t, err)

	replica := vs.createVolumeMsg(t, "pg-data-r1")
	replica.Groups[0].Volume.ReplicaOf = volumeRef(target)
	res, err := vs.vhandler(vs.ctx, replica)
	require.NoError(t, err)
	require.NotNil(t, res)

	t.Run("backref counted", func(t *testing.T) {
		require.Equal(t, uint64(1), vs.dkeeper.GetVolumeReplicaCount(vs.ctx, volumeGroupID(target)))
	})

	t.Run("replica smaller than its source rejected", func(t *testing.T) {
		small := vs.createVolumeMsg(t, "pg-data-small")
		small.Groups[0].Volume.ReplicaOf = volumeRef(target)
		small.Groups[0].Resources[0].Storage[0].Quantity = rtypes.NewResourceValue(unit.Gi)

		res, err := vs.vhandler(vs.ctx, small)
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrInvalidVolumeReplica)
		require.ErrorContains(t, err, "below the source volume")
	})

	t.Run("replica beyond max_replicas rejected", func(t *testing.T) {
		second := vs.createVolumeMsg(t, "pg-data-r2")
		second.Groups[0].Volume.ReplicaOf = volumeRef(target)

		res, err := vs.vhandler(vs.ctx, second)
		require.Nil(t, res)
		require.True(t, errors.Is(err, v1.ErrInvalidVolumeReplica))
	})

	t.Run("closing the replica frees the slot", func(t *testing.T) {
		_, err := vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: replica.ID})
		require.NoError(t, err)
		require.Zero(t, vs.dkeeper.GetVolumeReplicaCount(vs.ctx, volumeGroupID(target)))

		second := vs.createVolumeMsg(t, "pg-data-r2")
		second.Groups[0].Volume.ReplicaOf = volumeRef(target)

		res, err := vs.vhandler(vs.ctx, second)
		require.NoError(t, err)
		require.NotNil(t, res)
	})

	t.Run("replica of a dead volume rejected", func(t *testing.T) {
		_, err := vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: target.ID})
		require.NoError(t, err)

		msg := vs.createVolumeMsg(t, "pg-data-r3")
		msg.Groups[0].Volume.ReplicaOf = volumeRef(target)

		res, err := vs.vhandler(vs.ctx, msg)
		require.Nil(t, res)
		require.True(t, errors.Is(err, v1.ErrInvalidVolumeReplica))
	})
}

func TestVolumeCloseWhileAttached(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	msg := vs.createVolumeMsg(t, "pg-data")
	_, err := vs.vhandler(vs.ctx, msg)
	require.NoError(t, err)

	vs.market.attached[volumeGroupID(msg)] = true

	t.Run("close deployment rejected", func(t *testing.T) {
		res, err := vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: msg.ID})
		require.Nil(t, res)
		require.True(t, errors.Is(err, v1.ErrVolumeAttached))
	})

	t.Run("close group rejected", func(t *testing.T) {
		res, err := vs.vhandler(vs.ctx, &dvbeta.MsgCloseGroup{ID: volumeGroupID(msg)})
		require.Nil(t, res)
		require.True(t, errors.Is(err, v1.ErrVolumeAttached))
	})

	t.Run("pause group rejected", func(t *testing.T) {
		// a volume is never paused, attached or not
		res, err := vs.vhandler(vs.ctx, &dvbeta.MsgPauseGroup{ID: volumeGroupID(msg)})
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrVolumeNotPausable)
	})

	t.Run("close passes once detached", func(t *testing.T) {
		vs.market.attached[volumeGroupID(msg)] = false

		res, err := vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: msg.ID})
		require.NoError(t, err)
		require.NotNil(t, res)
	})
}

func TestVolumePauseAndStartRejected(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	// createVolume creates a volume group whose first order is closed, as
	// after its lease closed without a re-listing.
	createVolume := func(t *testing.T, vid string) dvbeta.Group {
		t.Helper()

		msg := vs.createVolumeMsg(t, vid)
		_, err := vs.vhandler(vs.ctx, msg)
		require.NoError(t, err)

		order, found := vs.mkeeper.GetOrder(vs.ctx, mv1.MakeOrderID(volumeGroupID(msg), 1))
		require.True(t, found)
		require.NoError(t, vs.mkeeper.OnOrderClosed(vs.ctx, order))

		group, found := vs.dkeeper.GetGroup(vs.ctx, volumeGroupID(msg))
		require.True(t, found)

		return group
	}

	requireNoOrder := func(t *testing.T, gid v1.GroupID) {
		t.Helper()

		_, found := vs.mkeeper.GetOrder(vs.ctx, mv1.MakeOrderID(gid, 2))
		require.False(t, found, "the volume group must not list a fresh order")
	}

	t.Run("pause rejected", func(t *testing.T) {
		msg := vs.createVolumeMsg(t, "pg-open")
		_, err := vs.vhandler(vs.ctx, msg)
		require.NoError(t, err)

		res, err := vs.vhandler(vs.ctx, &dvbeta.MsgPauseGroup{ID: volumeGroupID(msg)})
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrVolumeNotPausable)

		group, found := vs.dkeeper.GetGroup(vs.ctx, volumeGroupID(msg))
		require.True(t, found)
		require.Equal(t, dvbeta.GroupOpen, group.State)
	})

	t.Run("start of a paused volume rejected", func(t *testing.T) {
		// a provider closing the lease at the end of the tenant's own close
		// leaves the group paused
		group := createVolume(t, "pg-paused")
		require.NoError(t, vs.dkeeper.OnPauseGroup(vs.ctx, group))

		res, err := vs.vhandler(vs.ctx, &dvbeta.MsgStartGroup{ID: group.ID})
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrVolumeNotPausable)

		group, found := vs.dkeeper.GetGroup(vs.ctx, group.ID)
		require.True(t, found)
		require.Equal(t, dvbeta.GroupPaused, group.State)
		requireNoOrder(t, group.ID)
	})

	t.Run("start of an unfunded volume rejected", func(t *testing.T) {
		group := createVolume(t, "pg-unfunded")
		require.NoError(t, vs.dkeeper.OnCloseGroup(vs.ctx, group, dvbeta.GroupInsufficientFunds))

		res, err := vs.vhandler(vs.ctx, &dvbeta.MsgStartGroup{ID: group.ID})
		require.Nil(t, res)
		require.ErrorIs(t, err, v1.ErrVolumeNotPausable)

		group, found := vs.dkeeper.GetGroup(vs.ctx, group.ID)
		require.True(t, found)
		require.Equal(t, dvbeta.GroupInsufficientFunds, group.State)
		requireNoOrder(t, group.ID)
	})
}

func TestVolumeCloseOfEndedVolumeSucceeds(t *testing.T) {
	vs := setupVolumeTestSuite(t)

	// with no lease to wait out, the first close ends the volume at once
	msg := vs.createVolumeMsg(t, "pg-data")
	_, err := vs.vhandler(vs.ctx, msg)
	require.NoError(t, err)
	_, err = vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: msg.ID})
	require.NoError(t, err)

	group, found := vs.dkeeper.GetGroup(vs.ctx, volumeGroupID(msg))
	require.True(t, found)
	require.Equal(t, dvbeta.GroupClosed, group.State)

	// a close that finds the volume already ended, as its provider may leave
	// it, succeeds and changes nothing
	res, err := vs.vhandler(vs.ctx, &dvbeta.MsgCloseDeployment{ID: msg.ID})
	require.NoError(t, err)
	require.NotNil(t, res)

	res, err = vs.vhandler(vs.ctx, &dvbeta.MsgCloseGroup{ID: volumeGroupID(msg)})
	require.NoError(t, err)
	require.NotNil(t, res)

	deployment, found := vs.dkeeper.GetDeployment(vs.ctx, msg.ID)
	require.True(t, found)
	require.Equal(t, v1.DeploymentClosed, deployment.State)

	group, found = vs.dkeeper.GetGroup(vs.ctx, volumeGroupID(msg))
	require.True(t, found)
	require.Equal(t, dvbeta.GroupClosed, group.State)
}
