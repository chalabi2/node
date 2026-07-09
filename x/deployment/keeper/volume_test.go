package keeper_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	types "pkg.akt.dev/go/node/deployment/v1"
	dvbeta "pkg.akt.dev/go/node/deployment/v1beta5"
	testutil "pkg.akt.dev/node/v2/testutil/fixtures"

	"pkg.akt.dev/node/v2/x/deployment/keeper"
	"pkg.akt.dev/node/v2/x/deployment/keeper/keys"
)

func Test_VolumeVidIndex_Create(t *testing.T) {
	ctx, kpr := setupKeeper(t)

	deployment := testutil.Deployment(t)
	group := testutil.VolumeDeploymentGroup(t, deployment.ID, "pg-data")

	require.NoError(t, kpr.Create(ctx, deployment, dvbeta.Groups{group}))

	t.Run("vid resolves to the volume group", func(t *testing.T) {
		gid, found := kpr.GetVolumeByVid(ctx, deployment.ID.Owner, "pg-data")
		require.True(t, found)
		assert.Equal(t, group.ID, gid)
	})

	t.Run("unknown vid does not resolve", func(t *testing.T) {
		_, found := kpr.GetVolumeByVid(ctx, deployment.ID.Owner, "no-such-vid")
		assert.False(t, found)
	})

	t.Run("other owner does not resolve", func(t *testing.T) {
		_, found := kpr.GetVolumeByVid(ctx, testutil.AccAddress(t).String(), "pg-data")
		assert.False(t, found)
	})
}

func Test_VolumeVidIndex_SurvivesClose(t *testing.T) {
	ctx, kpr := setupKeeper(t)

	deployment := testutil.Deployment(t)
	group := testutil.VolumeDeploymentGroup(t, deployment.ID, "pg-data")

	require.NoError(t, kpr.Create(ctx, deployment, dvbeta.Groups{group}))
	require.NoError(t, kpr.OnCloseGroup(ctx, group, dvbeta.GroupClosed))

	// the entry keeps resolving to the dead-but-retained volume so the vid
	// gate and adoption can find it
	gid, found := kpr.GetVolumeByVid(ctx, deployment.ID.Owner, "pg-data")
	require.True(t, found)
	assert.Equal(t, group.ID, gid)
}

func Test_VolumeVidIndex_AdoptionOverwrites(t *testing.T) {
	ctx, kpr := setupKeeper(t)

	owner := testutil.AccAddress(t).String()

	dead := testutil.Deployment(t)
	dead.ID.Owner = owner
	deadGroup := testutil.VolumeDeploymentGroup(t, dead.ID, "pg-data")

	require.NoError(t, kpr.Create(ctx, dead, dvbeta.Groups{deadGroup}))
	require.NoError(t, kpr.OnCloseGroup(ctx, deadGroup, dvbeta.GroupClosed))

	adopting := testutil.Deployment(t)
	adopting.ID.Owner = owner
	adoptingGroup := testutil.VolumeDeploymentGroup(t, adopting.ID, "pg-data")
	adoptingGroup.GroupSpec.Volume.Adopt = &types.VolumeRef{
		Owner: owner,
		DSeq:  dead.ID.DSeq,
		GSeq:  1,
		Name:  "data",
	}

	require.NoError(t, kpr.Create(ctx, adopting, dvbeta.Groups{adoptingGroup}))

	t.Run("vid re-resolves to the adopting group", func(t *testing.T) {
		gid, found := kpr.GetVolumeByVid(ctx, owner, "pg-data")
		require.True(t, found)
		assert.Equal(t, adoptingGroup.ID, gid)
	})

	t.Run("adoption event emitted", func(t *testing.T) {
		testutil.EnsureEvent(t, ctx.EventManager().Events().ToABCIEvents(), &types.EventVolumeAdopted{
			ID:      adoptingGroup.ID,
			Adopted: *adoptingGroup.GroupSpec.Volume.Adopt,
			Vid:     "pg-data",
		})
	})
}

func Test_VolumeReplicaCount(t *testing.T) {
	ctx, kpr := setupKeeper(t)

	owner := testutil.AccAddress(t).String()

	target := testutil.Deployment(t)
	target.ID.Owner = owner
	targetGroup := testutil.VolumeDeploymentGroup(t, target.ID, "pg-data")
	targetGroup.GroupSpec.Volume.MaxReplicas = 2

	require.NoError(t, kpr.Create(ctx, target, dvbeta.Groups{targetGroup}))
	require.Zero(t, kpr.GetVolumeReplicaCount(ctx, targetGroup.ID))

	replica := testutil.Deployment(t)
	replica.ID.Owner = owner
	replicaGroup := testutil.VolumeDeploymentGroup(t, replica.ID, "pg-data-replica")
	replicaGroup.GroupSpec.Volume.ReplicaOf = &types.VolumeRef{
		Owner: owner,
		DSeq:  target.ID.DSeq,
		GSeq:  1,
		Name:  "data",
	}

	require.NoError(t, kpr.Create(ctx, replica, dvbeta.Groups{replicaGroup}))

	t.Run("backref counted on create", func(t *testing.T) {
		assert.Equal(t, uint64(1), kpr.GetVolumeReplicaCount(ctx, targetGroup.ID))
	})

	t.Run("backref released on close", func(t *testing.T) {
		require.NoError(t, kpr.OnCloseGroup(ctx, replicaGroup, dvbeta.GroupClosed))
		assert.Zero(t, kpr.GetVolumeReplicaCount(ctx, targetGroup.ID))
	})

	t.Run("re-close does not double-decrement", func(t *testing.T) {
		closed := replicaGroup
		closed.State = dvbeta.GroupInsufficientFunds
		require.NoError(t, kpr.OnCloseGroup(ctx, closed, dvbeta.GroupClosed))
		assert.Zero(t, kpr.GetVolumeReplicaCount(ctx, targetGroup.ID))
	})
}

func Test_RebuildVolumeIndexes(t *testing.T) {
	ctx, ikpr := setupKeeper(t)
	kpr := ikpr.(*keeper.Keeper)

	owner := testutil.AccAddress(t).String()

	// seed groups directly, bypassing Create, the way genesis import does
	setGroup := func(group dvbeta.Group) {
		require.NoError(t, kpr.Groups().Set(ctx, keys.GroupIDToKey(group.ID), group))
	}

	dead := testutil.Deployment(t)
	dead.ID.Owner = owner
	deadGroup := testutil.VolumeDeploymentGroup(t, dead.ID, "pg-data")
	deadGroup.State = dvbeta.GroupClosed
	deadGroup.CreatedAt = 10
	setGroup(deadGroup)

	live := testutil.Deployment(t)
	live.ID.Owner = owner
	liveGroup := testutil.VolumeDeploymentGroup(t, live.ID, "pg-data")
	liveGroup.CreatedAt = 20
	setGroup(liveGroup)

	replica := testutil.Deployment(t)
	replica.ID.Owner = owner
	replicaGroup := testutil.VolumeDeploymentGroup(t, replica.ID, "pg-data-replica")
	replicaGroup.GroupSpec.Volume.ReplicaOf = &types.VolumeRef{
		Owner: owner,
		DSeq:  live.ID.DSeq,
		GSeq:  1,
		Name:  "data",
	}
	setGroup(replicaGroup)

	closedReplica := testutil.Deployment(t)
	closedReplica.ID.Owner = owner
	closedReplicaGroup := testutil.VolumeDeploymentGroup(t, closedReplica.ID, "pg-data-replica2")
	closedReplicaGroup.State = dvbeta.GroupClosed
	closedReplicaGroup.GroupSpec.Volume.ReplicaOf = &types.VolumeRef{
		Owner: owner,
		DSeq:  live.ID.DSeq,
		GSeq:  1,
		Name:  "data",
	}
	setGroup(closedReplicaGroup)

	require.NoError(t, kpr.RebuildVolumeIndexes(ctx))

	t.Run("live group outranks the dead holder", func(t *testing.T) {
		gid, found := kpr.GetVolumeByVid(ctx, owner, "pg-data")
		require.True(t, found)
		assert.Equal(t, liveGroup.ID, gid)
	})

	t.Run("only live replicas counted", func(t *testing.T) {
		assert.Equal(t, uint64(1), kpr.GetVolumeReplicaCount(ctx, liveGroup.ID))
	})
}
