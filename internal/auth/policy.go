package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Password rules follow NIST SP 800-63B: a length range, no composition
// rules, no forced rotation. The upper bound stops the hashing of huge
// inputs.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 256
)

// ValidatePassword checks a new password against the rules.
func ValidatePassword(password string) error {
	if !utf8.ValidString(password) {
		return models.Invalid("the password is not valid UTF-8")
	}
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLength || n > MaxPasswordLength {
		return models.Invalid(fmt.Sprintf("the password must be %d to %d characters long", MinPasswordLength, MaxPasswordLength))
	}
	return nil
}

// usernameRE is the character set of usernames. It is a subset of what the
// agent accepts as an actor name, so every username can be recorded there.
var usernameRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)

// ValidUsername reports whether s has the shape of a username. Input that
// does not is never written to a log or the audit trail.
func ValidUsername(s string) bool { return usernameRE.MatchString(s) }

// ValidateUsername checks a new username.
func ValidateUsername(username string) error {
	if !ValidUsername(username) {
		return models.Invalid("the username must be 1 to 32 characters: a lower-case letter, then lower-case letters, digits, '.', '_' or '-'")
	}
	return nil
}

// ReservedUsername is the audit log's actor for what the agent does by
// itself (audit.System). A user of that name could not be told from it.
const ReservedUsername = "system"

// MaxDisplayNameLength bounds a user's display name.
const MaxDisplayNameLength = 128

// ValidateDisplayName checks a display name: printable, bounded.
func ValidateDisplayName(name string) error {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxDisplayNameLength {
		return models.Invalid(fmt.Sprintf("the display name must be at most %d characters", MaxDisplayNameLength))
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return models.Invalid("the display name must not contain control characters")
		}
	}
	return nil
}

// tokenBytes is the size of a session token: 256 bits from crypto/rand.
const tokenBytes = 32

// NewToken returns a new random token, base64url without padding.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns what is stored for a token: its SHA-256, in hex. A fast
// hash is right here because the input has 256 bits of entropy.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
