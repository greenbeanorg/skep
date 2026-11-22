package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func existsCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return existsYes
	}
	var ce *exitCodeError
	if !errors.As(err, &ce) {
		t.Fatalf("admin exists returned %v, not an exit code", err)
	}
	return ce.code
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestAdminExists(t *testing.T) {
	cfg, dataDir := fastConfig(t), t.TempDir()
	base := []string{"-dev", "-config", cfg, "-data-dir", dataDir}
	exists := func(username string) (int, string) {
		var out bytes.Buffer
		e := testEnv(&out, io.Discard)
		err := run(context.Background(), append([]string{"admin", "exists", "--username", username}, base...), e)
		return existsCode(t, err), out.String()
	}

	// No database: an error, and nothing is created.
	if code, _ := exists("andy"); code != existsError {
		t.Fatalf("no api.db: exit %d, want %d", code, existsError)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "api")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admin exists created the API's data directory: %v", err)
	}

	e, _, _ := adminEnv(testPassword + "\n")
	if err := run(context.Background(), append([]string{"admin", "create", "--username", "andy", "--password-stdin"}, base...), e); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dataDir, "api", "api.db")
	before := fileSum(t, db)

	if code, out := exists("andy"); code != existsYes || !strings.Contains(out, `"andy" exists (role admin)`) {
		t.Errorf("andy: exit %d, %q", code, out)
	}
	if code, out := exists("bob"); code != existsNo || !strings.Contains(out, `no user "bob"`) {
		t.Errorf("bob: exit %d, %q", code, out)
	}
	if code, _ := exists("Not A Name"); code != existsError {
		t.Errorf("invalid name: exit %d, want %d", code, existsError)
	}
	if after := fileSum(t, db); after != before {
		t.Error("admin exists changed api.db")
	}

	// Disabled users still exist; the state is shown.
	e, _, _ = adminEnv(testPassword + "\n")
	if err := run(context.Background(), append([]string{"admin", "create", "--username", "carol", "--password-stdin"}, base...), e); err != nil {
		t.Fatal(err)
	}
	e, _, _ = adminEnv("")
	if err := run(context.Background(), append([]string{"admin", "disable", "--username", "carol"}, base...), e); err != nil {
		t.Fatal(err)
	}
	if code, out := exists("carol"); code != existsYes || !strings.Contains(out, "role admin, disabled") {
		t.Errorf("carol: exit %d, %q", code, out)
	}
}

func TestAdminExistsRefusesWhatIsNotADatabase(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, db string){
		"a directory": func(t *testing.T, db string) {
			if err := os.Mkdir(db, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"an empty file (no schema, never migrated)": func(t *testing.T, db string) {
			if err := os.WriteFile(db, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a symbolic link": func(t *testing.T, db string) {
			if err := os.Symlink("/etc/passwd", db); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dataDir, "api"), 0o700); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(dataDir, "api", "api.db")
			prepare(t, db)
			e := testEnv(io.Discard, io.Discard)
			err := run(context.Background(), []string{"admin", "exists", "--username", "andy", "-dev", "-config", fastConfig(t), "-data-dir", dataDir}, e)
			if code := existsCode(t, err); code != existsError {
				t.Fatalf("exit %d, want %d (%v)", code, existsError, err)
			}
			if st, err := os.Lstat(db); err == nil && st.Mode().IsRegular() && st.Size() != 0 {
				t.Error("admin exists wrote to the file")
			}
		})
	}
}
