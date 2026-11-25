package clusterstate

// ValidDisplayName exposes the display-name rule to the external test that
// keeps it equal to internal/auth's.
func ValidDisplayName(name string) bool { return checkDisplayName(name) == nil }
