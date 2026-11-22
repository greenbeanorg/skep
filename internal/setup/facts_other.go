//go:build !freebsd

package setup

import "context"

// HostProber refuses on anything but FreeBSD: Linux is the development
// environment only, and setup has nothing to inspect there.
type HostProber struct{}

// Probe implements Prober.
func (HostProber) Probe(context.Context) (Facts, error) { return Facts{}, ErrNotFreeBSD }
