package clusterconf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testID = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"

func TestWriteLoadRoundTripAndMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cluster.json")
	want := Config{NodeID: testID, NodeName: "smith", ListenAddress: "10.79.20.81"}
	if err := Write(p, want, -1, -1); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %04o, want 0640", info.Mode().Perm())
	}
	got, err := Load(p)
	if err != nil || got != want {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if got.PeerAddress() != "10.79.20.81:8444" {
		t.Fatalf("PeerAddress = %q", got.PeerAddress())
	}
	// Replacing it leaves no temporary file behind.
	want.ListenAddress = ""
	if err := Write(p, want, -1, -1); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("%d files left, want 1", len(entries))
	}
	if got, _ := Load(p); got.PeerAddress() != "" {
		t.Fatalf("a node without a listen address has peer address %q", got.PeerAddress())
	}
}

func TestLoadRefusals(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "missing.json")); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("missing file: %v", err)
	}
	cases := map[string]string{
		"unknown field":  `{"node_id":"` + testID + `","node_name":"a","port":1}`,
		"bad node id":    `{"node_id":"X","node_name":"a"}`,
		"bad name":       `{"node_id":"` + testID + `","node_name":"a b"}`,
		"address + port": `{"node_id":"` + testID + `","node_name":"a","listen_address":"10.0.0.1:8444"}`,
		"unspecified":    `{"node_id":"` + testID + `","node_name":"a","listen_address":"0.0.0.0"}`,
		"unspecified v6": `{"node_id":"` + testID + `","node_name":"a","listen_address":"::"}`,
		"host name":      `{"node_id":"` + testID + `","node_name":"a","listen_address":"smith"}`,
		"two values":     `{"node_id":"` + testID + `","node_name":"a"}{}`,
	}
	for name, body := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWriteRefusesInvalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cluster.json")
	if err := Write(p, Config{NodeID: "nope", NodeName: "a"}, -1, -1); err == nil {
		t.Fatal("an invalid configuration was written")
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("a file was left")
	}
}
