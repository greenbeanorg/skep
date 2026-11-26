package containers

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/internal/templates"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

// fixture is the container service on a scratch agent.db, a real task
// engine, the template service, and the fake host, with two built
// templates: one with sshd and one without.
type fixture struct {
	t        *testing.T
	db       *sql.DB
	fake     *hostfake.Fake
	engine   *tasks.Engine
	svc      *Service
	sub      Submission
	minimal  string
	withSSHD string
}

// newFixture builds the service on the fake host (16 GiB of physical
// memory); each tweak may change its options first.
func newFixture(t *testing.T, tweaks ...func(*Options)) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "agent", "agent.db"))
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
	conf := hostops.Confinement{Pools: []string{"devpool"}, DatasetRoot: "skep", JailRunDir: "/var/run/skep/jails",
		Bridges: []string{"bridge0", "bridge1"}, ManagementInterfaces: []string{"vtnet0"}}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := hostfake.New(conf, hostfake.Options{Logger: quiet})
	t.Cleanup(fake.Close)
	engine, err := tasks.New(tasks.Options{Store: database.NewTaskStore(db), Audit: database.NewAuditLog(db), NodeID: nodeID, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	tsvc, err := templates.New(templates.Options{NodeID: nodeID, Store: database.NewTemplateStore(db), Engine: engine,
		Host: fake.Ops(), Packages: []string{"FreeBSD-set-minimal"}, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	tsvcSSH, err := templates.New(templates.Options{NodeID: nodeID, Store: database.NewTemplateStore(db), Engine: engine,
		Host: fake.Ops(), Packages: []string{"FreeBSD-set-minimal", "FreeBSD-ssh"}, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{NodeID: nodeID, Store: database.NewContainerStore(db), Templates: database.NewTemplateStore(db),
		Snapshots: database.NewSnapshotStore(db), MaxSnapshots: 4, Engine: engine, Host: fake.Ops(), Logger: quiet,
		PhysicalMemory: 16 << 30}
	for _, tweak := range tweaks {
		tweak(&opts)
	}
	svc, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range append(tsvc.Definitions(), svc.Definitions()...) {
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
	f := &fixture{t: t, db: db, fake: fake, engine: engine, svc: svc,
		sub: Submission{Actor: models.TaskActor{ID: "u-1", Name: "andy"}, PeerUID: 1001, RequestID: "req-1", SourceIP: "192.0.2.7"}}
	build := func(s *templates.Service, name string) string {
		created, err := s.CreateTemplate(ctx, models.TemplateCreate{Name: name, Pool: "devpool", Source: "pkgbase"},
			templates.Submission{Actor: models.TaskActor{ID: "u-1", Name: "andy"}, PeerUID: 1001})
		if err != nil {
			t.Fatal(err)
		}
		if task := f.wait(created.Task.ID); task.Status != models.TaskSucceeded {
			t.Fatalf("template build: %s %s", task.ErrorCode, task.ErrorMessage)
		}
		return created.Template.ID
	}
	f.minimal = build(tsvc, "minimal")
	f.withSSHD = build(tsvcSSH, "minimal-ssh")
	return f
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

func (f *fixture) succeed(task models.Task, err error) tasks.Task {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
	done := f.wait(task.ID)
	if done.Status != models.TaskSucceeded {
		f.t.Fatalf("%s: %s %s %s", task.Type, done.Status, done.ErrorCode, done.ErrorMessage)
	}
	return done
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var me *models.Error
	if !errors.As(err, &me) || me.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

func sshKey() string {
	typ := "ssh-ed25519"
	blob := binary.BigEndian.AppendUint32(nil, uint32(len(typ)))
	blob = append(blob, typ...)
	blob = binary.BigEndian.AppendUint32(blob, 32)
	blob = append(blob, make([]byte, 32)...)
	return typ + " " + base64.StdEncoding.EncodeToString(blob) + " test@skep"
}

func (f *fixture) create(req models.ContainerCreate) models.Container {
	f.t.Helper()
	created, err := f.svc.CreateContainer(context.Background(), req, f.sub)
	if err != nil {
		f.t.Fatal(err)
	}
	// The reply's container is a snapshot that can be read after the task
	// has finished, so its Provisioned is not asserted.
	if created.Task.Type != models.TaskContainerCreate {
		f.t.Fatalf("created = %+v", created)
	}
	f.succeed(created.Task, nil)
	c, err := f.svc.Container(context.Background(), created.Container.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func TestLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "web1", TemplateID: f.minimal})
	if !c.Provisioned || c.StartedOnce || c.Hostname != "web1" || c.State != models.ContainerStopped ||
		c.TemplateName != "minimal" || c.Pool != "devpool" || c.Release != hostfake.SimulatedRelease {
		t.Fatalf("after create: %+v", c)
	}
	if _, ok := f.fake.JailFiles(c.ID); ok {
		t.Fatal("files were written into a jail without an SSH key")
	}

	// Start: started_once becomes 1, the jail runs.
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	c, _ = f.svc.Container(ctx, c.ID)
	if c.State != models.ContainerRunning || !c.StartedOnce || c.JID == nil || c.DesiredState != models.ContainerRunning {
		t.Fatalf("after start: %+v", c)
	}
	// Refusals while running, each audited as denied.
	_, err := f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub)
	wantCode(t, err, models.CodeContainerNotStopped)
	_, err = f.svc.DeleteContainer(ctx, c.ID, f.sub)
	wantCode(t, err, models.CodeContainerNotStopped)
	host := "web2"
	_, err = f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Hostname: &host}, f.sub)
	wantCode(t, err, models.CodeContainerNotStopped)
	if n := f.count(`SELECT count(*) FROM audit_log WHERE outcome = 'denied' AND resource_id = ?`, c.ID); n != 3 {
		t.Fatalf("%d denials audited, want 3", n)
	}

	// Restart, stop with a timeout.
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "restart"}, f.sub))
	timeout := 30
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop", Timeout: &timeout}, f.sub))
	c, _ = f.svc.Container(ctx, c.ID)
	if c.State != models.ContainerStopped || c.JID != nil || c.DesiredState != models.ContainerStopped {
		t.Fatalf("after stop: %+v", c)
	}
	_, err = f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub)
	wantCode(t, err, models.CodeContainerNotRunning)

	// A hostname change after the first start is allowed (it is a jail
	// parameter, not a write into the tree).
	f.succeed(f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Hostname: &host}, f.sub))
	if c, _ = f.svc.Container(ctx, c.ID); c.Hostname != "web2" {
		t.Fatalf("hostname = %q", c.Hostname)
	}

	// Delete destroys the dataset and lists it in the result.
	done := f.succeed(f.svc.DeleteContainer(ctx, c.ID, f.sub))
	if !strings.Contains(string(done.Result), "skep/jail/"+c.ID) {
		t.Fatalf("delete result = %s", done.Result)
	}
	if _, err := f.svc.Container(ctx, c.ID); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	calls := strings.Join(f.fake.Calls(), "\n")
	for _, want := range []string{"zfs clone devpool/skep/templates/" + f.minimal + "@skep-base devpool/skep/jail/" + c.ID,
		"jail -c skep-" + c.ID, "jail -r skep-" + c.ID, "zfs destroy -r devpool/skep/jail/" + c.ID} {
		if !strings.Contains(calls, want) {
			t.Errorf("the host never saw %q", want)
		}
	}
}

func TestSSHKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := sshKey()

	// On a template without sshd: refused, audited, nothing created.
	_, err := f.svc.CreateContainer(ctx, models.ContainerCreate{Name: "nossh", TemplateID: f.minimal, SSHAuthorizedKey: key}, f.sub)
	wantCode(t, err, models.CodeContainerSSHUnavailable)
	if n := f.count(`SELECT count(*) FROM audit_log WHERE outcome = 'denied' AND action = ?`, models.TaskContainerCreate); n != 1 {
		t.Fatalf("%d denials audited", n)
	}
	if n := f.count(`SELECT count(*) FROM containers`); n != 0 {
		t.Fatalf("%d containers after a refusal", n)
	}

	// On a template with sshd: rc.conf and the key are written.
	c := f.create(models.ContainerCreate{Name: "ssh1", TemplateID: f.withSSHD, SSHAuthorizedKey: "  " + key + " "})
	files, ok := f.fake.JailFiles(c.ID)
	if !ok || files.AuthorizedKey != key || strings.Join(files.RCConf, "\n") != strings.Join(hostops.SSHRCConf, "\n") || c.SSHAuthorizedKey != key {
		t.Fatalf("files = %+v, %v; container key %q", files, ok, c.SSHAuthorizedKey)
	}
	// Before the first start the key can change and be removed.
	empty := ""
	f.succeed(f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{SSHAuthorizedKey: &empty}, f.sub))
	if files, _ := f.fake.JailFiles(c.ID); files.AuthorizedKey != "" || len(files.RCConf) != 0 {
		t.Fatalf("after removing the key: %+v", files)
	}
	f.succeed(f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{SSHAuthorizedKey: &key}, f.sub))
	// After the first start: container_started, audited, nothing written.
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))
	before := len(f.fake.Calls())
	_, err = f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{SSHAuthorizedKey: &empty}, f.sub)
	wantCode(t, err, models.CodeContainerStarted)
	for _, call := range f.fake.Calls()[before:] {
		if strings.HasPrefix(call, "configure jail") {
			t.Fatalf("wrote into a started jail: %s", call)
		}
	}
	// The same key again is no change, so not refused.
	f.succeed(f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{SSHAuthorizedKey: &key}, f.sub))

	// A key that is not a key, and a key with options.
	for _, bad := range []string{"nonsense", `command="/bin/sh" ` + key} {
		_, err := f.svc.CreateContainer(ctx, models.ContainerCreate{Name: "bad", TemplateID: f.withSSHD, SSHAuthorizedKey: bad}, f.sub)
		wantCode(t, err, models.CodeInvalidRequest)
	}
}

func TestCreateRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		req  models.ContainerCreate
		code string
	}{
		"bad name":         {models.ContainerCreate{Name: "../x", TemplateID: f.minimal}, models.CodeInvalidRequest},
		"name not a host":  {models.ContainerCreate{Name: "web_1", TemplateID: f.minimal}, models.CodeInvalidRequest},
		"bad hostname":     {models.ContainerCreate{Name: "w", Hostname: "a;b", TemplateID: f.minimal}, models.CodeInvalidRequest},
		"unknown template": {models.ContainerCreate{Name: "w", TemplateID: "9f2a9c1e-7b4d-4e8f-9a01-23456789abcd"}, models.CodeInvalidRequest},
		"template not ID":  {models.ContainerCreate{Name: "w", TemplateID: "minimal"}, models.CodeInvalidRequest},
		"mlock":            {models.ContainerCreate{Name: "w", TemplateID: f.minimal, Options: &models.ContainerOptions{Mlock: true}}, models.CodeMlockRequiresMemoryLimit},
	} {
		_, err := f.svc.CreateContainer(ctx, c.req, f.sub)
		var me *models.Error
		if !errors.As(err, &me) || me.Code != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	// An explicit hostname makes a name with '_' fine; a repeated name is
	// refused by the companion.
	f.create(models.ContainerCreate{Name: "web_1", Hostname: "web-1", TemplateID: f.minimal})
	created, err := f.svc.CreateContainer(ctx, models.ContainerCreate{Name: "web_1", Hostname: "web-1", TemplateID: f.minimal}, f.sub)
	wantCode(t, err, models.CodeContainerNameTaken)
	_ = created
	// sysvipc is accepted and reaches the jail.conf renderer.
	c := f.create(models.ContainerCreate{Name: "pg", TemplateID: f.minimal, Options: &models.ContainerOptions{SysVIPC: true}})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	timeout := 301
	_, err = f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop", Timeout: &timeout}, f.sub)
	wantCode(t, err, models.CodeInvalidRequest)
	_, err = f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "force_stop"}, f.sub)
	wantCode(t, err, models.CodeInvalidRequest)
}

func TestIdempotentCreate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sub := f.sub
	sub.IdempotencyKey = "k-1"
	req := models.ContainerCreate{Name: "web1", TemplateID: f.minimal}
	first, err := f.svc.CreateContainer(ctx, req, sub)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.CreateContainer(ctx, req, sub)
	if err != nil || again.Task.ID != first.Task.ID || again.Container.ID != first.Container.ID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	other := req
	other.Name = "web2"
	_, err = f.svc.CreateContainer(ctx, other, sub)
	wantCode(t, err, models.CodeIdempotencyConflict)
}

func TestObserve(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "web1", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	// Removed behind Skep's back: the next read sees it, with the drift
	// explained.
	f.fake.SetJailStopped(c.ID)
	list, err := f.svc.Containers(ctx, models.ContainerListOptions{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	got := list.Items[0]
	if got.State != models.ContainerStopped || got.DesiredState != models.ContainerRunning || !strings.Contains(got.StateDetail, "not running") {
		t.Fatalf("after an outside removal: %+v", got)
	}
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Containers(ctx, models.ContainerListOptions{Limit: 201}); err == nil {
		t.Fatal("a limit over the maximum was accepted")
	}
	log, err := f.svc.ConsoleLog(ctx, c.ID)
	if err != nil || log.Truncated {
		t.Fatalf("console log = %+v, %v", log, err)
	}
}

func TestTemplateInUse(t *testing.T) {
	f := newFixture(t)
	f.create(models.ContainerCreate{Name: "web1", TemplateID: f.minimal})
	// The template's dataset has a clone, and its row a container: it
	// cannot go while the jail exists.
	if err := database.NewTemplateStore(f.db).Delete(context.Background(), f.minimal); err == nil {
		t.Fatal("a template in use was deleted")
	}
}

// TestObserveAllSweepsLeftoverDevfs: at agent start a devfs left on the
// root of a jail that does not run is unmounted; a running jail's root is
// not touched (amendment l).
func TestObserveAllSweepsLeftoverDevfs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	stopped := f.create(models.ContainerCreate{Name: "stopped", TemplateID: f.minimal})
	running := f.create(models.ContainerCreate{Name: "running", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, running.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.fake.LeaveDevfs(stopped.ID)
	f.fake.LeaveDevfs(running.ID)
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if f.fake.HasLeftoverDevfs(stopped.ID) {
		t.Fatal("the stopped jail's leftover devfs is still there")
	}
	if !f.fake.HasLeftoverDevfs(running.ID) {
		t.Fatal("the sweep touched a running jail's root")
	}
}

func TestObserveAllSweepsHalfStartedJail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	half := f.create(models.ContainerCreate{Name: "half", TemplateID: f.minimal})
	whole := f.create(models.ContainerCreate{Name: "whole", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, half.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.succeed(f.svc.Action(ctx, whole.ID, models.ContainerAction{Action: "start"}, f.sub))
	// The agent died between jail -c and the end of /etc/rc: the jail is
	// there, and the container is still recorded starting.
	if err := database.NewContainerStore(f.db).MarkStarting(ctx, half.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	running, err := f.fake.Ops().Jails.RunningJails(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := running[half.ID]; ok {
		t.Fatal("the half-started jail is still running")
	}
	if _, ok := running[whole.ID]; !ok {
		t.Fatal("a running jail was removed")
	}
	if !slices.Contains(f.fake.Calls(), "jail -r skep-"+half.ID+" (no exec.stop)") {
		t.Fatalf("not removed without exec.stop: %q", f.fake.Calls())
	}
	got, err := f.svc.Container(ctx, half.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != models.ContainerStopped || got.JID != nil || !strings.Contains(got.StateDetail, "interrupted by an agent restart") {
		t.Fatalf("after the sweep: %+v", got)
	}
	if got, _ := f.svc.Container(ctx, whole.ID); got.State != models.ContainerRunning {
		t.Fatalf("the whole jail: %+v", got)
	}
}

// fileConsoleLog is the fake host's jails with the console log read from
// a real file, as the FreeBSD host reads it: its last maxBytes bytes.
type fileConsoleLog struct {
	hostops.Jails
	path string
}

func (j fileConsoleLog) ConsoleLogTail(_ string, maxBytes int) (string, error) {
	b, err := os.ReadFile(j.path)
	if err != nil {
		return "", err
	}
	if len(b) > maxBytes {
		b = b[len(b)-maxBytes:]
	}
	return string(b), nil
}

// TestConsoleLogIsCleanedWhenServed: the console log as served has no
// format or control characters but newline and tab, while console.log on
// disk keeps every byte the jail wrote (amendment r).
func TestConsoleLogIsCleanedWhenServed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	f := newFixture(t, func(o *Options) { o.Host.Jails = fileConsoleLog{Jails: o.Host.Jails, path: path} })
	c := f.create(models.ContainerCreate{Name: "web1", TemplateID: f.minimal})
	raw := "Starting syslogd.\r\n\x1b[31mfake \u202eok\u202c\x07\n\tindented\u2066x\u2069\u200b\n" +
		"c1 \u009b2J here\u0085\n--- shutdown stderr ---\nlast\x7f line\ufeff\n"
	want := "Starting syslogd.\n[31mfake ok\n\tindentedx\nc1 2J here\n--- shutdown stderr ---\nlast line\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	log, err := f.svc.ConsoleLog(ctx, c.ID)
	if err != nil || log.Text != want || log.Truncated {
		t.Fatalf("console log = %+v, %v\nwant %q", log, err, want)
	}
	// What the API hands out is the JSON of that: no escape for a control
	// character other than \n and \t, and no format character.
	body, err := json.Marshal(log)
	if err != nil {
		t.Fatal(err)
	}
	var back models.ContainerConsoleLog
	if err := json.Unmarshal(body, &back); err != nil || back.Text != want {
		t.Fatalf("round trip = %q, %v", back.Text, err)
	}
	for _, r := range back.Text {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			t.Fatalf("the served log kept %U", r)
		}
	}
	// The file is unchanged.
	if disk, err := os.ReadFile(path); err != nil || string(disk) != raw {
		t.Fatalf("console.log on disk changed: %q, %v", disk, err)
	}
	for _, kept := range []string{"\x1b", "\u202e", "\u009b", "\r", "\ufeff"} {
		if disk, _ := os.ReadFile(path); !strings.Contains(string(disk), kept) {
			t.Fatalf("console.log on disk lost %q", kept)
		}
	}

	// Truncated is about the file: a full-size tail is truncated even when
	// the cleaning made the text shorter.
	// The last 64 KiB hold ten U+202E (3 bytes each).
	full := "older output\n" + strings.Repeat("x", ConsoleLogMax-30) + strings.Repeat("\u202e", 10)
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err = f.svc.ConsoleLog(ctx, c.ID)
	if err != nil || !log.Truncated || len(log.Text) != ConsoleLogMax-30 || strings.ContainsRune(log.Text, '\u202e') {
		t.Fatalf("full-size log: truncated %v, %d bytes, %v", log.Truncated, len(log.Text), err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("\u202e", 100)+"short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if log, err = f.svc.ConsoleLog(ctx, c.ID); err != nil || log.Truncated || log.Text != "short\n" {
		t.Fatalf("short log = %+v, %v", log, err)
	}
}

// The system document reports the host's locked-memory total in effect:
// the setting when there is one, otherwise a quarter of physical memory.
func TestCapabilitiesReportLockedMemoryTotal(t *testing.T) {
	ctx := context.Background()
	caps, err := newFixture(t).svc.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if caps.MaxLockedMemoryBytes != 4<<30 {
		t.Fatalf("default total = %d, want a quarter of 16 GiB", caps.MaxLockedMemoryBytes)
	}
	caps, err = newFixture(t, func(o *Options) { o.MaxLockedMemory = 1 << 30 }).svc.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if caps.MaxLockedMemoryBytes != 1<<30 {
		t.Fatalf("configured total = %d", caps.MaxLockedMemoryBytes)
	}
}

// F7 of the 8d-1 review: the API's name check (models.ValidGuestName) is
// this package's rule.
func TestNameRuleIsTheModels(t *testing.T) {
	names := []string{"", "a b", "web1", "Web.1_x-y", strings.Repeat("a", 62), strings.Repeat("a", 63), strings.Repeat("a", 64), "é", "aé"}
	for b := 0; b < 256; b++ {
		names = append(names, string([]byte{byte(b)}), "a"+string([]byte{byte(b)}))
	}
	for _, name := range names {
		if nameRE.MatchString(name) != models.ValidGuestName(name) {
			t.Errorf("%q: containers %v, models %v", name, nameRE.MatchString(name), models.ValidGuestName(name))
		}
	}
}
