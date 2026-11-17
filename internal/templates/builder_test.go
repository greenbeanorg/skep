package templates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

const tpl = "3f2a9c1e-7b4d-4e8f-9a01-23456789abcd"

// fakeHost is hostops.Templates as the builder sees it. It records every
// call and fails where a test says.
type fakeHost struct {
	calls       []string
	hostRelease string
	tplRelease  string
	pkgs        []hostops.PackageScripts
	triggers    []hostops.TriggerFile
	deferred    []string
	hostKeys    []string
	cache       []string
	// hostFiles are returned in order, one per HostFiles call; the last
	// repeats.
	hostFiles []hostops.HostFileState
	fail      map[string]error
	// cancel, when set, is called when the named step runs.
	cancelAt string
	cancel   context.CancelFunc
	jailUp   bool
	exists   bool
	// ctxErrs records whether each call's context was already done.
	ctxErrs map[string]error
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		hostRelease: "15.1-RELEASE-p6", tplRelease: "15.1-RELEASE-p6", pkgs: minimalSet(),
		triggers:  []hostops.TriggerFile{{Path: "usr/share/pkg/triggers/mandoc.ucl", Regular: true, Content: []byte("m")}},
		hostFiles: []hostops.HostFileState{{"/etc/rc.conf": "a"}},
		fail:      map[string]error{}, ctxErrs: map[string]error{},
	}
}

func (f *fakeHost) step(ctx context.Context, name string) error {
	f.calls = append(f.calls, name)
	f.ctxErrs[name] = ctx.Err()
	if f.cancelAt == name && f.cancel != nil {
		f.cancel()
		return ctx.Err()
	}
	return f.fail[name]
}

func (f *fakeHost) HostRelease(ctx context.Context) (string, error) {
	return f.hostRelease, f.step(ctx, "HostRelease")
}
func (f *fakeHost) PkgbaseAvailable() (bool, string) {
	return f.fail["PkgbaseAvailable"] == nil, "no pkg"
}
func (f *fakeHost) DistAvailable() (bool, string) {
	return f.fail["DistAvailable"] == nil, "no base.txz"
}
func (f *fakeHost) EnsureTemplatesParent(ctx context.Context, _ string) error {
	return f.step(ctx, "EnsureTemplatesParent")
}
func (f *fakeHost) CheckTemplatesParent(ctx context.Context, _ string) error {
	return f.step(ctx, "CheckTemplatesParent")
}
func (f *fakeHost) PrepareTemplate(ctx context.Context, _, _ string) (hostops.TemplatePaths, error) {
	if err := f.step(ctx, "PrepareTemplate"); err != nil {
		return hostops.TemplatePaths{}, err
	}
	f.exists = true
	return hostops.TemplatePaths{Root: "/t/" + tpl}, nil
}
func (f *fakeHost) TemplatePaths(context.Context, string, string) (hostops.TemplatePaths, error) {
	return hostops.TemplatePaths{}, nil
}
func (f *fakeHost) RemoveWorkArea(ctx context.Context, _, _ string) error {
	return f.step(ctx, "RemoveWorkArea")
}
func (f *fakeHost) FinishTemplate(ctx context.Context, _, _ string) (hostops.Dataset, error) {
	return hostops.Dataset{Name: "skeptest/skep/templates/" + tpl, UsedBytes: 132 << 20}, f.step(ctx, "FinishTemplate")
}
func (f *fakeHost) DestroyTemplate(ctx context.Context, _, _ string) ([]hostops.Dataset, error) {
	err := f.step(ctx, "DestroyTemplate")
	if err == nil {
		f.exists = false
	}
	return nil, err
}
func (f *fakeHost) PkgInstall(ctx context.Context, _, _ string, pkgs []string) error {
	return f.step(ctx, "PkgInstall "+strings.Join(pkgs, ","))
}
func (f *fakeHost) PkgScripts(ctx context.Context, _, _ string) ([]hostops.PackageScripts, error) {
	return f.pkgs, f.step(ctx, "PkgScripts")
}
func (f *fakeHost) TriggerFiles(ctx context.Context, _, _ string) ([]hostops.TriggerFile, error) {
	return f.triggers, f.step(ctx, "TriggerFiles")
}
func (f *fakeHost) TemplateRelease(ctx context.Context, _, _ string) (string, error) {
	return f.tplRelease, f.step(ctx, "TemplateRelease")
}
func (f *fakeHost) DeferredTriggers(ctx context.Context, _, _ string) ([]string, error) {
	return f.deferred, f.step(ctx, "DeferredTriggers")
}
func (f *fakeHost) SSHHostKeys(ctx context.Context, _, _ string) ([]string, error) {
	return f.hostKeys, f.step(ctx, "SSHHostKeys")
}
func (f *fakeHost) RemoveCatalogues(ctx context.Context, _, _ string) error {
	return f.step(ctx, "RemoveCatalogues")
}
func (f *fakeHost) CacheEntries(ctx context.Context, _, _ string) ([]string, error) {
	return f.cache, f.step(ctx, "CacheEntries")
}
func (f *fakeHost) ExtractDist(ctx context.Context, _, _ string) (string, error) {
	return strings.Repeat("ab", 32), f.step(ctx, "ExtractDist")
}
func (f *fakeHost) StartBuildJail(ctx context.Context, _, _ string) error {
	f.jailUp = true // jail -c may succeed before the check fails
	return f.step(ctx, "StartBuildJail")
}
func (f *fakeHost) BuildJailExec(ctx context.Context, _, _ string, c hostops.BuildCommand) (bool, error) {
	err := f.step(ctx, "exec "+c.String())
	return err == nil, err
}
func (f *fakeHost) RemoveBuildJail(ctx context.Context, _ string) error {
	err := f.step(ctx, "RemoveBuildJail")
	if err == nil {
		f.jailUp = false
	}
	return err
}
func (f *fakeHost) BuildJails(context.Context) ([]string, error) { return nil, nil }
func (f *fakeHost) HostFiles(ctx context.Context) (hostops.HostFileState, error) {
	n := 0
	for _, c := range f.calls {
		if c == "HostFiles" {
			n++
		}
	}
	err := f.step(ctx, "HostFiles")
	return f.hostFiles[min(n, len(f.hostFiles)-1)], err
}

func testBuilder(f *fakeHost) *Builder {
	// The fake's mandoc.ucl content stands in for the real file.
	return &Builder{Host: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func withFakeTrigger(t *testing.T) {
	t.Helper()
	saved := allowlists["15.1"]
	a := saved
	a.Triggers = []TriggerEntry{{Path: "usr/share/pkg/triggers/mandoc.ucl", SHA256: sum([]byte("m")),
		Commands: []hostops.BuildCommand{hostops.BuildMakewhatisMan, hostops.BuildMakewhatisOpenSSL}}}
	allowlists["15.1"] = a
	t.Cleanup(func() { allowlists["15.1"] = saved })
}

func pkgbaseSpec() BuildSpec {
	return BuildSpec{Pool: "skeptest", TemplateID: tpl, Source: models.TemplateSourcePkgbase, Packages: []string{"FreeBSD-set-minimal"}}
}

func code(err error) string {
	var me *models.Error
	if errors.As(err, &me) {
		return me.Code
	}
	return ""
}

// TestBuildSequence checks the order of the decision: parent, host
// hashes, dataset, install, scripts, triggers, release, jail and the
// fixed commands, deferred triggers, catalogues and cache, host hashes,
// work area, snapshot.
func TestBuildSequence(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	res, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"HostRelease", "EnsureTemplatesParent", "HostFiles", "PrepareTemplate", "PkgInstall FreeBSD-set-minimal",
		"PkgScripts", "TriggerFiles", "TemplateRelease", "StartBuildJail",
		"exec /usr/sbin/certctl rehash",
		"exec /usr/sbin/pwd_mkdb -i -p -d /etc /etc/master.passwd",
		"exec /usr/sbin/services_mkdb -l -q -o /var/db/services.db /etc/services",
		"exec /usr/bin/cap_mkdb -l /etc/login.conf",
		"exec /usr/bin/makewhatis /usr/share/man",
		"exec /usr/bin/makewhatis /usr/share/openssl/man",
		"RemoveBuildJail", "DeferredTriggers", "SSHHostKeys", "RemoveCatalogues", "CacheEntries", "HostFiles",
		"RemoveWorkArea", "FinishTemplate",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls =\n%s\nwant\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if res.Release != "15.1-RELEASE-p6" || res.PatchDiffers || res.Packages != 4 || res.ScriptsAllowed != 4 ||
		len(res.Commands) != 6 || res.HostFiles != 1 || res.Snapshot != "skeptest/skep/templates/"+tpl+"@skep-base" {
		t.Fatalf("result = %+v", res)
	}
	if f.jailUp || !f.exists {
		t.Fatalf("jail up %v, template exists %v", f.jailUp, f.exists)
	}
}

func TestBuildRunsOnlyTheCommandsAskedFor(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.pkgs = []hostops.PackageScripts{f.pkgs[0]} // FreeBSD-runtime alone
	f.triggers = nil
	res, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/sbin/pwd_mkdb -i -p -d /etc /etc/master.passwd", "/usr/sbin/services_mkdb -l -q -o /var/db/services.db /etc/services"}
	if !reflect.DeepEqual(res.Commands, want) {
		t.Fatalf("commands = %v", res.Commands)
	}
}

// assertCleanedUp checks what every failure must leave: no jail, no
// template, and the host files compared after the clean-up.
func assertCleanedUp(t *testing.T, f *fakeHost) {
	t.Helper()
	if f.jailUp {
		t.Error("the build jail was left")
	}
	if f.exists {
		t.Error("the template dataset was left")
	}
	if f.calls[len(f.calls)-1] != "HostFiles" {
		t.Errorf("the host files were not compared last: %v", f.calls)
	}
}

func TestBuildRefusesUnknownScript(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.pkgs[3].Scripts["post-install"] = "cap_mkdb -l /etc/login.conf; touch /etc/owned"
	_, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	if code(err) != models.CodeTemplateScriptUnknown {
		t.Fatalf("err = %v", err)
	}
	assertCleanedUp(t, f)
	for _, c := range f.calls {
		if c == "StartBuildJail" || strings.HasPrefix(c, "exec ") {
			t.Fatalf("%s ran after the refusal", c)
		}
	}
}

func TestBuildRefusesUnknownTrigger(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.triggers[0].Content = []byte("changed")
	_, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	if code(err) != models.CodeTemplateScriptUnknown {
		t.Fatalf("err = %v", err)
	}
	assertCleanedUp(t, f)
}

func TestBuildAfterInstallHook(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	b := testBuilder(f)
	b.afterInstall = func(context.Context, hostops.TemplatePaths) error {
		f.triggers[0].Content = []byte("tampered after install")
		return nil
	}
	if _, err := b.Build(context.Background(), pkgbaseSpec(), nil); code(err) != models.CodeTemplateScriptUnknown {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildCommandFailure(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.fail["exec /usr/bin/cap_mkdb -l /etc/login.conf"] = &hostops.CommandError{Name: "jexec", ExitCode: 1,
		Stderr: "cap_mkdb: /etc/login.conf: No such file or directory", Err: errors.New("exit status 1")}
	_, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeTemplateBuildFailed ||
		!strings.Contains(me.Message, "cap_mkdb: /etc/login.conf: No such file or directory") {
		t.Fatalf("err = %v", err)
	}
	assertCleanedUp(t, f)
}

func TestBuildCancelStillCleansUp(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancelAt, f.cancel = "exec /usr/sbin/pwd_mkdb -i -p -d /etc /etc/master.passwd", cancel
	_, err := testBuilder(f).Build(ctx, pkgbaseSpec(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	assertCleanedUp(t, f)
	// The clean-up ran on a live context of its own.
	for _, c := range []string{"RemoveBuildJail", "DestroyTemplate"} {
		if f.ctxErrs[c] != nil {
			t.Errorf("%s ran on a done context", c)
		}
	}
}

func TestBuildJailCheckFailureRemovesJail(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.fail["StartBuildJail"] = errors.New("hostops: the build jail does not have the required parameters: allow.suser=true")
	if _, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil); err == nil {
		t.Fatal("no error")
	}
	assertCleanedUp(t, f)
}

func TestBuildJailThatWillNotGo(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.fail["DeferredTriggers"] = &models.Error{Code: models.CodeTemplateTriggerLeft, Message: "x"}
	f.deferred = []string{"var/db/pkg/triggers/x"}
	f.fail["DeferredTriggers"] = nil
	f.fail["exec /usr/bin/makewhatis /usr/share/man"] = &hostops.CommandError{Name: "jexec", ExitCode: 1, Stderr: "boom"}
	f.fail["RemoveBuildJail"] = errors.New("still there after jail -r")
	_, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeTemplateBuildFailed || me.Details["cleanup_error"] == nil ||
		!strings.Contains(me.Message, "still there") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRefusesNewerRelease(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.tplRelease = "15.2-RELEASE"
	if _, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil); code(err) != models.CodeReleaseUnsupported {
		t.Fatalf("err = %v", err)
	}
	assertCleanedUp(t, f)
	// A newer patch level builds, and is flagged.
	f = newFakeHost()
	f.tplRelease = "15.1-RELEASE-p7"
	res, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	if err != nil || !res.PatchDiffers {
		t.Fatalf("p7 on p6: %+v, %v", res, err)
	}
}

func TestBuildDeferredTriggerLeft(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.deferred = []string{"var/db/pkg/triggers/oR3hHb5PjH"}
	_, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeTemplateTriggerLeft || fmt.Sprint(me.Details["files"]) != "[var/db/pkg/triggers/oR3hHb5PjH]" {
		t.Fatalf("err = %v", err)
	}
	assertCleanedUp(t, f)
}

func TestBuildRefusesSSHHostKeys(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.hostKeys = []string{"etc/ssh/ssh_host_ed25519_key", "etc/ssh/ssh_host_ed25519_key.pub"}
	_, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeTemplateBuildFailed || !strings.Contains(me.Message, "SSH host keys") ||
		fmt.Sprint(me.Details["files"]) != "[etc/ssh/ssh_host_ed25519_key etc/ssh/ssh_host_ed25519_key.pub]" {
		t.Fatalf("err = %v", err)
	}
	assertCleanedUp(t, f)
}

func TestBuildCacheNotEmpty(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.cache = []string{"var/cache/pkg/x.pkg"}
	if _, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil); err == nil {
		t.Fatal("a non-empty cache was accepted")
	}
	assertCleanedUp(t, f)
}

func TestBuildHostChanged(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.hostFiles = []hostops.HostFileState{{"/etc/rc.conf": "a"}, {"/etc/rc.conf": "b", "/etc/new": "c"}}
	_, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeTemplateHostChanged ||
		!reflect.DeepEqual(me.Details["paths"], []string{"/etc/new", "/etc/rc.conf"}) {
		t.Fatalf("err = %v", err)
	}
	assertCleanedUp(t, f)
}

// A failed build that also changed the host reports the host change, with
// the build's own error in the details.
func TestBuildFailureAndHostChanged(t *testing.T) {
	withFakeTrigger(t)
	f := newFakeHost()
	f.hostFiles = []hostops.HostFileState{{"/etc/rc.conf": "a"}, {"/etc/rc.conf": "b"}}
	f.fail["PkgInstall FreeBSD-set-minimal"] = &hostops.CommandError{Name: "pkg", ExitCode: 3, Stderr: "pkg: No packages available"}
	_, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeTemplateHostChanged ||
		!strings.Contains(fmt.Sprint(me.Details["build_error"]), "No packages available") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildRefusesBeforeTouchingTheHost(t *testing.T) {
	withFakeTrigger(t)
	for name, mutate := range map[string]func(f *fakeHost) string{
		"no allowlist": func(f *fakeHost) string { f.hostRelease = "15.2-RELEASE"; return models.CodeReleaseUnsupported },
		"no pkg": func(f *fakeHost) string {
			f.fail["PkgbaseAvailable"] = errors.New("x")
			return models.CodeStorageFeatureUnsupported
		},
		"bad parent": func(f *fakeHost) string {
			f.fail["EnsureTemplatesParent"] = &models.Error{Code: models.CodeJailParentInsecure}
			return models.CodeJailParentInsecure
		},
	} {
		f := newFakeHost()
		want := mutate(f)
		_, err := testBuilder(f).Build(context.Background(), pkgbaseSpec(), nil)
		if code(err) != want {
			t.Errorf("%s: err = %v, want %s", name, err, want)
		}
		for _, c := range f.calls {
			if c == "PrepareTemplate" || c == "DestroyTemplate" {
				t.Errorf("%s: %s ran", name, c)
			}
		}
	}
}

func TestBuildDist(t *testing.T) {
	f := newFakeHost()
	spec := BuildSpec{Pool: "skeptest", TemplateID: tpl, Source: models.TemplateSourceDist}
	res, err := testBuilder(f).Build(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"HostRelease", "EnsureTemplatesParent", "HostFiles", "PrepareTemplate", "ExtractDist", "TemplateRelease",
		"DeferredTriggers", "SSHHostKeys", "RemoveCatalogues", "CacheEntries", "HostFiles", "RemoveWorkArea", "FinishTemplate"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %v", f.calls)
	}
	if res.ArchiveSHA256 == "" || len(res.Commands) != 0 {
		t.Fatalf("result = %+v", res)
	}
	f = newFakeHost()
	f.fail["ExtractDist"] = &models.Error{Code: models.CodeTemplateArchiveMismatch}
	if _, err := testBuilder(f).Build(context.Background(), spec, nil); code(err) != models.CodeTemplateArchiveMismatch {
		t.Fatalf("mismatch: %v", err)
	}
	assertCleanedUp(t, f)
	f = newFakeHost()
	f.tplRelease = "16.0-RELEASE"
	if _, err := testBuilder(f).Build(context.Background(), spec, nil); code(err) != models.CodeReleaseUnsupported {
		t.Fatalf("dist release: %v", err)
	}
}
