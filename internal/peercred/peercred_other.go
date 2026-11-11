//go:build !freebsd && !linux

package peercred

func fromFD(int) (Cred, error) { return Cred{}, ErrUnsupported }
