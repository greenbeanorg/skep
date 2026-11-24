//go:build !freebsd

package nestedcheck

// HostProblem: the setting exists on FreeBSD only.
func HostProblem() string { return "" }
