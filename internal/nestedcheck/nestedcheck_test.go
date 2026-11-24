package nestedcheck

import (
	"errors"
	"strings"
	"testing"
)

type fake struct {
	guest    string
	guestErr error
	vid      uint32
	vidErr   error
}

func (f fake) String(string) (string, error) { return f.guest, f.guestErr }
func (f fake) Uint32(string) (uint32, error) { return f.vid, f.vidErr }

func TestProblem(t *testing.T) {
	absent := errors.New("no such sysctl")
	for name, c := range map[string]struct {
		r    fake
		warn bool
	}{
		"bare metal, on":       {fake{guest: "none", vid: 1}, false},
		"guest unreadable":     {fake{guestErr: absent, vid: 1}, false},
		"nested, off":          {fake{guest: "kvm", vid: 0}, false},
		"nested, vmm unloaded": {fake{guest: "kvm", vidErr: absent}, false},
		"nested, on":           {fake{guest: "kvm", vid: 1}, true},
		"other hypervisor, on": {fake{guest: "vmware", vid: 1}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got := Problem(c.r)
			if (got != "") != c.warn {
				t.Fatalf("problem %q, want warning %v", got, c.warn)
			}
			if c.warn && (!strings.Contains(got, "freeze") || !strings.Contains(got, VIDSysctl+`="0"`) || !strings.Contains(got, "/boot/loader.conf")) {
				t.Errorf("message: %q", got)
			}
		})
	}
}
