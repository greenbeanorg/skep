//go:build freebsd

package nestedcheck

import "testing"

// The real reader agrees with Problem on whatever host runs this test.
func TestHostReader(t *testing.T) {
	guest, err := Host{}.String(guestSysctl)
	if err != nil || guest == "" {
		t.Fatalf("kern.vm_guest = %q, %v", guest, err)
	}
	vid, err := Host{}.Uint32(VIDSysctl)
	t.Logf("kern.vm_guest=%s vid=%d err=%v", guest, vid, err)
	want := guest != "none" && err == nil && vid != 0
	if got := HostProblem() != ""; got != want {
		t.Fatalf("HostProblem warns = %v, want %v", got, want)
	}
}
