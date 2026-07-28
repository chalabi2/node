package verification

import (
	"crypto/sha256"
	"encoding/json"
	"math"

	errorsmod "cosmossdk.io/errors"
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	vtypes "pkg.akt.dev/go/node/verification/v1"

	"pkg.akt.dev/node/v2/x/verification/keeper"
	moduletypes "pkg.akt.dev/node/v2/x/verification/types"
)

func ValidateGenesis(data *vtypes.GenesisState) error {
	if data == nil {
		return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: nil genesis state")
	}
	if err := keeper.ValidateParams(data.Params); err != nil {
		return err
	}

	denom := data.Params.BondL1.Denom
	seenAuditors := make(map[string]vtypes.AuditorRecord, len(data.Auditors))
	for _, record := range data.Auditors {
		if _, err := sdk.AccAddressFromBech32(record.Address); err != nil {
			return sdkerrors.ErrInvalidAddress.Wrap("verification: invalid auditor address")
		}
		if _, exists := seenAuditors[record.Address]; exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: duplicate auditor")
		}
		seenAuditors[record.Address] = record
		if err := validateTier(record.MaxAttestationTier); err != nil {
			return err
		}
		if _, valid := vtypes.AuditorStatus_name[int32(record.Status)]; !valid || record.Status == vtypes.AuditorStatusUnspecified {
			return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid auditor status %d", record.Status)
		}
		if _, valid := vtypes.BondStatus_name[int32(record.BondStatus)]; !valid || record.BondStatus == vtypes.BondStatusUnspecified {
			return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid auditor bond status %d", record.BondStatus)
		}
		if err := validateGenesisCoin("auditor bond", record.BondAmount, denom, true); err != nil {
			return err
		}
		if record.RegisteredAt.IsZero() {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: auditor registration time is required")
		}
		if record.Status != vtypes.AuditorStatusRemoved &&
			record.Status != vtypes.AuditorStatusResigned &&
			record.RenewalDeadline.IsZero() {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: auditor renewal deadline is required")
		}
		if record.BondStatus == vtypes.BondStatusUnbonding {
			if record.BondUnbondingCompletionTime == nil || record.BondUnbondingCompletionTime.IsZero() || record.BondAmount.IsZero() {
				return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: unbonding auditor requires a positive bond and completion time")
			}
		} else if record.BondUnbondingCompletionTime != nil {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: non-unbonding auditor has an unbonding completion time")
		}
		minBond := vtypes.MinBondForTier(data.Params, record.MaxAttestationTier)
		switch record.Status {
		case vtypes.AuditorStatusPendingBond:
			if record.BondStatus != vtypes.BondStatusNotBonded ||
				!record.BondAmount.Amount.LT(minBond.Amount) {
				return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: pending auditor bond state is inconsistent")
			}
		case vtypes.AuditorStatusActive:
			if record.BondStatus != vtypes.BondStatusBonded ||
				record.BondAmount.Amount.LT(minBond.Amount) {
				return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: active auditor bond state is inconsistent")
			}
		case vtypes.AuditorStatusFrozen:
			if record.BondStatus != vtypes.BondStatusFrozen {
				return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: frozen auditor bond state is inconsistent")
			}
		}
		if record.BondStatus == vtypes.BondStatusFrozen &&
			record.Status != vtypes.AuditorStatusFrozen &&
			record.Status != vtypes.AuditorStatusLapsed {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: frozen bond has inconsistent auditor status")
		}
	}

	seenEscrows := make(map[uint64]vtypes.AuditEscrowRecord, len(data.AuditEscrows))
	attestationsByEscrow := make(map[uint64]uint32, len(data.Attestations))
	var maxEscrowID uint64
	for _, record := range data.AuditEscrows {
		if record.ID == 0 {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: audit escrow ID must be positive")
		}
		if _, exists := seenEscrows[record.ID]; exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: duplicate audit escrow")
		}
		if _, err := sdk.AccAddressFromBech32(record.Provider); err != nil {
			return sdkerrors.ErrInvalidAddress.Wrap("verification: invalid audit escrow provider address")
		}
		if err := validateTier(record.RequestedTier); err != nil {
			return err
		}
		if err := validateGenesisCapabilities(record.RequestedCapabilities); err != nil {
			return err
		}
		if err := validateGenesisCoin("audit escrow fee", record.Fee, denom, false); err != nil {
			return err
		}
		if err := validateGenesisCoin("provider audit deposit", record.ProviderDeposit, denom, true); err != nil {
			return err
		}
		if record.OpenedAt.IsZero() || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(record.OpenedAt) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: invalid audit escrow timestamps")
		}
		if err := validateAuditEscrowLifecycle(record); err != nil {
			return err
		}
		if record.ConsumedByAuditor != "" {
			if _, exists := seenAuditors[record.ConsumedByAuditor]; !exists {
				return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: audit escrow references unknown auditor")
			}
		}
		seenEscrows[record.ID] = record
		if record.ID > maxEscrowID {
			maxEscrowID = record.ID
		}
	}

	seenAttestations := make(map[string]struct{}, len(data.Attestations))
	for _, record := range data.Attestations {
		if err := validateProviderAuditor(record.Provider, record.Auditor); err != nil {
			return err
		}
		key := record.Provider + "/" + record.Auditor
		if _, exists := seenAttestations[key]; exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: duplicate attestation")
		}
		seenAttestations[key] = struct{}{}
		if err := validateTier(record.Tier); err != nil {
			return err
		}
		if err := validateGenesisCapabilities(record.Capabilities); err != nil {
			return err
		}
		if _, exists := seenAuditors[record.Auditor]; !exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: attestation references unknown auditor")
		}
		if err := validateGenesisCoin("attestation fee", record.Fee, denom, false); err != nil {
			return err
		}
		if err := validateGenesisCoin("attestation deposit", record.Deposit, denom, false); err != nil {
			return err
		}
		if record.CreatedAt.IsZero() || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: invalid attestation timestamps")
		}
		if _, valid := vtypes.AttestationStatus_name[int32(record.Status)]; !valid || record.Status == vtypes.AttestationStatusUnspecified {
			return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid attestation status %d", record.Status)
		}
		if record.Status == vtypes.AttestationStatusVoided {
			if _, valid := vtypes.VoidedReason_name[int32(record.VoidedReason)]; !valid || record.VoidedReason == vtypes.VoidedReasonUnspecified {
				return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid attestation void reason %d", record.VoidedReason)
			}
		} else if record.VoidedReason != vtypes.VoidedReasonUnspecified {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: non-voided attestation has a void reason")
		}
		escrow, exists := seenEscrows[record.AuditEscrowID]
		if !exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: attestation references missing audit escrow")
		}
		if escrow.Provider != record.Provider ||
			escrow.ConsumedByAuditor != record.Auditor ||
			!escrow.Fee.IsEqual(record.Fee) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: attestation and audit escrow relationship is inconsistent")
		}
		if !vtypes.TierAtLeast(record.Tier, escrow.RequestedTier) ||
			!genesisCapabilitiesInclude(record.Capabilities, escrow.RequestedCapabilities) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: attestation does not satisfy its audit escrow request")
		}
		if err := validateAttestationEscrowLifecycle(record, escrow); err != nil {
			return err
		}
		attestationsByEscrow[record.AuditEscrowID]++
	}

	for _, escrow := range data.AuditEscrows {
		count := attestationsByEscrow[escrow.ID]
		if escrow.Status == vtypes.AuditEscrowStatusConsumed && count != 1 {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: consumed audit escrow must reference one attestation")
		}
		if escrow.Status != vtypes.AuditEscrowStatusConsumed &&
			escrow.Status != vtypes.AuditEscrowStatusSettled &&
			count != 0 {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: unconsumed audit escrow references an attestation")
		}
	}

	seenDiscrepancies := make(map[uint64]vtypes.DiscrepancyEvent, len(data.Discrepancies))
	var maxDiscrepancyID uint64
	for _, record := range data.Discrepancies {
		if record.ID == 0 {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: discrepancy ID must be positive")
		}
		if _, exists := seenDiscrepancies[record.ID]; exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: duplicate discrepancy")
		}
		if err := validateProviderAuditor(record.Provider, record.AuditorA); err != nil {
			return err
		}
		if err := validateProviderAuditor(record.Provider, record.AuditorB); err != nil {
			return err
		}
		if record.AuditorA == record.AuditorB {
			return errorsmod.Wrap(moduletypes.ErrSelfAttestation, "verification: discrepancy auditors match")
		}
		if err := validateTier(record.AuditorATier); err != nil {
			return err
		}
		if err := validateTier(record.AuditorBTier); err != nil {
			return err
		}
		if _, exists := seenAuditors[record.AuditorA]; !exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: discrepancy references unknown auditor A")
		}
		if _, exists := seenAuditors[record.AuditorB]; !exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: discrepancy references unknown auditor B")
		}
		if _, valid := vtypes.DiscrepancyStatus_name[int32(record.ResolutionStatus)]; !valid || record.ResolutionStatus == vtypes.DiscrepancyStatusUnspecified {
			return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid discrepancy status %d", record.ResolutionStatus)
		}
		if record.Timestamp.IsZero() {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: discrepancy timestamp is required")
		}
		if err := validateGenesisDiscrepancyResolution(record); err != nil {
			return err
		}
		if record.ResolutionStatus == vtypes.DiscrepancyStatusPending ||
			record.ResolutionStatus == vtypes.DiscrepancyStatusTimedOut {
			if err := validateDiscrepancyRelationships(record, data.Attestations, data.Auditors, data.Discrepancies); err != nil {
				return err
			}
		}
		seenDiscrepancies[record.ID] = record
		if record.ID > maxDiscrepancyID {
			maxDiscrepancyID = record.ID
		}
	}

	for _, auditor := range data.Auditors {
		if auditor.BondStatus == vtypes.BondStatusFrozen &&
			!auditorHasPendingGenesisDiscrepancy(data.Discrepancies, auditor.Address) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: frozen auditor has no pending discrepancy")
		}
	}

	seenProviderBonds := make(map[string]vtypes.ProviderBondRecord, len(data.ProviderBonds))
	for _, record := range data.ProviderBonds {
		if _, err := sdk.AccAddressFromBech32(record.Provider); err != nil {
			return sdkerrors.ErrInvalidAddress.Wrap("verification: invalid provider bond address")
		}
		if _, exists := seenProviderBonds[record.Provider]; exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: duplicate provider bond")
		}
		seenProviderBonds[record.Provider] = record
		if err := validateGenesisCoin("provider bonded amount", record.BondedAmount, denom, true); err != nil {
			return err
		}
		if record.Slashed != (record.LastSlashTime != nil) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: provider bond slash state is inconsistent")
		}
		if record.LastSlashTime != nil && record.LastSlashTime.IsZero() {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: provider bond slash time is invalid")
		}
		for _, entry := range record.UnbondingEntries {
			if err := validateGenesisCoin("provider unbonding amount", entry.Amount, denom, false); err != nil {
				return err
			}
			if entry.CompletionTime.IsZero() {
				return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: provider unbonding completion time is required")
			}
		}
	}

	seenProviderSnapshots := make(map[string]vtypes.ProviderSnapshotRecord, len(data.ProviderSnapshots))
	for _, record := range data.ProviderSnapshots {
		if _, err := sdk.AccAddressFromBech32(record.Provider); err != nil {
			return sdkerrors.ErrInvalidAddress.Wrap("verification: invalid provider snapshot address")
		}
		if _, exists := seenProviderSnapshots[record.Provider]; exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: duplicate provider snapshot")
		}
		seenProviderSnapshots[record.Provider] = record
		if len(record.SnapshotHash) != sha256.Size {
			return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: snapshot hash must be %d bytes", sha256.Size)
		}
		if record.PostedAt.IsZero() ||
			record.SnapshotTimestamp.IsZero() ||
			record.ComplianceDeadline.IsZero() ||
			record.SnapshotTimestamp.After(record.PostedAt) ||
			!record.ComplianceDeadline.After(record.PostedAt) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: invalid provider snapshot timestamps")
		}
	}

	for _, attestation := range data.Attestations {
		if attestation.Status != vtypes.AttestationStatusValid {
			continue
		}
		auditor := seenAuditors[attestation.Auditor]
		if !vtypes.TierAtLeast(auditor.MaxAttestationTier, attestation.Tier) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: valid attestation exceeds auditor authority")
		}
		if !vtypes.TierRequiresSnapshot(attestation.Tier) {
			continue
		}
		snapshot, found := seenProviderSnapshots[attestation.Provider]
		if !found || snapshot.Suspended {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: valid attestation requires a compliant provider snapshot")
		}
		if !vtypes.TierRequiresProviderBond(attestation.Tier) {
			continue
		}
		bond, found := seenProviderBonds[attestation.Provider]
		if !found {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: valid attestation requires a provider bond")
		}
		required := keeper.RequiredProviderBond(data.Params, attestation.Tier, snapshot.ResourceSummary)
		if bond.BondedAmount.Denom != required.Denom || bond.BondedAmount.Amount.LT(required.Amount) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: provider bond does not support valid attestation")
		}
	}

	seenGraces := make(map[uint64]vtypes.ProviderVerificationGraceRecord, len(data.VerificationGraces))
	var maxGraceID uint64
	for _, record := range data.VerificationGraces {
		if record.ID == 0 {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: grace record ID must be positive")
		}
		if _, exists := seenGraces[record.ID]; exists {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: duplicate grace record")
		}
		if _, err := sdk.AccAddressFromBech32(record.Provider); err != nil {
			return sdkerrors.ErrInvalidAddress.Wrap("verification: invalid grace provider address")
		}
		if err := validateTier(record.PreservedTier); err != nil {
			return err
		}
		if _, valid := vtypes.VerificationGraceStatus_name[int32(record.Status)]; !valid || record.Status == vtypes.VerificationGraceStatusUnspecified {
			return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid grace status %d", record.Status)
		}
		if record.StartedAt.IsZero() || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(record.StartedAt) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: invalid grace timestamps")
		}
		if len(record.SourceDiscrepancyIDs) == 0 {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: grace record requires a source discrepancy")
		}
		seenSources := make(map[uint64]struct{}, len(record.SourceDiscrepancyIDs))
		activeSource := false
		for _, id := range record.SourceDiscrepancyIDs {
			if _, duplicate := seenSources[id]; duplicate {
				return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: duplicate grace source discrepancy")
			}
			seenSources[id] = struct{}{}
			discrepancy, exists := seenDiscrepancies[id]
			if !exists || discrepancy.Provider != record.Provider {
				return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: grace record references an invalid discrepancy")
			}
			if genesisDiscrepancySupportsActiveGrace(discrepancy) {
				activeSource = true
			}
		}
		if record.Status == vtypes.VerificationGraceStatusActive && !activeSource {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: active grace has no qualifying source")
		}
		seenGraces[record.ID] = record
		if record.ID > maxGraceID {
			maxGraceID = record.ID
		}
	}

	for _, record := range data.Discrepancies {
		if record.GraceRecordID == 0 {
			continue
		}
		grace, exists := seenGraces[record.GraceRecordID]
		if !exists || grace.Provider != record.Provider || !containsID(grace.SourceDiscrepancyIDs, record.ID) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: discrepancy references an invalid grace record")
		}
	}

	if err := validateNextGenesisID("next discrepancy ID", data.NextDiscrepancyID, maxDiscrepancyID); err != nil {
		return err
	}
	if err := validateNextGenesisID("next audit escrow ID", data.NextAuditEscrowID, maxEscrowID); err != nil {
		return err
	}
	if err := validateNextGenesisID("next grace record ID", data.NextGraceRecordID, maxGraceID); err != nil {
		return err
	}
	return nil
}

func InitGenesis(ctx sdk.Context, k keeper.Keeper, data *vtypes.GenesisState) {
	if err := ValidateGenesis(data); err != nil {
		panic(err)
	}

	k.InitializeModuleAccount(ctx)
	k.SetParams(ctx, data.Params)

	for _, record := range data.Auditors {
		if err := k.SetAuditor(ctx, record); err != nil {
			panic(err)
		}
	}
	for _, record := range data.Attestations {
		if err := k.SetAttestation(ctx, record); err != nil {
			panic(err)
		}
	}
	for _, record := range data.Discrepancies {
		k.SetDiscrepancy(ctx, record)
	}
	for _, record := range data.ProviderBonds {
		if err := k.SetProviderBond(ctx, record); err != nil {
			panic(err)
		}
		provider, err := sdk.AccAddressFromBech32(record.Provider)
		if err != nil {
			panic(err)
		}
		for _, entry := range record.UnbondingEntries {
			if err = k.QueueProviderBondUnbonding(ctx, provider, entry.CompletionTime); err != nil {
				panic(err)
			}
		}
	}
	for _, record := range data.ProviderSnapshots {
		if err := k.SetProviderSnapshot(ctx, record); err != nil {
			panic(err)
		}
	}
	for _, record := range data.AuditEscrows {
		if err := k.SetAuditEscrow(ctx, record); err != nil {
			panic(err)
		}
	}
	for _, record := range data.VerificationGraces {
		if err := k.SetProviderVerificationGrace(ctx, record); err != nil {
			panic(err)
		}
	}

	k.SetNextDiscrepancyID(ctx, data.NextDiscrepancyID)
	k.SetNextAuditEscrowID(ctx, data.NextAuditEscrowID)
	k.SetNextGraceRecordID(ctx, data.NextGraceRecordID)
}

func ExportGenesis(ctx sdk.Context, k keeper.Keeper) *vtypes.GenesisState {
	state := &vtypes.GenesisState{
		Params:            k.GetParams(ctx),
		NextDiscrepancyID: k.GetNextDiscrepancyID(ctx),
		NextAuditEscrowID: k.GetNextAuditEscrowID(ctx),
		NextGraceRecordID: k.GetNextGraceRecordID(ctx),
	}

	k.WithAuditors(ctx, func(record vtypes.AuditorRecord) bool {
		state.Auditors = append(state.Auditors, record)
		return false
	})
	k.WithProviderBonds(ctx, func(record vtypes.ProviderBondRecord) bool {
		state.ProviderBonds = append(state.ProviderBonds, record)
		return false
	})
	k.WithProviderSnapshots(ctx, func(record vtypes.ProviderSnapshotRecord) bool {
		state.ProviderSnapshots = append(state.ProviderSnapshots, record)
		return false
	})
	k.WithDiscrepancies(ctx, vtypes.DiscrepancyStatusUnspecified, func(record vtypes.DiscrepancyEvent) bool {
		state.Discrepancies = append(state.Discrepancies, record)
		return false
	})
	k.WithAuditEscrows(ctx, func(record vtypes.AuditEscrowRecord) bool {
		state.AuditEscrows = append(state.AuditEscrows, record)
		return false
	})
	k.WithVerificationGraces(ctx, func(record vtypes.ProviderVerificationGraceRecord) bool {
		state.VerificationGraces = append(state.VerificationGraces, record)
		return false
	})
	k.WithAttestations(ctx, func(record vtypes.AttestationRecord) bool {
		state.Attestations = append(state.Attestations, record)
		return false
	})

	return state
}

func DefaultGenesisState() *vtypes.GenesisState {
	return &vtypes.GenesisState{
		Params:            keeper.DefaultParams(),
		NextDiscrepancyID: 1,
		NextAuditEscrowID: 1,
		NextGraceRecordID: 1,
	}
}

func GetGenesisStateFromAppState(cdc codec.JSONCodec, appState map[string]json.RawMessage) *vtypes.GenesisState {
	if appState[moduletypes.ModuleName] == nil {
		return DefaultGenesisState()
	}

	genesisState := new(vtypes.GenesisState)
	cdc.MustUnmarshalJSON(appState[moduletypes.ModuleName], genesisState)
	return genesisState
}

func validateProviderAuditor(provider, auditor string) error {
	if _, err := sdk.AccAddressFromBech32(provider); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap("verification: invalid provider address")
	}
	if _, err := sdk.AccAddressFromBech32(auditor); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap("verification: invalid auditor address")
	}
	if provider == auditor {
		return moduletypes.ErrSelfAttestation
	}

	return nil
}

func validateTier(tier vtypes.VerificationTier) error {
	switch tier {
	case vtypes.TierIdentified, vtypes.TierVerified, vtypes.TierEstablished, vtypes.TierTrusted:
		return nil
	default:
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid tier %d", tier)
	}
}

func validateGenesisCoin(name string, coin sdk.Coin, denom string, allowZero bool) error {
	if !coin.IsValid() || coin.Denom != denom || coin.Amount.IsNegative() || (!allowZero && !coin.IsPositive()) {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid %s", name)
	}
	return nil
}

func validateGenesisCapabilities(capabilities []vtypes.CapabilityFlag) error {
	seen := make(map[vtypes.CapabilityFlag]struct{}, len(capabilities))
	for _, capability := range capabilities {
		switch capability {
		case vtypes.CapabilityTEEHardwareAttestation,
			vtypes.CapabilityConfidentialComputing,
			vtypes.CapabilityPersistentStorage,
			vtypes.CapabilityBareMetal:
		default:
			return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid capability %d", capability)
		}
		if _, exists := seen[capability]; exists {
			return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: duplicate capability %s", capability)
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func validateAuditEscrowLifecycle(record vtypes.AuditEscrowRecord) error {
	if _, valid := vtypes.FeeStatus_name[int32(record.FeeStatus)]; !valid || record.FeeStatus == vtypes.FeeStatusUnspecified {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid audit escrow fee status %d", record.FeeStatus)
	}
	if _, valid := vtypes.ProviderDepositStatus_name[int32(record.ProviderDepositStatus)]; !valid ||
		record.ProviderDepositStatus == vtypes.ProviderDepositStatusUnspecified {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid provider deposit status %d", record.ProviderDepositStatus)
	}
	if _, valid := vtypes.AuditEscrowStatus_name[int32(record.Status)]; !valid ||
		record.Status == vtypes.AuditEscrowStatusUnspecified {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid audit escrow status %d", record.Status)
	}
	if _, valid := vtypes.AuditEscrowSettlementReason_name[int32(record.SettlementReason)]; !valid {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid audit escrow settlement reason %d", record.SettlementReason)
	}
	if _, valid := vtypes.FaultAttribution_name[int32(record.FaultAttribution)]; !valid {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid audit escrow fault attribution %d", record.FaultAttribution)
	}

	consumed := record.ConsumedByAuditor != "" && record.ConsumedAt != nil
	if (record.ConsumedByAuditor == "") != (record.ConsumedAt == nil) {
		return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: audit escrow consumption state is incomplete")
	}
	if consumed &&
		(record.ConsumedAt.IsZero() ||
			record.ConsumedAt.Before(record.OpenedAt) ||
			!record.ConsumedAt.Before(record.ExpiresAt)) {
		return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: invalid audit escrow consumption time")
	}

	switch record.Status {
	case vtypes.AuditEscrowStatusOpen:
		if consumed ||
			record.FeeStatus != vtypes.FeeStatusEscrowed ||
			record.ProviderDepositStatus != vtypes.ProviderDepositStatusEscrowed ||
			record.SettlementReason != vtypes.AuditEscrowSettlementReasonUnspecified ||
			record.FaultAttribution != vtypes.FaultAttributionUnspecified {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: inconsistent open audit escrow")
		}
	case vtypes.AuditEscrowStatusConsumed:
		if !consumed ||
			record.FeeStatus != vtypes.FeeStatusEscrowed ||
			record.ProviderDepositStatus != vtypes.ProviderDepositStatusEscrowed ||
			record.SettlementReason != vtypes.AuditEscrowSettlementReasonUnspecified ||
			record.FaultAttribution != vtypes.FaultAttributionUnspecified {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: inconsistent consumed audit escrow")
		}
	case vtypes.AuditEscrowStatusCancelled:
		if consumed ||
			record.FeeStatus != vtypes.FeeStatusReturnedToProvider ||
			record.ProviderDepositStatus != vtypes.ProviderDepositStatusReturnedToProvider ||
			record.SettlementReason != vtypes.AuditEscrowSettlementReasonCancelledUnconsumed ||
			record.FaultAttribution != vtypes.FaultAttributionNoFault {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: inconsistent cancelled audit escrow")
		}
	case vtypes.AuditEscrowStatusExpired:
		if consumed ||
			record.FeeStatus != vtypes.FeeStatusReturnedToProvider ||
			record.ProviderDepositStatus != vtypes.ProviderDepositStatusReturnedToProvider ||
			record.SettlementReason != vtypes.AuditEscrowSettlementReasonExpiredUnconsumed ||
			record.FaultAttribution != vtypes.FaultAttributionNoFault {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: inconsistent expired audit escrow")
		}
	case vtypes.AuditEscrowStatusSettled:
		if !isFinalGenesisFeeStatus(record.FeeStatus) ||
			!isFinalGenesisProviderDepositStatus(record.ProviderDepositStatus) ||
			record.SettlementReason == vtypes.AuditEscrowSettlementReasonUnspecified ||
			record.SettlementReason == vtypes.AuditEscrowSettlementReasonCancelledUnconsumed ||
			record.SettlementReason == vtypes.AuditEscrowSettlementReasonExpiredUnconsumed ||
			record.FaultAttribution == vtypes.FaultAttributionUnspecified {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: inconsistent settled audit escrow")
		}
		if record.SettlementReason == vtypes.AuditEscrowSettlementReasonProviderFault &&
			(record.FaultAttribution != vtypes.FaultAttributionProviderFault ||
				record.ProviderDepositStatus != vtypes.ProviderDepositStatusSlashed) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: provider-fault escrow has inconsistent attribution")
		}
		if record.SettlementReason == vtypes.AuditEscrowSettlementReasonNoFault &&
			(record.FaultAttribution != vtypes.FaultAttributionNoFault ||
				record.ProviderDepositStatus != vtypes.ProviderDepositStatusReturnedToProvider) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: no-fault escrow has inconsistent attribution")
		}
		if !consumed && record.FeeStatus != vtypes.FeeStatusReturnedToProvider {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: unconsumed escrow fee was not returned to provider")
		}
	}
	return nil
}

func validateAttestationEscrowLifecycle(record vtypes.AttestationRecord, escrow vtypes.AuditEscrowRecord) error {
	if len(record.EvidenceHash) != sha256.Size {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: evidence hash must be %d bytes", sha256.Size)
	}
	if _, valid := vtypes.FeeStatus_name[int32(record.FeeStatus)]; !valid ||
		record.FeeStatus == vtypes.FeeStatusUnspecified {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid attestation fee status %d", record.FeeStatus)
	}
	if _, valid := vtypes.DepositStatus_name[int32(record.DepositStatus)]; !valid ||
		record.DepositStatus == vtypes.DepositStatusUnspecified {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid attestation deposit status %d", record.DepositStatus)
	}
	if _, valid := vtypes.FaultAttribution_name[int32(record.FaultAttribution)]; !valid {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid attestation fault attribution %d", record.FaultAttribution)
	}

	held := record.FeeStatus == vtypes.FeeStatusEscrowed &&
		(record.DepositStatus == vtypes.DepositStatusEscrowed ||
			record.DepositStatus == vtypes.DepositStatusPendingDiscrepancy)
	settled := isFinalGenesisFeeStatus(record.FeeStatus) && isFinalGenesisDepositStatus(record.DepositStatus)
	switch {
	case held:
		if escrow.Status != vtypes.AuditEscrowStatusConsumed ||
			escrow.FeeStatus != vtypes.FeeStatusEscrowed ||
			escrow.ProviderDepositStatus != vtypes.ProviderDepositStatusEscrowed {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: attestation has inconsistent consumed escrow")
		}
	case settled:
		if escrow.Status != vtypes.AuditEscrowStatusSettled ||
			escrow.FeeStatus != record.FeeStatus ||
			!isFinalGenesisProviderDepositStatus(escrow.ProviderDepositStatus) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: attestation has inconsistent settled escrow")
		}
	default:
		return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: attestation fund statuses are inconsistent")
	}

	switch record.Status {
	case vtypes.AttestationStatusValid:
		if !held || record.DepositStatus != vtypes.DepositStatusEscrowed ||
			record.FaultAttribution != vtypes.FaultAttributionUnspecified {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: valid attestation has inconsistent lifecycle state")
		}
	case vtypes.AttestationStatusVoided:
		if record.VoidedReason == vtypes.VoidedReasonDiscrepancy && held {
			if record.DepositStatus != vtypes.DepositStatusPendingDiscrepancy ||
				record.FaultAttribution != vtypes.FaultAttributionUnspecified {
				return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: pending discrepancy attestation is inconsistent")
			}
		} else if !settled || record.FaultAttribution == vtypes.FaultAttributionUnspecified {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: voided attestation is not finally settled")
		}
	case vtypes.AttestationStatusExpired, vtypes.AttestationStatusRevoked, vtypes.AttestationStatusRemoved:
		if !settled || record.FaultAttribution == vtypes.FaultAttributionUnspecified {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: terminal attestation is not finally settled")
		}
	}
	return nil
}

func validateDiscrepancyRelationships(
	record vtypes.DiscrepancyEvent,
	attestations []vtypes.AttestationRecord,
	auditors []vtypes.AuditorRecord,
	discrepancies []vtypes.DiscrepancyEvent,
) error {
	for _, auditor := range []string{record.AuditorA, record.AuditorB} {
		auditorRecord, found := findGenesisAuditor(auditors, auditor)
		if !found {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: discrepancy auditor is missing")
		}
		if record.ResolutionStatus == vtypes.DiscrepancyStatusPending &&
			auditorRecord.BondStatus != vtypes.BondStatusFrozen {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: pending discrepancy auditor is not frozen")
		}
		attestation, found := findGenesisAttestation(attestations, record.Provider, auditor)
		if record.ResolutionStatus == vtypes.DiscrepancyStatusTimedOut &&
			found &&
			attestation.CreatedAt.After(record.Timestamp) {
			continue
		}
		if !found ||
			attestation.Status != vtypes.AttestationStatusVoided ||
			attestation.VoidedReason != vtypes.VoidedReasonDiscrepancy {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: pending discrepancy attestation is missing or not voided")
		}
		switch record.ResolutionStatus {
		case vtypes.DiscrepancyStatusPending:
			if attestation.FeeStatus == vtypes.FeeStatusEscrowed &&
				attestation.DepositStatus == vtypes.DepositStatusPendingDiscrepancy {
				continue
			}
		case vtypes.DiscrepancyStatusTimedOut:
			if attestation.FeeStatus == vtypes.FeeStatusReturnedToProvider &&
				attestation.DepositStatus == vtypes.DepositStatusSlashed {
				continue
			}
		}
		if !isFinalGenesisFeeStatus(attestation.FeeStatus) ||
			!isFinalGenesisDepositStatus(attestation.DepositStatus) ||
			!hasOtherTerminalGenesisDiscrepancy(discrepancies, record.ID, record.Provider, auditor) {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: discrepancy attestation funds are inconsistent")
		}
	}
	return nil
}

func validateGenesisDiscrepancyResolution(record vtypes.DiscrepancyEvent) error {
	switch record.ResolutionStatus {
	case vtypes.DiscrepancyStatusPending:
		if record.ResolutionReason != vtypes.DiscrepancyResolutionReasonUnspecified ||
			record.FaultAttribution != vtypes.FaultAttributionUnspecified ||
			len(record.ResolutionEvidenceHash) != 0 {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: pending discrepancy has resolution data")
		}
	case vtypes.DiscrepancyStatusResolved:
		if err := keeper.ValidateReasonAttribution(record.ResolutionReason, record.FaultAttribution); err != nil {
			return err
		}
		if len(record.ResolutionEvidenceHash) != sha256.Size {
			return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: resolution evidence hash must be %d bytes", sha256.Size)
		}
	case vtypes.DiscrepancyStatusTimedOut:
		if record.ResolutionReason != vtypes.DiscrepancyResolutionReasonUnspecified ||
			record.FaultAttribution != vtypes.FaultAttributionInconclusive ||
			len(record.ResolutionEvidenceHash) != 0 {
			return errorsmod.Wrap(moduletypes.ErrInvalidReason, "verification: timed-out discrepancy has inconsistent resolution data")
		}
	}
	return nil
}

func auditorHasPendingGenesisDiscrepancy(records []vtypes.DiscrepancyEvent, auditor string) bool {
	for _, record := range records {
		if record.ResolutionStatus == vtypes.DiscrepancyStatusPending &&
			(record.AuditorA == auditor || record.AuditorB == auditor) {
			return true
		}
	}
	return false
}

func findGenesisAuditor(records []vtypes.AuditorRecord, address string) (vtypes.AuditorRecord, bool) {
	for _, record := range records {
		if record.Address == address {
			return record, true
		}
	}
	return vtypes.AuditorRecord{}, false
}

func findGenesisAttestation(records []vtypes.AttestationRecord, provider, auditor string) (vtypes.AttestationRecord, bool) {
	for _, record := range records {
		if record.Provider == provider && record.Auditor == auditor {
			return record, true
		}
	}
	return vtypes.AttestationRecord{}, false
}

func hasOtherTerminalGenesisDiscrepancy(
	records []vtypes.DiscrepancyEvent,
	currentID uint64,
	provider string,
	auditor string,
) bool {
	for _, record := range records {
		if record.ID == currentID ||
			record.Provider != provider ||
			(record.AuditorA != auditor && record.AuditorB != auditor) {
			continue
		}
		if record.ResolutionStatus == vtypes.DiscrepancyStatusResolved ||
			record.ResolutionStatus == vtypes.DiscrepancyStatusTimedOut {
			return true
		}
	}
	return false
}

func containsID(ids []uint64, target uint64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func genesisDiscrepancySupportsActiveGrace(record vtypes.DiscrepancyEvent) bool {
	switch record.ResolutionStatus {
	case vtypes.DiscrepancyStatusPending, vtypes.DiscrepancyStatusTimedOut:
		return true
	case vtypes.DiscrepancyStatusResolved:
		return record.FaultAttribution != vtypes.FaultAttributionProviderFault &&
			record.FaultAttribution != vtypes.FaultAttributionSharedFault
	default:
		return false
	}
}

func validateNextGenesisID(name string, nextID, maxID uint64) error {
	if nextID == 0 || nextID <= maxID || nextID == math.MaxUint64 {
		return errorsmod.Wrapf(moduletypes.ErrInvalidReason, "verification: invalid %s", name)
	}
	return nil
}

func genesisCapabilitiesInclude(actual, requested []vtypes.CapabilityFlag) bool {
	available := make(map[vtypes.CapabilityFlag]struct{}, len(actual))
	for _, capability := range actual {
		available[capability] = struct{}{}
	}
	for _, capability := range requested {
		if _, found := available[capability]; !found {
			return false
		}
	}
	return true
}

func isFinalGenesisFeeStatus(status vtypes.FeeStatus) bool {
	return status == vtypes.FeeStatusReleasedToAuditor ||
		status == vtypes.FeeStatusReturnedToProvider
}

func isFinalGenesisDepositStatus(status vtypes.DepositStatus) bool {
	return status == vtypes.DepositStatusReturnedToAuditor ||
		status == vtypes.DepositStatusSlashed
}

func isFinalGenesisProviderDepositStatus(status vtypes.ProviderDepositStatus) bool {
	return status == vtypes.ProviderDepositStatusReturnedToProvider ||
		status == vtypes.ProviderDepositStatusSlashed
}
