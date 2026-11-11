// Package config defines the typed Skep configuration model and loads it
// from defaults, an optional JSON file, environment variables, and command
// line overrides, in that order of increasing precedence.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Default locations and values.
const (
	// DefaultConfigPath is read when no path is given and the file exists.
	DefaultConfigPath = "/usr/local/etc/skep/agent.json"
	// DefaultListenAddress binds to loopback: the 0.1.0 API has no auth.
	DefaultListenAddress = "127.0.0.1:8765"
	// DefaultDataDir is the production state directory.
	DefaultDataDir = "/var/db/skep"
	// DefaultDevDataDir is the state directory in dev mode.
	DefaultDevDataDir = ".skep-dev"
	// DatabaseFileName is the SQLite file created inside the data directory.
	DatabaseFileName = "skep.db"
)

// Environment variable names.
const (
	EnvConfig    = "SKEP_CONFIG"
	EnvDev       = "SKEP_DEV"
	EnvListen    = "SKEP_LISTEN"
	EnvDataDir   = "SKEP_DATA_DIR"
	EnvLogLevel  = "SKEP_LOG_LEVEL"
	EnvLogFormat = "SKEP_LOG_FORMAT"
)

// Duration is a time.Duration that is written as a string such as "5s" in
// JSON configuration files.
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Config is the complete agent configuration.
type Config struct {
	// DevMode enables development defaults: a local data directory, text
	// logs, and debug logging. It never enables anything privileged.
	DevMode   bool            `json:"dev_mode"`
	DataDir   string          `json:"data_dir"`
	API       APIConfig       `json:"api"`
	Log       LogConfig       `json:"log"`
	Database  DatabaseConfig  `json:"database"`
	Discovery DiscoveryConfig `json:"discovery"`
}

// APIConfig configures the HTTP API listener.
type APIConfig struct {
	ListenAddress string `json:"listen_address"`
	// AllowInsecureRemote permits binding to a non-loopback address. The
	// 0.1.0 API is unauthenticated and unencrypted, so this is off by
	// default and must be an explicit, deliberate choice.
	AllowInsecureRemote bool     `json:"allow_insecure_remote"`
	ReadTimeout         Duration `json:"read_timeout"`
	WriteTimeout        Duration `json:"write_timeout"`
	ShutdownTimeout     Duration `json:"shutdown_timeout"`
}

// LogConfig configures structured logging.
type LogConfig struct {
	// Level is one of debug, info, warn, error.
	Level string `json:"level"`
	// Format is "json" or "text".
	Format string `json:"format"`
}

// DatabaseConfig configures the local SQLite database.
type DatabaseConfig struct {
	// Path is the SQLite file. It defaults to skep.db in the data directory.
	Path string `json:"path"`
}

// DiscoveryConfig configures read-only host discovery.
type DiscoveryConfig struct {
	// CommandTimeout bounds each external read-only command.
	CommandTimeout Duration `json:"command_timeout"`
	// CacheTTL is how long a discovery result is reused.
	CacheTTL Duration `json:"cache_ttl"`
}

// Overrides are values set on the command line. Zero values mean "not set".
type Overrides struct {
	DevMode       *bool
	ListenAddress string
	DataDir       string
	LogLevel      string
	LogFormat     string
}

// LoadOptions controls Load.
type LoadOptions struct {
	// Path is an explicit configuration file. It must exist when set.
	Path string
	// DefaultPath is read when neither Path nor SKEP_CONFIG is set; it is
	// optional and may not exist. Defaults to DefaultConfigPath.
	DefaultPath string
	// LookupEnv reads environment variables. Defaults to os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Overrides are command line values, applied last.
	Overrides Overrides
}

// Load builds the configuration: file, then environment, then overrides,
// then defaults for anything still unset, followed by validation.
func Load(opts LoadOptions) (Config, error) {
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	var cfg Config
	path, explicit := opts.Path, opts.Path != ""
	if !explicit {
		if v, ok := lookup(EnvConfig); ok && v != "" {
			path, explicit = v, true
		} else if path = opts.DefaultPath; path == "" {
			path = DefaultConfigPath
		}
	}
	if err := loadFile(path, explicit, &cfg); err != nil {
		return Config{}, err
	}
	if err := applyEnv(lookup, &cfg); err != nil {
		return Config{}, err
	}
	applyOverrides(opts.Overrides, &cfg)
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadFile(path string, explicit bool, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return nil // the default file is optional
		}
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	if dec.More() {
		return fmt.Errorf("config: parse %s: unexpected data after the configuration object", path)
	}
	return nil
}

func applyEnv(lookup func(string) (string, bool), cfg *Config) error {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	if v := get(EnvDev); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: %s=%q is not a boolean", EnvDev, v)
		}
		cfg.DevMode = b
	}
	if v := get(EnvListen); v != "" {
		cfg.API.ListenAddress = v
	}
	if v := get(EnvDataDir); v != "" {
		cfg.DataDir = v
	}
	if v := get(EnvLogLevel); v != "" {
		cfg.Log.Level = v
	}
	if v := get(EnvLogFormat); v != "" {
		cfg.Log.Format = v
	}
	return nil
}

func applyOverrides(o Overrides, cfg *Config) {
	if o.DevMode != nil {
		cfg.DevMode = *o.DevMode
	}
	if o.ListenAddress != "" {
		cfg.API.ListenAddress = o.ListenAddress
	}
	if o.DataDir != "" {
		cfg.DataDir = o.DataDir
	}
	if o.LogLevel != "" {
		cfg.Log.Level = o.LogLevel
	}
	if o.LogFormat != "" {
		cfg.Log.Format = o.LogFormat
	}
}

// applyDefaults fills every unset field. Some defaults depend on DevMode.
func (c *Config) applyDefaults() {
	if c.DataDir == "" {
		if c.DevMode {
			c.DataDir = DefaultDevDataDir
		} else {
			c.DataDir = DefaultDataDir
		}
	}
	if c.API.ListenAddress == "" {
		c.API.ListenAddress = DefaultListenAddress
	}
	if c.API.ReadTimeout == 0 {
		c.API.ReadTimeout = Duration(15 * time.Second)
	}
	if c.API.WriteTimeout == 0 {
		c.API.WriteTimeout = Duration(30 * time.Second)
	}
	if c.API.ShutdownTimeout == 0 {
		c.API.ShutdownTimeout = Duration(10 * time.Second)
	}
	if c.Log.Level == "" {
		if c.DevMode {
			c.Log.Level = "debug"
		} else {
			c.Log.Level = "info"
		}
	}
	if c.Log.Format == "" {
		if c.DevMode {
			c.Log.Format = "text"
		} else {
			c.Log.Format = "json"
		}
	}
	c.Log.Level = strings.ToLower(c.Log.Level)
	c.Log.Format = strings.ToLower(c.Log.Format)
	if c.Database.Path == "" {
		c.Database.Path = filepath.Join(c.DataDir, DatabaseFileName)
	}
	if c.Discovery.CommandTimeout == 0 {
		c.Discovery.CommandTimeout = Duration(5 * time.Second)
	}
	if c.Discovery.CacheTTL == 0 {
		c.Discovery.CacheTTL = Duration(10 * time.Second)
	}
}

// Validate reports every problem in the configuration as one error.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.DataDir == "" {
		add("data_dir must not be empty")
	}
	if c.Database.Path == "" {
		add("database.path must not be empty")
	}
	if err := validateListen(c.API.ListenAddress, c.API.AllowInsecureRemote); err != nil {
		errs = append(errs, err)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level %q must be one of debug, info, warn, error", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format %q must be json or text", c.Log.Format)
	}
	for name, d := range map[string]Duration{
		"api.read_timeout":          c.API.ReadTimeout,
		"api.write_timeout":         c.API.WriteTimeout,
		"api.shutdown_timeout":      c.API.ShutdownTimeout,
		"discovery.command_timeout": c.Discovery.CommandTimeout,
		"discovery.cache_ttl":       c.Discovery.CacheTTL,
	} {
		if d <= 0 {
			add("%s must be positive", name)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config: invalid configuration: %w", errors.Join(errs...))
}

func validateListen(addr string, allowRemote bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("api.listen_address %q is not host:port: %w", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("api.listen_address %q has an invalid port", addr)
	}
	if allowRemote || IsLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("api.listen_address %q is not a loopback address; the API is "+
		"unauthenticated in this release, so binding elsewhere requires "+
		"api.allow_insecure_remote=true", addr)
}

// IsLoopbackHost reports whether host is "localhost" or a loopback IP
// literal. An empty host means "all interfaces" and is not loopback.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
