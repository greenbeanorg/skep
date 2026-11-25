package clusterstate

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// User entry types (step 8c-1, D2). Users are replicated; roles are not
// (D1): a user carries the ID of one of the three built-in roles.
const (
	TypeUserCreate   = "user.create"
	TypeUserUpdate   = "user.update"
	TypeUserPassword = "user.password"
	// TypeUserRehash replaces a hash with a stronger one of the same
	// password, only while the stored hash is still the old one.
	TypeUserRehash = "user.rehash"
	TypeUserDelete = "user.delete"
	// TypeUsersImport fills a cluster without users with a 0.1 host's
	// users (D14).
	TypeUsersImport = "users.import"
)

// UserTypes are the user entry types, the only ones users-write accepts.
var UserTypes = []string{TypeUserCreate, TypeUserUpdate, TypeUserPassword, TypeUserRehash, TypeUserDelete, TypeUsersImport}

// IsUserType reports whether typ is a user entry type.
func IsUserType(typ string) bool { return slices.Contains(UserTypes, typ) }

// The built-in roles a user may have (D1).
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Limits (D7).
const (
	// MaxUsers is the most users the cluster holds.
	MaxUsers = 2000
	// MaxImportBytes bounds an encoded users.import entry.
	MaxImportBytes = 192 << 10
	// MaxHashLength bounds a password hash.
	MaxHashLength = 512
	// MaxDisplayName bounds a display name, in characters.
	MaxDisplayName = 128
	// hashPrefix starts every accepted hash.
	hashPrefix = "$argon2id$"
	// reservedUsername is the audit's actor for what Skep does itself.
	reservedUsername = "system"
)

// Refusal codes of user entries (D5).
const (
	CodeUsernameTaken = "username_taken"
	CodeLastAdmin     = "last_admin"
	CodeUnknownRole   = "unknown_role"
	CodeNotFound      = "not_found"
	CodeUsersExist    = "users_exist"
	CodeHashChanged   = "hash_changed"
	CodeTooManyUsers  = "too_many_users"
	CodeInvalid       = "invalid"
)

// UsernameRE is the username rule. It is internal/auth's; a test keeps the
// two equal (clusterstate does not import internal/auth).
var UsernameRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)

// RefusedError is a refusal with a code.
type RefusedError struct {
	Code string
	Text string
}

func (e *RefusedError) Error() string { return e.Text }

func refuse(code, format string, args ...any) error {
	return &RefusedError{Code: code, Text: fmt.Sprintf(format, args...)}
}

// RefusalCode returns the code of a refused user entry, or "".
func RefusalCode(err error) string {
	var re *RefusedError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

// User is one replicated user. Times are Unix seconds.
type User struct {
	ID                string `json:"id"`
	Username          string `json:"username"`
	DisplayName       string `json:"display_name"`
	PasswordHash      string `json:"password_hash"`
	RoleID            string `json:"role_id"`
	Disabled          bool   `json:"disabled"`
	CredentialEpoch   uint64 `json:"credential_epoch"`
	PasswordChangedAt int64  `json:"password_changed_at"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

// UserCreate is the data of user.create, version 1. At is the proposing
// leader's clock.
type UserCreate struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name"`
	PasswordHash string `json:"password_hash"`
	RoleID       string `json:"role_id"`
	At           int64  `json:"at"`
}

// UserUpdate is the data of user.update, version 1. An absent field is
// unchanged.
type UserUpdate struct {
	ID          string  `json:"id"`
	DisplayName *string `json:"display_name,omitempty"`
	RoleID      *string `json:"role_id,omitempty"`
	Disabled    *bool   `json:"disabled,omitempty"`
	At          int64   `json:"at"`
}

// UserPassword is the data of user.password, version 1.
type UserPassword struct {
	ID           string `json:"id"`
	PasswordHash string `json:"password_hash"`
	At           int64  `json:"at"`
}

// UserRehash is the data of user.rehash, version 1.
type UserRehash struct {
	ID      string `json:"id"`
	OldHash string `json:"old_hash"`
	NewHash string `json:"new_hash"`
}

// UserDelete is the data of user.delete, version 1.
type UserDelete struct {
	ID string `json:"id"`
}

// ImportUser is one user of users.import, with the host's own times.
type ImportUser struct {
	ID                string `json:"id"`
	Username          string `json:"username"`
	DisplayName       string `json:"display_name"`
	PasswordHash      string `json:"password_hash"`
	RoleID            string `json:"role_id"`
	Disabled          bool   `json:"disabled"`
	PasswordChangedAt int64  `json:"password_changed_at"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

// UsersImport is the data of users.import, version 1.
type UsersImport struct {
	Users []ImportUser `json:"users"`
	At    int64        `json:"at"`
}

func checkDisplayName(name string) error {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxDisplayName {
		return refuse(CodeInvalid, "the display name must be valid text of at most %d characters", MaxDisplayName)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f { // internal/auth's rule (F3 of the 8c-1 review)
			return refuse(CodeInvalid, "the display name contains a control character")
		}
	}
	return nil
}

func checkRole(role string) error {
	switch role {
	case RoleAdmin, RoleOperator, RoleViewer:
		return nil
	}
	return refuse(CodeUnknownRole, "the role %q is not one of admin, operator and viewer", role)
}

func checkHash(h string) error {
	if !strings.HasPrefix(h, hashPrefix) || len(h) > MaxHashLength {
		// The value is never repeated: it is a password hash.
		return refuse(CodeInvalid, "the password hash is not an argon2id hash of at most %d bytes", MaxHashLength)
	}
	return nil
}

func checkUserValues(id, username, display, role, hash string) error {
	switch {
	case !ValidNodeID(id):
		return refuse(CodeInvalid, "the user ID is not a lower-case UUID")
	case !UsernameRE.MatchString(username) || username == reservedUsername:
		return refuse(CodeInvalid, "the username is not valid")
	}
	if err := checkDisplayName(display); err != nil {
		return err
	}
	if err := checkRole(role); err != nil {
		return err
	}
	return checkHash(hash)
}

func checkTime(at int64) error {
	if at <= 0 {
		return refuse(CodeInvalid, "the entry has no time")
	}
	return nil
}

func enabledAdmins(users map[string]User) int {
	n := 0
	for _, u := range users {
		if u.RoleID == RoleAdmin && !u.Disabled {
			n++
		}
	}
	return n
}

func (s state) userByName(name string) (User, bool) {
	for _, u := range s.users {
		if u.Username == name {
			return u, true
		}
	}
	return User{}, false
}

// sortedUsers returns the users sorted by ID.
func (s state) sortedUsers() []User {
	out := make([]User, 0, len(s.users))
	for _, id := range slices.Sorted(maps.Keys(s.users)) {
		out = append(out, s.users[id])
	}
	return out
}

// applyUser applies a user entry (D2–D7). The caller has checked that the
// cluster exists. It changes s only when it returns nil.
func (s *state) applyUser(e Entry, index uint64) (change, error) {
	before := enabledAdmins(s.users)
	next := maps.Clone(s.users)
	if next == nil {
		next = map[string]User{}
	}
	found := func(id string) (User, error) {
		u, ok := next[id]
		if !ok {
			return User{}, refuse(CodeNotFound, "there is no user %s", id)
		}
		return u, nil
	}
	switch e.Type {
	case TypeUserCreate:
		var d UserCreate
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if err := checkUserValues(d.ID, d.Username, d.DisplayName, d.RoleID, d.PasswordHash); err != nil {
			return change{}, err
		}
		if err := checkTime(d.At); err != nil {
			return change{}, err
		}
		if _, ok := next[d.ID]; ok {
			return change{}, refuse(CodeInvalid, "the user ID %s is taken", d.ID)
		}
		if _, ok := s.userByName(d.Username); ok {
			return change{}, refuse(CodeUsernameTaken, "a user named %s exists", d.Username)
		}
		if len(next) >= MaxUsers {
			return change{}, refuse(CodeTooManyUsers, "the cluster holds %d users, the most it may", MaxUsers)
		}
		next[d.ID] = User{ID: d.ID, Username: d.Username, DisplayName: d.DisplayName, PasswordHash: d.PasswordHash, RoleID: d.RoleID,
			CredentialEpoch: 1, PasswordChangedAt: d.At, CreatedAt: d.At, UpdatedAt: d.At}

	case TypeUserUpdate:
		var d UserUpdate
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if err := checkTime(d.At); err != nil {
			return change{}, err
		}
		u, err := found(d.ID)
		if err != nil {
			return change{}, err
		}
		changed := false
		if d.DisplayName != nil && *d.DisplayName != u.DisplayName {
			if err := checkDisplayName(*d.DisplayName); err != nil {
				return change{}, err
			}
			u.DisplayName, changed = *d.DisplayName, true
		}
		if d.RoleID != nil && *d.RoleID != u.RoleID {
			if err := checkRole(*d.RoleID); err != nil {
				return change{}, err
			}
			u.RoleID, changed = *d.RoleID, true
			u.CredentialEpoch++
		}
		if d.Disabled != nil && *d.Disabled != u.Disabled {
			u.Disabled, changed = *d.Disabled, true
			u.CredentialEpoch++
		}
		if changed {
			u.UpdatedAt = d.At
		}
		next[u.ID] = u

	case TypeUserPassword:
		var d UserPassword
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if err := checkTime(d.At); err != nil {
			return change{}, err
		}
		u, err := found(d.ID)
		if err != nil {
			return change{}, err
		}
		if err := checkHash(d.PasswordHash); err != nil {
			return change{}, err
		}
		u.PasswordHash, u.PasswordChangedAt, u.UpdatedAt = d.PasswordHash, d.At, d.At
		u.CredentialEpoch++
		next[u.ID] = u

	case TypeUserRehash:
		var d UserRehash
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		u, err := found(d.ID)
		if err != nil {
			return change{}, err
		}
		if err := checkHash(d.NewHash); err != nil {
			return change{}, err
		}
		if u.PasswordHash != d.OldHash {
			return change{}, refuse(CodeHashChanged, "the password of %s was changed since", u.Username)
		}
		u.PasswordHash = d.NewHash
		next[u.ID] = u

	case TypeUserDelete:
		var d UserDelete
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if _, err := found(d.ID); err != nil {
			return change{}, err
		}
		delete(next, d.ID)

	case TypeUsersImport:
		var d UsersImport
		if err := decodeOrStall(e, &d); err != nil {
			return change{}, err
		}
		if len(s.users) > 0 {
			return change{}, refuse(CodeUsersExist, "the cluster has %d users already; an import only fills a cluster without users", len(s.users))
		}
		if err := checkTime(d.At); err != nil {
			return change{}, err
		}
		if len(d.Users) == 0 {
			return change{}, refuse(CodeInvalid, "the import holds no user")
		}
		if len(d.Users) > MaxUsers {
			return change{}, refuse(CodeTooManyUsers, "the import holds %d users; the cluster holds at most %d", len(d.Users), MaxUsers)
		}
		names := map[string]bool{}
		for _, iu := range d.Users {
			// Each refusal names the user it is about (F3): the ID
			// and username, quoted, as they may be the invalid values.
			who := fmt.Sprintf("user %q (%q)", iu.ID, iu.Username)
			if err := checkUserValues(iu.ID, iu.Username, iu.DisplayName, iu.RoleID, iu.PasswordHash); err != nil {
				return change{}, refuse(RefusalCode(err), "%s: %s", who, err.Error())
			}
			if iu.CreatedAt <= 0 || iu.UpdatedAt <= 0 || iu.PasswordChangedAt < 0 {
				return change{}, refuse(CodeInvalid, "%s: no valid times", who)
			}
			if _, ok := next[iu.ID]; ok {
				return change{}, refuse(CodeInvalid, "%s: the user ID is twice in the import", who)
			}
			if names[iu.Username] {
				return change{}, refuse(CodeUsernameTaken, "%s: the username is twice in the import", who)
			}
			names[iu.Username] = true
			next[iu.ID] = User{ID: iu.ID, Username: iu.Username, DisplayName: iu.DisplayName, PasswordHash: iu.PasswordHash,
				RoleID: iu.RoleID, Disabled: iu.Disabled, CredentialEpoch: 1, PasswordChangedAt: iu.PasswordChangedAt,
				CreatedAt: iu.CreatedAt, UpdatedAt: iu.UpdatedAt}
		}
	default:
		return change{}, fmt.Errorf("not a user entry type: %s", e.Type)
	}
	// D4: no entry leaves the cluster without an enabled administrator
	// it had.
	if before > 0 && enabledAdmins(next) == 0 {
		return change{}, refuse(CodeLastAdmin, "this would leave no enabled administrator")
	}
	s.users, s.usersIndex = next, index
	return change{users: true}, nil
}

// describeUser returns what a user entry names for the audit (D10), by the
// state before it: never a hash.
func (s state) describeUser(e Entry) (a AuditEntry) {
	fill := func(id string) {
		a.UserID = id
		if u, ok := s.users[id]; ok {
			a.Username, a.Role, a.UserDisabled = u.Username, u.RoleID, u.Disabled
		}
	}
	switch e.Type {
	case TypeUserCreate:
		var d UserCreate
		if decodeData(e.Data, &d) == nil {
			a.UserID, a.Username, a.Role, a.At = d.ID, d.Username, d.RoleID, d.At
		}
	case TypeUserUpdate:
		var d UserUpdate
		if decodeData(e.Data, &d) == nil {
			fill(d.ID)
			a.At = d.At
			if d.RoleID != nil {
				a.Role = *d.RoleID
			}
			if d.Disabled != nil {
				a.UserDisabled = *d.Disabled
			}
		}
	case TypeUserPassword:
		var d UserPassword
		if decodeData(e.Data, &d) == nil {
			fill(d.ID)
			a.At = d.At
		}
	case TypeUserRehash:
		var d UserRehash
		if decodeData(e.Data, &d) == nil {
			fill(d.ID)
		}
	case TypeUserDelete:
		var d UserDelete
		if decodeData(e.Data, &d) == nil {
			fill(d.ID)
		}
	case TypeUsersImport:
		var d UsersImport
		if decodeData(e.Data, &d) == nil {
			a.At, a.Count = d.At, len(d.Users)
			for _, u := range d.Users {
				a.Usernames = append(a.Usernames, u.Username)
			}
			if a.Usernames == nil {
				a.Usernames = []string{}
			}
		}
	}
	return a
}
