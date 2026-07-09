package keys

import (
	"cosmossdk.io/collections"

	dtypes "pkg.akt.dev/go/node/deployment/v1"
)

// AttachmentKey is the forward attachment-index key: the volume group part
// first, then the attached compute lease. Prefix-iterating a volume group
// part yields its attachments in deterministic key order.
type AttachmentKey = collections.Pair[GroupPartKey, LeasePrimaryKey]

// AttachmentRevKey is the reverse attachment-index key: the compute lease
// first, then the volume group part it attaches.
type AttachmentRevKey = collections.Pair[LeasePrimaryKey, GroupPartKey]

// GroupPartKeyCodec is the key codec for GroupPartKey
var GroupPartKeyCodec = collections.TripleKeyCodec(
	collections.StringKey,
	collections.Uint64Key,
	collections.Uint32Key,
)

// AttachmentKeyCodec is the key codec for AttachmentKey
var AttachmentKeyCodec = collections.PairKeyCodec(GroupPartKeyCodec, LeasePrimaryKeyCodec)

// AttachmentRevKeyCodec is the key codec for AttachmentRevKey
var AttachmentRevKeyCodec = collections.PairKeyCodec(LeasePrimaryKeyCodec, GroupPartKeyCodec)

// GroupIDToPartKey converts a deployment GroupID to a GroupPartKey for use
// with the attachment indexes.
func GroupIDToPartKey(id dtypes.GroupID) GroupPartKey {
	return collections.Join3(id.Owner, id.DSeq, id.GSeq)
}

// PartKeyToGroupID converts a GroupPartKey back to a deployment GroupID.
func PartKeyToGroupID(key GroupPartKey) dtypes.GroupID {
	return dtypes.GroupID{
		Owner: key.K1(),
		DSeq:  key.K2(),
		GSeq:  key.K3(),
	}
}
