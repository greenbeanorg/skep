package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakePW is a pw(8) stand-in for the package script tests. It keeps users
// and groups as passwd-style lines in $PWSTATE/{users,groups} and logs
// every call to $PWSTATE/calls. Only the forms the scripts use exist.
const fakePW = `#!/bin/sh
st="$PWSTATE"
echo "pw $*" >> "$st/calls"
cmd="$1"; shift
case "$cmd" in
usershow)
	if [ "$1" = "-u" ]; then awk -F: -v id="$2" '$3==id' "$st/users" | grep . ; exit $?; fi
	awk -F: -v n="$1" '$1==n' "$st/users" | grep . ; exit $? ;;
groupshow)
	if [ "$1" = "-g" ]; then awk -F: -v id="$2" '$3==id' "$st/groups" | grep . ; exit $?; fi
	awk -F: -v n="$1" '$1==n' "$st/groups" | grep . ; exit $? ;;
groupadd)
	[ -n "$PWFAIL_GROUPADD" ] && exit 1
	echo "$1:*:$3:" >> "$st/groups" ;;
useradd)
	[ -n "$PWFAIL_USERADD" ] && exit 1
	echo "$1:*:$3:$5::0:0:x:/nonexistent:/usr/sbin/nologin" >> "$st/users" ;;
groupdel)
	grep -v "^$1:" "$st/groups" > "$st/g.tmp"; mv "$st/g.tmp" "$st/groups" ;;
*) exit 64 ;;
esac
`

type scriptEnv struct {
	t     *testing.T
	bin   string
	state string
	vmm   string
	env   []string
}

func newScriptEnv(t *testing.T, users, groups, jails string) *scriptEnv {
	t.Helper()
	s := &scriptEnv{t: t, bin: t.TempDir(), state: t.TempDir(), vmm: filepath.Join(t.TempDir(), "vmm")}
	write := func(p, data string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(s.bin, "pw"), fakePW, 0o755)
	write(filepath.Join(s.bin, "jls"), "#!/bin/sh\nprintf '%s' \"$FAKE_JLS\"\n", 0o755)
	write(filepath.Join(s.state, "users"), users, 0o644)
	write(filepath.Join(s.state, "groups"), groups, 0o644)
	write(filepath.Join(s.state, "calls"), "", 0o644)
	s.env = []string{
		// The scripts set their own PATH; this one must not matter.
		"PATH=/nonexistent",
		"PWSTATE=" + s.state,
		"FAKE_JLS=" + jails,
		"SKEP_VMM_DIR=" + s.vmm,
	}
	return s
}

// scriptPATH is the PATH line both package scripts set.
const scriptPATH = "\nPATH=/sbin:/bin:/usr/sbin:/usr/bin\nexport PATH\n"

// run runs a copy of the script whose fixed PATH line is replaced by one
// that finds the fakes first (the only change), with s.env.
func (s *scriptEnv) run(script string, extra ...string) (int, string) {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(s.t), "packaging/pkg", script))
	if err != nil {
		s.t.Fatal(err)
	}
	if strings.Count(string(b), scriptPATH) != 1 {
		s.t.Fatalf("%s does not set its PATH (%q) exactly once", script, scriptPATH)
	}
	copyPath := filepath.Join(s.t.TempDir(), script)
	body := strings.Replace(string(b), scriptPATH, "\nPATH="+s.bin+":/usr/bin:/bin\nexport PATH\n", 1)
	if err := os.WriteFile(copyPath, []byte(body), 0o755); err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command("sh", copyPath)
	cmd.Env = append(append([]string{}, s.env...), extra...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatal(err)
	}
	return code, string(out)
}

func (s *scriptEnv) file(name string) string {
	b, err := os.ReadFile(filepath.Join(s.state, name))
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

// Lines of an existing account, for the tests' passwd and group files.
const (
	skepUser     = "skep:*:409:409::0:0:x:/nonexistent:/usr/sbin/nologin\n"
	skepGroup    = "skep:*:409:\n"
	clusterUser  = "skep-cluster:*:410:410::0:0:x:/nonexistent:/usr/sbin/nologin\n"
	clusterGroup = "skep-cluster:*:410:\n"
)

func TestPreInstallCreatesAccount409(t *testing.T) {
	s := newScriptEnv(t, "root:*:0:0::0:0:Charlie &:/root:/bin/sh\n", "wheel:*:0:root\n", "")
	code, out := s.run("pre-install.sh")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	calls := s.file("calls")
	if !strings.Contains(calls, "pw groupadd skep -g 409\n") ||
		!strings.Contains(calls, "pw useradd skep -u 409 -g skep -d /nonexistent -s /usr/sbin/nologin -c Skep service account\n") {
		t.Fatalf("calls:\n%s", calls)
	}
	if !strings.Contains(s.file("users"), "skep:*:409:") || !strings.Contains(s.file("groups"), "skep:*:409:") {
		t.Fatal("account not created")
	}
	// The cluster service's account, UID and GID 410 (ADR 0013 B1).
	if !strings.Contains(calls, "pw groupadd skep-cluster -g 410\n") ||
		!strings.Contains(calls, "pw useradd skep-cluster -u 410 -g skep-cluster -d /nonexistent -s /usr/sbin/nologin -c Skep cluster service account\n") {
		t.Fatalf("calls:\n%s", calls)
	}
	if !strings.Contains(s.file("users"), "skep-cluster:*:410:") || !strings.Contains(s.file("groups"), "skep-cluster:*:410:") {
		t.Fatal("skep-cluster account not created")
	}
}

// An upgrade from 0.1: the skep account exists, skep-cluster does not.
func TestPreInstallAddsClusterAccountOnUpgrade(t *testing.T) {
	s := newScriptEnv(t, skepUser, skepGroup, "")
	code, out := s.run("pre-install.sh")
	if code != 0 || strings.Contains(out, "warning") {
		t.Fatalf("exit %d: %s", code, out)
	}
	calls := s.file("calls")
	if strings.Contains(calls, "groupadd skep ") || strings.Contains(calls, "useradd skep ") {
		t.Fatalf("touched the existing skep account:\n%s", calls)
	}
	if !strings.Contains(s.file("users"), "skep-cluster:*:410:") || !strings.Contains(s.file("groups"), "skep-cluster:*:410:") {
		t.Fatal("skep-cluster account not created")
	}
	if strings.Count(s.file("users"), "\n") != 2 || strings.Count(s.file("groups"), "\n") != 2 {
		t.Fatalf("users:\n%sgroups:\n%s", s.file("users"), s.file("groups"))
	}
}

func TestPreInstallClusterAccountRefusals(t *testing.T) {
	cases := map[string][2]string{
		"only the user":  {skepUser + clusterUser, skepGroup},
		"only the group": {skepUser, skepGroup + clusterGroup},
		"UID 410 taken":  {skepUser + "uhub:*:410:410::0:0:x:/nonexistent:/usr/sbin/nologin\n", skepGroup},
		"GID 410 taken":  {skepUser, skepGroup + "uhub:*:410:\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newScriptEnv(t, c[0], c[1], "")
			code, out := s.run("pre-install.sh")
			if code == 0 || !strings.Contains(out, "refusing") {
				t.Fatalf("exit %d: %s", code, out)
			}
			if strings.Contains(s.file("calls"), "add") {
				t.Fatalf("changed something while refusing: %s", s.file("calls"))
			}
		})
	}
}

func TestPreInstallUndoesClusterGroupWhenUseraddFails(t *testing.T) {
	s := newScriptEnv(t, skepUser, skepGroup, "")
	code, _ := s.run("pre-install.sh", "PWFAIL_USERADD=1")
	if code == 0 {
		t.Fatal("succeeded although useradd failed")
	}
	if strings.Contains(s.file("groups"), "skep-cluster:") {
		t.Fatal("left a group without its user")
	}
}

func TestPreInstallKeepsExistingAccount(t *testing.T) {
	s := newScriptEnv(t, skepUser+clusterUser, skepGroup+clusterGroup, "")
	code, out := s.run("pre-install.sh")
	if code != 0 || strings.Contains(s.file("calls"), "add") || strings.Contains(out, "warning") {
		t.Fatalf("exit %d, out %q, calls %q", code, out, s.file("calls"))
	}
}

func TestPreInstallWarnsOnOtherNumbers(t *testing.T) {
	// hearst: set up by hand before ADR 0012, UID/GID 1002.
	s := newScriptEnv(t, "skep:*:1002:1002::0:0:x:/nonexistent:/usr/sbin/nologin\n"+
		"skep-cluster:*:1003:1003::0:0:x:/nonexistent:/usr/sbin/nologin\n", "skep:*:1002:\nskep-cluster:*:1003:\n", "")
	code, out := s.run("pre-install.sh")
	if code != 0 || !strings.Contains(out, "warning") || !strings.Contains(out, "1002") || strings.Contains(s.file("calls"), "add") {
		t.Fatalf("exit %d, out %q, calls %q", code, out, s.file("calls"))
	}
	if !strings.Contains(out, "skep-cluster account exists with UID 1003") {
		t.Fatalf("no warning for skep-cluster: %q", out)
	}
}

func TestPreInstallRefusals(t *testing.T) {
	cases := map[string][2]string{
		"only the user":  {"skep:*:409:409::0:0:x:/nonexistent:/usr/sbin/nologin\n", ""},
		"only the group": {"", "skep:*:409:\n"},
		"UID 409 taken":  {"evcc:*:409:409::0:0:x:/nonexistent:/usr/sbin/nologin\n", ""},
		"GID 409 taken":  {"", "evcc:*:409:\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newScriptEnv(t, c[0], c[1], "")
			code, out := s.run("pre-install.sh")
			if code == 0 || !strings.Contains(out, "refusing") {
				t.Fatalf("exit %d: %s", code, out)
			}
			if strings.Contains(s.file("calls"), "add") {
				t.Fatalf("changed something while refusing: %s", s.file("calls"))
			}
		})
	}
}

func TestPreInstallUndoesGroupWhenUseraddFails(t *testing.T) {
	s := newScriptEnv(t, "", "", "")
	code, _ := s.run("pre-install.sh", "PWFAIL_USERADD=1")
	if code == 0 {
		t.Fatal("succeeded although useradd failed")
	}
	if strings.Contains(s.file("groups"), "skep:") {
		t.Fatal("left a group without its user")
	}
}

func TestPreDeinstall(t *testing.T) {
	t.Run("no guests", func(t *testing.T) {
		s := newScriptEnv(t, "", "", "")
		if code, out := s.run("pre-deinstall.sh"); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
	})
	t.Run("running jail", func(t *testing.T) {
		s := newScriptEnv(t, "", "", "skep-1111\nother\n")
		code, out := s.run("pre-deinstall.sh")
		if code == 0 || !strings.Contains(out, "jail skep-1111") || strings.Contains(out, "other") {
			t.Fatalf("exit %d: %s", code, out)
		}
		// pkg 2.9.2 deletes anyway: the message is a warning that says
		// so, and names the guard that does refuse.
		for _, w := range []string{"WARNING", "pkg deletes the files anyway", "skep-setup remove"} {
			if !strings.Contains(out, w) {
				t.Errorf("output lacks %q: %s", w, out)
			}
		}
		if strings.Contains(out, "refusing") {
			t.Errorf("claims to refuse: %s", out)
		}
	})
	t.Run("running VM", func(t *testing.T) {
		s := newScriptEnv(t, "", "", "")
		os.MkdirAll(filepath.Join(s.vmm, "skep-2222"), 0o755)
		os.MkdirAll(filepath.Join(s.vmm, "notskep"), 0o755)
		code, out := s.run("pre-deinstall.sh")
		if code == 0 || !strings.Contains(out, "VM skep-2222") || strings.Contains(out, "notskep") {
			t.Fatalf("exit %d: %s", code, out)
		}
	})
	t.Run("non-Skep guests only", func(t *testing.T) {
		s := newScriptEnv(t, "", "", "www\n")
		os.MkdirAll(filepath.Join(s.vmm, "myvm"), 0o755)
		if code, out := s.run("pre-deinstall.sh"); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
	})
	t.Run("upgrade with guests", func(t *testing.T) {
		s := newScriptEnv(t, "", "", "skep-1111\n")
		os.MkdirAll(filepath.Join(s.vmm, "skep-2222"), 0o755)
		if code, out := s.run("pre-deinstall.sh", "PKG_UPGRADE=true"); code != 0 {
			t.Fatalf("refused an upgrade: exit %d: %s", code, out)
		}
	})
	t.Run("other root", func(t *testing.T) {
		s := newScriptEnv(t, "", "", "skep-1111\n")
		if code, out := s.run("pre-deinstall.sh", "PKG_ROOTDIR=/mnt"); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
	})
}
