
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

* (x/escrow) AEP-87: EventAccountRunway emitted at the end of every account settle with open payments: projected depletion height = current + floor(funds / sum of open payment rates), 0 when funds cover no full block. Additive typed event, no state change.

### State Machine Breaking

* (x/deployment) AEP-87: vid index (owner, vid -> GroupID) and replica backref counters in the deployment store; rebuilt deterministically on genesis import.
* (x/deployment) AEP-87: volume-group gates in CreateDeployment (feature flag, params bounds, vid gate, adoption and replica validation) and ErrVolumeAttached on CloseDeployment/CloseGroup while attachments exist.
* (x/market) AEP-87: volume attachment indexes (0x15/0x16) maintained in the keeper create/close routines; cascade-detach with reason substitution on every volume lease close path; rebuilt deterministically on genesis import.
* (x/market) AEP-87: CreateBid volume gates (reclamation floor, storage-quantity equality, adoption-provider gate, colocation contract for attaching groups), CreateLease re-verification, tenant volume closes routed through the reclamation window, and the migrate/evict auto-re-order carve-out.

### Improvements

* (sdk) Bump Cosmos SDK version to [v0.38.3](https://github.com/cosmos/cosmos-sdk/releases/tag/v0.38.3)

### Bug Fixes

* (x/market) AEP-87: OnEscrowAccountClosed ignores escrow accounts from foreign scopes instead of returning an error; a hook error aborts saveAccount mid-write. Bid-escrow account closes are proven byte-identical across the fix by regression tests.
* Fix bug in ditribution and querying rewards
