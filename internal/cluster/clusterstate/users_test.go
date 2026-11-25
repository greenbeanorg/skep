package clusterstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	userA = "00000000-0000-4000-8000-00000000a001"
	userB = "00000000-0000-4000-8000-00000000a002"
	userC = "00000000-0000-4000-8000-00000000a003"
	hash1 = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaDE"
	hash2 = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaDI"
)

// userEntry encodes a user entry.
func userEntry(t *testing.T, typ, op string, data any) []byte {
	t.Helper()
	e, err := NewEntry(typ, op, "user:andy", nodeA, data)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ptr[T any](v T) *T { return &v }

// usersMachine is a machine with a cluster and an enabled admin (userA).
func usersMachine(t *testing.T) *Machine {
	t.Helper()
	m := New()
	m.Apply(1, initEntry(t, "op-init"))
	if r := m.Apply(2, userEntry(t, TypeUserCreate, "c-a", UserCreate{ID: userA, Username: "andy", PasswordHash: hash1, RoleID: RoleAdmin, At: 100})); r.Err != nil {
		t.Fatal(r.Err)
	}
	return m
}

// Every apply rule and refusal code of the user entries (D2–D7).
func TestUserEntriesApplyAndRefuse(t *testing.T) {
	cases := []struct {
		name, typ string
		data      any
		code      string // "" applies
	}{
		{"create viewer", TypeUserCreate, UserCreate{ID: userB, Username: "vera", DisplayName: "Vera", PasswordHash: hash1, RoleID: RoleViewer, At: 101}, ""},
		{"username taken", TypeUserCreate, UserCreate{ID: userC, Username: "andy", PasswordHash: hash1, RoleID: RoleViewer, At: 101}, CodeUsernameTaken},
		{"ID taken", TypeUserCreate, UserCreate{ID: userA, Username: "other", PasswordHash: hash1, RoleID: RoleViewer, At: 101}, CodeInvalid},
		{"unknown role", TypeUserCreate, UserCreate{ID: userC, Username: "carl", PasswordHash: hash1, RoleID: "writer", At: 101}, CodeUnknownRole},
		{"bad ID", TypeUserCreate, UserCreate{ID: "A0", Username: "carl", PasswordHash: hash1, RoleID: RoleViewer, At: 101}, CodeInvalid},
		{"upper-case ID", TypeUserCreate, UserCreate{ID: strings.ToUpper(userC), Username: "carl", PasswordHash: hash1, RoleID: RoleViewer, At: 101}, CodeInvalid},
		{"bad username", TypeUserCreate, UserCreate{ID: userC, Username: "Carl", PasswordHash: hash1, RoleID: RoleViewer, At: 101}, CodeInvalid},
		{"reserved username", TypeUserCreate, UserCreate{ID: userC, Username: "system", PasswordHash: hash1, RoleID: RoleViewer, At: 101}, CodeInvalid},
		{"control character", TypeUserCreate, UserCreate{ID: userC, Username: "carl", DisplayName: "a\x07b", PasswordHash: hash1, RoleID: RoleViewer, At: 101}, CodeInvalid},
		{"long display name", TypeUserCreate, UserCreate{ID: userC, Username: "carl", DisplayName: strings.Repeat("é", 129), PasswordHash: hash1, RoleID: RoleViewer, At: 101}, CodeInvalid},
		{"not argon2id", TypeUserCreate, UserCreate{ID: userC, Username: "carl", PasswordHash: "$2y$10$x", RoleID: RoleViewer, At: 101}, CodeInvalid},
		{"long hash", TypeUserCreate, UserCreate{ID: userC, Username: "carl", PasswordHash: hashPrefix + strings.Repeat("x", 503), RoleID: RoleViewer, At: 101}, CodeInvalid},
		{"no time", TypeUserCreate, UserCreate{ID: userC, Username: "carl", PasswordHash: hash1, RoleID: RoleViewer}, CodeInvalid},
		{"update unknown", TypeUserUpdate, UserUpdate{ID: userC, DisplayName: ptr("x"), At: 102}, CodeNotFound},
		{"update bad role", TypeUserUpdate, UserUpdate{ID: userB, RoleID: ptr("root"), At: 102}, CodeUnknownRole},
		{"update bad name", TypeUserUpdate, UserUpdate{ID: userB, DisplayName: ptr("\x1b[2J"), At: 102}, CodeInvalid},
		{"demote last admin", TypeUserUpdate, UserUpdate{ID: userA, RoleID: ptr(RoleViewer), At: 102}, CodeLastAdmin},
		{"disable last admin", TypeUserUpdate, UserUpdate{ID: userA, Disabled: ptr(true), At: 102}, CodeLastAdmin},
		{"delete last admin", TypeUserDelete, UserDelete{ID: userA}, CodeLastAdmin},
		{"delete unknown", TypeUserDelete, UserDelete{ID: userC}, CodeNotFound},
		{"password unknown", TypeUserPassword, UserPassword{ID: userC, PasswordHash: hash2, At: 103}, CodeNotFound},
		{"password not a hash", TypeUserPassword, UserPassword{ID: userB, PasswordHash: "plain", At: 103}, CodeInvalid},
		{"rehash changed", TypeUserRehash, UserRehash{ID: userB, OldHash: hash2, NewHash: hash2}, CodeHashChanged},
		{"rehash", TypeUserRehash, UserRehash{ID: userB, OldHash: hash1, NewHash: hash2}, ""},
		{"import into users", TypeUsersImport, UsersImport{Users: []ImportUser{{ID: userC, Username: "carl", PasswordHash: hash1, RoleID: RoleAdmin, CreatedAt: 1, UpdatedAt: 1}}, At: 104}, CodeUsersExist},
		{"delete viewer", TypeUserDelete, UserDelete{ID: userB}, ""},
	}
	m := usersMachine(t)
	for i, tt := range cases {
		idx := uint64(10 + i)
		r := m.Apply(idx, userEntry(t, tt.typ, fmt.Sprintf("op-%d", i), tt.data))
		if errors.Is(r.Err, ErrStalled) {
			t.Fatalf("%s: stalled: %v", tt.name, r.Err)
		}
		if got := RefusalCode(r.Err); got != tt.code || (tt.code == "" && r.Err != nil) {
			t.Errorf("%s: code %q (%v), want %q", tt.name, got, r.Err, tt.code)
		}
		if tt.code != "" && !errors.Is(r.Err, ErrRefused) {
			t.Errorf("%s: a refusal is not ErrRefused: %v", tt.name, r.Err)
		}
	}
	// Undecodable data stalls, as for every other type (D3).
	bad := []byte(`{"type":"user.create","version":1,"op_id":"x","actor":"a","origin":"n","data":{"id":5}}`)
	if r := m.Apply(100, bad); !errors.Is(r.Err, ErrStalled) {
		t.Fatalf("undecodable user data: %+v", r)
	}
}

// The import fills only an empty cluster, checks every user, and keeps the
// host's own times; at most MaxUsers (D7, D14).
func TestUsersImport(t *testing.T) {
	m := New()
	m.Apply(1, initEntry(t, "op-init"))
	imp := func(i uint64, users ...ImportUser) Result {
		return m.Apply(i, userEntry(t, TypeUsersImport, fmt.Sprintf("imp-%d", i), UsersImport{Users: users, At: 500}))
	}
	a := ImportUser{ID: userA, Username: "andy", PasswordHash: hash1, RoleID: RoleAdmin, PasswordChangedAt: 7, CreatedAt: 5, UpdatedAt: 6}
	b := ImportUser{ID: userB, Username: "vera", PasswordHash: hash2, RoleID: RoleViewer, Disabled: true, CreatedAt: 5, UpdatedAt: 6}
	if r := imp(2); RefusalCode(r.Err) != CodeInvalid {
		t.Fatalf("empty import: %v", r.Err)
	}
	dupName := b
	dupName.Username = "andy"
	if r := imp(3, a, dupName); RefusalCode(r.Err) != CodeUsernameTaken {
		t.Fatalf("duplicate name: %v", r.Err)
	}
	dupID := b
	dupID.ID = userA
	if r := imp(4, a, dupID); RefusalCode(r.Err) != CodeInvalid {
		t.Fatalf("duplicate ID: %v", r.Err)
	}
	noTime := b
	noTime.CreatedAt = 0
	if r := imp(5, a, noTime); RefusalCode(r.Err) != CodeInvalid {
		t.Fatalf("no time: %v", r.Err)
	}
	many := make([]ImportUser, MaxUsers+1)
	if r := imp(6, many...); RefusalCode(r.Err) != CodeTooManyUsers {
		t.Fatalf("too many: %v", r.Err)
	}
	if v := m.Users(); len(v.Users) != 0 || v.Index != 0 {
		t.Fatalf("a refused import changed the users: %+v", v)
	}
	if r := imp(7, a, b); r.Err != nil {
		t.Fatal(r.Err)
	}
	v := m.Users()
	if v.Index != 7 || len(v.Users) != 2 || v.Users[0].CreatedAt != 5 || v.Users[0].PasswordChangedAt != 7 || !v.Users[1].Disabled || v.Users[1].CredentialEpoch != 1 {
		t.Fatalf("imported: %+v", v)
	}
	if r := imp(8, ImportUser{ID: userC, Username: "carl", PasswordHash: hash1, RoleID: RoleAdmin, CreatedAt: 1, UpdatedAt: 1}); RefusalCode(r.Err) != CodeUsersExist {
		t.Fatalf("second import: %v", r.Err)
	}
}

// At most MaxUsers users (D7).
func TestTooManyUsers(t *testing.T) {
	m := usersMachine(t)
	m.mu.Lock()
	for i := 1; i < MaxUsers; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		m.st.users[id] = User{ID: id, Username: fmt.Sprintf("u%d", i), RoleID: RoleViewer}
	}
	m.mu.Unlock()
	r := m.Apply(5, userEntry(t, TypeUserCreate, "c-x", UserCreate{ID: userC, Username: "carl", PasswordHash: hash1, RoleID: RoleViewer, At: 9}))
	if RefusalCode(r.Err) != CodeTooManyUsers {
		t.Fatalf("user %d: %v", MaxUsers+1, r.Err)
	}
}

// Epochs (D6): a password change, a role change and a change of the
// disabled flag raise it by one; a display-name change and a rehash do not.
func TestCredentialEpochs(t *testing.T) {
	m := usersMachine(t)
	m.Apply(3, userEntry(t, TypeUserCreate, "c-b", UserCreate{ID: userB, Username: "vera", PasswordHash: hash1, RoleID: RoleViewer, At: 101}))
	epoch := func() uint64 { u, _ := m.UserByID(userB); return u.CredentialEpoch }
	steps := []struct {
		typ  string
		data any
		want uint64
	}{
		{TypeUserUpdate, UserUpdate{ID: userB, DisplayName: ptr("Vera V."), At: 102}, 1},
		{TypeUserRehash, UserRehash{ID: userB, OldHash: hash1, NewHash: hash2}, 1},
		{TypeUserPassword, UserPassword{ID: userB, PasswordHash: hash1, At: 103}, 2},
		{TypeUserUpdate, UserUpdate{ID: userB, RoleID: ptr(RoleOperator), At: 104}, 3},
		{TypeUserUpdate, UserUpdate{ID: userB, Disabled: ptr(true), At: 105}, 4},
		{TypeUserUpdate, UserUpdate{ID: userB, Disabled: ptr(true), RoleID: ptr(RoleOperator), At: 106}, 4},
		{TypeUserUpdate, UserUpdate{ID: userB, Disabled: ptr(false), RoleID: ptr(RoleAdmin), At: 107}, 6},
	}
	for i, s := range steps {
		if r := m.Apply(uint64(10+i), userEntry(t, s.typ, fmt.Sprintf("e-%d", i), s.data)); r.Err != nil {
			t.Fatalf("step %d: %v", i, r.Err)
		}
		if got := epoch(); got != s.want {
			t.Fatalf("step %d (%s): epoch %d, want %d", i, s.typ, got, s.want)
		}
	}
	u, _ := m.UserByID(userB)
	// An update that changed nothing keeps the update time.
	if u.UpdatedAt != 107 || u.PasswordChangedAt != 103 || u.CreatedAt != 101 || u.DisplayName != "Vera V." {
		t.Fatalf("times: %+v", u)
	}
	// Now there are two enabled admins: one may go.
	if r := m.Apply(30, userEntry(t, TypeUserDelete, "d-a", UserDelete{ID: userA})); r.Err != nil || m.UsersIndex() != 30 {
		t.Fatalf("delete one of two admins: %v", r.Err)
	}
}

// D8: an operation ID is forgotten once ForgetAfter indexes have passed,
// decided from the entry's index only: two machines that applied the same
// entries forget at the same entry, and a replay after that is applied
// (here refused by the state) instead of being a duplicate.
func TestOperationIDsAreForgottenAtTheSameIndex(t *testing.T) {
	run := func(m *Machine) []string {
		var out []string
		m.Apply(1, initEntry(t, "op-init"))
		create := userEntry(t, TypeUserCreate, "c-a", UserCreate{ID: userA, Username: "andy", PasswordHash: hash1, RoleID: RoleAdmin, At: 100})
		m.Apply(2, create)
		for _, i := range []uint64{50, ForgetAfter + 1, ForgetAfter + 2} {
			r := m.Apply(i, create)
			out = append(out, fmt.Sprintf("%d dup=%v code=%s", i, r.Duplicate, RefusalCode(r.Err)))
		}
		return out
	}
	a, b := run(New()), run(New())
	// Applied at index 2: remembered at 2+ForgetAfter-1, forgotten at
	// 2+ForgetAfter.
	want := []string{"50 dup=true code=", fmt.Sprintf("%d dup=true code=", ForgetAfter+1), fmt.Sprintf("%d dup=false code=%s", ForgetAfter+2, CodeInvalid)}
	if strings.Join(a, ";") != strings.Join(want, ";") || strings.Join(a, ";") != strings.Join(b, ";") {
		t.Fatalf("machine a: %v\nmachine b: %v\nwant      %v", a, b, want)
	}
	// The forgotten state is in the snapshot, format 5.
	m := New()
	run(m)
	raw, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil || snap.Format != 5 || !snap.Forgot || len(snap.Ops) != 0 {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
}

// D9: format 5 only once the state holds users or forgot an operation ID;
// it carries the users, the index and the epochs.
func TestSnapshotFormat5(t *testing.T) {
	m := New()
	m.Apply(1, initEntry(t, "op-init"))
	raw, _ := m.Snapshot()
	var snap snapshot
	if json.Unmarshal(raw, &snap); snap.Format != 2 || snap.Users != nil {
		t.Fatalf("no users: format %d", snap.Format)
	}
	m.Apply(2, userEntry(t, TypeUserCreate, "c-a", UserCreate{ID: userA, Username: "andy", PasswordHash: hash1, RoleID: RoleAdmin, At: 100}))
	m.Apply(3, userEntry(t, TypeUserPassword, "p-a", UserPassword{ID: userA, PasswordHash: hash2, At: 101}))
	raw, _ = m.Snapshot()
	if json.Unmarshal(raw, &snap); snap.Format != 5 || len(snap.Users) != 1 || snap.UsersIndex != 3 {
		t.Fatalf("users: %+v", snap)
	}
	n := New()
	changed := false
	n.SetOnUsers(func() { changed = true })
	if err := n.Restore(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if u, ok := n.UserByID(userA); !ok || u.CredentialEpoch != 2 || u.PasswordHash != hash2 || n.UsersIndex() != 3 || !changed {
		t.Fatalf("restored: %+v %v", u, changed)
	}
	if r := n.Apply(4, userEntry(t, TypeUserPassword, "p-a", UserPassword{ID: userA, PasswordHash: hash1, At: 102})); !r.Duplicate || r.OpIndex != 3 {
		t.Fatalf("the restored machine applied an old operation again: %+v", r)
	}
}

type captureAudit struct{ entries []AuditEntry }

func (c *captureAudit) AuditEntry(a AuditEntry) error { c.entries = append(c.entries, a); return nil }
func (c *captureAudit) AuditSnapshot(uint64, uint64, []Member) error {
	return nil
}

// D10: user entries are audited with the user's ID, name, role and
// disabled flag, the outcome and the refusal code; an import with its count
// and usernames; never a hash.
func TestUserAuditRecords(t *testing.T) {
	m := New()
	c := &captureAudit{}
	m.SetAuditWriter(c)
	m.Apply(1, initEntry(t, "op-init"))
	m.Apply(2, userEntry(t, TypeUsersImport, "imp", UsersImport{Users: []ImportUser{
		{ID: userA, Username: "andy", PasswordHash: hash1, RoleID: RoleAdmin, CreatedAt: 1, UpdatedAt: 1},
		{ID: userB, Username: "vera", PasswordHash: hash1, RoleID: RoleViewer, CreatedAt: 1, UpdatedAt: 1}}, At: 9}))
	m.Apply(3, userEntry(t, TypeUserUpdate, "u-b", UserUpdate{ID: userB, Disabled: ptr(true), At: 10}))
	m.Apply(4, userEntry(t, TypeUserDelete, "d-a", UserDelete{ID: userA}))
	m.Apply(5, userEntry(t, TypeUserRehash, "r-b", UserRehash{ID: userB, OldHash: hash2, NewHash: hash2}))
	m.Apply(6, userEntry(t, TypeUserPassword, "p-b", UserPassword{ID: userB, PasswordHash: hash2, At: 11}))
	got := c.entries[1:]
	if len(got) != 5 {
		t.Fatalf("%d records", len(got))
	}
	if g := got[0]; g.Count != 2 || strings.Join(g.Usernames, ",") != "andy,vera" || g.Outcome != "applied" || g.At != 9 {
		t.Errorf("import: %+v", g)
	}
	if g := got[1]; g.UserID != userB || g.Username != "vera" || g.Role != RoleViewer || !g.UserDisabled || g.Outcome != "applied" {
		t.Errorf("update: %+v", g)
	}
	if g := got[2]; g.Outcome != "refused" || g.Code != CodeLastAdmin || g.Username != "andy" {
		t.Errorf("refused delete: %+v", g)
	}
	if g := got[3]; g.Code != CodeHashChanged || g.UserID != userB {
		t.Errorf("refused rehash: %+v", g)
	}
	for _, g := range got {
		b, _ := json.Marshal(g)
		if bytes.Contains(b, []byte("$argon2id$")) {
			t.Errorf("an audit record carries a hash: %s", b)
		}
	}
}
