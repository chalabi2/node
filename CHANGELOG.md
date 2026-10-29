
<!--
Guiding Principles:

Changelogs are for humans, not machines.
There should be an entry for every single version.
The same types of changes should be grouped.
Versions and sections should be linkable.
The latest version comes first.
The release date of each version is displayed.
Mention whether you follow Semantic Versioning.

Usage:

Change log entries are to be added to the Unreleased section under the
appropriate stanza (see below). Each entry should ideally include a tag and
the Github issue reference in the following format:

* (<tag>) \#<issue-number> message

The issue numbers will later be link-ified during the release process so you do
not have to worry about including a link manually, but you can if you wish.

Types of changes (Stanzas):

"Features" for new features.
"Improvements" for changes in existing functionality.
"Deprecated" for soon-to-be removed features.
"Bug Fixes" for any bug fixes.
"Client Breaking" for breaking CLI commands and REST routes used by end-users.
"API Breaking" for breaking exported APIs used by developers building on SDK.
"State Machine Breaking" for any changes that result in a different AppState given same genesisState and txList.

Ref: https://keepachangelog.com/en/1.0.0/
-->

# Changelog

## [Unreleased]

* (ibc-go) Use ibc v4.4.0 https://pkg.akt.dev/node/pull/1825

### Features

* (x/escrow) Storage market: EventAccountRunway emitted at the end of every account settle with open payments: projected depletion height = current + floor(funds / sum of open payment rates), 0 when funds cover no full block. Additive typed event, no state change.
* (x/market) In-place lease resize: MsgResizeLease (tenant) records a pending request with the group's new resource units and manifest hash; the lease's provider applies it with MsgAcceptLeaseResize at a price within the request's ceiling or declines with MsgRejectLeaseResize, and the tenant can withdraw it with MsgCancelLeaseResize. Query/LeaseResizes lists pending requests, for a tenant, a provider or, with owner and dseq, one deployment. Ships off behind Params.LeaseResizeEnabled.

### State Machine Breaking

* (x/deployment) Storage market: vid index (owner, vid -> GroupID) and replica backref counters in the deployment store; rebuilt deterministically on genesis import.
* (x/deployment) Storage market: volume-group gates in CreateDeployment (feature flag, params bounds, vid gate, adoption and replica validation) and ErrVolumeAttached on CloseDeployment/CloseGroup while attachments exist. The vid gate reads an index holder that never had a lease but was created to adopt a volume as that volume, so closing an adoption before it is leased does not free a vid whose data is still retained, and an adoption must name the volume that holds the data.
* (x/deployment) Storage market: MsgPauseGroup and MsgStartGroup on a volume group fail with ErrVolumeNotPausable (chain-sdk deployment/v1 code 34). A paused volume group would hold its vid with no lease, and starting one lists an order any provider could win with an empty volume while the provider of its last lease still holds the data; a tenant closes a volume or moves it.
* (x/deployment) Storage market: the six volume error variants (ErrVolumeOrdersDisabled, ErrVidInUse, ErrVidRetained, ErrVolumeAttached, ErrInvalidVolumeAdoption, ErrInvalidVolumeReplica) moved from provisional node-side registrations (codes 100+) to chain-sdk deployment/v1 (codes 27-32); messages unchanged.
* (x/market) Storage market: Lease.ClosedAt (block time, unix seconds) written on every lease close path; LastVolumeLeaseClosedAt anchors the vid retention gate and adoption validation on the dead volume's most recent closed lease. Leases closed before v2.2.0 carry closed_at=0 and report no anchor (expired semantics), so the upgrade needs no backfill.
* (x/market) Storage market: volume attachment indexes (0x15/0x16) maintained in the keeper create/close routines; cascade-detach with reason substitution on every volume lease close path; rebuilt deterministically on genesis import.
* (x/market) Storage market: CreateBid volume gates (the bid must carry the order's VolumePolicy unchanged, and no other bid may carry one, so provider software that predates volumes cannot win a volume order; reclamation floor, storage-quantity equality, adoption-provider gate, colocation contract for attaching groups), CreateLease re-verification, tenant volume closes routed through the reclamation window, and the auto-re-order carve-out: MsgLeaseStartReclaim on a volume lease is recorded and announced as volume_evict whatever its reason, and a provider's MsgCloseBid on a volume lease re-lists the group unless the tenant started the window to close the volume, recording the reason the window ran with.
* (x/market) Lease resize state: pending requests (at most one per deployment, they carry a deployment-wide manifest hash) under prefix 0x17, exported in genesis. Accepting rewrites the group spec, the active order spec, the lease and active bid price and, when the request has one, the deployment hash, in one message. Closing or reclaiming a lease drops its pending resize with EventLeaseResizeCanceled, and MsgUpdateDeployment is refused with ErrLeaseResizePending while a resize is pending, since its accept would undo the update. Each request carries a chain-set digest (SHA-256 of its encoding), emitted with the resize events; MsgAcceptLeaseResize and MsgRejectLeaseResize must name it, and fail with ErrLeaseResizeChanged when the pending request is a different one. A volume at either end of a live replication cannot be resized, and adopting a volume, or replicating one, requires at least its current size, which a resize may have grown.
* (x/escrow) PaymentUpdateRate settles the account at the old rate before switching an open payment to a new one, so no unsettled block is re-priced, and re-emits EventAccountRunway at the new rates.
* (upgrades) Storage market: v2.2.0 upgrade handler. Volumes and lease resize are new fields and messages on deployment/v1beta4 and market/v1beta5, so stored records decode unchanged and there are no store migrations or ConsensusVersion bumps. Seeds the storage-market params (VolumeOrdersEnabled=false, MinVolumeReclamationWindow=24h, MaxVolumeSize=32Ti, MaxVolumeRetention=720h, MaxVolumeReplicas=4) and the lease resize params (LeaseResizeEnabled=false, LeaseResizeTTL=10m) and initializes the empty volume/attachment indexes (0x13/0x15/0x16). No store keys added or removed.

### Improvements

* (sdk) Bump Cosmos SDK version to [v0.38.3](https://github.com/cosmos/cosmos-sdk/releases/tag/v0.38.3)

### Bug Fixes

* (x/market) Storage market: OnEscrowAccountClosed ignores escrow accounts from foreign scopes instead of returning an error; a hook error aborts saveAccount mid-write. Bid-escrow account closes are proven byte-identical across the fix by regression tests.
* Fix bug in ditribution and querying rewards
