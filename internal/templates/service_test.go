package templates

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

// fixture is the template service on a scratch agent.db, a real task
// engine, and the fake host.
type fixture struct {
	t      *testing.T
	db     *sql.DB
	fake   *hostfake.Fake
	engine *tasks.Engine
	svc    *Service
	sub    Submission
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, filepath.Join(dir, "agent", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	nodeID, err := database.EnsureLocalNode(ctx, db, "testnode")
	if err != nil {
		t.Fatal(err)
	}
	conf := hostops.Confinement{Pools: []string{"devpool"}, DatasetRoot: "skep",
		Directories: []hostops.DirPool{{Name: "dirpool", Path: "/srv/dirpool"}}}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := hostfake.New(conf, hostfake.Options{Logger: quiet})
	t.Cleanup(fake.Close)
	engine, err := tasks.New(tasks.Options{Store: database.NewTaskStore(db), Audit: database.NewAuditLog(db), NodeID: nodeID, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{NodeID: nodeID, Store: database.NewTemplateStore(db), Engine: engine, Host: fake.Ops(),
		Packages: []string{"FreeBSD-set-minimal"}, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range svc.Definitions() {
		if err := engine.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Shutdown(sctx)
	})
	return &fixture{t: t, db: db, fake: fake, engine: engine, svc: svc,
		sub: Submission{Actor: models.TaskActor{ID: "u-1", Name: "andy"}, PeerUID: 1001, RequestID: "req-1", SourceIP: "192.0.2.7"}}
}

func (f *fixture) wait(id string) tasks.Task {
	f.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		task, err := f.engine.Get(context.Background(), id)
		if err != nil {
			f.t.Fatal(err)
		}
		if task.Status.Terminal() {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("task %s did not finish", id)
	return tasks.Task{}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestCreateAndDeleteTemplate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.svc.CreateTemplate(ctx, models.TemplateCreate{Name: "minimal", Pool: "devpool", Source: "pkgbase"}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	// The reply's template is a snapshot that can be read after the task
	// has finished, so its Provisioned is not asserted (amendment k, as Q5).
	if created.Task.Type != models.TaskTemplateCreate || created.Template.Packages[0] != "FreeBSD-set-minimal" {
		t.Fatalf("created = %+v", created)
	}
	task := f.wait(created.Task.ID)
	if task.Status != models.TaskSucceeded {
		t.Fatalf("build: %s %s %s", task.Status, task.ErrorCode, task.ErrorMessage)
	}
	tpl, err := f.svc.Template(ctx, created.Template.ID)
	if err != nil || !tpl.Provisioned || tpl.Release != hostfake.SimulatedRelease || tpl.HostRelease != hostfake.SimulatedRelease ||
		tpl.PatchDiffers || tpl.UsedBytes == 0 {
		t.Fatalf("template = %+v, %v", tpl, err)
	}
	calls := strings.Join(f.fake.Calls(), "\n")
	for _, want := range []string{"pkg install FreeBSD-set-minimal", "jail -c skep-build-", "/usr/sbin/certctl rehash",
		"/etc/services", "zfs snapshot devpool/skep/templates/" + tpl.ID + "@skep-base"} {
		if !strings.Contains(calls, want) {
			t.Errorf("the simulated build did not %q:\n%s", want, calls)
		}
	}
	list, err := f.svc.Templates(ctx, models.TemplateListOptions{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// A second template of the same name is refused, and audited.
	_, err = f.svc.CreateTemplate(ctx, models.TemplateCreate{Name: "minimal", Pool: "devpool", Source: "pkgbase"}, f.sub)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeTemplateNameTaken {
		t.Fatalf("a repeated name = %v", err)
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE outcome = 'denied' AND action = ?`, models.TaskTemplateCreate); n != 1 {
		t.Fatalf("denied audit entries = %d", n)
	}

	del, err := f.svc.DeleteTemplate(ctx, tpl.ID, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if task := f.wait(del.ID); task.Status != models.TaskSucceeded {
		t.Fatalf("delete: %s %s %s", task.Status, task.ErrorCode, task.ErrorMessage)
	}
	if _, err := f.svc.Template(ctx, tpl.ID); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
	if !strings.Contains(strings.Join(f.fake.Calls(), "\n"), "zfs destroy -r devpool/skep/templates/"+tpl.ID) {
		t.Fatal("the dataset was not destroyed")
	}
}

func TestCreateTemplateRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		req  models.TemplateCreate
		code string
	}{
		"bad name":       {models.TemplateCreate{Name: "-x", Pool: "devpool", Source: "pkgbase"}, models.CodeInvalidRequest},
		"name with /":    {models.TemplateCreate{Name: "a/b", Pool: "devpool", Source: "pkgbase"}, models.CodeInvalidRequest},
		"unknown pool":   {models.TemplateCreate{Name: "a", Pool: "tank", Source: "pkgbase"}, models.CodePoolNotAllowed},
		"directory pool": {models.TemplateCreate{Name: "a", Pool: "dirpool", Source: "pkgbase"}, models.CodeStorageFeatureUnsupported},
		"unknown source": {models.TemplateCreate{Name: "a", Pool: "devpool", Source: "https://example.com/base.txz"}, models.CodeInvalidRequest},
		"no dist":        {models.TemplateCreate{Name: "a", Pool: "devpool", Source: "dist"}, models.CodeStorageFeatureUnsupported},
	} {
		_, err := f.svc.CreateTemplate(ctx, c.req, f.sub)
		var me *models.Error
		if !errors.As(err, &me) || me.Code != c.code {
			t.Errorf("%s: err = %v, want %s", name, err, c.code)
		}
	}
	if n := f.count(`SELECT count(*) FROM container_templates`); n != 0 {
		t.Fatalf("rows after refusals = %d", n)
	}
}

func TestCreateTemplateIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sub := f.sub
	sub.IdempotencyKey = "k1"
	req := models.TemplateCreate{Name: "minimal", Pool: "devpool", Source: "pkgbase"}
	a, err := f.svc.CreateTemplate(ctx, req, sub)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.svc.CreateTemplate(ctx, req, sub)
	if err != nil || b.Task.ID != a.Task.ID || b.Template.ID != a.Template.ID {
		t.Fatalf("replay = %+v, %v", b, err)
	}
	req.Name = "other"
	_, err = f.svc.CreateTemplate(ctx, req, sub)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeIdempotencyConflict {
		t.Fatalf("a different request with the same key = %v", err)
	}
	f.wait(a.Task.ID)
}

func TestSources(t *testing.T) {
	f := newFixture(t)
	src := f.svc.Sources(context.Background())
	if src.HostRelease != hostfake.SimulatedRelease || !src.Pkgbase.Available || src.Dist.Available || src.Dist.Reason == "" ||
		len(src.Packages) != 1 {
		t.Fatalf("sources = %+v", src)
	}
	f.svc.packages = nil
	if src := f.svc.Sources(context.Background()); src.Pkgbase.Available {
		t.Fatal("pkgbase is available without packages")
	}
}

func TestSweepBuildJails(t *testing.T) {
	f := newFakeHost()
	f.calls = nil
	var logs strings.Builder
	s := &Service{host: sweepHost{fakeHost: f, ids: []string{tpl}}, log: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := s.SweepBuildJails(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, ",") != "RemoveBuildJail" {
		t.Fatalf("calls = %v", f.calls)
	}
	if !strings.Contains(logs.String(), "event=build_jail_removed") || !strings.Contains(logs.String(), "event=build_jail_sweep found=1 removed=1 failed=0") {
		t.Fatalf("log:\n%s", logs.String())
	}
	logs.Reset()
	f.fail["RemoveBuildJail"] = errors.New("still there")
	if err := s.SweepBuildJails(context.Background()); err == nil {
		t.Fatal("a failed removal was not reported")
	}
	if !strings.Contains(logs.String(), "event=build_jail_sweep found=1 removed=0 failed=1") {
		t.Fatalf("log:\n%s", logs.String())
	}
	// Nothing to sweep: no removal, and the sweep still says it ran.
	logs.Reset()
	f.calls = nil
	empty := &Service{host: sweepHost{fakeHost: f}, log: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := empty.SweepBuildJails(context.Background()); err != nil || len(f.calls) != 0 {
		t.Fatalf("empty sweep: %v, calls %v", err, f.calls)
	}
	if !strings.Contains(logs.String(), "event=build_jail_sweep found=0 removed=0 failed=0") {
		t.Fatalf("log:\n%s", logs.String())
	}
}

type sweepHost struct {
	*fakeHost
	ids []string
}

func (h sweepHost) BuildJails(context.Context) ([]string, error) { return h.ids, nil }
