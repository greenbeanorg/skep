package ctlstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoundTripAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skepctl")
	s := New(dir)

	// Nothing exists yet: empty state, nothing created.
	if _, ok, err := s.Trust("https://h:8443"); ok || err != nil {
		t.Fatalf("Trust on empty state = %v, %v", ok, err)
	}
	if cur, err := s.Current(); cur != "" || err != nil {
		t.Fatalf("Current on empty state = %q, %v", cur, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("reading created the directory")
	}

	at := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	pin := Trust{PublicKeySHA256: "sha256:" + strings.Repeat("ab", 32), TrustedAt: at}
	if err := s.SetTrust("https://h:8443", pin); err != nil {
		t.Fatal(err)
	}
	sess := Session{Username: "andy", Token: "secret-token", CreatedAt: at, ExpiresAt: at.Add(time.Hour)}
	if err := s.SetSession("https://h:8443", sess); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSession("https://other:8443", Session{Username: "carol", Token: "t2"}); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, PinsFile): 0o600, filepath.Join(dir, SessionsFile): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("the directory holds %d entries, want 2 (no leftover temporary files)", len(entries))
	}

	got, ok, err := s.Trust("https://h:8443")
	if err != nil || !ok || got != pin {
		t.Errorf("Trust = %+v, %v, %v", got, ok, err)
	}
	gs, ok, err := s.Session("https://h:8443")
	if err != nil || !ok || gs != sess {
		t.Errorf("Session = %+v, %v, %v", gs, ok, err)
	}
	if cur, _ := s.Current(); cur != "https://other:8443" {
		t.Errorf("Current = %q, want the last login", cur)
	}

	if err := s.DeleteSession("https://other:8443"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Session("https://other:8443"); ok {
		t.Error("the session survived DeleteSession")
	}
	if _, ok, _ := s.Session("https://h:8443"); !ok {
		t.Error("DeleteSession removed another server's session")
	}
	if err := s.DeleteSession("https://never:8443"); err != nil {
		t.Errorf("deleting an absent session: %v", err)
	}
	if _, ok, _ := s.Trust("https://h:8443"); !ok {
		t.Error("the pin was lost")
	}

	// A CA entry keeps its mode and file; an entry without a mode is a pin.
	ca := Trust{Mode: TrustCA, CAFile: "/etc/ssl/operator-ca.pem", TrustedAt: at}
	if err := s.SetTrust("https://ca:8443", ca); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Trust("https://ca:8443"); got != ca || !got.VerifiesChain() {
		t.Errorf("CA entry = %+v", got)
	}
	if got, _, _ := s.Trust("https://h:8443"); got.VerifiesChain() {
		t.Error("an entry without a mode verifies the chain")
	}
}

func TestRefusesLooseOrForeignFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skepctl")
	s := New(dir)
	if err := s.SetSession("https://h:8443", Session{Username: "andy", Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, SessionsFile)

	// A file others can read is not used.
	os.Chmod(file, 0o644)
	if _, _, err := s.Session("https://h:8443"); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("0644 session file: %v", err)
	}
	// Nor written over silently.
	if err := s.SetSession("https://h:8443", Session{}); err == nil {
		t.Error("a 0644 session file was rewritten")
	}
	os.Chmod(file, 0o600)

	// A directory others can enter is not used.
	os.Chmod(dir, 0o755)
	if _, _, err := s.Trust("x"); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("0755 directory: %v", err)
	}
	os.Chmod(dir, 0o700)

	// A file owned by someone else is not used.
	foreign := New(dir)
	foreign.uid = s.uid + 1
	if _, _, err := foreign.Session("https://h:8443"); err == nil || !strings.Contains(err.Error(), "not to you") {
		t.Errorf("foreign owner: %v", err)
	}

	// A symlink is not followed.
	os.Rename(file, file+".real")
	os.Symlink(file+".real", file)
	if _, _, err := s.Session("https://h:8443"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("symlink: %v", err)
	}
	os.Remove(file)

	// Garbage and unknown versions are errors, not empty state.
	os.WriteFile(file, []byte("not json"), 0o600)
	if _, _, err := s.Session("x"); err == nil {
		t.Error("an invalid file was accepted")
	}
	os.WriteFile(file, []byte(`{"version": 9, "sessions": {}}`), 0o600)
	if _, _, err := s.Session("x"); err == nil || !strings.Contains(err.Error(), "version 9") {
		t.Errorf("unknown version: %v", err)
	}
}

func TestDefaultDir(t *testing.T) {
	t.Setenv(DirEnv, "/somewhere/else")
	if d, err := DefaultDir(); err != nil || d != "/somewhere/else" {
		t.Errorf("DefaultDir with %s = %q, %v", DirEnv, d, err)
	}
	t.Setenv(DirEnv, "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if d, err := DefaultDir(); err != nil || d != "/xdg/skepctl" {
		t.Errorf("DefaultDir = %q, %v", d, err)
	}
}
