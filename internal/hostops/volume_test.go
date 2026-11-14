package hostops

import (
	"context"
	"reflect"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

func TestResizeAndDestroyVolume(t *testing.T) {
	ctx := context.Background()
	ds := "skeptest/skep/vm/" + vmA + "/disk1"
	list := "zfs list -H -p -t filesystem,volume -o " + listCols + " " + ds
	run := &script{answers: map[string]any{list: vol(ds, vmA, diskA1, 12)}}
	h := testHost(t, run, true)
	spec := VolumeSpec{Pool: "skeptest", VMID: vmA, DiskID: diskA1, Slot: 1, SizeBytes: 16 << 30}

	if _, err := h.ResizeVolume(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if m := mutations(run.calls); !reflect.DeepEqual(m, []string{"zfs set volsize=17179869184 " + ds}) {
		t.Errorf("mutations = %v", m)
	}
	for name, bad := range map[string]VolumeSpec{
		"shrink":     {Pool: "skeptest", VMID: vmA, DiskID: diskA1, Slot: 1, SizeBytes: 4 << 30},
		"same size":  {Pool: "skeptest", VMID: vmA, DiskID: diskA1, Slot: 1, SizeBytes: 8 << 30},
		"unaligned":  {Pool: "skeptest", VMID: vmA, DiskID: diskA1, Slot: 1, SizeBytes: 16<<30 + 512},
		"other disk": {Pool: "skeptest", VMID: vmA, DiskID: diskA, Slot: 1, SizeBytes: 16 << 30},
		"other VM":   {Pool: "skeptest", VMID: vmB, DiskID: diskA1, Slot: 1, SizeBytes: 16 << 30},
	} {
		run.calls = nil
		if _, err := h.ResizeVolume(ctx, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if m := mutations(run.calls); len(m) != 0 {
			t.Errorf("%s: commands ran: %v", name, m)
		}
	}
	run.answers["zfs set volsize=34359738368 "+ds] = &CommandError{Name: "zfs", ExitCode: 1, Stderr: "cannot set property for '" + ds + "': out of space"}
	_, err := h.ResizeVolume(ctx, VolumeSpec{Pool: "skeptest", VMID: vmA, DiskID: diskA1, Slot: 1, SizeBytes: 32 << 30})
	wantCode(t, err, models.CodeInsufficientSpace)

	// Destroy: refused while a snapshot is left, then without -r.
	run.calls = nil
	run.answers["zfs list -H -p -t snapshot -o "+listCols+" -d 1 "+ds] = snapLine(ds, "skep-"+snap1, snap1, vmA, 100, "")
	if _, err := h.DestroyVolume(ctx, spec); err == nil {
		t.Error("a volume with a snapshot was destroyed")
	}
	if m := mutations(run.calls); len(m) != 0 {
		t.Errorf("commands ran: %v", m)
	}
	run.answers["zfs list -H -p -t snapshot -o "+listCols+" -d 1 "+ds] = ""
	if _, err := h.DestroyVolume(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if m := mutations(run.calls); !reflect.DeepEqual(m, []string{"zfs destroy " + ds}) {
		t.Errorf("mutations = %v", m)
	}
	run.calls = nil
	if _, err := h.DestroyVolume(ctx, VolumeSpec{Pool: "skeptest", VMID: vmA, DiskID: diskA, Slot: 1}); err == nil {
		t.Error("destroyed a volume whose disk ID does not match")
	}
	if m := mutations(run.calls); len(m) != 0 {
		t.Errorf("commands ran: %v", m)
	}
}
