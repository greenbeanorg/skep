//go:build freebsd

package nestedcheck

import "golang.org/x/sys/unix"

// Host reads the running kernel's sysctls.
type Host struct{}

// String implements Reader.
func (Host) String(name string) (string, error) { return unix.Sysctl(name) }

// Uint32 implements Reader.
func (Host) Uint32(name string) (uint32, error) { return unix.SysctlUint32(name) }

// HostProblem is Problem on the running host.
func HostProblem() string { return Problem(Host{}) }
