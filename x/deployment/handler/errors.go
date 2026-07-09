package handler

import (
	errorsmod "cosmossdk.io/errors"

	v1 "pkg.akt.dev/go/node/deployment/v1"
)

// AEP-87 volume error variants, registered in the deployment codespace.
// Codes start at 100 to stay clear of the chain-sdk deployment/v1 range,
// which owns the low codes and grows with it.
var (
	// ErrVolumeOrdersDisabled is the error when a volume group is created
	// while the market VolumeOrdersEnabled feature flag is off
	ErrVolumeOrdersDisabled = errorsmod.Register(v1.ModuleName, 100, "volume orders disabled")
	// ErrVidInUse is the error when the vid resolves to a live volume of
	// the same owner
	ErrVidInUse = errorsmod.Register(v1.ModuleName, 101, "vid in use by a live volume")
	// ErrVidRetained is the error when the vid resolves to a dead volume
	// still inside its retention window and the create does not adopt it
	ErrVidRetained = errorsmod.Register(v1.ModuleName, 102, "vid retained by a dead volume; adopt it or wait out the retention window")
	// ErrVolumeAttached is the error when closing a volume group that
	// still has attached compute leases
	ErrVolumeAttached = errorsmod.Register(v1.ModuleName, 103, "volume has attached leases; close compute first")
	// ErrInvalidVolumeAdoption is the error when the adopt reference fails
	// stateful validation
	ErrInvalidVolumeAdoption = errorsmod.Register(v1.ModuleName, 104, "invalid volume adoption")
	// ErrInvalidVolumeReplica is the error when the replica_of reference
	// fails stateful validation
	ErrInvalidVolumeReplica = errorsmod.Register(v1.ModuleName, 105, "invalid volume replica")
)
