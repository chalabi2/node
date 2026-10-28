package handler

import (
	errorsmod "cosmossdk.io/errors"

	mv1 "pkg.akt.dev/go/node/market/v1"
)

// storage-market volume error variants, registered in the market codespace.
// Codes start at 100 to stay clear of the chain-sdk market/v1 range,
// which owns the low codes and grows with it.
var (
	// ErrVolumeOrdersDisabled is the error when a volume-group bid is
	// placed while the market VolumeOrdersEnabled feature flag is off
	ErrVolumeOrdersDisabled = errorsmod.Register(mv1.ModuleName, 100, "volume orders disabled")
	// ErrVolumeReclamationWindowTooShort is the error when a volume-group
	// bid offers a reclamation window below the governance floor
	ErrVolumeReclamationWindowTooShort = errorsmod.Register(mv1.ModuleName, 101, "volume bid reclamation window below governance floor")
	// ErrVolumeOfferMismatch is the error when a volume-group bid's offered
	// storage quantities do not equal the order's
	ErrVolumeOfferMismatch = errorsmod.Register(mv1.ModuleName, 102, "volume bid storage offer does not equal order quantities")
	// ErrVolumeAdoptionProvider is the error when an adoption-order bid
	// comes from a provider other than the one retaining the dead volume
	ErrVolumeAdoptionProvider = errorsmod.Register(mv1.ModuleName, 103, "adoption bids are gated to the retaining provider")
	// ErrVolumeRefInvalid is the error when an attaching-group volume
	// reference does not resolve to a volume group owned by the tenant
	ErrVolumeRefInvalid = errorsmod.Register(mv1.ModuleName, 104, "invalid volume reference")
	// ErrVolumeNotColocated is the error when the referenced volume has no
	// active lease with the bidding provider
	ErrVolumeNotColocated = errorsmod.Register(mv1.ModuleName, 105, "volume has no active lease with this provider")
	// ErrVolumeAttachmentLimit is the error when the referenced volume has
	// reached its max_attachments bound
	ErrVolumeAttachmentLimit = errorsmod.Register(mv1.ModuleName, 106, "volume attachment limit reached")
	// ErrVolumePolicyNotAccepted is the error when a bid does not carry its
	// order's volume policy: a volume-order bid without it or with another
	// policy, or a bid on any other order with one
	ErrVolumePolicyNotAccepted = errorsmod.Register(mv1.ModuleName, 107, "bid volume policy does not match the order")
	// ErrVolumeNotAttachable is the error when an attaching group references a
	// bucket, which clients reach over the network rather than by attaching.
	ErrVolumeNotAttachable = errorsmod.Register(mv1.ModuleName, 108, "volume is a bucket and cannot be attached")
)
