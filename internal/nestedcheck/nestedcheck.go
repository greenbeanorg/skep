// Package nestedcheck detects a FreeBSD host running under another
// hypervisor with VT-x virtual interrupt delivery on, where the first
// bhyve guest can freeze the whole host. It only reads sysctls: it never
// refuses anything and never changes the host.
package nestedcheck

import "fmt"

const (
	guestSysctl = "kern.vm_guest"
	// VIDSysctl exists only while vmm is loaded on a VT-x host.
	VIDSysctl = "hw.vmm.vmx.cap.virtual_interrupt_delivery"
)

// Reader reads sysctls; faked in tests.
type Reader interface {
	String(name string) (string, error)
	Uint32(name string) (uint32, error)
}

// Problem returns the warning for this host, or "" when there is none. A
// value it cannot read is no warning: without vmm loaded, or on AMD-V, the
// setting does not exist.
func Problem(r Reader) string {
	guest, err := r.String(guestSysctl)
	if err != nil || guest == "" || guest == "none" {
		return ""
	}
	vid, err := r.Uint32(VIDSysctl)
	if err != nil || vid == 0 {
		return ""
	}
	return fmt.Sprintf("this host runs under a hypervisor with %s=%d: nested bhyve can freeze the whole host on the first VM start; "+
		"add %s=\"0\" to /boot/loader.conf and reboot", VIDSysctl, vid, VIDSysctl)
}
