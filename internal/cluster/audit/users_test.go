package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

// D10 of step 8c-1: the new fields are omitted when empty, so a record of
// a membership entry is byte for byte what the 8b-2b build wrote, and an
// audit holding both kinds verifies.
func TestUserFieldsAreOmittedWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Started(1, 0); err != nil {
		t.Fatal(err)
	}
	if err := l.AuditEntry(clusterstate.AuditEntry{Epoch: 1, Index: 1, Type: "member.promoted", Version: 1, OpID: "a", Actor: "system",
		Origin: "n", Outcome: OutcomeApplied, NodeID: "x"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(Dir(dir), SegmentName(1)))
	for _, key := range []string{"user_id", "username", "role", "user_disabled", "count", "usernames", "code"} {
		if bytes.Contains(b, []byte(`"`+key+`"`)) {
			t.Fatalf("a membership record carries %q: %s", key, b)
		}
	}
	if err := l.AuditEntry(clusterstate.AuditEntry{Epoch: 1, Index: 2, Type: "users.import", Version: 1, OpID: "b", Actor: "local:0",
		Origin: "n", Outcome: OutcomeRefused, Code: "users_exist", Count: 2, Usernames: []string{"andy", "vera"}}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	rep, err := Verify(Dir(dir))
	if err != nil || rep.Break != nil || rep.Records != 3 || rep.Last.Code != "users_exist" || rep.Last.Count != 2 {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}
