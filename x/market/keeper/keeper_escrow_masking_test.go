package keeper_test

import (
	"bytes"
	"testing"

	sdkmath "cosmossdk.io/math"
	storetypes "cosmossdk.io/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	dv1 "pkg.akt.dev/go/node/deployment/v1"
	escrowid "pkg.akt.dev/go/node/escrow/id/v1"
	emodule "pkg.akt.dev/go/node/escrow/module"
	etypes "pkg.akt.dev/go/node/escrow/types/v1"
	mv1 "pkg.akt.dev/go/node/market/v1"
	mvbeta "pkg.akt.dev/go/node/market/v2beta1"
)

// Regression coverage for the masking at OnBidClosed:
//
//	_ = k.ekeeper.AccountClose(ctx, bid.ID.ToEscrowAccountID())
//
// Bid escrow accounts live in ScopeBid, so the escrow account-close hook
// (x/market/hooks.OnEscrowAccountClosed) sees a foreign-scope XID. The hook
// runs after saveAccount has already applied refunds and store.Set, so a hook
// error there is swallowed by the masking above and the close still lands in
// the store. These tests pin that behavior: the bid-escrow close must be
// byte-identical whether the account-close hook errors on the foreign scope
// (pre-fix behavior) or ignores it (fixed behavior).

// Test_OnBidClosed_MaskedBidEscrowAccountClose exercises the real path: a bid
// with a ScopeBid escrow account is closed through OnBidClosed and the escrow
// account must end up closed in the store, with the masked call swallowing
// any account-close hook outcome.
func Test_OnBidClosed_MaskedBidEscrowAccountClose(t *testing.T) {
	ctx, k, suite := setupKeeper(t)
	bid, _ := createBid(t, suite)

	aid := bid.ID.ToEscrowAccountID()
	require.Equal(t, escrowid.ScopeBid, aid.Scope)

	// sanity: the account-close hook cannot resolve a deployment ID from a
	// bid-scope escrow account.
	_, err := dv1.DeploymentIDFromEscrowID(aid)
	require.Error(t, err)

	require.NoError(t, k.OnBidClosed(ctx, bid))

	got, ok := k.GetBid(ctx, bid.ID)
	require.True(t, ok)
	require.Equal(t, mvbeta.BidClosed, got.State)

	// the escrow account close was fully applied despite the masking: the
	// account is closed in the store and a second close reports it as such.
	acc, err := suite.EscrowKeeper().GetAccount(ctx, aid)
	require.NoError(t, err)
	require.Equal(t, etypes.StateClosed, acc.State.State)

	err = suite.EscrowKeeper().AccountClose(ctx, aid)
	require.ErrorIs(t, err, emodule.ErrAccountClosed)

	// idempotent re-close of the bid is a no-op.
	require.NoError(t, k.OnBidClosed(ctx, got))
}

// Test_OnBidClosed_BidEscrowCloseByteIdentical proves the bid-escrow
// account-close behavior is byte-identical whether the account-close hook
// errors on the foreign scope (the pre-fix hook returned the
// DeploymentIDFromEscrowID error) or returns nil (the fixed hook). The
// erroring variant is simulated by an extra registered hook, and the full
// escrow and market stores plus emitted events are compared between runs.
func Test_OnBidClosed_BidEscrowCloseByteIdentical(t *testing.T) {
	run := func(t *testing.T, hookErrors bool) (map[string][]byte, map[string][]byte, sdk.Events) {
		t.Helper()

		ctx, k, suite := setupKeeper(t)

		// deterministic fixtures so store bytes are comparable across runs
		owner := sdk.AccAddress(bytes.Repeat([]byte{0x01}, 20))
		provider := sdk.AccAddress(bytes.Repeat([]byte{0x02}, 20))

		bidID := mv1.MakeBidID(
			mv1.MakeOrderID(dv1.MakeGroupID(dv1.DeploymentID{Owner: owner.String(), DSeq: 1}, 1), 1),
			provider,
		)
		bid := mvbeta.Bid{
			ID:    bidID,
			State: mvbeta.BidActive,
			Price: sdk.NewDecCoinFromDec("uact", sdkmath.LegacyNewDec(100)),
		}
		require.NoError(t, k.SaveBid(ctx, bid))

		aid := bidID.ToEscrowAccountID()
		require.NoError(t, suite.EscrowKeeper().AccountCreate(ctx, aid, provider, []etypes.Depositor{{
			Owner:   provider.String(),
			Height:  ctx.BlockHeight(),
			Balance: sdk.NewDecCoinFromCoin(sdk.NewInt64Coin("uact", 5000000)),
		}}))

		// pre-fix, OnEscrowAccountClosed returned the
		// DeploymentIDFromEscrowID error for the foreign scope; simulate it
		// with an extra hook so both hook outcomes flow through the masking.
		hookCalled := false
		suite.EscrowKeeper().AddOnAccountClosedHook(func(_ sdk.Context, obj etypes.Account) error {
			hookCalled = true
			if hookErrors && obj.ID.Scope == escrowid.ScopeBid {
				return dv1.ErrInvalidEscrowID
			}
			return nil
		})

		require.NoError(t, k.OnBidClosed(ctx, bid))
		require.True(t, hookCalled, "account-close hook must have run")

		return dumpStore(ctx, suite.EscrowKeeper().StoreKey()),
			dumpStore(ctx, k.StoreKey()),
			ctx.EventManager().Events()
	}

	escrowErr, marketErr, eventsErr := run(t, true)
	escrowOK, marketOK, eventsOK := run(t, false)

	require.Equal(t, escrowErr, escrowOK, "escrow store must be byte-identical")
	require.Equal(t, marketErr, marketOK, "market store must be byte-identical")
	require.Equal(t, eventsErr, eventsOK, "emitted events must be identical")

	// and the run is not vacuous: the account really closed.
	require.NotEmpty(t, escrowOK)
}

func dumpStore(ctx sdk.Context, skey storetypes.StoreKey) map[string][]byte {
	store := ctx.KVStore(skey)

	iter := store.Iterator(nil, nil)
	defer func() {
		_ = iter.Close()
	}()

	out := make(map[string][]byte)
	for ; iter.Valid(); iter.Next() {
		out[string(iter.Key())] = append([]byte(nil), iter.Value()...)
	}

	return out
}
