package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

// V8: an audit written by the build of main (c0e1f60; testdata made with
// that build's audit package) still verifies with this build, and this
// build appends guest records to it that verify too.
func TestMainBuildAuditVerifiesAndGrows(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "main-c0e1f60", "audit", SegmentName(1)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(dir), SegmentName(1)), src, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := Verify(Dir(dir))
	if err != nil || rep.Break != nil || rep.Records != 5 {
		t.Fatalf("main's audit: %+v %v", rep, err)
	}
	l, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.AuditEntry(clusterstate.AuditEntry{Epoch: 1, Index: 6, Type: clusterstate.TypeGuestAdd, Version: 1, OpID: "g-1",
		Actor: "reconciliation", Origin: "n", Outcome: OutcomeApplied, GuestID: "22222222-2222-4222-8222-222222222222",
		GuestKind: "container", GuestName: "web1", GuestOwner: "n", At: 1795000001}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	rep, err = Verify(Dir(dir))
	if err != nil || rep.Break != nil || rep.Records != 6 {
		t.Fatalf("after a guest record: %+v %v", rep, err)
	}
	b, _ := os.ReadFile(filepath.Join(Dir(dir), SegmentName(1)))
	if !bytes.HasPrefix(b, src) || !bytes.Contains(b, []byte(`"guest_name":"web1"`)) || !bytes.Contains(b, []byte(`"guest_owner":"n"`)) {
		t.Fatalf("the records: %s", b)
	}
	// A record without a guest names none of the new keys.
	for _, key := range []string{"guest_id", "guest_kind", "guest_name", "guest_owner"} {
		if bytes.Contains(src, []byte(`"`+key+`"`)) {
			t.Errorf("main's records hold %q", key)
		}
	}
}
