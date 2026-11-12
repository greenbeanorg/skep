package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noEnv(string) (string, bool) { return "", false }

// noDefault keeps tests independent of any real file at DefaultConfigPath.
const noDefault = "/nonexistent/skep-test/agent.json"

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadProductionDefaults(t *testing.T) {
	cfg, err := Load(LoadOptions{LookupEnv: noEnv, DefaultPath: noDefault})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DevMode {
		t.Error("dev mode must be off by default")
	}
	if cfg.API.ListenAddress != DefaultListenAddress {
		t.Errorf("listen = %q", cfg.API.ListenAddress)
	}
	if cfg.API.AllowInsecureRemote {
		t.Error("remote binding must be off by default")
	}
	if cfg.DataDir != DefaultDataDir {
		t.Errorf("data dir = %q", cfg.DataDir)
	}
	if cfg.Database.Path != filepath.Join(DefaultDataDir, "agent", "agent.db") {
		t.Errorf("database path = %q", cfg.Database.Path)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Errorf("log = %+v", cfg.Log)
	}
	if cfg.Discovery.CommandTimeout.Std() != 5*time.Second {
		t.Errorf("command timeout = %v", cfg.Discovery.CommandTimeout.Std())
	}
}

func TestLoadDevDefaults(t *testing.T) {
	dev := true
	cfg, err := Load(LoadOptions{LookupEnv: noEnv, DefaultPath: noDefault, Overrides: Overrides{DevMode: &dev}})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DevMode || cfg.DataDir != DefaultDevDataDir {
		t.Errorf("dev defaults not applied: %+v", cfg)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "text" {
		t.Errorf("log = %+v", cfg.Log)
	}
	if cfg.API.ListenAddress != DefaultListenAddress {
		t.Errorf("dev mode must still bind to loopback, got %q", cfg.API.ListenAddress)
	}
}

func TestPrecedenceFileEnvOverrides(t *testing.T) {
	path := writeFile(t, `{
		"data_dir": "/from/file",
		"api": {"listen_address": "127.0.0.1:1111", "read_timeout": "3s"},
		"log": {"level": "warn", "format": "text"},
		"discovery": {"cache_ttl": "1m"}
	}`)
	cfg, err := Load(LoadOptions{
		Path: path,
		LookupEnv: envMap(map[string]string{
			EnvListen:   "127.0.0.1:2222",
			EnvLogLevel: "ERROR",
		}),
		Overrides: Overrides{ListenAddress: "[::1]:3333"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.ListenAddress != "[::1]:3333" {
		t.Errorf("override must win, got %q", cfg.API.ListenAddress)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("env must beat file and be lower-cased, got %q", cfg.Log.Level)
	}
	if cfg.Log.Format != "text" || cfg.DataDir != "/from/file" {
		t.Errorf("file values lost: %+v", cfg)
	}
	if cfg.API.ReadTimeout.Std() != 3*time.Second || cfg.Discovery.CacheTTL.Std() != time.Minute {
		t.Errorf("durations not parsed: %+v", cfg)
	}
	if cfg.Database.Path != filepath.Join("/from/file", "agent", "agent.db") {
		t.Errorf("database path = %q", cfg.Database.Path)
	}
}

func TestConfigPathFromEnv(t *testing.T) {
	path := writeFile(t, `{"dev_mode": true}`)
	cfg, err := Load(LoadOptions{LookupEnv: envMap(map[string]string{EnvConfig: path})})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DevMode {
		t.Error("file named by SKEP_CONFIG was not read")
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		opts LoadOptions
		want string
	}{
		{"missing explicit file", LoadOptions{Path: "/nonexistent/skep.json"}, "open"},
		{"unknown field", LoadOptions{Path: writeFile(t, `{"nope": 1}`)}, "unknown field"},
		{"bad json", LoadOptions{Path: writeFile(t, `{`)}, "parse"},
		{"trailing data", LoadOptions{Path: writeFile(t, `{} {}`)}, "unexpected data"},
		{"bad duration", LoadOptions{Path: writeFile(t, `{"api":{"read_timeout":"soon"}}`)}, "invalid duration"},
		{"negative duration", LoadOptions{Path: writeFile(t, `{"api":{"read_timeout":"-1s"}}`)}, "must be positive"},
		{"bad level", LoadOptions{Overrides: Overrides{LogLevel: "loud"}}, "log.level"},
		{"bad format", LoadOptions{Overrides: Overrides{LogFormat: "xml"}}, "log.format"},
		{"bad listen", LoadOptions{Overrides: Overrides{ListenAddress: "nope"}}, "host:port"},
		{"bad port", LoadOptions{Overrides: Overrides{ListenAddress: "127.0.0.1:99999"}}, "invalid port"},
		{"all interfaces", LoadOptions{Overrides: Overrides{ListenAddress: ":8765"}}, "not a loopback"},
		{"remote address", LoadOptions{Overrides: Overrides{ListenAddress: "10.79.20.80:8765"}}, "not a loopback"},
		{"bad dev env", LoadOptions{LookupEnv: envMap(map[string]string{EnvDev: "maybe"})}, "not a boolean"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.opts.LookupEnv == nil {
				tt.opts.LookupEnv = noEnv
			}
			tt.opts.DefaultPath = noDefault
			_, err := Load(tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestRemoteBindRequiresOptIn(t *testing.T) {
	path := writeFile(t, `{"api": {"listen_address": "0.0.0.0:8765", "allow_insecure_remote": true}}`)
	cfg, err := Load(LoadOptions{Path: path, LookupEnv: noEnv})
	if err != nil {
		t.Fatalf("explicit opt-in must be accepted: %v", err)
	}
	if cfg.API.ListenAddress != "0.0.0.0:8765" {
		t.Errorf("listen = %q", cfg.API.ListenAddress)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "127.1.2.3": true, "::1": true,
		"": false, "0.0.0.0": false, "::": false, "10.0.0.1": false, "example.org": false,
	} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestDurationJSONRoundTrip(t *testing.T) {
	d := Duration(90 * time.Second)
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Duration
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if back != d {
		t.Errorf("round trip: %v != %v", back, d)
	}
	if err := back.UnmarshalJSON([]byte(`5`)); err == nil {
		t.Error("a bare number must be rejected")
	}
}

// TestSampleConfigMatchesDefaults keeps the shipped sample file loadable and
// in step with the built-in production defaults.
func TestSampleConfigMatchesDefaults(t *testing.T) {
	sample, err := Load(LoadOptions{Path: "../../packaging/etc/agent.json.sample", LookupEnv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := Load(LoadOptions{LookupEnv: noEnv, DefaultPath: noDefault})
	if err != nil {
		t.Fatal(err)
	}
	if sample != defaults {
		t.Errorf("sample config differs from the defaults:\nsample   %+v\ndefaults %+v", sample, defaults)
	}
}
