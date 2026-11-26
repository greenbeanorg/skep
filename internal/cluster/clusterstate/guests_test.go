package clusterstate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	guest1 = "a1b2c3d4-0000-4000-8000-000000000001"
	guest2 = "a1b2c3d4-0000-4000-8000-000000000002"
	guest3 = "a1b2c3d4-0000-4000-8000-000000000003"
	task1  = "b1b2c3d4-0000-4000-8000-000000000001"
)

// twoMembers is a cluster of smith (nodeA) and langrishe (nodeB), both in
// state member; the next index is 5.
func twoMembers(t *testing.T) *Machine {
	t.Helper()
	m := New()
	for i, raw := range [][]byte{
		initEntry(t, "op-1"),
		pendingEntry(t, "op-2", nodeB, "langrishe", "10.0.0.2:8444", pinB, 2000000000),
		joinEntry(t, "op-3", nodeB, pinB, 1900000000),
		encode(t, TypeMemberPromoted, "op-4", MemberPromoted{NodeID: nodeB}),
	} {
		if r := m.Apply(uint64(i+1), raw); r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	return m
}

func reserve(t *testing.T, op, kind, name, key, owner string) []byte {
	return encode(t, TypeGuestReserve, op, GuestReserve{Kind: kind, Name: name, Key: key, Owner: owner, At: 1900000100})
}

func add(t *testing.T, op, id, kind, name, owner string) []byte {
	return encode(t, TypeGuestAdd, op, GuestAdd{ID: id, Kind: kind, Name: name, TaskID: task1, Owner: owner, At: 1900000200})
}

func wantRefused(t *testing.T, r Result, code string) {
	t.Helper()
	if !errors.Is(r.Err, ErrRefused) || RefusalCode(r.Err) != code {
		t.Fatalf("result %+v, want refused %s", r, code)
	}
}

func wantApplied(t *testing.T, r Result, unchanged bool) {
	t.Helper()
	if r.Err != nil || r.Duplicate || r.Unchanged != unchanged {
		t.Fatalf("result %+v, want applied (unchanged %v)", r, unchanged)
	}
}

// D12, D13: a reservation, its exceptions and refusals; a release.
func TestGuestReserveAndRelease(t *testing.T) {
	m := twoMembers(t)
	wantApplied(t, m.Apply(5, reserve(t, "r-1", KindContainer, "web1", "k-1", nodeB)), false)
	// The same owner and key: a success that changes nothing.
	wantApplied(t, m.Apply(6, reserve(t, "r-2", KindContainer, "web1", "k-1", nodeB)), true)
	// Another key or owner: name_taken, naming the holder's node.
	r := m.Apply(7, reserve(t, "r-3", KindContainer, "web1", "k-2", nodeA))
	wantRefused(t, r, CodeNameTaken)
	if !strings.Contains(r.Err.Error(), "langrishe") {
		t.Errorf("the refusal does not name the holder: %v", r.Err)
	}
	// The same name of another kind is free.
	wantApplied(t, m.Apply(8, reserve(t, "r-4", KindVM, "web1", "k-3", nodeA)), false)
	// Invalid values, a non-member owner.
	for i, bad := range []GuestReserve{
		{Kind: "jail", Name: "x", Key: "k", Owner: nodeA, At: 1},
		{Kind: KindVM, Name: "", Key: "k", Owner: nodeA, At: 1},
		{Kind: KindVM, Name: "a\x1fb", Key: "k", Owner: nodeA, At: 1},
		{Kind: KindVM, Name: "a\x7fb", Key: "k", Owner: nodeA, At: 1},
		{Kind: KindVM, Name: strings.Repeat("x", 256), Key: "k", Owner: nodeA, At: 1},
		{Kind: KindVM, Name: "x", Key: "a b", Owner: nodeA, At: 1},
		{Kind: KindVM, Name: "x", Key: "k", Owner: nodeA, At: 0},
	} {
		wantRefused(t, m.Apply(uint64(20+i), encode(t, TypeGuestReserve, "bad-"+string(rune('a'+i)), bad)), CodeInvalid)
	}
	wantRefused(t, m.Apply(30, reserve(t, "r-5", KindVM, "x", "k", nodeC)), CodeOwnerNotMember)
	// A UTF-8 name of 255 bytes is valid.
	wantApplied(t, m.Apply(31, reserve(t, "r-6", KindVM, strings.Repeat("é", 127)+"x", "k", nodeA)), false)
	// A release must match kind, name, key and owner; otherwise nothing.
	wantApplied(t, m.Apply(32, encode(t, TypeGuestRelease, "rel-1", GuestRelease{Kind: KindContainer, Name: "web1", Key: "k-1", Owner: nodeA})), true)
	wantApplied(t, m.Apply(33, encode(t, TypeGuestRelease, "rel-2", GuestRelease{Kind: KindContainer, Name: "web1", Key: "k-1", Owner: nodeB})), false)
	g := m.Guests()
	if len(g.Reservations) != 2 || g.Index != 33 {
		t.Fatalf("after the release: %+v", g)
	}
	if r := m.Apply(34, reserve(t, "r-7", KindContainer, "web1", "k-9", nodeA)); r.Err != nil {
		t.Fatalf("the released name: %v", r.Err)
	}
}

// D14, D16: an add consumes this owner's reservation and takes its key; it
// is never refused for its name, and the second guest of a name is
// flagged; the same ID with the same owner is unchanged, with another
// owner not_owner.
func TestGuestAddAndFlag(t *testing.T) {
	m := twoMembers(t)
	wantApplied(t, m.Apply(5, reserve(t, "r-1", KindContainer, "web1", "k-1", nodeB)), false)
	wantApplied(t, m.Apply(6, add(t, "a-1", guest1, KindContainer, "web1", nodeB)), false)
	g := m.Guests()
	if len(g.Reservations) != 0 || len(g.Guests) != 1 || g.Guests[0].Key != "k-1" || g.Guests[0].AddedIndex != 6 || g.Guests[0].Flagged {
		t.Fatalf("after the add: %+v", g)
	}
	// The reservation exception holds for the guest too.
	wantApplied(t, m.Apply(7, reserve(t, "r-2", KindContainer, "web1", "k-1", nodeB)), true)
	wantApplied(t, m.Apply(8, add(t, "a-2", guest1, KindContainer, "web1", nodeB)), true)
	wantRefused(t, m.Apply(9, add(t, "a-3", guest1, KindContainer, "web1", nodeA)), CodeNotOwner)
	// A second web1, on smith, without a reservation: added, flagged.
	wantApplied(t, m.Apply(10, add(t, "a-4", guest2, KindContainer, "web1", nodeA)), false)
	for _, x := range m.Guests().Guests {
		if x.Flagged != (x.ID == guest2) {
			t.Errorf("flag of %s: %v", x.ID, x.Flagged)
		}
	}
	// D15: a removal by another owner is refused; the owner's removes,
	// and the remaining web1 is no longer flagged.
	wantRefused(t, m.Apply(11, encode(t, TypeGuestRemove, "x-1", GuestRemove{ID: guest1, Owner: nodeA})), CodeNotOwner)
	wantApplied(t, m.Apply(12, encode(t, TypeGuestRemove, "x-2", GuestRemove{ID: guest1, Owner: nodeB})), false)
	wantApplied(t, m.Apply(13, encode(t, TypeGuestRemove, "x-3", GuestRemove{ID: guest1, Owner: nodeB})), true)
	if gs := m.Guests().Guests; len(gs) != 1 || gs[0].ID != guest2 || gs[0].Flagged {
		t.Fatalf("after the removal: %+v", gs)
	}
	wantRefused(t, m.Apply(14, add(t, "a-5", "NOT-A-UUID", KindVM, "x", nodeA)), CodeInvalid)
	wantRefused(t, m.Apply(15, add(t, "a-6", guest3, KindVM, "x", nodeC)), CodeOwnerNotMember)
}

// D12/D14: guests and reservations together stop at MaxGuests; an add
// that consumes its own reservation still fits.
func TestGuestLimit(t *testing.T) {
	m := twoMembers(t)
	st := m.st.clone()
	for i := 0; i < MaxGuests-1; i++ {
		id := "c0000000-0000-4000-8000-" + strings.Repeat("0", 12-len(itoa(i))) + itoa(i)
		st.guests[id] = Guest{ID: id, Kind: KindVM, Name: "g" + itoa(i), Owner: nodeA, AddedIndex: 1, At: 1}
	}
	m.st = st
	wantApplied(t, m.Apply(5, reserve(t, "r-1", KindVM, "last", "k", nodeA)), false)
	wantRefused(t, m.Apply(6, reserve(t, "r-2", KindVM, "over", "k", nodeA)), CodeTooManyGuests)
	wantRefused(t, m.Apply(7, add(t, "a-1", guest1, KindVM, "other", nodeA)), CodeTooManyGuests)
	wantApplied(t, m.Apply(8, add(t, "a-2", guest2, KindVM, "last", nodeA)), false)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

// D17: member.removing is refused while the node owns a guest or a
// reservation.
func TestRemovingRefusedWhileOwningGuests(t *testing.T) {
	m := twoMembers(t)
	wantApplied(t, m.Apply(5, reserve(t, "r-1", KindVM, "db1", "k-1", nodeB)), false)
	if r := m.Apply(6, removingEntry(t, "rm-1", nodeB)); r.Err == nil || !strings.Contains(r.Err.Error(), "owns 0 guests and 1 name reservations") {
		t.Fatalf("removal with a reservation: %+v", r)
	}
	wantApplied(t, m.Apply(7, add(t, "a-1", guest1, KindVM, "db1", nodeB)), false)
	if r := m.Apply(8, removingEntry(t, "rm-2", nodeB)); r.Err == nil {
		t.Fatal("removal with a guest was applied")
	}
	wantApplied(t, m.Apply(9, encode(t, TypeGuestRemove, "x-1", GuestRemove{ID: guest1, Owner: nodeB})), false)
	if r := m.Apply(10, removingEntry(t, "rm-3", nodeB)); r.Err != nil {
		t.Fatalf("removal without guests: %v", r.Err)
	}
}

// guestHistory feeds a machine the same entries: guests, reservations, a
// flagged pair, a release and a removal.
func guestHistory(t *testing.T) *Machine {
	m := twoMembers(t)
	for i, raw := range [][]byte{
		reserve(t, "r-1", KindContainer, "web1", "k-1", nodeB),
		add(t, "a-1", guest1, KindContainer, "web1", nodeB),
		add(t, "a-2", guest2, KindContainer, "web1", nodeA),
		reserve(t, "r-2", KindVM, "db1", "k-2", nodeA),
		reserve(t, "r-3", KindVM, "db2", "k-3", nodeA),
		encode(t, TypeGuestRelease, "rel-1", GuestRelease{Kind: KindVM, Name: "db2", Key: "k-3", Owner: nodeA}),
		add(t, "a-3", guest3, KindVM, "app", nodeB),
		encode(t, TypeGuestRemove, "x-1", GuestRemove{ID: guest3, Owner: nodeB}),
	} {
		m.Apply(uint64(5+i), raw)
	}
	return m
}

// V7: two machines fed the same entries in the same order end with
// byte-identical snapshots (format 6), which restore to the same state,
// the flag computed again.
func TestGuestsDeterministicSnapshot(t *testing.T) {
	a, b := guestHistory(t), guestHistory(t)
	sa, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := b.Snapshot()
	if !bytes.Equal(sa, sb) {
		t.Fatalf("snapshots differ:\n%s\n%s", sa, sb)
	}
	if !bytes.Contains(sa, []byte(`"format":6`)) || bytes.Contains(sa, []byte("flagged")) {
		t.Fatalf("snapshot: %s", sa)
	}
	n := New()
	if err := n.Restore(bytes.NewReader(sa)); err != nil {
		t.Fatal(err)
	}
	g := n.Guests()
	if len(g.Guests) != 2 || len(g.Reservations) != 1 || g.Index != 12 {
		t.Fatalf("restored: %+v", g)
	}
	for _, x := range g.Guests {
		if x.Flagged != (x.ID == guest2) {
			t.Errorf("restored flag of %s: %v", x.ID, x.Flagged)
		}
	}
	if again, _ := n.Snapshot(); !bytes.Equal(again, sa) {
		t.Fatal("the restored machine's snapshot differs")
	}
}

// D19: without a guest ever, the snapshot is what main writes (format 2
// to 5); V8: every format main wrote is still read.
func TestSnapshotFormatsWithoutGuestsAndGolden(t *testing.T) {
	m := twoMembers(t)
	if b, _ := m.Snapshot(); !bytes.Contains(b, []byte(`"format":2`)) || bytes.Contains(b, []byte("guests")) {
		t.Fatalf("no guests: %s", b)
	}
	for _, f := range []string{"format2.json", "format3.json", "format4.json", "format5.json"} {
		b, err := os.ReadFile(filepath.Join("testdata", "main-c0e1f60", f))
		if err != nil {
			t.Fatal(err)
		}
		n := New()
		if err := n.Restore(bytes.NewReader(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		// Main's snapshot, restored here and written again, is the same
		// bytes: the format did not change while there is no guest.
		if again, err := n.Snapshot(); err != nil || !bytes.Equal(again, b) {
			t.Errorf("%s written again differs:\n%s\n%s", f, b, again)
		}
	}
}
