package containers

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// snapshot takes a snapshot and waits for it.
func (f *fixture) snapshot(id, name string) models.Snapshot {
	f.t.Helper()
	created, err := f.svc.CreateSnapshot(context.Background(), id, models.SnapshotCreate{Name: name}, f.sub)
	f.succeed(created.Task, err)
	snap, err := f.svc.Snapshot(context.Background(), id, created.Snapshot.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return snap
}

// clone clones and waits, returning the clone.
func (f *fixture) clone(srcID string, req models.ContainerClone) models.Container {
	f.t.Helper()
	created, err := f.svc.CloneContainer(context.Background(), srcID, req, f.sub)
	f.succeed(created.Task, err)
	c, err := f.svc.Container(context.Background(), created.Container.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func TestSnapshotCreateRollbackDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "web1", TemplateID: f.minimal})

	s1 := f.snapshot(c.ID, "before")
	if !s1.Provisioned || s1.Consistency != models.SnapshotConsistent || s1.ResourceKind != models.ContainerResource ||
		s1.Observed == nil || !s1.Observed.Exists {
		t.Fatalf("stopped snapshot: %+v", s1)
	}
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	s2 := f.snapshot(c.ID, "running")
	if s2.Consistency != models.SnapshotCrashConsistent {
		t.Fatalf("running snapshot: %+v", s2)
	}
	// The name is unique per jail.
	_, err := f.svc.CreateSnapshot(ctx, c.ID, models.SnapshotCreate{Name: "before"}, f.sub)
	wantCode(t, err, models.CodeSnapshotNameTaken)

	// Rollback needs a stopped jail.
	_, err = f.svc.RollbackSnapshot(ctx, c.ID, s2.ID, models.SnapshotRollback{}, f.sub)
	wantCode(t, err, models.CodeContainerNotStopped)
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))

	// An older snapshot needs destroy_newer, which names what goes.
	_, err = f.svc.RollbackSnapshot(ctx, c.ID, s1.ID, models.SnapshotRollback{}, f.sub)
	wantCode(t, err, models.CodeSnapshotNotLatest)
	done := f.succeed(f.svc.RollbackSnapshot(ctx, c.ID, s1.ID, models.SnapshotRollback{DestroyNewer: true}, f.sub))
	if !strings.Contains(string(done.Result), s2.ID) {
		t.Fatalf("the rollback result does not name the destroyed snapshot: %s", done.Result)
	}
	list, err := f.svc.Snapshots(ctx, c.ID)
	if err != nil || len(list.Items) != 1 || list.Items[0].ID != s1.ID {
		t.Fatalf("after rollback: %+v, %v", list, err)
	}
	// started_once stays 1 after a rollback to a snapshot taken before the
	// first start (decision 13), so the SSH key still cannot change.
	c, _ = f.svc.Container(ctx, c.ID)
	if !c.StartedOnce {
		t.Fatal("a rollback reset started_once")
	}

	f.succeed(f.svc.DeleteSnapshot(ctx, c.ID, s1.ID, f.sub))
	if list, _ := f.svc.Snapshots(ctx, c.ID); len(list.Items) != 0 {
		t.Fatalf("after delete: %+v", list)
	}
	if n := f.count(`SELECT COUNT(*) FROM tasks WHERE type LIKE 'container.snapshot.%' AND status = 'succeeded'`); n != 4 {
		t.Fatalf("%d snapshot tasks succeeded, want 4", n)
	}
}

func TestSnapshotLimitAndUnprovisioned(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "web1", TemplateID: f.minimal})
	for _, n := range []string{"a", "b", "c", "d"} {
		f.snapshot(c.ID, n)
	}
	_, err := f.svc.CreateSnapshot(ctx, c.ID, models.SnapshotCreate{Name: "e"}, f.sub)
	wantCode(t, err, models.CodeSnapshotLimit)
	_, err = f.svc.CreateSnapshot(ctx, c.ID, models.SnapshotCreate{Name: "-bad"}, f.sub)
	wantCode(t, err, models.CodeInvalidRequest)
}

func TestCloneDHCPJail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	src := f.create(models.ContainerCreate{Name: "web1", TemplateID: f.minimal,
		NICs:   []models.ContainerNICSpec{{Bridge: "bridge0"}},
		Limits: &models.ContainerLimits{DiskQuota: 1 << 30}})
	f.succeed(f.svc.Action(ctx, src.ID, models.ContainerAction{Action: "start"}, f.sub))
	snap := f.snapshot(src.ID, "s1")

	c := f.clone(src.ID, models.ContainerClone{Name: "web2", SnapshotID: snap.ID})
	if c.ID == src.ID || c.ClonedFromSnapshotID != snap.ID || !c.StartedOnce || !c.Provisioned ||
		c.Hostname != "web2" || c.TemplateID != src.TemplateID || c.Limits != src.Limits {
		t.Fatalf("clone: %+v", c)
	}
	if len(c.NICs) != 1 || c.NICs[0].MACAddress == src.NICs[0].MACAddress || c.NICs[0].ID == src.NICs[0].ID ||
		c.NICs[0].Interface != "skep0" || c.NICs[0].IPv4.Mode != models.IPv4DHCP || c.NICs[0].Bridge != "bridge0" {
		t.Fatalf("clone NICs: %+v (source %+v)", c.NICs, src.NICs)
	}
	calls := strings.Join(f.fake.Calls(), "\n")
	ds, _ := hostops.Confinement{Pools: []string{"devpool"}, DatasetRoot: "skep"}.JailDataset("devpool", c.ID)
	if !strings.Contains(calls, "zfs clone ") || !strings.Contains(calls, ds) {
		t.Fatalf("calls:\n%s", calls)
	}
	// The clone has its own refquota (clones do not inherit it), and its
	// started tree is not written into.
	if !strings.Contains(calls, "zfs set refquota=1073741824 "+ds) {
		t.Fatalf("the clone's quota was not set:\n%s", calls)
	}
	// The snapshot, and the source, are protected while the clone exists.
	snap, _ = f.svc.Snapshot(ctx, src.ID, snap.ID)
	if len(snap.Clones) != 1 || snap.Clones[0].ID != c.ID {
		t.Fatalf("snapshot clones: %+v", snap.Clones)
	}
	_, err := f.svc.DeleteSnapshot(ctx, src.ID, snap.ID, f.sub)
	wantCode(t, err, models.CodeSnapshotHasClones)
	f.succeed(f.svc.Action(ctx, src.ID, models.ContainerAction{Action: "stop"}, f.sub))
	_, err = f.svc.DeleteContainer(ctx, src.ID, f.sub)
	wantCode(t, err, models.CodeSnapshotHasClones)

	// The clone starts on its own NIC.
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))
	f.succeed(f.svc.DeleteContainer(ctx, c.ID, f.sub))
	f.succeed(f.svc.DeleteSnapshot(ctx, src.ID, snap.ID, f.sub))
	f.succeed(f.svc.DeleteContainer(ctx, src.ID, f.sub))
}

func TestCloneFromCurrentStateTakesSnapshot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	src := f.create(models.ContainerCreate{Name: "web1", TemplateID: f.minimal})
	c := f.clone(src.ID, models.ContainerClone{Name: "web2", Hostname: "web2.lab"})
	if c.StartedOnce || c.Hostname != "web2.lab" {
		t.Fatalf("clone of a jail that never started: %+v", c)
	}
	list, _ := f.svc.Snapshots(ctx, src.ID)
	if len(list.Items) != 1 || list.Items[0].Name != "web2" || list.Items[0].ID != c.ClonedFromSnapshotID ||
		list.Items[0].Consistency != models.SnapshotConsistent {
		t.Fatalf("snapshot left behind: %+v", list.Items)
	}
	// An unknown snapshot is refused before anything is made.
	_, err := f.svc.CloneContainer(ctx, src.ID, models.ContainerClone{Name: "web3", SnapshotID: src.ID}, f.sub)
	wantCode(t, err, models.CodeInvalidRequest)
	// Without a snapshot ID the task's snapshot takes the clone's name,
	// so a taken name is refused there first, as for VMs.
	_, err = f.svc.CloneContainer(ctx, src.ID, models.ContainerClone{Name: "web2"}, f.sub)
	wantCode(t, err, models.CodeSnapshotNameTaken)
	_, err = f.svc.CloneContainer(ctx, src.ID, models.ContainerClone{Name: "web2", SnapshotID: c.ClonedFromSnapshotID}, f.sub)
	wantCode(t, err, models.CodeContainerNameTaken)
}

func TestCloneStaticAddressing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	src := f.create(models.ContainerCreate{Name: "web1", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{
			{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "192.0.2.1")},
			{Bridge: "bridge1"},
		}})

	// Without new addressing for the static NIC: 422, nothing made.
	before := f.count(`SELECT COUNT(*) FROM containers`)
	_, err := f.svc.CloneContainer(ctx, src.ID, models.ContainerClone{Name: "web2"}, f.sub)
	wantCode(t, err, models.CodeCloneAddressingRequired)
	_, err = f.svc.CloneContainer(ctx, src.ID, models.ContainerClone{Name: "web2", NICs: []models.ContainerNICSpec{
		{IPv4: static("192.0.2.10/25", "")}, {}}}, f.sub)
	wantCode(t, err, models.CodeCloneAddressingRequired)
	_, err = f.svc.CloneContainer(ctx, src.ID, models.ContainerClone{Name: "web2", NICs: []models.ContainerNICSpec{{}}}, f.sub)
	wantCode(t, err, models.CodeInvalidRequest)
	if n := f.count(`SELECT COUNT(*) FROM containers`); n != before {
		t.Fatalf("a refused clone left a row: %d, want %d", n, before)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action = ? AND outcome = 'denied'`, models.TaskContainerClone); n == 0 {
		t.Fatal("the refusal was not audited")
	}

	// The source never started: the clone's tree is written with its own
	// addressing.
	c := f.clone(src.ID, models.ContainerClone{Name: "web2", NICs: []models.ContainerNICSpec{
		{IPv4: static("192.0.2.11/24", "192.0.2.1")}, {}}})
	files, ok := f.fake.JailFiles(c.ID)
	want := []string{`ifconfig_skep0="inet 192.0.2.11/24"`, `defaultrouter="192.0.2.1"`, `ifconfig_skep1="SYNCDHCP"`}
	if !ok || !slices.Equal(files.RCConf, want) || len(files.Nameservers) != 0 {
		t.Fatalf("clone files: %+v", files)
	}
	if c.NICs[1].Bridge != "bridge1" || c.NICs[0].IPv4.Address != "192.0.2.11/24" {
		t.Fatalf("clone NICs: %+v", c.NICs)
	}

	// Once the source has started, new addressing cannot be written into
	// the clone's tree (decision 13).
	f.succeed(f.svc.Action(ctx, src.ID, models.ContainerAction{Action: "start"}, f.sub))
	_, err = f.svc.CloneContainer(ctx, src.ID, models.ContainerClone{Name: "web3", NICs: []models.ContainerNICSpec{
		{IPv4: static("192.0.2.12/24", "192.0.2.1")}, {}}}, f.sub)
	wantCode(t, err, models.CodeContainerStarted)
	// Without new addressing the answer is the same: asking for addressing
	// would send the client to the refusal above.
	_, err = f.svc.CloneContainer(ctx, src.ID, models.ContainerClone{Name: "web3"}, f.sub)
	wantCode(t, err, models.CodeContainerStarted)
	if !strings.Contains(err.Error(), "cannot be cloned") {
		t.Fatalf("message: %v", err)
	}
	// So is a change of that NIC to DHCP.
	_, err = f.svc.CloneContainer(ctx, src.ID, models.ContainerClone{Name: "web3", NICs: []models.ContainerNICSpec{
		{IPv4: &models.ContainerIPv4{Mode: models.IPv4DHCP}}, {}}}, f.sub)
	wantCode(t, err, models.CodeContainerStarted)
}

// A started DHCP jail can still be cloned: the rule is about static
// addresses.
func TestCloneStartedDHCPJailStillWorks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	src := f.create(models.ContainerCreate{Name: "d1", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0"}}})
	f.succeed(f.svc.Action(ctx, src.ID, models.ContainerAction{Action: "start"}, f.sub))
	if c := f.clone(src.ID, models.ContainerClone{Name: "d2"}); !c.StartedOnce {
		t.Fatalf("clone: %+v", c)
	}
}

func TestStartRefusedWhenAddressHeld(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.create(models.ContainerCreate{Name: "a", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "")}}})
	b := f.create(models.ContainerCreate{Name: "b", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/25", "")}}})
	other := f.create(models.ContainerCreate{Name: "other", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge1", IPv4: static("192.0.2.10/24", "")}}})

	// b is stopped while a starts: nothing holds the address yet.
	f.succeed(f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "start"}, f.sub))
	_, err := f.svc.Action(ctx, b.ID, models.ContainerAction{Action: "start"}, f.sub)
	wantCode(t, err, models.CodeContainerAddressInUse)
	if !strings.Contains(err.Error(), "running jail a holds 192.0.2.10/24 on bridge0") {
		t.Fatalf("refusal: %v", err)
	}
	// Another bridge is another network.
	f.succeed(f.svc.Action(ctx, other.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.succeed(f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "stop"}, f.sub))
	f.succeed(f.svc.Action(ctx, b.ID, models.ContainerAction{Action: "start"}, f.sub))
}
