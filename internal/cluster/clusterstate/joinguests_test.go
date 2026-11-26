package clusterstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	jg1 = "c0000000-0000-4000-8000-000000000001"
	jg2 = "c0000000-0000-4000-8000-000000000002"
	jg3 = "c0000000-0000-4000-8000-000000000003"
)

func encodeV(t *testing.T, typ string, version int, op string, data any) []byte {
	t.Helper()
	e, err := NewEntryVersion(typ, version, op, ActorSystem, nodeA, data)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func joinV2(t *testing.T, op, id, p string, at int64, guests ...JoinGuest) []byte {
	return encodeV(t, TypeMemberJoin, 2, op, MemberJoinV2{NodeID: id, Pin: p, At: at, Guests: guests})
}

func removingV2(t *testing.T, op, id string, guests, reservations int) []byte {
	return encodeV(t, TypeMemberRemoving, 2, op, MemberRemovingV2{NodeID: id, Guests: guests, Reservations: reservations})
}

// inventoryAt is smith (A) and langrishe (B) in state member and star (C)
// pending, with guests web1 (container, B), dup (container, A then B: the
// one added first is A's) and a reservation of the VM name db1 (A). The
// next index is 11.
func inventoryAt(t *testing.T) *Machine {
	t.Helper()
	m := twoMembers(t)
	for i, raw := range [][]byte{
		add(t, "a-1", guest1, KindContainer, "web1", nodeB),
		add(t, "a-2", guest2, KindContainer, "dup", nodeA),
		add(t, "a-3", guest3, KindContainer, "dup", nodeB),
		reserve(t, "r-1", KindVM, "db1", "k-1", nodeA),
		pendingEntry(t, "p-c", nodeC, "star", "10.0.0.3:8444", pinC, 2000000000),
		{}, // index 10 is not used
	} {
		if len(raw) == 0 {
			continue
		}
		wantApplied(t, m.Apply(uint64(i+5), raw), false)
	}
	return m
}

// reordered is a machine in m's state whose maps were filled in another
// order: restored from m's snapshot with every list reversed (V3).
func reordered(t *testing.T, m *Machine) *Machine {
	t.Helper()
	b, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(s.Members)
	slices.Reverse(s.Pendings)
	slices.Reverse(s.Guests)
	slices.Reverse(s.Reservations)
	rb, _ := json.Marshal(s)
	n := New()
	if err := n.Restore(bytes.NewReader(rb)); err != nil {
		t.Fatal(err)
	}
	if again, _ := n.Snapshot(); !bytes.Equal(again, b) {
		t.Fatal("the reordered machine is not in the same state")
	}
	return n
}

// onBoth applies raw at index to a and b and checks that both give the
// same result, text and code, and end in the same state.
func onBoth(t *testing.T, a, b *Machine, index uint64, raw []byte) Result {
	t.Helper()
	ra, rb := a.Apply(index, raw), b.Apply(index, raw)
	if fmt.Sprint(ra.Err) != fmt.Sprint(rb.Err) || RefusalCode(ra.Err) != RefusalCode(rb.Err) {
		t.Fatalf("the two machines differ: %v / %v", ra.Err, rb.Err)
	}
	sa, _ := a.Snapshot()
	sb, _ := b.Snapshot()
	if !bytes.Equal(sa, sb) {
		t.Fatalf("the two machines' states differ:\n%s\n%s", sa, sb)
	}
	return ra
}

// D3, D27: every refusal of member.join version 2, on two machines whose
// maps were filled in different orders, with the same result and text;
// then the accepted join adds the guests.
func TestJoinV2RefusalsAreTheSameOnTwoMachines(t *testing.T) {
	ok := JoinGuest{ID: jg1, Kind: KindVM, Name: "ok1"}
	many := make([]JoinGuest, MaxJoinGuests+1)
	for i := range many {
		many[i] = JoinGuest{ID: fmt.Sprintf("c0000000-0000-4000-8000-%012d", i+10), Kind: KindContainer, Name: fmt.Sprintf("j%d", i)}
	}
	cases := []struct {
		name   string
		raw    func(t *testing.T) []byte
		code   string
		reason string
	}{
		{"no pending entry (version 1's check first)", func(t *testing.T) []byte { return joinV2(t, "j", nodeA, pinA, 1900000000, ok) }, "", "there is no pending entry"},
		{"expired (version 1's check)", func(t *testing.T) []byte { return joinV2(t, "j", nodeC, pinC, 2000000001, ok) }, "", "expired"},
		{"empty list", func(t *testing.T) []byte { return joinV2(t, "j", nodeC, pinC, 1900000000) }, CodeInvalid, "1 to 1000 guests, not 0"},
		{"over 1000", func(t *testing.T) []byte { return joinV2(t, "j", nodeC, pinC, 1900000000, many...) }, CodeInvalid, "not 1001"},
		{"upper-case ID", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, JoinGuest{ID: strings.ToUpper(jg1), Kind: KindVM, Name: "x"})
		}, CodeInvalid, "not a lower-case UUID"},
		{"kind", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, JoinGuest{ID: jg1, Kind: "jail", Name: "x"})
		}, CodeInvalid, `kind "jail"`},
		{"name", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, JoinGuest{ID: jg1, Kind: KindVM, Name: "a\x7f"})
		}, CodeInvalid, "name the cluster does not accept"},
		{"ID twice", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, ok, JoinGuest{ID: jg1, Kind: KindContainer, Name: "y"})
		}, CodeInvalid, "in the list twice"},
		{"name twice", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, ok, JoinGuest{ID: jg2, Kind: KindVM, Name: "ok1"})
		}, CodeInvalid, `the vm name "ok1" is in the list twice`},
		{"ID in the inventory", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, ok, JoinGuest{ID: guest1, Kind: KindVM, Name: "z"})
		}, CodeInvalid, "the guest ID " + guest1 + " is already in the cluster's inventory"},
		{"a guest's name: the first in list order", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, ok, JoinGuest{ID: jg2, Kind: KindContainer, Name: "web1"},
				JoinGuest{ID: jg3, Kind: KindVM, Name: "db1"})
		}, CodeNameTaken, `the container name "web1" belongs to a guest of node langrishe`},
		{"a reservation's name", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, JoinGuest{ID: jg3, Kind: KindVM, Name: "db1"}, ok)
		}, CodeNameTaken, `the vm name "db1" is reserved by node smith`},
		{"a flagged name: the guest added first", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, JoinGuest{ID: jg3, Kind: KindContainer, Name: "dup"})
		}, CodeNameTaken, `the container name "dup" belongs to a guest of node smith`},
		{"same name, other kind is free", func(t *testing.T) []byte {
			return joinV2(t, "j", nodeC, pinC, 1900000000, JoinGuest{ID: jg3, Kind: KindVM, Name: "web1"})
		}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := inventoryAt(t)
			b := reordered(t, a)
			r := onBoth(t, a, b, 11, c.raw(t))
			if c.reason == "" {
				wantApplied(t, r, false)
				return
			}
			if r.Err == nil || !strings.Contains(r.Err.Error(), c.reason) || RefusalCode(r.Err) != c.code {
				t.Fatalf("got %v (code %q), want %q (code %q)", r.Err, RefusalCode(r.Err), c.reason, c.code)
			}
			if g := a.Guests(); len(g.Guests) != 3 || g.Index != 8 {
				t.Fatalf("a refusal changed the inventory: %+v", g)
			}
			if _, ok := a.Member(nodeC); ok {
				t.Fatal("a refused join created the member entry")
			}
		})
	}
}

// D3f: the guests, the reservations and the list stay within MaxGuests.
func TestJoinV2TooManyGuests(t *testing.T) {
	m := inventoryAt(t) // 3 guests and 1 reservation
	b, _ := m.Snapshot()
	var s snapshot
	_ = json.Unmarshal(b, &s)
	for i := 0; len(s.Guests)+len(s.Reservations) < MaxGuests-1; i++ {
		s.Guests = append(s.Guests, Guest{ID: fmt.Sprintf("e0000000-0000-4000-8000-%012d", i), Kind: KindVM, Name: fmt.Sprintf("v%d", i), Owner: nodeA, AddedIndex: 9, At: 1})
	}
	rb, _ := json.Marshal(s)
	full := New()
	if err := full.Restore(bytes.NewReader(rb)); err != nil {
		t.Fatal(err)
	}
	two := []JoinGuest{{ID: jg1, Kind: KindContainer, Name: "n1"}, {ID: jg2, Kind: KindContainer, Name: "n2"}}
	wantRefused(t, full.Apply(11, joinV2(t, "j-1", nodeC, pinC, 1900000000, two...)), CodeTooManyGuests)
	wantApplied(t, full.Apply(12, joinV2(t, "j-2", nodeC, pinC, 1900000000, two[0])), false)
	if n := len(full.Guests().Guests) + len(full.Guests().Reservations); n != MaxGuests {
		t.Fatalf("%d guests and reservations", n)
	}
}

// D3: the accepted join adds every guest with the member entry: owner the
// joining node, no key, no task ID, AddedIndex the entry's index, At the
// entry's time; the guests index is the entry's index. Its audit record
// names the guests (D7).
func TestJoinV2AddsTheGuests(t *testing.T) {
	m := inventoryAt(t)
	fa := &fakeAudit{}
	m.SetAuditWriter(fa)
	list := []JoinGuest{{ID: jg2, Kind: KindContainer, Name: "j2"}, {ID: jg1, Kind: KindVM, Name: "v1"}}
	r := m.Apply(11, joinV2(t, "j-1", nodeC, pinC, 1900000123, list...))
	wantApplied(t, r, false)
	if mem, ok := m.Member(nodeC); !ok || mem.State != StateJoining || mem.JoinedIndex != 11 {
		t.Fatalf("member: %+v", mem)
	}
	inv := m.Guests()
	if inv.Index != 11 || len(inv.Guests) != 5 {
		t.Fatalf("inventory: %+v", inv)
	}
	for _, g := range inv.Guests {
		if g.ID != jg1 && g.ID != jg2 {
			continue
		}
		if g.Owner != nodeC || g.Key != "" || g.TaskID != "" || g.AddedIndex != 11 || g.At != 1900000123 || g.Flagged {
			t.Fatalf("guest %+v", g)
		}
	}
	a := fa.entries[len(fa.entries)-1]
	if a.Type != TypeMemberJoin || a.Version != 2 || a.NodeID != nodeC || !slices.Equal(a.Guests, list) || a.GuestCount != 0 {
		t.Fatalf("audit: %+v", a)
	}
	// A repeat of the operation is a duplicate.
	if r := m.Apply(12, joinV2(t, "j-1", nodeC, pinC, 1900000123, list...)); !r.Duplicate {
		t.Fatalf("repeat: %+v", r)
	}
}

// joinedWithGuests: inventoryAt, star joined with two guests (index 11),
// langrishe owns web1, the second dup and a new reservation (index 12).
func joinedWithGuests(t *testing.T) *Machine {
	t.Helper()
	m := inventoryAt(t)
	wantApplied(t, m.Apply(11, joinV2(t, "j-1", nodeC, pinC, 1900000000,
		JoinGuest{ID: jg1, Kind: KindVM, Name: "s1"}, JoinGuest{ID: jg2, Kind: KindContainer, Name: "s2"})), false)
	wantApplied(t, m.Apply(12, reserve(t, "r-2", KindContainer, "web2", "k-2", nodeB)), false)
	return m
}

// D5, D27: member.removing version 2 and its refusals, on two machines with
// the same result; version 1 as before.
func TestRemovingV2(t *testing.T) {
	cases := []struct {
		name   string
		raw    func(t *testing.T) []byte
		code   string
		reason string
	}{
		{"counts 0", func(t *testing.T) []byte { return removingV2(t, "rm", nodeB, 0, 0) }, CodeInvalid, "counts above 0"},
		{"a count below 0", func(t *testing.T) []byte { return removingV2(t, "rm", nodeB, 3, -1) }, CodeInvalid, "counts above 0"},
		{"not a member", func(t *testing.T) []byte { return removingV2(t, "rm", jg3, 1, 0) }, "", "is not a member"},
		{"wrong counts", func(t *testing.T) []byte { return removingV2(t, "rm", nodeB, 2, 0) }, "",
			"member langrishe owns 2 guests and 1 name reservations, not the 2 and 0 this removal forgets"},
		{"version 1 of a member that owns guests", func(t *testing.T) []byte { return removingEntry(t, "rm", nodeB) }, "",
			"owns 2 guests and 1 name reservations in the inventory; it cannot be removed while it owns any (step 8d-2 brings --forget-guests)"},
		{"version 1 with forget_guests", func(t *testing.T) []byte { return removingEntry(t, "rm", nodeB, guest1) }, "", "cannot be named in this version"},
		{"another member is joining (D7a)", func(t *testing.T) []byte { return removingV2(t, "rm", nodeB, 2, 1) }, "", "star is joining"},
		{"the joining member that brought guests", func(t *testing.T) []byte { return removingV2(t, "rm", nodeC, 2, 0) }, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := joinedWithGuests(t)
			b := reordered(t, a)
			r := onBoth(t, a, b, 13, c.raw(t))
			if c.reason == "" {
				wantApplied(t, r, false)
				if stateIn(a, nodeC) != StateRemoving {
					t.Fatal("not removing")
				}
				return
			}
			if r.Err == nil || !strings.Contains(r.Err.Error(), c.reason) || RefusalCode(r.Err) != c.code {
				t.Fatalf("got %v (code %q), want %q (code %q)", r.Err, RefusalCode(r.Err), c.reason, c.code)
			}
		})
	}
}

// D5, D6, D7: a member that owns guests and reservations is removed with
// version 2, member.removed removes what it owns (nothing of the others),
// and the records carry the counts.
func TestRemovedForgetsGuestsAndReservations(t *testing.T) {
	a := joinedWithGuests(t)
	wantApplied(t, a.Apply(13, promotedEntry(t, "pr-c", nodeC)), false)
	b := reordered(t, a)
	fa := &fakeAudit{}
	a.SetAuditWriter(fa)
	onBoth(t, a, b, 14, removingV2(t, "rm-b", nodeB, 2, 1))
	if r := onBoth(t, a, b, 15, removingV2(t, "rm-b2", nodeB, 2, 1)); r.Err == nil || !strings.Contains(r.Err.Error(), "already being removed") {
		t.Fatalf("again: %v", r.Err)
	}
	r := onBoth(t, a, b, 16, removedEntry(t, "rmd-b", nodeB))
	wantApplied(t, r, false)
	inv := a.Guests()
	if inv.Index != 16 || len(inv.Reservations) != 1 || inv.Reservations[0].Owner != nodeA {
		t.Fatalf("reservations: %+v", inv)
	}
	var owners []string
	for _, g := range inv.Guests {
		owners = append(owners, g.Owner)
	}
	if len(inv.Guests) != 3 || slices.Contains(owners, nodeB) {
		t.Fatalf("guests: %+v", inv.Guests)
	}
	rec := map[string]AuditEntry{}
	for _, e := range fa.entries {
		rec[e.OpID] = e
	}
	if x := rec["rm-b"]; x.Version != 2 || x.GuestCount != 2 || x.ReservationCount != 1 || x.Name != "langrishe" || x.Guests != nil {
		t.Fatalf("removing record: %+v", x)
	}
	if x := rec["rm-b2"]; x.Outcome != "refused" || x.GuestCount != 2 {
		t.Fatalf("refused removing record: %+v", x)
	}
	if x := rec["rmd-b"]; x.GuestCount != 2 || x.ReservationCount != 1 || x.Name != "langrishe" {
		t.Fatalf("removed record: %+v", x)
	}
	// A member.removed that removes nothing carries no counts.
	m := threeMembers(t)
	fm := &fakeAudit{}
	m.SetAuditWriter(fm)
	wantApplied(t, m.Apply(11, removingEntry(t, "rm-c", nodeC)), false)
	wantApplied(t, m.Apply(12, removedEntry(t, "rmd-c", nodeC)), false)
	if x := fm.entries[1]; x.GuestCount != 0 || x.ReservationCount != 0 || m.Guests().Index != 0 {
		t.Fatalf("plain removal: %+v, index %d", x, m.Guests().Index)
	}
}

// V4 (D8): a snapshot taken between a version 2 member.removing and its
// member.removed restores to a state in which member.removed removes the
// same guests.
func TestSnapshotBetweenRemovingAndRemoved(t *testing.T) {
	a := joinedWithGuests(t)
	wantApplied(t, a.Apply(13, removingV2(t, "rm-c", nodeC, 2, 0)), false)
	snap, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	if err := b.Restore(bytes.NewReader(snap)); err != nil {
		t.Fatal(err)
	}
	onBoth(t, a, b, 14, removedEntry(t, "rmd-c", nodeC))
	for _, g := range b.Guests().Guests {
		if g.Owner == nodeC {
			t.Fatalf("a guest of the removed node is left: %+v", g)
		}
	}
	if b.Guests().Index != 14 || len(b.Guests().Guests) != 3 {
		t.Fatalf("restored machine: %+v", b.Guests())
	}
}

// D18: a joining owner can write no guest entry that adds anything; once
// promoted, its guest.remove of a guest it brought applies.
func TestJoiningOwnerGuestWrites(t *testing.T) {
	m := joinedWithGuests(t)
	wantRefused(t, m.Apply(13, reserve(t, "r-c", KindVM, "x1", "k-c", nodeC)), CodeOwnerNotMember)
	wantRefused(t, m.Apply(14, add(t, "a-c", jg3, KindVM, "x1", nodeC)), CodeOwnerNotMember)
	wantApplied(t, m.Apply(15, promotedEntry(t, "pr-c", nodeC)), false)
	wantApplied(t, m.Apply(16, encode(t, TypeGuestRemove, "x-c", GuestRemove{ID: jg1, Owner: nodeC})), false)
	for _, g := range m.Guests().Guests {
		if g.ID == jg1 {
			t.Fatal("the guest was not removed")
		}
	}
}

// D1: NewEntry writes version 1; NewEntryVersion writes the versions this
// build supports and no other.
func TestNewEntryVersion(t *testing.T) {
	if e, _ := NewEntry(TypeMemberJoin, "o", ActorSystem, nodeA, MemberJoin{}); e.Version != 1 {
		t.Fatalf("NewEntry wrote version %d", e.Version)
	}
	if e, err := NewEntryVersion(TypeMemberRemoving, 2, "o", ActorSystem, nodeA, MemberRemovingV2{}); err != nil || e.Version != 2 {
		t.Fatalf("version 2: %+v %v", e, err)
	}
	for _, v := range []int{0, 3} {
		if _, err := NewEntryVersion(TypeMemberJoin, v, "o", ActorSystem, nodeA, MemberJoin{}); err == nil {
			t.Fatalf("version %d was built", v)
		}
	}
	if _, err := NewEntryVersion(TypeMemberPromoted, 2, "o", ActorSystem, nodeA, MemberPromoted{}); err == nil {
		t.Fatal("member.promoted version 2 was built")
	}
}

// D4: the two limits.
func TestJoinGuestLimits(t *testing.T) {
	long := make([]JoinGuest, MaxJoinGuests)
	for i := range long {
		long[i] = JoinGuest{ID: fmt.Sprintf("c0000000-0000-4000-8000-%012d", i), Kind: KindVM, Name: strings.Repeat("n", 200)}
	}
	if err := CheckJoinGuestLimits(long); err == nil || !strings.Contains(err.Error(), "bytes as JSON") {
		t.Fatalf("1000 long names: %v", err)
	}
	if err := CheckJoinGuestLimits(append(long, long[0])); err == nil || !strings.Contains(err.Error(), "at most 1000") {
		t.Fatalf("1001: %v", err)
	}
	for i := range long {
		long[i].Name = fmt.Sprintf("g%d", i)
	}
	if err := CheckJoinGuestLimits(long); err != nil {
		t.Fatalf("1000 short names: %v", err)
	}
}
