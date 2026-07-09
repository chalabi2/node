package keys

import (
	"cosmossdk.io/collections"
)

// VolumeVidKey is the composite key for the vid index: (owner, vid). The
// index resolves the tenant-chosen data-continuity label to the volume
// group currently holding it — live or dead-but-retained — and enforces
// per-owner vid uniqueness among live volumes.
type VolumeVidKey = collections.Pair[string, string]

// VolumeVidKeyCodec is the key codec for VolumeVidKey
var VolumeVidKeyCodec = collections.PairKeyCodec(
	collections.StringKey,
	collections.StringKey,
)

// VolumeVidToKey builds a VolumeVidKey from an owner address and a vid label
func VolumeVidToKey(owner, vid string) VolumeVidKey {
	return collections.Join(owner, vid)
}
