package keeper_test

import (
	"testing"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	escrowid "pkg.akt.dev/go/node/escrow/id/v1"
	"pkg.akt.dev/go/node/escrow/module"
	etypes "pkg.akt.dev/go/node/escrow/types/v1"
	"pkg.akt.dev/go/sdkutil"
	"pkg.akt.dev/go/testutil"

	"pkg.akt.dev/node/v2/testutil/state"
)

// allowWithdrawals lets escrow pay providers out, as closing a payment and
// overdrawing an account do.
func allowWithdrawals(ssuite *state.TestSuite) {
	ssuite.BankKeeper().
		On("SendCoinsFromModuleToAccount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Maybe()
}

func Test_PaymentUpdateRate_SettlesAtOldRateFirst(t *testing.T) {
	ssuite := state.SetupTestSuite(t)
	ekeeper := ssuite.EscrowKeeper()

	// deposit 1000, one payment at 10 per block
	aid, pids := setupRunwayAccount(t, ssuite, 1000, 10)
	h0 := ssuite.Context().BlockHeight()

	// ten blocks later the rate goes up to 30
	ctx := ssuite.Context().WithBlockHeight(h0 + 10)
	require.NoError(t, ekeeper.PaymentUpdateRate(ctx, pids[0], testutil.ACTDecCoin(t, 30)))

	// those ten blocks are charged at the old rate, through this block
	acc, err := ekeeper.GetAccount(ctx, aid)
	require.NoError(t, err)
	require.Equal(t, etypes.StateOpen, acc.State.State)
	require.Equal(t, h0+10, acc.State.SettledAt)
	require.Equal(t, sdkmath.LegacyNewDec(900), acc.State.Funds[0].Amount)

	payment, err := ekeeper.GetPayment(ctx, pids[0])
	require.NoError(t, err)
	require.Equal(t, etypes.StateOpen, payment.State.State)
	require.Equal(t, testutil.ACTDecCoin(t, 30), payment.State.Rate)
	require.Equal(t, sdkmath.LegacyNewDec(100), payment.State.Balance.Amount)

	// and the blocks after it at the new one
	ctx = ctx.WithBlockHeight(h0 + 15)
	od, err := ekeeper.AccountSettle(ctx, aid)
	require.NoError(t, err)
	require.False(t, od)

	acc, err = ekeeper.GetAccount(ctx, aid)
	require.NoError(t, err)
	require.Equal(t, sdkmath.LegacyNewDec(750), acc.State.Funds[0].Amount)

	payment, err = ekeeper.GetPayment(ctx, pids[0])
	require.NoError(t, err)
	require.Equal(t, sdkmath.LegacyNewDec(250), payment.State.Balance.Amount)
}

func Test_PaymentUpdateRate_RunwayEvents(t *testing.T) {
	ssuite := state.SetupTestSuite(t)
	ekeeper := ssuite.EscrowKeeper()

	// deposit 1000, payments at 10 and 20 per block
	aid, pids := setupRunwayAccount(t, ssuite, 1000, 10, 20)

	ctx := ssuite.Context().WithBlockHeight(ssuite.Context().BlockHeight() + 10)
	require.NoError(t, ekeeper.PaymentUpdateRate(ctx, pids[0], testutil.ACTDecCoin(t, 40)))

	// 10 blocks x 30 settled leaves 700. The settle reports the runway at
	// the old rates, floor(700 / 30) = 23 blocks, then the switch reports it
	// at the new ones, floor(700 / (40 + 20)) = 11 blocks.
	events := runwayEvents(t, ctx)
	require.Len(t, events, 2)
	require.Equal(t, aid, events[0].ID)
	require.Equal(t, ctx.BlockHeight()+23, events[0].ProjectedDepletionHeight)
	require.Equal(t, aid, events[1].ID)
	require.Equal(t, ctx.BlockHeight()+11, events[1].ProjectedDepletionHeight)

	// the other payment keeps its rate
	payment, err := ekeeper.GetPayment(ctx, pids[1])
	require.NoError(t, err)
	require.Equal(t, testutil.ACTDecCoin(t, 20), payment.State.Rate)
}

func Test_PaymentUpdateRate_SameBlockSettle(t *testing.T) {
	ssuite := state.SetupTestSuite(t)
	ekeeper := ssuite.EscrowKeeper()

	// deposit 1000, one payment at 10 per block
	aid, pids := setupRunwayAccount(t, ssuite, 1000, 10)
	h0 := ssuite.Context().BlockHeight()

	// a settle earlier in the block leaves nothing due, so the switch finds
	// no payment to settle and must update the stored one
	ctx := ssuite.Context().WithBlockHeight(h0 + 10)
	od, err := ekeeper.AccountSettle(ctx, aid)
	require.NoError(t, err)
	require.False(t, od)

	require.NoError(t, ekeeper.PaymentUpdateRate(ctx, pids[0], testutil.ACTDecCoin(t, 30)))

	payment, err := ekeeper.GetPayment(ctx, pids[0])
	require.NoError(t, err)
	require.Equal(t, etypes.StateOpen, payment.State.State)
	require.Equal(t, testutil.ACTDecCoin(t, 30), payment.State.Rate)
	require.Equal(t, sdkmath.LegacyNewDec(100), payment.State.Balance.Amount)

	// nothing is charged twice
	acc, err := ekeeper.GetAccount(ctx, aid)
	require.NoError(t, err)
	require.Equal(t, h0+10, acc.State.SettledAt)
	require.Equal(t, sdkmath.LegacyNewDec(900), acc.State.Funds[0].Amount)

	// the first event is the settle's at the old rate, floor(900 / 10); the
	// switch adds none of its own settle and reports floor(900 / 30)
	events := runwayEvents(t, ctx)
	require.Len(t, events, 2)
	require.Equal(t, ctx.BlockHeight()+90, events[0].ProjectedDepletionHeight)
	require.Equal(t, aid, events[1].ID)
	require.Equal(t, ctx.BlockHeight()+30, events[1].ProjectedDepletionHeight)

	// the next blocks are charged at the new rate
	ctx = ctx.WithBlockHeight(h0 + 12)
	od, err = ekeeper.AccountSettle(ctx, aid)
	require.NoError(t, err)
	require.False(t, od)

	acc, err = ekeeper.GetAccount(ctx, aid)
	require.NoError(t, err)
	require.Equal(t, sdkmath.LegacyNewDec(840), acc.State.Funds[0].Amount)
}

func Test_PaymentUpdateRate_Rejects(t *testing.T) {
	// requireRate asserts the stored payment still carries the rate.
	requireRate := func(t *testing.T, ssuite *state.TestSuite, pid escrowid.Payment, rate int64) {
		t.Helper()

		payment, err := ssuite.EscrowKeeper().GetPayment(ssuite.Context(), pid)
		require.NoError(t, err)
		require.Equal(t, testutil.ACTDecCoin(t, rate), payment.State.Rate)
	}

	t.Run("overdrawn account", func(t *testing.T) {
		ssuite := state.SetupTestSuite(t)
		allowWithdrawals(ssuite)

		// deposit 100, one payment at 30 per block: 10 blocks owe 300
		_, pids := setupRunwayAccount(t, ssuite, 100, 30)

		ctx := ssuite.Context().WithBlockHeight(ssuite.Context().BlockHeight() + 10)
		err := ssuite.EscrowKeeper().PaymentUpdateRate(ctx, pids[0], testutil.ACTDecCoin(t, 10))
		require.ErrorIs(t, err, module.ErrAccountOverdrawn)
		requireRate(t, ssuite, pids[0], 30)
	})

	t.Run("closed payment", func(t *testing.T) {
		ssuite := state.SetupTestSuite(t)
		allowWithdrawals(ssuite)

		_, pids := setupRunwayAccount(t, ssuite, 1000, 10)

		ctx := ssuite.Context().WithBlockHeight(ssuite.Context().BlockHeight() + 10)
		require.NoError(t, ssuite.EscrowKeeper().PaymentClose(ctx, pids[0]))

		err := ssuite.EscrowKeeper().PaymentUpdateRate(ctx, pids[0], testutil.ACTDecCoin(t, 30))
		require.ErrorIs(t, err, module.ErrPaymentClosed)
		requireRate(t, ssuite, pids[0], 10)
	})

	for _, tc := range []struct {
		name string
		rate sdk.DecCoin
		err  error
	}{
		{
			name: "wrong denom",
			rate: sdk.NewDecCoin(sdkutil.DenomUakt, sdkmath.NewInt(30)),
			err:  module.ErrInvalidDenomination,
		},
		{
			name: "zero rate",
			rate: sdk.NewDecCoin(sdkutil.DenomUact, sdkmath.ZeroInt()),
			err:  module.ErrPaymentRateZero,
		},
		{
			name: "negative rate",
			rate: sdk.DecCoin{Denom: sdkutil.DenomUact, Amount: sdkmath.LegacyNewDec(-30)},
			err:  module.ErrPaymentRateZero,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ssuite := state.SetupTestSuite(t)

			aid, pids := setupRunwayAccount(t, ssuite, 1000, 10)
			h0 := ssuite.Context().BlockHeight()

			ctx := ssuite.Context().WithBlockHeight(h0 + 10)
			err := ssuite.EscrowKeeper().PaymentUpdateRate(ctx, pids[0], tc.rate)
			require.ErrorIs(t, err, tc.err)
			requireRate(t, ssuite, pids[0], 10)

			// the check runs before the settle
			acc, err := ssuite.EscrowKeeper().GetAccount(ctx, aid)
			require.NoError(t, err)
			require.Equal(t, h0, acc.State.SettledAt)
		})
	}
}
