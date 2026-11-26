package models

import "regexp"

// guestNameRE is the agents' rule for a VM's and a container's name
// (internal/vm and internal/containers nameRE; a test keeps them equal).
var guestNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// GuestNameRule is what ValidGuestName accepts, in words.
const GuestNameRule = "a name is 1 to 63 letters, digits, '.', '_' or '-', starting with a letter or digit"

// ValidGuestName reports whether name is a valid VM or container name: 1
// to 63 ASCII letters, digits, '.', '_' or '-', starting with a letter or
// digit. The API checks it before it reserves the name in the cluster (F7
// of the 8d-1 review); the agents keep their own check.
func ValidGuestName(name string) bool { return guestNameRE.MatchString(name) }
