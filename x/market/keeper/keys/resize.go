package keys

import (
	"cosmossdk.io/collections"

	dtypes "pkg.akt.dev/go/node/deployment/v1"
)

// DeploymentPartKey is (owner, dseq). Pending lease resizes are keyed by
// deployment, because the manifest hash a resize carries covers every group
// of the deployment, so a deployment can have only one in flight.
type DeploymentPartKey = collections.Pair[string, uint64]

// DeploymentPartKeyCodec is the key codec for DeploymentPartKey
var DeploymentPartKeyCodec = collections.PairKeyCodec(collections.StringKey, collections.Uint64Key)

// DeploymentIDToPartKey converts a DeploymentID to a DeploymentPartKey.
func DeploymentIDToPartKey(id dtypes.DeploymentID) DeploymentPartKey {
	return collections.Join(id.Owner, id.DSeq)
}
