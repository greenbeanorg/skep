package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	if cfg.ShutdownTimeout.Std() != 10*time.Second {
		t.Errorf("shutdown timeout = %v", cfg.ShutdownTimeout.Std())
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
	if cfg.Tasks.Workers != 4 || cfg.Tasks.QueueSize != 64 {
		t.Errorf("tasks = %+v", cfg.Tasks)
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
}

func TestPrecedenceFileEnvOverrides(t *testing.T) {
	path := writeFile(t, `{
		"data_dir": "/from/file",
		"socket": {"path": "/from/file.sock"},
		"shutdown_timeout": "3s",
		"log": {"level": "warn", "format": "text"},
		"discovery": {"cache_ttl": "1m"}
	}`)
	cfg, err := Load(LoadOptions{
		Path: path,
		LookupEnv: envMap(map[string]string{
			EnvSocket:   "/from/env.sock",
			EnvLogLevel: "ERROR",
		}),
		Overrides: Overrides{SocketPath: "/from/flag.sock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket.Path != "/from/flag.sock" {
		t.Errorf("override must win, got %q", cfg.Socket.Path)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("env must beat file and be lower-cased, got %q", cfg.Log.Level)
	}
	if cfg.Log.Format != "text" || cfg.DataDir != "/from/file" {
		t.Errorf("file values lost: %+v", cfg)
	}
	if cfg.ShutdownTimeout.Std() != 3*time.Second || cfg.Discovery.CacheTTL.Std() != time.Minute {
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
		{"bad duration", LoadOptions{Path: writeFile(t, `{"shutdown_timeout":"soon"}`)}, "invalid duration"},
		{"negative duration", LoadOptions{Path: writeFile(t, `{"shutdown_timeout":"-1s"}`)}, "must be positive"},
		{"the old api section", LoadOptions{Path: writeFile(t, `{"api":{"listen_address":"127.0.0.1:8765"}}`)}, "unknown field"},
		{"bad level", LoadOptions{Overrides: Overrides{LogLevel: "loud"}}, "log.level"},
		{"bad format", LoadOptions{Overrides: Overrides{LogFormat: "xml"}}, "log.format"},
		{"too many workers", LoadOptions{Path: writeFile(t, `{"tasks":{"workers":65}}`)}, "tasks.workers"},
		{"negative workers", LoadOptions{Path: writeFile(t, `{"tasks":{"workers":-1}}`)}, "tasks.workers"},
		{"huge queue", LoadOptions{Path: writeFile(t, `{"tasks":{"queue_size":5000}}`)}, "tasks.queue_size"},
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
	if !reflect.DeepEqual(sample, defaults) {
		t.Errorf("sample config differs from the defaults:\nsample   %+v\ndefaults %+v", sample, defaults)
	}
}

func TestSocketDefaults(t *testing.T) {
	prod, err := Load(LoadOptions{LookupEnv: noEnv, DefaultPath: noDefault})
	if err != nil {
		t.Fatal(err)
	}
	if prod.Socket.Path != DefaultSocketPath || prod.Socket.Group != "skep" ||
		!reflect.DeepEqual(prod.Socket.AllowedUsers, []string{"root", "skep"}) {
		t.Errorf("production socket = %+v", prod.Socket)
	}
	if prod.Socket.RequestTimeout.Std() != 30*time.Second || prod.Socket.MaxConnections != 64 {
		t.Errorf("production socket limits = %+v", prod.Socket)
	}

	dev := true
	cfg, err := Load(LoadOptions{LookupEnv: noEnv, DefaultPath: noDefault, Overrides: Overrides{DevMode: &dev, DataDir: "/tmp/skep-dev"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket.Path != "/tmp/skep-dev/run/agent.sock" || cfg.Socket.Group != "" {
		t.Errorf("dev socket = %+v", cfg.Socket)
	}
	// In dev mode only the developer's own UID may connect.
	if !reflect.DeepEqual(cfg.Socket.AllowedUsers, []string{strconv.Itoa(os.Geteuid())}) {
		t.Errorf("dev allow-list = %v", cfg.Socket.AllowedUsers)
	}
}

func TestSocketOverridesAndErrors(t *testing.T) {
	cfg, err := Load(LoadOptions{
		DefaultPath: noDefault,
		LookupEnv:   envMap(map[string]string{EnvSocket: "/tmp/env.sock"}),
	})
	if err != nil || cfg.Socket.Path != "/tmp/env.sock" {
		t.Errorf("env: %+v, %v", cfg.Socket, err)
	}
	cfg, err = Load(LoadOptions{
		DefaultPath: noDefault,
		LookupEnv:   envMap(map[string]string{EnvSocket: "/tmp/env.sock"}),
		Overrides:   Overrides{SocketPath: "/tmp/flag.sock"},
	})
	if err != nil || cfg.Socket.Path != "/tmp/flag.sock" {
		t.Errorf("flag: %+v, %v", cfg.Socket, err)
	}

	for name, tt := range map[string]struct{ body, want string }{
		"empty allow-list": {`{"socket":{"allowed_users":[]}}`, "allowed_users must not be empty"},
		"bad user":         {`{"socket":{"allowed_users":["a b"]}}`, "not a user name"},
		"bad group":        {`{"socket":{"group":"a/b"}}`, "socket.group"},
		"long path":        {`{"socket":{"path":"/tmp/` + strings.Repeat("x", 90) + `"}}`, "longer than"},
		"too many conns":   {`{"socket":{"max_connections":5000}}`, "max_connections"},
		"bad timeout":      {`{"socket":{"request_timeout":"-1s"}}`, "must be positive"},
	} {
		_, err := Load(LoadOptions{Path: writeFile(t, tt.body), LookupEnv: noEnv})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tt.want)
		}
	}
}

func loadAPI(t *testing.T, opts APILoadOptions) (API, error) {
	t.Helper()
	if opts.LookupEnv == nil {
		opts.LookupEnv = noEnv
	}
	if opts.DefaultPath == "" {
		opts.DefaultPath = noDefault
	}
	return LoadAPI(opts)
}

func TestAPIDefaults(t *testing.T) {
	cfg, err := loadAPI(t, APILoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DevMode || cfg.DataDir != DefaultDataDir || cfg.ListenAddress != "127.0.0.1:8443" ||
		cfg.AgentSocket != DefaultSocketPath || cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Errorf("production defaults = %+v", cfg)
	}

	dev := true
	cfg, err = loadAPI(t, APILoadOptions{Overrides: APIOverrides{DevMode: &dev, DataDir: "/tmp/skep-dev"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Path != "/tmp/skep-dev/api/api.db" || cfg.TLSDir() != "/tmp/skep-dev/api/tls" {
		t.Errorf("dev state paths: %s, %s", cfg.Database.Path, cfg.TLSDir())
	}
	// Dev mode does not weaken password hashing.
	if cfg.Auth.Argon2.MemoryKiB != 65536 || cfg.Auth.Argon2.Passes != 3 {
		t.Errorf("dev argon2 = %+v", cfg.Auth.Argon2)
	}
	// Dev mode finds the dev agent's socket and still binds to loopback.
	if cfg.AgentSocket != "/tmp/skep-dev/run/agent.sock" || cfg.ListenAddress != "127.0.0.1:8443" || cfg.Log.Format != "text" {
		t.Errorf("dev defaults = %+v", cfg)
	}
}

func TestAPIPrecedence(t *testing.T) {
	path := writeFile(t, `{"listen_address": "127.0.0.1:1111", "agent_socket": "/file.sock", "read_timeout": "3s", "log": {"level": "warn"}}`)
	cfg, err := loadAPI(t, APILoadOptions{
		Path:      path,
		LookupEnv: envMap(map[string]string{EnvAPIListen: "127.0.0.1:2222", EnvSocket: "/env.sock", EnvLogLevel: "ERROR"}),
		Overrides: APIOverrides{ListenAddress: "[::1]:3333"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "[::1]:3333" || cfg.AgentSocket != "/env.sock" || cfg.Log.Level != "error" ||
		cfg.ReadTimeout.Std() != 3*time.Second {
		t.Errorf("precedence: %+v", cfg)
	}

	cfg, err = loadAPI(t, APILoadOptions{LookupEnv: envMap(map[string]string{EnvAPIConfig: writeFile(t, `{"dev_mode": true}`)})})
	if err != nil || !cfg.DevMode {
		t.Errorf("SKEP_API_CONFIG: %+v, %v", cfg, err)
	}
}

func TestAPIErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		opts APILoadOptions
		want string
	}{
		"missing file":          {APILoadOptions{Path: "/nonexistent/api.json"}, "open"},
		"unknown field":         {APILoadOptions{Path: writeFile(t, `{"socket": {}}`)}, "unknown field"},
		"bad listen":            {APILoadOptions{Overrides: APIOverrides{ListenAddress: "nope"}}, "host:port"},
		"bad port":              {APILoadOptions{Overrides: APIOverrides{ListenAddress: "127.0.0.1:99999"}}, "invalid port"},
		"long idle timeout":     {APILoadOptions{Path: writeFile(t, `{"auth": {"session": {"idle_timeout": "25h", "absolute_timeout": "48h"}}}`)}, "idle_timeout"},
		"absolute below idle":   {APILoadOptions{Path: writeFile(t, `{"auth": {"session": {"idle_timeout": "2h", "absolute_timeout": "1h"}}}`)}, "absolute_timeout"},
		"absolute over 30 days": {APILoadOptions{Path: writeFile(t, `{"auth": {"session": {"absolute_timeout": "721h"}}}`)}, "absolute_timeout"},
		"negative login limit":  {APILoadOptions{Path: writeFile(t, `{"auth": {"login": {"per_address_per_minute": -1}}}`)}, "per_address_per_minute"},
		"bad trusted proxy":     {APILoadOptions{Path: writeFile(t, `{"auth": {"trusted_proxies": ["proxy.example.org"]}}`)}, "trusted_proxies"},
		"bad timeout":           {APILoadOptions{Path: writeFile(t, `{"write_timeout": "-1s"}`)}, "must be positive"},
		"cert without key":      {APILoadOptions{Path: writeFile(t, `{"tls": {"cert_file": "/etc/ssl/skep.crt"}}`)}, "must be set together"},
		"relative cert path":    {APILoadOptions{Path: writeFile(t, `{"tls": {"cert_file": "skep.crt", "key_file": "skep.key"}}`)}, "absolute path"},
		"weak argon2 memory":    {APILoadOptions{Path: writeFile(t, `{"auth": {"argon2": {"memory_kib": 8192}}}`)}, "auth.argon2.memory_kib"},
		"one argon2 pass":       {APILoadOptions{Path: writeFile(t, `{"auth": {"argon2": {"passes": 1}}}`)}, "auth.argon2.passes"},
		"huge argon2 memory":    {APILoadOptions{Path: writeFile(t, `{"auth": {"argon2": {"memory_kib": 4194304}}}`)}, "auth.argon2.memory_kib"},
		"bad level":             {APILoadOptions{Overrides: APIOverrides{LogLevel: "loud"}}, "log.level"},
		"bad dev env":           {APILoadOptions{LookupEnv: envMap(map[string]string{EnvDev: "maybe"})}, "not a boolean"},
	} {
		if _, err := loadAPI(t, tt.opts); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tt.want)
		}
	}
}

// TestAPISampleConfigMatchesDefaults keeps the shipped sample loadable and
// in step with the built-in production defaults.
func TestAPISampleConfigMatchesDefaults(t *testing.T) {
	sample, err := loadAPI(t, APILoadOptions{Path: "../../packaging/etc/api.json.sample"})
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := loadAPI(t, APILoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sample, defaults) {
		t.Errorf("sample config differs from the defaults:\nsample   %+v\ndefaults %+v", sample, defaults)
	}
}

// TestAPIListenAddressIsAnExplicitChoice: loopback is the default, and
// opening the API to a network is one explicit setting (ADR 0006
// decision 7).
func TestAPIListenAddressIsAnExplicitChoice(t *testing.T) {
	cfg, err := loadAPI(t, APILoadOptions{})
	if err != nil || !IsLoopbackHost(strings.Split(cfg.ListenAddress, ":")[0]) {
		t.Fatalf("default listen address %q is not loopback (%v)", cfg.ListenAddress, err)
	}
	for _, addr := range []string{"10.79.20.170:8443", ":8443", "[::]:8443"} {
		cfg, err := loadAPI(t, APILoadOptions{Path: writeFile(t, `{"listen_address": "`+addr+`"}`)})
		if err != nil || cfg.ListenAddress != addr {
			t.Errorf("listen_address %q: %+v, %v", addr, cfg.ListenAddress, err)
		}
	}
}

func TestAPIAuthDefaultsAndProxies(t *testing.T) {
	cfg, err := loadAPI(t, APILoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if a.Session.IdleTimeout.Std() != 30*time.Minute || a.Session.AbsoluteTimeout.Std() != 12*time.Hour ||
		a.Login.PerAddressPerMinute != 10 || a.Login.FailuresBeforeDelay != 5 || a.Login.MaxDelay.Std() != time.Minute ||
		a.Login.PerUsernameFailures != 50 || len(a.TrustedProxies) != 0 {
		t.Errorf("auth defaults = %+v", a)
	}
	cfg, err = loadAPI(t, APILoadOptions{Path: writeFile(t, `{"auth": {"trusted_proxies": ["10.0.0.5", "192.0.2.77/24", "2001:db8::/32"]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	prefixes, err := cfg.TrustedProxyPrefixes()
	if err != nil || len(prefixes) != 3 || prefixes[0].String() != "10.0.0.5/32" || prefixes[1].String() != "192.0.2.0/24" {
		t.Errorf("prefixes = %v, %v", prefixes, err)
	}
}

func TestConfinementDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(LoadOptions{LookupEnv: noEnv, DefaultPath: noDefault})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh install can touch nothing: no pool, no bridge.
	if len(cfg.Storage.Pools) != 0 || len(cfg.Network.Bridges) != 0 {
		t.Errorf("default confinement allows something: %+v %+v", cfg.Storage, cfg.Network)
	}
	if cfg.Storage.DatasetRoot != "skep" || cfg.Storage.MediaDir != "/var/db/skep/media" ||
		cfg.Hypervisor.Bhyve.FirmwareDir != DefaultFirmwareDir ||
		*cfg.Hypervisor.Bhyve.HostMemoryReserveBytes != 1<<30 ||
		cfg.Hypervisor.Bhyve.WatchInterval.Std() != 2*time.Second {
		t.Errorf("defaults = %+v %+v", cfg.Storage, cfg.Hypervisor.Bhyve)
	}
	if cfg.Storage.CapacityWarnPercent != 80 || cfg.Storage.CapacityFailPercent != 90 {
		t.Errorf("capacity thresholds = %d, %d", cfg.Storage.CapacityWarnPercent, cfg.Storage.CapacityFailPercent)
	}
	if !reflect.DeepEqual(cfg.Storage.TemplatePackages, []string{"FreeBSD-set-minimal"}) || cfg.Storage.DistDir != "" {
		t.Errorf("template defaults = %v, %q", cfg.Storage.TemplatePackages, cfg.Storage.DistDir)
	}
	if cfg.VMRunDir() != "/var/run/skep/vm" || cfg.VMStateDir() != "/var/db/skep/agent/vm" {
		t.Errorf("VM directories = %q, %q", cfg.VMRunDir(), cfg.VMStateDir())
	}

	path := writeFile(t, `{
		"hypervisor": {"bhyve": {"host_memory_reserve_bytes": 0, "max_vcpus_per_vm": 2}},
		"storage": {"pools": ["skeptest"], "dataset_root": "skep/lab", "media_dir": "/srv/iso"},
		"network": {"bridges": ["bridge0"], "management_interfaces": ["vtnet0"]}
	}`)
	cfg, err = Load(LoadOptions{Path: path, LookupEnv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Storage.Directories, []DirectoryPool{}) {
		t.Errorf("directories = %#v", cfg.Storage.Directories)
	}
	path = writeFile(t, `{
		"hypervisor": {"bhyve": {"host_memory_reserve_bytes": 0, "max_vcpus_per_vm": 2}},
		"storage": {"pools": ["skeptest"], "dataset_root": "skep/lab", "media_dir": "/srv/iso",
			"directories": [{"name": "dirpool", "path": "/var/tmp/skep/dirpool"}]},
		"network": {"bridges": ["bridge0"], "management_interfaces": ["vtnet0"]}
	}`)
	cfg, err = Load(LoadOptions{Path: path, LookupEnv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Storage.Directories, []DirectoryPool{{Name: "dirpool", Path: "/var/tmp/skep/dirpool"}}) {
		t.Errorf("directories = %+v", cfg.Storage.Directories)
	}
	if !reflect.DeepEqual(cfg.Storage.Pools, []string{"skeptest"}) || cfg.Storage.DatasetRoot != "skep/lab" ||
		cfg.Storage.MediaDir != "/srv/iso" || !reflect.DeepEqual(cfg.Network.Bridges, []string{"bridge0"}) ||
		*cfg.Hypervisor.Bhyve.HostMemoryReserveBytes != 0 || cfg.Hypervisor.Bhyve.MaxVCPUsPerVM != 2 {
		t.Errorf("loaded = %+v %+v %+v", cfg.Storage, cfg.Network, cfg.Hypervisor.Bhyve)
	}

	for name, body := range map[string]string{
		"pool with a slash":         `{"storage": {"pools": ["tank/data"]}}`,
		"pool with a space":         `{"storage": {"pools": ["tank; reboot"]}}`,
		"pool twice":                `{"storage": {"pools": ["tank", "tank"]}}`,
		"root with dots":            `{"storage": {"dataset_root": "../zroot"}}`,
		"absolute root":             `{"storage": {"dataset_root": "/skep"}}`,
		"root with a snapshot":      `{"storage": {"dataset_root": "skep@now"}}`,
		"relative media dir":        `{"storage": {"media_dir": "media"}}`,
		"relative firmware dir":     `{"hypervisor": {"bhyve": {"firmware_dir": "firmware"}}}`,
		"negative reserve":          `{"hypervisor": {"bhyve": {"host_memory_reserve_bytes": -1}}}`,
		"bridge that is not one":    `{"network": {"bridges": ["bridge0; ifconfig"]}}`,
		"management as a bridge":    `{"network": {"bridges": ["vtnet0"], "management_interfaces": ["vtnet0"]}}`,
		"bad management":            `{"network": {"management_interfaces": ["VTNET 0"]}}`,
		"watch interval too fast":   `{"hypervisor": {"bhyve": {"watch_interval": "1ms"}}}`,
		"directory named as a pool": `{"storage": {"pools": ["tank"], "directories": [{"name": "tank", "path": "/srv/a"}]}}`,
		"directory relative":        `{"storage": {"directories": [{"name": "d", "path": "srv/a"}]}}`,
		"directory not clean":       `{"storage": {"directories": [{"name": "d", "path": "/srv/../etc"}]}}`,
		"directory root":            `{"storage": {"directories": [{"name": "d", "path": "/"}]}}`,
		"directory bad name":        `{"storage": {"directories": [{"name": "d/x", "path": "/srv/a"}]}}`,
		"directory path twice":      `{"storage": {"directories": [{"name": "a", "path": "/srv/a"}, {"name": "b", "path": "/srv/a"}]}}`,
		"directory nested":          `{"storage": {"directories": [{"name": "a", "path": "/srv/a"}, {"name": "b", "path": "/srv/a/b"}]}}`,
		"capacity warn above fail":  `{"storage": {"capacity_warn_percent": 95}}`,
		"capacity fail above 100":   `{"storage": {"capacity_fail_percent": 101}}`,
		"capacity warn negative":    `{"storage": {"capacity_warn_percent": -5}}`,
		"capacity equal":            `{"storage": {"capacity_warn_percent": 70, "capacity_fail_percent": 70}}`,
		"template package not base": `{"storage": {"template_packages": ["nginx"]}}`,
		"template package injected": `{"storage": {"template_packages": ["FreeBSD-set-minimal -o RUN_SCRIPTS=true"]}}`,
		"template package option":   `{"storage": {"template_packages": ["-oRUN_SCRIPTS=true"]}}`,
		"template package twice":    `{"storage": {"template_packages": ["FreeBSD-set-minimal", "FreeBSD-set-minimal"]}}`,
		"dist dir relative":         `{"storage": {"dist_dir": "dist"}}`,
		"dist dir not clean":        `{"storage": {"dist_dir": "/srv/../etc"}}`,
		"dist dir root":             `{"storage": {"dist_dir": "/"}}`,
	} {
		if _, err := Load(LoadOptions{Path: writeFile(t, body), LookupEnv: noEnv}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The confinement cannot come from the environment.
	cfg, err = Load(LoadOptions{DefaultPath: noDefault, LookupEnv: envMap(map[string]string{
		"SKEP_STORAGE_POOLS": "zroot", "SKEP_POOLS": "zroot", "SKEP_NETWORK_BRIDGES": "bridge0",
	})})
	if err != nil || len(cfg.Storage.Pools) != 0 || len(cfg.Network.Bridges) != 0 {
		t.Errorf("the environment changed the confinement: %+v %+v, %v", cfg.Storage, cfg.Network, err)
	}
}
