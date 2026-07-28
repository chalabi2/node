package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	sdk "github.com/cosmos/cosmos-sdk/types"

	atypes "pkg.akt.dev/go/node/audit/v1"
	dbeta "pkg.akt.dev/go/node/deployment/v1beta4"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v1beta5"
	ptypes "pkg.akt.dev/go/node/provider/v1beta4"
	attr "pkg.akt.dev/go/node/types/attributes/v1"
	vtypes "pkg.akt.dev/go/node/verification/v1"

	verificationtypes "pkg.akt.dev/node/v2/x/verification/types"
)

func TestBidFilterRequiresLegacyAndVerificationMatches(t *testing.T) {
	provider := sdk.AccAddress("provider-address-000")
	providerAttributes := attr.Attributes{{Key: "region", Value: "us-west"}}
	requirement := &vtypes.VerificationRequirement{MinTier: vtypes.TierIdentified}
	order := mvbeta.Order{Spec: dbeta.GroupSpec{Requirements: attr.PlacementRequirements{
		Attributes:   providerAttributes,
		Verification: requirement,
	}}}
	msg := &mvbeta.MsgCreateBid{ID: mv1.BidID{Provider: provider.String()}}
	providerRecord := ptypes.Provider{
		Owner:      provider.String(),
		Attributes: providerAttributes,
	}

	t.Run("legacy mismatch short circuits verification", func(t *testing.T) {
		verification := &bidFilterVerificationKeeper{
			active:   true,
			bestTier: vtypes.TierIdentified,
		}
		server := msgServer{keepers: Keepers{
			Audit:        bidFilterAuditKeeper{},
			Verification: verification,
		}}
		mismatch := order
		mismatch.Spec.Requirements.Attributes = attr.Attributes{{Key: "region", Value: "eu-west"}}

		err := server.bidFilter(sdk.Context{}, msg, mismatch, provider, providerRecord)
		require.ErrorIs(t, err, mv1.ErrAttributeMismatch)
		require.Zero(t, verification.factCalls)
	})

	t.Run("verification mismatch rejects legacy match", func(t *testing.T) {
		verification := &bidFilterVerificationKeeper{active: true}
		server := msgServer{keepers: Keepers{
			Audit:        bidFilterAuditKeeper{},
			Verification: verification,
		}}

		err := server.bidFilter(sdk.Context{}, msg, order, provider, providerRecord)
		require.ErrorIs(t, err, verificationtypes.ErrInsufficientVerificationTier)
		require.NotZero(t, verification.factCalls)
	})

	t.Run("both matches accept bid", func(t *testing.T) {
		verification := &bidFilterVerificationKeeper{
			active:   true,
			bestTier: vtypes.TierIdentified,
		}
		server := msgServer{keepers: Keepers{
			Audit:        bidFilterAuditKeeper{},
			Verification: verification,
		}}

		require.NoError(t, server.bidFilter(sdk.Context{}, msg, order, provider, providerRecord))
	})
}

func TestVerificationBidFilterBypassesDisabledRequirements(t *testing.T) {
	requirement := &vtypes.VerificationRequirement{
		MinTier:              vtypes.TierTrusted,
		RequiredCapabilities: []vtypes.CapabilityFlag{vtypes.CapabilityBareMetal},
		RequiredAuditors:     []string{"auditor"},
		MinAuditorCount:      2,
	}

	require.NoError(t, (msgServer{}).verificationBidFilter(sdk.Context{}, nil, requirement))

	inactive := &bidFilterVerificationKeeper{}
	server := msgServer{keepers: Keepers{Verification: inactive}}
	require.NoError(t, server.verificationBidFilter(sdk.Context{}, nil, requirement))
	require.Zero(t, inactive.factCalls)

	active := &bidFilterVerificationKeeper{active: true}
	server.keepers.Verification = active
	require.NoError(t, server.verificationBidFilter(sdk.Context{}, nil, nil))
	require.Zero(t, active.factCalls)

	require.NoError(t, server.verificationBidFilter(sdk.Context{}, nil, &vtypes.VerificationRequirement{
		RequiredCapabilities: []vtypes.CapabilityFlag{vtypes.CapabilityBareMetal},
		RequiredAuditors:     []string{"auditor"},
		MinAuditorCount:      2,
	}))
	require.Zero(t, active.factCalls)
}

func TestVerificationBidFilterAppliesRequirementsInOrder(t *testing.T) {
	tests := []struct {
		name        string
		requirement vtypes.VerificationRequirement
		keeper      bidFilterVerificationKeeper
		wantErr     error
	}{
		{
			name:        "snapshot suspended",
			requirement: vtypes.VerificationRequirement{MinTier: vtypes.TierVerified},
			keeper: bidFilterVerificationKeeper{
				active: true,
			},
			wantErr: verificationtypes.ErrProviderSnapshotSuspended,
		},
		{
			name:        "identified tier does not require snapshot",
			requirement: vtypes.VerificationRequirement{MinTier: vtypes.TierIdentified},
			keeper: bidFilterVerificationKeeper{
				active:   true,
				bestTier: vtypes.TierIdentified,
			},
		},
		{
			name:        "tier below requirement",
			requirement: vtypes.VerificationRequirement{MinTier: vtypes.TierEstablished},
			keeper: bidFilterVerificationKeeper{
				active:            true,
				snapshotCompliant: true,
				bestTier:          vtypes.TierIdentified,
			},
			wantErr: verificationtypes.ErrInsufficientVerificationTier,
		},
		{
			name:        "grace preserves tier",
			requirement: vtypes.VerificationRequirement{MinTier: vtypes.TierEstablished},
			keeper: bidFilterVerificationKeeper{
				active:            true,
				snapshotCompliant: true,
				bestTier:          vtypes.TierUnspecified,
				graceTier:         vtypes.TierEstablished,
				hasGrace:          true,
			},
		},
		{
			name:        "grace does not reduce a better valid tier",
			requirement: vtypes.VerificationRequirement{MinTier: vtypes.TierTrusted},
			keeper: bidFilterVerificationKeeper{
				active:            true,
				snapshotCompliant: true,
				bestTier:          vtypes.TierTrusted,
				graceTier:         vtypes.TierIdentified,
				hasGrace:          true,
			},
		},
		{
			name: "missing capability",
			requirement: vtypes.VerificationRequirement{
				MinTier:              vtypes.TierIdentified,
				RequiredCapabilities: []vtypes.CapabilityFlag{vtypes.CapabilityPersistentStorage},
			},
			keeper: bidFilterVerificationKeeper{
				active:   true,
				bestTier: vtypes.TierTrusted,
			},
			wantErr: verificationtypes.ErrMissingCapability,
		},
		{
			name: "insufficient auditor count",
			requirement: vtypes.VerificationRequirement{
				MinTier:         vtypes.TierIdentified,
				MinAuditorCount: 2,
			},
			keeper: bidFilterVerificationKeeper{
				active:         true,
				bestTier:       vtypes.TierTrusted,
				qualifiedCount: 1,
			},
			wantErr: verificationtypes.ErrInsufficientAuditorCount,
		},
		{
			name: "default auditor mode accepts any",
			requirement: vtypes.VerificationRequirement{
				MinTier:          vtypes.TierIdentified,
				RequiredAuditors: []string{"auditor-a", "auditor-b"},
			},
			keeper: bidFilterVerificationKeeper{
				active:   true,
				bestTier: vtypes.TierTrusted,
				attestations: []vtypes.AttestationRecord{{
					Auditor: "auditor-b",
					Tier:    vtypes.TierIdentified,
				}},
			},
		},
		{
			name: "all auditor mode requires every auditor",
			requirement: vtypes.VerificationRequirement{
				MinTier:          vtypes.TierIdentified,
				RequiredAuditors: []string{"auditor-a", "auditor-b"},
				AuditorMode:      vtypes.AuditorSelectionModeAll,
			},
			keeper: bidFilterVerificationKeeper{
				active:   true,
				bestTier: vtypes.TierTrusted,
				attestations: []vtypes.AttestationRecord{{
					Auditor: "auditor-a",
					Tier:    vtypes.TierIdentified,
				}},
			},
			wantErr: verificationtypes.ErrRequiredAuditorNotFound,
		},
		{
			name: "named auditor must meet tier",
			requirement: vtypes.VerificationRequirement{
				MinTier:          vtypes.TierEstablished,
				RequiredAuditors: []string{"auditor-a"},
			},
			keeper: bidFilterVerificationKeeper{
				active:            true,
				snapshotCompliant: true,
				bestTier:          vtypes.TierTrusted,
				attestations: []vtypes.AttestationRecord{{
					Auditor: "auditor-a",
					Tier:    vtypes.TierIdentified,
				}},
			},
			wantErr: verificationtypes.ErrRequiredAuditorNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := msgServer{keepers: Keepers{Verification: &test.keeper}}
			err := server.verificationBidFilter(sdk.Context{}, nil, &test.requirement)
			if test.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, test.wantErr)
			}
		})
	}
}

type bidFilterVerificationKeeper struct {
	active            bool
	snapshotCompliant bool
	bestTier          vtypes.VerificationTier
	graceTier         vtypes.VerificationTier
	hasGrace          bool
	capabilities      map[vtypes.CapabilityFlag]bool
	qualifiedCount    uint32
	attestations      []vtypes.AttestationRecord
	factCalls         int
}

type bidFilterAuditKeeper struct{}

func (bidFilterAuditKeeper) GetProviderAttributes(sdk.Context, sdk.Address) (atypes.AuditedProviders, bool) {
	return nil, false
}

func (k *bidFilterVerificationKeeper) IsVerificationActive(sdk.Context) bool {
	return k.active
}

func (k *bidFilterVerificationKeeper) GetProviderValidAttestations(sdk.Context, sdk.Address) ([]vtypes.AttestationRecord, bool) {
	k.factCalls++
	return k.attestations, len(k.attestations) > 0
}

func (k *bidFilterVerificationKeeper) IsProviderSnapshotCompliant(sdk.Context, sdk.Address) bool {
	k.factCalls++
	return k.snapshotCompliant
}

func (k *bidFilterVerificationKeeper) GetProviderBestTier(sdk.Context, sdk.Address) vtypes.VerificationTier {
	k.factCalls++
	return k.bestTier
}

func (k *bidFilterVerificationKeeper) GetProviderGraceTier(sdk.Context, sdk.Address) (vtypes.VerificationTier, bool) {
	k.factCalls++
	return k.graceTier, k.hasGrace
}

func (k *bidFilterVerificationKeeper) ProviderHasCapability(_ sdk.Context, _ sdk.Address, capability vtypes.CapabilityFlag) bool {
	k.factCalls++
	return k.capabilities[capability]
}

func (k *bidFilterVerificationKeeper) CountQualifiedAuditors(sdk.Context, sdk.Address, vtypes.VerificationTier) uint32 {
	k.factCalls++
	return k.qualifiedCount
}

var _ VerificationKeeper = (*bidFilterVerificationKeeper)(nil)
var _ AuditKeeper = bidFilterAuditKeeper{}
