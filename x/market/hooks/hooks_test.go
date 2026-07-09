package hooks

import (
	"testing"

	"github.com/stretchr/testify/require"

	escrowid "pkg.akt.dev/go/node/escrow/id/v1"
	etypes "pkg.akt.dev/go/node/escrow/types/v1"
	mv1 "pkg.akt.dev/go/node/market/v1"
	"pkg.akt.dev/go/testutil"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// Test_OnEscrowAccountClosed_ForeignScope asserts that the account-close hook
// ignores accounts from any scope it does not own instead of returning an
// error. A hook error aborts saveAccount mid-write (refunds and store.Set are
// already applied), so it must never be raised for a foreign scope.
func Test_OnEscrowAccountClosed_ForeignScope(t *testing.T) {
	// nil keepers prove the hook returns before touching any state.
	h := New(nil, nil)

	for _, tt := range []struct {
		name string
		id   escrowid.Account
	}{
		{
			name: "bid scope",
			id:   mv1.MakeBidID(testutil.OrderID(t), testutil.AccAddress(t)).ToEscrowAccountID(),
		},
		{
			name: "invalid scope",
			id: escrowid.Account{
				Scope: escrowid.ScopeInvalid,
				XID:   "invalid",
			},
		},
		{
			name: "deployment scope with malformed xid",
			id: escrowid.Account{
				Scope: escrowid.ScopeDeployment,
				XID:   "not/a/deployment/id",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := h.OnEscrowAccountClosed(sdk.Context{}, etypes.Account{ID: tt.id})
			require.NoError(t, err)
		})
	}
}
