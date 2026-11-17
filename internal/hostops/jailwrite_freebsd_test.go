//go:build freebsd

package hostops

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The decision 13 writer, natively (the "Symlink rule" check of decision
// 16). It needs root: the files are made root:wheel.

// jailTree makes a jail root with etc/ and root/ and a directory outside
// it that a planted link may point at, and returns both.
func jailTree(t *testing.T) (root, outside string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("the writer sets root:wheel ownership; run as root on FreeBSD")
	}
	base := t.TempDir()
	root, outside = filepath.Join(base, "jail"), filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "etc"), filepath.Join(root, "root"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "victim"), []byte("untouched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func content(t *testing.T) jailContent {
	t.Helper()
	c, err := JailFiles{RCConf: SSHRCConf, AuthorizedKey: sshKey("ssh-ed25519", make([]byte, 32)) + " t"}.render()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// outsideUntouched fails if anything outside the jail root changed.
func outsideUntouched(t *testing.T, outside string) {
	t.Helper()
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 || entries[0].Name() != "victim" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the directory outside the jail root changed: %v", names)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "victim")); string(b) != "untouched\n" {
		t.Fatalf("the file outside the jail root changed: %q", b)
	}
}

func mode(t *testing.T, p string) (os.FileMode, uint32, uint32) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return fi.Mode(), st.Uid, st.Gid
}

func TestWriteJailFilesNative(t *testing.T) {
	root, outside := jailTree(t)
	c := content(t)
	if err := writeJailFiles(root, c); err != nil {
		t.Fatal(err)
	}
	rc, _ := os.ReadFile(filepath.Join(root, "etc/rc.conf"))
	keys, _ := os.ReadFile(filepath.Join(root, "root/.ssh/authorized_keys"))
	if !bytes.Equal(rc, c.rcConf) || !bytes.Equal(keys, c.authorizedKeys) {
		t.Fatalf("wrote %q / %q", rc, keys)
	}
	for p, want := range map[string]os.FileMode{
		"etc/rc.conf": 0o644, "root/.ssh": os.ModeDir | 0o700, "root/.ssh/authorized_keys": 0o600,
	} {
		if m, uid, gid := mode(t, filepath.Join(root, p)); m != want || uid != 0 || gid != 0 {
			t.Errorf("%s: %v %d:%d, want %v 0:0", p, m, uid, gid, want)
		}
	}
	// Written again, then removed: only Skep's files go.
	if err := writeJailFiles(root, c); err != nil {
		t.Fatal(err)
	}
	if err := writeJailFiles(root, jailContent{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"etc/rc.conf", "root/.ssh/authorized_keys"} {
		if _, err := os.Lstat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", p, err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*/.*skep-new"))
	matches2, _ := filepath.Glob(filepath.Join(root, "*/*/.*skep-new"))
	if len(matches)+len(matches2) > 0 {
		t.Errorf("temporary files left: %v %v", matches, matches2)
	}
	outsideUntouched(t, outside)
}

func TestWriteJailFilesRefusesForeignRCConf(t *testing.T) {
	root, outside := jailTree(t)
	os.WriteFile(filepath.Join(root, "etc/rc.conf"), []byte("hostname=\"mine\"\n"), 0o644)
	if err := writeJailFiles(root, content(t)); err == nil || !strings.Contains(err.Error(), "not written by Skep") {
		t.Fatalf("a foreign rc.conf was replaced: %v", err)
	}
	if err := writeJailFiles(root, jailContent{}); err == nil {
		t.Fatal("a foreign rc.conf was removed")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/rc.conf")); string(b) != "hostname=\"mine\"\n" {
		t.Fatalf("the foreign rc.conf changed: %q", b)
	}
	outsideUntouched(t, outside)
}

// Each planted link is one a jail could make if it had run. Every write
// must fail, or write inside the root, and nothing outside may change.
func TestWriteJailFilesPlantedLinks(t *testing.T) {
	for name, plant := range map[string]func(root, outside string) error{
		// The last component is a link to a file outside (decision 13's
		// own example, rc.conf -> ../../etc/master.passwd).
		"rc.conf link to an outside file": func(root, outside string) error {
			return os.Symlink(filepath.Join(outside, "victim"), filepath.Join(root, "etc/rc.conf"))
		},
		"rc.conf relative link out": func(root, outside string) error {
			return os.Symlink("../../outside/victim", filepath.Join(root, "etc/rc.conf"))
		},
		// A directory in the path is a link outside, absolute and relative.
		"etc absolute link out": func(root, outside string) error {
			os.RemoveAll(filepath.Join(root, "etc"))
			return os.Symlink(outside, filepath.Join(root, "etc"))
		},
		"etc relative link out": func(root, outside string) error {
			os.RemoveAll(filepath.Join(root, "etc"))
			return os.Symlink("../outside", filepath.Join(root, "etc"))
		},
		"root home link out": func(root, outside string) error {
			os.RemoveAll(filepath.Join(root, "root"))
			return os.Symlink(outside, filepath.Join(root, "root"))
		},
		".ssh link out": func(root, outside string) error {
			return os.Symlink(outside, filepath.Join(root, "root/.ssh"))
		},
		"authorized_keys link out": func(root, outside string) error {
			os.Mkdir(filepath.Join(root, "root/.ssh"), 0o700)
			return os.Symlink(filepath.Join(outside, "victim"), filepath.Join(root, "root/.ssh/authorized_keys"))
		},
		"temporary name link out": func(root, outside string) error {
			return os.Symlink(filepath.Join(outside, "victim"), filepath.Join(root, "etc/.rc.conf.skep-new"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, outside := jailTree(t)
			if err := plant(root, outside); err != nil {
				t.Fatal(err)
			}
			err := writeJailFiles(root, content(t))
			t.Logf("write: %v", err)
			outsideUntouched(t, outside)
			// A write that succeeded replaced the link inside the root,
			// never followed it.
			if err == nil {
				for _, p := range []string{"etc/rc.conf", "root/.ssh/authorized_keys"} {
					if m, _, _ := mode(t, filepath.Join(root, p)); !m.IsRegular() {
						t.Errorf("%s is %v after a successful write", p, m)
					}
				}
			}
			// Removal must not follow a link either.
			err = writeJailFiles(root, jailContent{})
			t.Logf("remove: %v", err)
			outsideUntouched(t, outside)
		})
	}
}

// TestResolveBeneathNative shows the kernel half on its own: the writer
// opens one component at a time, so O_NOFOLLOW already stops every link
// above, and O_RESOLVE_BENEATH is the second guard. Here a multi-component
// lookup through a link that leaves the root must fail with ENOTCAPABLE,
// and one through a link that stays inside must work.
func TestResolveBeneathNative(t *testing.T) {
	root, outside := jailTree(t)
	os.Symlink("../outside", filepath.Join(root, "out"))
	os.Symlink("etc", filepath.Join(root, "in"))
	rfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rfd)
	for _, p := range []string{"out/victim", "etc/../../outside/victim", "/etc/passwd", "../outside/victim"} {
		fd, err := unix.Openat(rfd, p, unix.O_RDONLY|unix.O_RESOLVE_BENEATH, 0)
		if err == nil {
			unix.Close(fd)
			t.Errorf("%s opened beneath the root", p)
			continue
		}
		t.Logf("%s: %v", p, err)
	}
	fd, err := unix.Openat(rfd, "in", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_RESOLVE_BENEATH, 0)
	if err != nil {
		t.Fatalf("a link that stays inside the root: %v", err)
	}
	unix.Close(fd)
	outsideUntouched(t, outside)
}

func TestWriteJailResolvConfNative(t *testing.T) {
	root, outside := jailTree(t)
	c, err := JailFiles{RCConf: []string{`ifconfig_sj0000000000n0="inet 192.0.2.10/24"`}, Nameservers: []string{"192.0.2.53"}}.render()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJailFiles(root, c); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "etc", "resolv.conf")
	if b, _ := os.ReadFile(p); string(b) != RCConfHeader+"nameserver 192.0.2.53\n" {
		t.Fatalf("resolv.conf = %q", b)
	}
	if m, uid, gid := mode(t, p); m != 0o644 || uid != 0 || gid != 0 {
		t.Fatalf("resolv.conf mode %v %d:%d", m, uid, gid)
	}
	// Without nameservers Skep's own is removed.
	c.resolvConf = nil
	if err := writeJailFiles(root, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("Skep's resolv.conf survived: %v", err)
	}
	// A template's own resolv.conf is left alone when there is nothing to
	// write, and refused when there is.
	if err := os.WriteFile(p, []byte("nameserver 10.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeJailFiles(root, c); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "nameserver 10.0.0.1\n" {
		t.Fatalf("a foreign resolv.conf changed: %q", b)
	}
	c2, _ := JailFiles{Nameservers: []string{"192.0.2.53"}}.render()
	if err := writeJailFiles(root, c2); err == nil {
		t.Fatal("a foreign resolv.conf was replaced")
	}
	// A planted link pointing outside is replaced, never followed.
	os.Remove(p)
	if err := os.Symlink(filepath.Join(outside, "victim"), p); err != nil {
		t.Fatal(err)
	}
	if err := writeJailFiles(root, c2); err == nil {
		t.Fatal("wrote through a planted resolv.conf link")
	}
	outsideUntouched(t, outside)
}
