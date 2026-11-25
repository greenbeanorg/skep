package clusterstate

import (
	"cmp"
	"slices"
)

// ForgetAfter is how many log indexes an applied operation ID is
// remembered for (D8 of step 8c-1): it is forgotten at the first entry
// whose index is at least its own plus ForgetAfter, decided from the
// entry's index alone, so every node forgets at the same entry.
const ForgetAfter = 100000

// opAt is an operation ID and the index that applied it.
type opAt struct {
	index uint64
	op    string
}

func orderOf(ops map[string]uint64) []opAt {
	out := make([]opAt, 0, len(ops))
	for op, i := range ops {
		out = append(out, opAt{index: i, op: op})
	}
	slices.SortFunc(out, func(a, b opAt) int {
		if c := cmp.Compare(a.index, b.index); c != 0 {
			return c
		}
		return cmp.Compare(a.op, b.op)
	})
	return out
}

// forget drops the operation IDs applied at or before index-ForgetAfter.
// It runs before the entry at index is checked for a duplicate.
func (m *Machine) forget(index uint64) {
	if index < ForgetAfter {
		return
	}
	limit := index - ForgetAfter
	n := 0
	for n < len(m.order) && m.order[n].index <= limit {
		delete(m.ops, m.order[n].op)
		n++
	}
	if n > 0 {
		m.order = slices.Delete(m.order, 0, n)
		m.forgot = true
	}
}

// SetOnUsers installs a function called, without the machine's lock,
// after every entry that changed the users and after a restore: the
// users-read long poll waits on it.
func (m *Machine) SetOnUsers(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onUsers = f
}

// UsersView is the replicated users as one read sees them.
type UsersView struct {
	ClusterID string
	// Index is the index of the last entry that changed the users.
	Index uint64
	// Users is sorted by ID.
	Users []User
}

// Users returns the users.
func (m *Machine) Users() UsersView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return UsersView{ClusterID: m.st.clusterID, Index: m.st.usersIndex, Users: m.st.sortedUsers()}
}

// UsersIndex returns the index of the last entry that changed the users.
func (m *Machine) UsersIndex() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.st.usersIndex
}

// UserByID returns one user.
func (m *Machine) UserByID(id string) (User, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.st.users[id]
	return u, ok
}
