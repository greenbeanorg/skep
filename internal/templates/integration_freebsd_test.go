//go:build freebsd

package templates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/pkg/models"
)

// The native template checks of ADR 0010 step 6, run on hearst as root
// with the real hostops. They are skipped unless SKEP_IT_TEMPLATE_POOL
// and SKEP_IT_TEMPLATE_ROOT name an allow-listed test pool and a dataset
// root in it (on hearst: skeptest and step6), because they build into the
// pool and fetch packages from the host's pkgbase repositories.
//
//	sudo env SKEP_IT_TEMPLATE_POOL=skeptest SKEP_IT_TEMPLATE_ROOT=step6 \
//	    ./templates.test -test.v -test.run Integration
//
// TestIntegrationBuild leaves the template it built in place for the
// comparison with the traced build; TestIntegrationRefusal leaves nothing.

func integrationOps(t *testing.T) (hostops.Ops, string) {
	t.Helper()
	pool, root := os.Getenv("SKEP_IT_TEMPLATE_POOL"), os.Getenv("SKEP_IT_TEMPLATE_ROOT")
	if pool == "" || root == "" {
		t.Skip("SKEP_IT_TEMPLATE_POOL and SKEP_IT_TEMPLATE_ROOT are not set")
	}
	if os.Geteuid() != 0 {
		t.Fatal("the template integration checks run as root")
	}
	dir := t.TempDir()
	ops, err := hostops.New(hostops.Confinement{Pools: []string{pool}, DatasetRoot: root,
		RunDir: filepath.Join(dir, "run"), StateDir: filepath.Join(dir, "state")},
		hostops.Options{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return ops, pool
}

func progressLog(t *testing.T) Progress {
	start := time.Now()
	return func(msg string) { t.Logf("[%6.1fs] %s", time.Since(start).Seconds(), msg) }
}

// noBuildJails fails if any skep-build-* jail exists.
func noBuildJails(t *testing.T, ops hostops.Ops) {
	t.Helper()
	jails, err := ops.Templates.BuildJails(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jails) != 0 {
		t.Errorf("build jails left: %v", jails)
	}
	out, _ := exec.Command("/usr/sbin/jls", "name").CombinedOutput()
	t.Logf("jls name: %q", strings.TrimSpace(string(out)))
}

func TestIntegrationBuild(t *testing.T) {
	ops, pool := integrationOps(t)
	id, err := ids.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("template ID %s", id)
	b := &Builder{Host: ops.Templates, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()
	res, err := b.Build(ctx, BuildSpec{Pool: pool, TemplateID: id, Source: models.TemplateSourcePkgbase,
		Packages: []string{"FreeBSD-set-minimal"}}, progressLog(t))
	if err != nil {
		var me *models.Error
		if errors.As(err, &me) {
			t.Fatalf("build failed: %s: %s %v", me.Code, me.Message, me.Details)
		}
		t.Fatalf("build failed: %v", err)
	}
	t.Logf("result: %+v", res)
	fmt.Printf("TEMPLATE_ID=%s\n", id)

	noBuildJails(t, ops)
	p, err := ops.Templates.TemplatePaths(context.Background(), pool, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"var/db/pkg/triggers", "var/db/pkg/repos", "var/cache/pkg"} {
		entries, err := os.ReadDir(filepath.Join(p.Root, rel))
		switch {
		case errors.Is(err, os.ErrNotExist):
			t.Logf("%s: absent", rel)
		case err != nil:
			t.Error(err)
		case len(entries) > 0:
			t.Errorf("%s holds %d entries", rel, len(entries))
		default:
			t.Logf("%s: empty", rel)
		}
	}
	if _, err := os.Lstat(p.Work); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the work area %s is still there: %v", p.Work, err)
	}
	for _, args := range [][]string{
		{"get", "-H", "-o", "property,value", "readonly,mounted,mountpoint,skep:template_id,used", p.Dataset},
		{"list", "-H", "-t", "snapshot", "-o", "name,used,skep:template_id", p.Dataset},
	} {
		out, err := exec.Command("/sbin/zfs", args...).CombinedOutput()
		t.Logf("zfs %s:\n%s", strings.Join(args, " "), out)
		if err != nil {
			t.Error(err)
		}
	}
	if !strings.HasPrefix(res.Release, "15.1-RELEASE") {
		t.Errorf("release %q", res.Release)
	}
	if res.HostFiles < 1000 {
		t.Errorf("only %d host files were compared", res.HostFiles)
	}
}

// TestIntegrationRefusal changes the mandoc trigger file in the template
// after pkg installed it, as a changed package would, and checks that the
// build is refused with template_script_unknown and that nothing is left.
func TestIntegrationRefusal(t *testing.T) {
	ops, pool := integrationOps(t)
	id, err := ids.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("template ID %s", id)
	b := &Builder{Host: ops.Templates, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	b.afterInstall = func(_ context.Context, p hostops.TemplatePaths) error {
		f, err := os.OpenFile(filepath.Join(p.Root, "usr/share/pkg/triggers/mandoc.ucl"), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString("-- changed by TestIntegrationRefusal\n")
		t.Logf("appended a line to %s/usr/share/pkg/triggers/mandoc.ucl", p.Root)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()
	_, err = b.Build(ctx, BuildSpec{Pool: pool, TemplateID: id, Source: models.TemplateSourcePkgbase,
		Packages: []string{"FreeBSD-set-minimal"}}, progressLog(t))
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeTemplateScriptUnknown {
		t.Fatalf("err = %v, want template_script_unknown", err)
	}
	t.Logf("refused: %s: %s", me.Code, me.Message)
	t.Logf("details: path=%v sha256=%v", me.Details["path"], me.Details["sha256"])
	text, _ := me.Details["text"].(string)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	t.Logf("text (%d bytes, last line): %q", len(text), lines[len(lines)-1])

	noBuildJails(t, ops)
	ds, _ := ops.Conf.TemplateDataset(pool, id)
	out, err := exec.Command("/sbin/zfs", "list", "-H", ds).CombinedOutput()
	if err == nil {
		t.Errorf("the template dataset is still there: %s", out)
	} else {
		t.Logf("zfs list %s: %s", ds, strings.TrimSpace(string(out)))
	}
	parent, _ := ops.Conf.TemplatesDataset(pool)
	mp, err := exec.Command("/sbin/zfs", "get", "-H", "-o", "value", "mountpoint", parent).Output()
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(strings.TrimSpace(string(mp)), ".work-"+id)
	if _, err := os.Lstat(work); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the work area %s is still there: %v", work, err)
	}
}
