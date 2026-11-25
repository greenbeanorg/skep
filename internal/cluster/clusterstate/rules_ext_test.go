package clusterstate_test

import (
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

// The state machine's username, display-name and role rules are
// internal/auth's (D3): clusterstate does not import internal/auth, so
// this test keeps the two equal.
func TestUserRulesEqualAuth(t *testing.T) {
	for _, name := range []string{"a", "andy", "a.b_c-d", "a1234567890123456789012345678901", "a12345678901234567890123456789012",
		"", "A", "1a", "_a", "an dy", "andy!", "ändy", "system", "-a", "a-"} {
		if got, want := clusterstate.UsernameRE.MatchString(name), auth.ValidUsername(name); got != want {
			t.Errorf("username %q: state machine %v, auth %v", name, got, want)
		}
	}
	if clusterstate.MaxDisplayName != auth.MaxDisplayNameLength {
		t.Errorf("display name: %d and %d", clusterstate.MaxDisplayName, auth.MaxDisplayNameLength)
	}
	// F3 of the 8c-1 review: the same characters are refused, the C1
	// controls (U+0080 to U+009F) included, and the same lengths.
	long := strings.Repeat("é", auth.MaxDisplayNameLength)
	for _, name := range []string{"", "Andy", "Ändy Ünicode", "a\x00b", "a\x07b", "a\tb", "a\nb", "a\x1fb", "a\x7fb",
		"a\u0080b", "a\u0085b", "a\u009fb", "a\u00a0b", "a\u200bb", "\xff", long, long + "x"} {
		if got, want := clusterstate.ValidDisplayName(name), auth.ValidateDisplayName(name) == nil; got != want {
			t.Errorf("display name %q: state machine %v, auth %v", name, got, want)
		}
	}
	if clusterstate.RoleAdmin != auth.RoleAdmin || clusterstate.RoleOperator != auth.RoleOperator || clusterstate.RoleViewer != auth.RoleViewer {
		t.Error("the built-in role IDs differ")
	}
	for _, code := range []string{auth.CodeUsernameTaken, auth.CodeLastAdmin, auth.CodeUnknownRole, auth.CodeTooManyUsers} {
		switch code {
		case clusterstate.CodeUsernameTaken, clusterstate.CodeLastAdmin, clusterstate.CodeUnknownRole, clusterstate.CodeTooManyUsers:
		default:
			t.Errorf("auth's code %q is not the state machine's", code)
		}
	}
}
