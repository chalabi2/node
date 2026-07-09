package keeper

import (
	"testing"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/assert"

	etypes "pkg.akt.dev/go/node/escrow/types/v1"
)

func TestAccountRunway(t *testing.T) {
	type pmnt struct {
		state etypes.State
		rate  string
	}

	for _, tt := range []struct {
		name     string
		height   int64
		funds    string
		payments []pmnt
		expected int64
		open     bool
	}{
		{
			name:     "no payments",
			height:   100,
			funds:    "1000",
			payments: nil,
			expected: 0,
			open:     false,
		},
		{
			name:   "no open payments",
			height: 100,
			funds:  "1000",
			payments: []pmnt{
				{state: etypes.StateOverdrawn, rate: "10"},
				{state: etypes.StateClosed, rate: "10"},
			},
			expected: 0,
			open:     false,
		},
		{
			name:   "single payment",
			height: 100,
			funds:  "1000",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "10"},
			},
			expected: 200,
			open:     true,
		},
		{
			name:   "multi payment sums open rates",
			height: 100,
			funds:  "1000",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "3"},
				{state: etypes.StateOpen, rate: "7"},
			},
			expected: 200,
			open:     true,
		},
		{
			name:   "overdrawn payments excluded from rate",
			height: 100,
			funds:  "1000",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "10"},
				{state: etypes.StateOverdrawn, rate: "90"},
			},
			expected: 200,
			open:     true,
		},
		{
			name:   "partial block floors down",
			height: 100,
			funds:  "105",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "10"},
			},
			expected: 110,
			open:     true,
		},
		{
			name:   "fractional rates floor down",
			height: 100,
			funds:  "1",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "0.5"},
				{state: etypes.StateOpen, rate: "0.25"},
			},
			// floor(1 / 0.75) = 1
			expected: 101,
			open:     true,
		},
		{
			name:   "funds cover no full block",
			height: 100,
			funds:  "5",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "10"},
			},
			expected: 0,
			open:     true,
		},
		{
			name:   "zero funds",
			height: 100,
			funds:  "0",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "10"},
			},
			expected: 0,
			open:     true,
		},
		{
			name:   "overdrawn account with negative funds",
			height: 100,
			funds:  "-20",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "10"},
			},
			expected: 0,
			open:     true,
		},
		{
			name:   "zero rate open payment",
			height: 100,
			funds:  "1000",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "0"},
			},
			expected: 0,
			open:     true,
		},
		{
			name:   "funds exactly cover whole blocks",
			height: 7,
			funds:  "30",
			payments: []pmnt{
				{state: etypes.StateOpen, rate: "10"},
			},
			expected: 10,
			open:     true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			acc := account{
				Account: etypes.Account{
					State: etypes.AccountState{
						Funds: []etypes.Balance{
							{
								Denom:  denom,
								Amount: sdkmath.LegacyMustNewDecFromStr(tt.funds),
							},
						},
					},
				},
			}

			payments := make([]payment, 0, len(tt.payments))
			for _, p := range tt.payments {
				payments = append(payments, payment{
					Payment: etypes.Payment{
						State: etypes.PaymentState{
							State: p.state,
							Rate:  sdk.NewDecCoinFromDec(denom, sdkmath.LegacyMustNewDecFromStr(p.rate)),
						},
					},
				})
			}

			depletion, open := accountRunway(tt.height, &acc, payments)
			assert.Equal(t, tt.open, open)
			assert.Equal(t, tt.expected, depletion)
		})
	}
}
