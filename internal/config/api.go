package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults for skep-api.
const (
	// DefaultAPIConfigPath is read when no path is given and the file
	// exists.
	DefaultAPIConfigPath = "/usr/local/etc/skep/api.json"
	// DefaultAPIListenAddress is loopback: opening the API to a network
	// is an explicit configuration change (ADR 0006 decision 7). Every
	// request except the health status and the login needs a session.
	DefaultAPIListenAddress = "127.0.0.1:8443"
)

// Environment variable names read by skep-api, in addition to SKEP_DEV,
// SKEP_DATA_DIR, SKEP_SOCKET, SKEP_LOG_LEVEL, and SKEP_LOG_FORMAT.
const (
	EnvAPIConfig = "SKEP_API_CONFIG"
	EnvAPIListen = "SKEP_API_LISTEN"
)

// API is the complete configuration of skep-api, the unprivileged process
// that serves the public API and reaches the host only through the agent's
// socket.
type API struct {
	// DevMode enables development defaults: a local data directory, text
	// logs, and debug logging. It never turns a security control off.
	DevMode bool   `json:"dev_mode"`
	DataDir string `json:"data_dir"`
	// ListenAddress is the TCP address of the public API.
	ListenAddress string `json:"listen_address"`
	// AgentSocket is the Unix socket of the agent's internal API.
	AgentSocket     string    `json:"agent_socket"`
	ReadTimeout     Duration  `json:"read_timeout"`
	WriteTimeout    Duration  `json:"write_timeout"`
	ShutdownTimeout Duration  `json:"shutdown_timeout"`
	Log             LogConfig `json:"log"`
	// Database is api.db: users, roles, sessions, and the access audit
	// log. It defaults to api/api.db in the data directory.
	Database DatabaseConfig `json:"database"`
	TLS      TLSConfig      `json:"tls"`
	Auth     AuthConfig     `json:"auth"`
	// Retention configures the archive of the access audit log (ADR 0007,
	// decision 7). tasks_days is a setting of the agent and is refused
	// here.
	Retention RetentionConfig `json:"retention"`
}

// AuthConfig configures authentication.
type AuthConfig struct {
	Argon2  Argon2Config  `json:"argon2"`
	Session SessionConfig `json:"session"`
	Login   LoginConfig   `json:"login"`
	// TrustedProxies lists the addresses or networks (CIDR) of reverse
	// proxies whose X-Forwarded-For header is believed when deciding a
	// request's source address. Empty, the default: the header is
	// ignored and the source address is the TCP peer.
	TrustedProxies []string `json:"trusted_proxies"`
}

// SessionConfig holds the session lifetimes.
type SessionConfig struct {
	// IdleTimeout ends a session that was not used for this long.
	// Default 30m, at most 24h.
	IdleTimeout Duration `json:"idle_timeout"`
	// AbsoluteTimeout ends a session this long after the login. Default
	// 12h, at most 720h (30 days).
	AbsoluteTimeout Duration `json:"absolute_timeout"`
}

// LoginConfig holds the login limits. They delay and refuse; they never
// lock an account.
type LoginConfig struct {
	// PerAddressPerMinute is the number of login attempts one source
	// address may make per minute. Default 10.
	PerAddressPerMinute int `json:"per_address_per_minute"`
	// FailuresBeforeDelay is the number of failures from one address for
	// one username after which further attempts are delayed, doubling
	// from one second. Default 5.
	FailuresBeforeDelay int `json:"failures_before_delay"`
	// MaxDelay caps that delay. Default 1m.
	MaxDelay Duration `json:"max_delay"`
	// PerUsernameFailures is the number of failures for one username,
	// from all addresses together, within 15 minutes, above which logins
	// for that username are refused until the window drains. Default 50.
	PerUsernameFailures int `json:"per_username_failures"`
}

// Limits of the session lifetimes.
const (
	MaxSessionIdleTimeout     = 24 * time.Hour
	MaxSessionAbsoluteTimeout = 30 * 24 * time.Hour
)

// Argon2Config holds the Argon2id cost parameters for new password hashes.
// They can be raised, never set below the minimum. Existing hashes keep
// working and are recomputed at the next successful login.
type Argon2Config struct {
	// MemoryKiB is the memory per hash. Default 65536 (64 MiB).
	MemoryKiB uint32 `json:"memory_kib"`
	// Passes is the number of passes. Default 3. If one hash takes much
	// less than 100 ms or much more than 500 ms on the host, change
	// this, not the memory ("skep-api admin hash-benchmark" measures it).
	Passes uint32 `json:"passes"`
}

// Bounds of the Argon2id parameters. The lower bound is the OWASP minimum
// (19 MiB, 2 passes); internal/auth enforces the same range.
const (
	DefaultArgon2MemoryKiB = 64 * 1024
	DefaultArgon2Passes    = 3
	MinArgon2MemoryKiB     = 19 * 1024
	MinArgon2Passes        = 2
	MaxArgon2MemoryKiB     = 1024 * 1024
	MaxArgon2Passes        = 16
)

// TLSConfig selects the certificate skep-api serves. The API is TLS 1.3
// only; there is no plaintext listener and no setting that adds one.
type TLSConfig struct {
	// CertFile and KeyFile name an operator-supplied certificate (PEM,
	// leaf first) and its key. Set both or neither. When they are empty,
	// skep-api generates a self-signed certificate in api/tls in the data
	// directory and renews it by itself.
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

// APIStateDir is skep-api's private directory, relative to the data
// directory. It holds api.db and the generated TLS key.
const APIStateDir = "api"

// StateDir returns skep-api's private state directory.
func (c API) StateDir() string { return filepath.Join(c.DataDir, APIStateDir) }

// AuditArchiveDir returns the directory of the access audit log's archive
// segments: next to api.db, in the directory only the service account can
// open.
func (c API) AuditArchiveDir() string {
	return filepath.Join(filepath.Dir(c.Database.Path), AuditArchiveDirName)
}

// TLSDir returns the directory of the generated key and certificate.
func (c API) TLSDir() string { return filepath.Join(c.StateDir(), "tls") }

// APIOverrides are skep-api's command line values. Zero values mean "not
// set".
type APIOverrides struct {
	DevMode       *bool
	ListenAddress string
	AgentSocket   string
	DataDir       string
	LogLevel      string
	LogFormat     string
}

// APILoadOptions controls LoadAPI.
type APILoadOptions struct {
	// Path is an explicit configuration file. It must exist when set.
	Path string
	// DefaultPath is read when neither Path nor SKEP_API_CONFIG is set;
	// it may not exist. Defaults to DefaultAPIConfigPath.
	DefaultPath string
	// LookupEnv reads environment variables. Defaults to os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Overrides are command line values, applied last.
	Overrides APIOverrides
}

// LoadAPI builds skep-api's configuration: file, then environment, then
// overrides, then defaults, followed by validation.
func LoadAPI(opts APILoadOptions) (API, error) {
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = osLookupEnv
	}
	var cfg API
	path, explicit := opts.Path, opts.Path != ""
	if !explicit {
		if v, ok := lookup(EnvAPIConfig); ok && v != "" {
			path, explicit = v, true
		} else if path = opts.DefaultPath; path == "" {
			path = DefaultAPIConfigPath
		}
	}
	if err := loadFile(path, explicit, &cfg); err != nil {
		return API{}, err
	}

	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	if v := get(EnvDev); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return API{}, fmt.Errorf("config: %s=%q is not a boolean", EnvDev, v)
		}
		cfg.DevMode = b
	}
	for name, dst := range map[string]*string{
		EnvAPIListen: &cfg.ListenAddress,
		EnvSocket:    &cfg.AgentSocket,
		EnvDataDir:   &cfg.DataDir,
		EnvLogLevel:  &cfg.Log.Level,
		EnvLogFormat: &cfg.Log.Format,
	} {
		if v := get(name); v != "" {
			*dst = v
		}
	}

	o := opts.Overrides
	if o.DevMode != nil {
		cfg.DevMode = *o.DevMode
	}
	for value, dst := range map[*string]*string{
		&o.ListenAddress: &cfg.ListenAddress,
		&o.AgentSocket:   &cfg.AgentSocket,
		&o.DataDir:       &cfg.DataDir,
		&o.LogLevel:      &cfg.Log.Level,
		&o.LogFormat:     &cfg.Log.Format,
	} {
		if *value != "" {
			*dst = *value
		}
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return API{}, err
	}
	return cfg, nil
}

func (c *API) applyDefaults() {
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
		if c.DevMode {
			c.DataDir = DefaultDevDataDir
		}
	}
	if c.ListenAddress == "" {
		c.ListenAddress = DefaultAPIListenAddress
	}
	if c.AgentSocket == "" {
		c.AgentSocket = DefaultSocketPath
		if c.DevMode {
			c.AgentSocket = filepath.Join(c.DataDir, filepath.FromSlash(DevSocketPath))
		}
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = Duration(15 * time.Second)
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = Duration(60 * time.Second)
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = Duration(10 * time.Second)
	}
	c.Log.applyDefaults(c.DevMode)
	if c.Database.Path == "" {
		c.Database.Path = filepath.Join(c.StateDir(), "api.db")
	}
	if c.Auth.Argon2.MemoryKiB == 0 {
		c.Auth.Argon2.MemoryKiB = DefaultArgon2MemoryKiB
	}
	if c.Auth.Argon2.Passes == 0 {
		c.Auth.Argon2.Passes = DefaultArgon2Passes
	}
	if c.Auth.Session.IdleTimeout == 0 {
		c.Auth.Session.IdleTimeout = Duration(30 * time.Minute)
	}
	if c.Auth.Session.AbsoluteTimeout == 0 {
		c.Auth.Session.AbsoluteTimeout = Duration(12 * time.Hour)
	}
	if c.Auth.Login.PerAddressPerMinute == 0 {
		c.Auth.Login.PerAddressPerMinute = 10
	}
	if c.Auth.Login.FailuresBeforeDelay == 0 {
		c.Auth.Login.FailuresBeforeDelay = 5
	}
	if c.Auth.Login.MaxDelay == 0 {
		c.Auth.Login.MaxDelay = Duration(time.Minute)
	}
	if c.Auth.Login.PerUsernameFailures == 0 {
		c.Auth.Login.PerUsernameFailures = 50
	}
	if c.Auth.TrustedProxies == nil {
		c.Auth.TrustedProxies = []string{}
	}
	// The access log grows with unauthenticated traffic: its entries are
	// removed 90 days after their month, once sealed and verified.
	c.Retention.applyDefaults(nil, DefaultAccessAuditLiveDays)
}

// TrustedProxyPrefixes parses auth.trusted_proxies. A bare address is a
// network of one.
func (c API) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, v := range c.Auth.TrustedProxies {
		if p, err := netip.ParsePrefix(v); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(v)
		if err != nil {
			return nil, fmt.Errorf("auth.trusted_proxies: %q is not an address or a network in CIDR form", v)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// Validate reports every problem in the configuration as one error.
func (c API) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if c.DataDir == "" {
		add("data_dir must not be empty")
	}
	if c.AgentSocket == "" {
		add("agent_socket must not be empty")
	}
	if err := c.validateListen(); err != nil {
		errs = append(errs, err)
	}
	if c.Database.Path == "" {
		add("database.path must not be empty")
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		add("tls.cert_file and tls.key_file must be set together")
	}
	for name, f := range map[string]string{"tls.cert_file": c.TLS.CertFile, "tls.key_file": c.TLS.KeyFile} {
		if f != "" && !filepath.IsAbs(f) {
			add("%s %q must be an absolute path", name, f)
		}
	}
	if m := c.Auth.Argon2.MemoryKiB; m < MinArgon2MemoryKiB || m > MaxArgon2MemoryKiB {
		add("auth.argon2.memory_kib must be between %d and %d", MinArgon2MemoryKiB, MaxArgon2MemoryKiB)
	}
	if t := c.Auth.Argon2.Passes; t < MinArgon2Passes || t > MaxArgon2Passes {
		add("auth.argon2.passes must be between %d and %d", MinArgon2Passes, MaxArgon2Passes)
	}
	idle, absolute := c.Auth.Session.IdleTimeout.Std(), c.Auth.Session.AbsoluteTimeout.Std()
	if idle < time.Minute || idle > MaxSessionIdleTimeout {
		add("auth.session.idle_timeout must be between 1m and %s", MaxSessionIdleTimeout)
	}
	if absolute < idle || absolute > MaxSessionAbsoluteTimeout {
		add("auth.session.absolute_timeout must be at least the idle timeout and at most %s", MaxSessionAbsoluteTimeout)
	}
	if n := c.Auth.Login.PerAddressPerMinute; n < 1 || n > 10000 {
		add("auth.login.per_address_per_minute must be between 1 and 10000")
	}
	if n := c.Auth.Login.FailuresBeforeDelay; n < 1 || n > 1000 {
		add("auth.login.failures_before_delay must be between 1 and 1000")
	}
	if d := c.Auth.Login.MaxDelay.Std(); d < time.Second || d > time.Hour {
		add("auth.login.max_delay must be between 1s and 1h")
	}
	if n := c.Auth.Login.PerUsernameFailures; n < 1 || n > 100000 {
		add("auth.login.per_username_failures must be between 1 and 100000")
	}
	if _, err := c.TrustedProxyPrefixes(); err != nil {
		errs = append(errs, err)
	}
	if c.Retention.TasksDays != nil {
		add("retention.tasks_days is a setting of skep-agent (agent.json): skep-api has no tasks")
	}
	errs = append(errs, c.Retention.validate()...)
	errs = append(errs, c.Log.validate()...)
	for name, d := range map[string]Duration{
		"read_timeout":     c.ReadTimeout,
		"write_timeout":    c.WriteTimeout,
		"shutdown_timeout": c.ShutdownTimeout,
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

func (c API) validateListen() error {
	_, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen_address %q is not host:port: %w", c.ListenAddress, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("listen_address %q has an invalid port", c.ListenAddress)
	}
	return nil
}
