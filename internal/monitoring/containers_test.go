package monitoring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	ctrA = "2d2d6f4e-5d0c-4a52-9a43-0d2f8a1f3c33"
	ctrB = "3e3e6f4e-5d0c-4a52-9a43-0d2f8a1f3c44"
	ctrC = "4f4f6f4e-5d0c-4a52-9a43-0d2f8a1f3c55"
)

type stubJails struct {
	hostops.Jails
	racct bool
	usage map[string]hostops.JailUsage
	errs  map[string]error
	calls []string
}

func (j *stubJails) RctlInfo(context.Context) (hostops.RctlInfo, error) {
	return hostops.RctlInfo{Enabled: j.racct, CPUs: 4}, nil
}

func (j *stubJails) JailUsage(_ context.Context, id string) (hostops.JailUsage, error) {
	j.calls = append(j.calls, id)
	if err := j.errs[id]; err != nil {
		return hostops.JailUsage{}, err
	}
	return j.usage[id], nil
}

type stubContainers struct{ items []models.Container }

// Containers pages one container at a time, so paging is exercised.
func (s stubContainers) Containers(_ context.Context, opts models.ContainerListOptions) (models.ContainerList, error) {
	i := 0
	if opts.Cursor != "" {
		i = int(opts.Cursor[0] - '0')
	}
	if i >= len(s.items) {
		return models.ContainerList{}, nil
	}
	list := models.ContainerList{Items: s.items[i : i+1]}
	if i+1 < len(s.items) {
		list.NextCursor = string(rune('0' + i + 1))
	}
	return list, nil
}

func containerSampler(t *testing.T, f *fixture, jails *stubJails, cs stubContainers) *Sampler {
	t.Helper()
	f.zfs.inventory["tank"] = append(f.zfs.inventory["tank"],
		hostops.Dataset{Name: "tank/skep/jail", Type: "filesystem", UsedBytes: 5000},
		hostops.Dataset{Name: "tank/skep/jail/" + ctrA, Type: "filesystem", UsedBytes: 3000, ReferencedBytes: 90 << 20, ContainerID: ctrA},
		hostops.Dataset{Name: "tank/skep/jail/" + ctrA + "@s1", Type: "snapshot", UsedBytes: 1000, ReferencedBytes: 80 << 20, ContainerID: ctrA},
		hostops.Dataset{Name: "tank/skep/jail/" + ctrB, Type: "filesystem", UsedBytes: 2000, ReferencedBytes: 78 << 20, ContainerID: ctrB},
	)
	s, err := New(Options{
		NodeID: "node-1",
		Host: hostops.Ops{
			Conf: hostops.Confinement{Pools: []string{"tank"}, DatasetRoot: "skep"},
			ZFS:  f.zfs, Dirs: &stubDirs{}, Procs: f.procs, Jails: jails,
		},
		VMs: stubVMs{}, Containers: cs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSampleReportsContainers(t *testing.T) {
	f := newFixture()
	jails := &stubJails{racct: true, usage: map[string]hostops.JailUsage{
		ctrA: {CPUPercent: 48, CPUTime: 12 * time.Second, ResidentBytes: 512 << 20, Processes: 3, OpenFiles: 168},
	}}
	cs := stubContainers{items: []models.Container{
		{ID: ctrA, Name: "a", Pool: "tank", State: models.ContainerRunning},
		{ID: ctrB, Name: "b", Pool: "tank", State: models.ContainerStopped},
	}}
	m, err := containerSampler(t, f, jails, cs).Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Containers) != 2 {
		t.Fatalf("containers = %+v", m.Containers)
	}
	a := m.Containers[0]
	if a.ID != ctrA || a.State != models.ContainerRunning || a.Error != "" ||
		a.CPUPercent == nil || *a.CPUPercent != 48 || a.CPUTimeSeconds == nil || *a.CPUTimeSeconds != 12 ||
		a.ResidentMemoryBytes == nil || *a.ResidentMemoryBytes != 512<<20 ||
		a.Processes == nil || *a.Processes != 3 || a.OpenFileSlots == nil || *a.OpenFileSlots != 168 {
		t.Errorf("running jail = %+v", a)
	}
	// The root's own figures, not its snapshot's.
	if a.DiskUsedBytes != 3000 || a.DiskReferencedBytes != 90<<20 {
		t.Errorf("running jail disk = %+v", a)
	}
	b := m.Containers[1]
	if b.CPUPercent != nil || b.ResidentMemoryBytes != nil || b.Processes != nil || b.OpenFileSlots != nil ||
		b.DiskUsedBytes != 2000 || b.DiskReferencedBytes != 78<<20 || b.Error != "" {
		t.Errorf("stopped jail = %+v", b)
	}
	// Only the running jail is read.
	if len(jails.calls) != 1 || jails.calls[0] != ctrA {
		t.Errorf("JailUsage calls = %q", jails.calls)
	}
}

func TestSampleContainersWithoutRacct(t *testing.T) {
	f := newFixture()
	jails := &stubJails{racct: false}
	cs := stubContainers{items: []models.Container{{ID: ctrA, Name: "a", Pool: "tank", State: models.ContainerRunning}}}
	m, err := containerSampler(t, f, jails, cs).Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := m.Containers[0]
	if a.CPUPercent != nil || a.Error != "" || a.DiskUsedBytes != 3000 {
		t.Errorf("jail without RACCT = %+v", a)
	}
	if len(jails.calls) != 0 {
		t.Errorf("JailUsage was called without RACCT: %q", jails.calls)
	}
}

func TestSampleContainerErrors(t *testing.T) {
	f := newFixture()
	jails := &stubJails{racct: true, errs: map[string]error{
		ctrA: errors.New("rctl: boom"),
		// Stopped since the watcher looked: not an error.
		ctrB: models.ErrNotFound,
	}}
	cs := stubContainers{items: []models.Container{
		{ID: ctrA, Name: "a", Pool: "tank", State: models.ContainerRunning},
		{ID: ctrB, Name: "b", Pool: "tank", State: models.ContainerRunning},
		{ID: ctrC, Name: "c", Pool: "gone", State: models.ContainerStopped},
	}}
	m, err := containerSampler(t, f, jails, cs).Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a := m.Containers[0]; a.Error != "its RACCT usage could not be read" || a.DiskUsedBytes != 3000 {
		t.Errorf("a = %+v", a)
	}
	if b := m.Containers[1]; b.Error != "" || b.CPUPercent != nil {
		t.Errorf("b = %+v", b)
	}
	if c := m.Containers[2]; c.Error != "the datasets of gone are not available" {
		t.Errorf("c = %+v", c)
	}
}

func TestSampleWithoutContainerLister(t *testing.T) {
	f := newFixture()
	m, err := f.sampler(t, 0, 0).Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Containers == nil || len(m.Containers) != 0 {
		t.Errorf("containers = %#v, want an empty list", m.Containers)
	}
}
