package auth

import "fmt"

// UserExistsAnswer is the line "skep-api admin exists" prints for a user
// that exists. skep-setup parses it, so both take it from here.
func UserExistsAnswer(username, role string, disabled bool) string {
	state := ""
	if disabled {
		state = ", disabled"
	}
	return fmt.Sprintf("user %q exists (role %s%s)", username, role, state)
}
