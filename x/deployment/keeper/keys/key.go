package keys

var (
	// Old prefixes — kept for migration reads
	DeploymentPrefixLegacy = []byte{0x11, 0x00}
	GroupPrefixLegacy      = []byte{0x12, 0x00}

	// New collections prefixes
	DeploymentPrefix           = []byte{0x11, 0x01}
	DeploymentIndexStatePrefix = []byte{0x11, 0x02}
	GroupPrefix                = []byte{0x12, 0x01}
	GroupIndexStatePrefix      = []byte{0x12, 0x02}
	GroupIndexDeploymentPrefix = []byte{0x12, 0x03}

	// Pending denom migration prefix
	PendingDenomMigrationPrefix = []byte{0x13, 0x01}

	// storage-market volume index prefixes. The design places the vid index under
	// the 0x13 family; {0x13, 0x01} is already taken by the pending denom
	// migration map on this fork, so the volume indexes take the next
	// second bytes.
	VolumeVidPrefix          = []byte{0x13, 0x02}
	VolumeReplicaCountPrefix = []byte{0x13, 0x03}

	ParamsKey = []byte{0x14, 0x00} // key for deployment module params
)
