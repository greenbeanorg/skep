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
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Default locations and values.
const (
	// DefaultConfigPath is read when no path is given and the file exists.
	DefaultConfigPath = "/usr/local/etc/skep/agent.json"
	// DefaultDataDir is the production state directory.
	DefaultDataDir = "/var/db/skep"
	// DefaultDevDataDir is the state directory in dev mode.
	DefaultDevDataDir = ".skep-dev"
	// DefaultSocketPath is the agent's Unix socket in production.
	DefaultSocketPath = "/var/run/skep/agent.sock"
	// DevSocketPath is the agent's socket in dev mode, relative to the
	// data directory.
	DevSocketPath = "run/agent.sock"
	// DefaultSocketGroup owns the socket in production; its members may
	// open it.
	DefaultSocketGroup = "skep"
	// AgentDatabasePath is the agent's SQLite file, relative to the data
	// directory. The agent's state lives in its own subdirectory because
	// skep-api will keep a separate database next to it (ADR 0004).
	AgentDatabasePath = "agent/agent.db"
)

// Environment variable names.
const (
	EnvConfig    = "SKEP_CONFIG"
	EnvDev       = "SKEP_DEV"
	EnvSocket    = "SKEP_SOCKET"
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

// Config is the complete agent configuration. The agent has no network
// listener: skep-api reaches it through the Unix socket only (ADR 0004).
type Config struct {
	// DevMode enables development defaults: a local data directory, text
	// logs, and debug logging. It never enables anything privileged.
	DevMode bool         `json:"dev_mode"`
	DataDir string       `json:"data_dir"`
	Socket  SocketConfig `json:"socket"`
	// ShutdownTimeout bounds a graceful stop: open requests and running
	// tasks get this long to finish.
	ShutdownTimeout Duration        `json:"shutdown_timeout"`
	Log             LogConfig       `json:"log"`
	Database        DatabaseConfig  `json:"database"`
	Discovery       DiscoveryConfig `json:"discovery"`
	Tasks           TasksConfig     `json:"tasks"`
	Retention       RetentionConfig `json:"retention"`
	// Hypervisor, Storage, and Network are the host confinement (ADR 0004
	// rule 4, ADR 0009 decision 1c): what Skep may touch on this host.
	// They are read from the root-owned file only; nothing in the API can
	// change them, and there is no environment variable or flag for them.
	Hypervisor HypervisorConfig `json:"hypervisor"`
	Storage    StorageConfig    `json:"storage"`
	Network    NetworkConfig    `json:"network"`
	// Containers configures jails (ADR 0010 decision 8).
	Containers ContainersConfig `json:"containers"`
}

// RetentionConfig says how long history is kept (ADR 0007). The fields are
// pointers so that a value that was not set can be told from an explicit
// zero, which means "never" for both numbers.
type RetentionConfig struct {
	// TasksDays is the age, in days since it finished, at which a task is
	// deleted with its events. 0 keeps tasks forever; otherwise at least
	// MinTaskRetentionDays. Default 90. The agent only.
	TasksDays *int `json:"tasks_days,omitempty"`
	// AuditLiveDays is the number of days after the end of its month that
	// a sealed audit segment's entries stay in the database. 0 seals
	// segments and never removes an entry; otherwise at least
	// MinAuditLiveDays. Default 0 for the agent's operation audit log and
	// 90 for skep-api's access audit log.
	AuditLiveDays *int `json:"audit_live_days"`
	// AuditArchive turns the monthly sealing of archive segments on.
	// Default true. When false nothing is sealed and no audit entry is
	// ever removed, whatever AuditLiveDays says.
	AuditArchive *bool `json:"audit_archive"`
}

// Limits of the retention settings.
const (
	// DefaultTaskRetentionDays is how long finished tasks are kept.
	DefaultTaskRetentionDays = 90
	// MinTaskRetentionDays is the shortest task retention. An idempotency
	// key is forgotten with its task, and a key replayed after that
	// creates a second task; seven days is far beyond any retry window.
	MinTaskRetentionDays = 7
	// DefaultAccessAuditLiveDays is how long skep-api's access audit
	// entries stay in the database after their month ended.
	DefaultAccessAuditLiveDays = 90
	// MinAuditLiveDays is the shortest time sealed audit entries stay in
	// the database, so a month can always be sealed before it is due for
	// removal.
	MinAuditLiveDays = 35
	// MaxRetentionDays bounds both numbers (100 years).
	MaxRetentionDays = 36500
	// AuditArchiveDirName is the archive directory, next to the database.
	AuditArchiveDirName = "audit-archive"
)

// applyDefaults fills the unset fields. liveDays is the default of
// AuditLiveDays, which differs between the two logs.
func (r *RetentionConfig) applyDefaults(tasksDays *int, liveDays int) {
	if r.TasksDays == nil {
		r.TasksDays = tasksDays
	}
	if r.AuditLiveDays == nil {
		r.AuditLiveDays = &liveDays
	}
	if r.AuditArchive == nil {
		on := true
		r.AuditArchive = &on
	}
}

func (r RetentionConfig) validate() []error {
	var errs []error
	if d := r.TasksDays; d != nil && *d != 0 && (*d < MinTaskRetentionDays || *d > MaxRetentionDays) {
		errs = append(errs, fmt.Errorf("retention.tasks_days must be 0 (keep forever) or between %d and %d: "+
			"an idempotency key is forgotten with its task", MinTaskRetentionDays, MaxRetentionDays))
	}
	if d := r.AuditLiveDays; d != nil && *d != 0 && (*d < MinAuditLiveDays || *d > MaxRetentionDays) {
		errs = append(errs, fmt.Errorf("retention.audit_live_days must be 0 (never remove) or between %d and %d: "+
			"a month must be sealed before it is due for removal", MinAuditLiveDays, MaxRetentionDays))
	}
	return errs
}

// AuditLiveDaysOrZero returns the number of days sealed entries stay live,
// or 0 when they are never removed: by choice, or because archiving is off.
func (r RetentionConfig) AuditLiveDaysOrZero() int {
	if r.AuditArchive == nil || !*r.AuditArchive || r.AuditLiveDays == nil {
		return 0
	}
	return *r.AuditLiveDays
}

// ArchiveEnabled reports whether archive segments are sealed.
func (r RetentionConfig) ArchiveEnabled() bool { return r.AuditArchive != nil && *r.AuditArchive }

// AuditArchiveDir returns the directory of the agent's audit archive
// segments: next to the database, in the directory only root can open.
func (c Config) AuditArchiveDir() string {
	return filepath.Join(filepath.Dir(c.Database.Path), AuditArchiveDirName)
}

// TasksConfig configures the task engine.
type TasksConfig struct {
	// Workers is the number of tasks that may run at once.
	Workers int `json:"workers"`
	// QueueSize is the number of accepted tasks that may wait for a worker.
	// Further submissions are refused until there is room.
	QueueSize int `json:"queue_size"`
}

// SocketConfig configures the Unix socket of the internal API, the only
// way skep-api reaches the agent (ADR 0004).
type SocketConfig struct {
	// Path is the socket file.
	Path string `json:"path"`
	// Group owns the socket, which gets mode 0660. Empty leaves the
	// group alone and uses mode 0600; that is the dev mode default.
	Group string `json:"group"`
	// AllowedUsers lists who may connect, by user name or numeric UID.
	// The agent checks the kernel-reported peer of every connection
	// against it. Production default: root and skep. Dev mode default:
	// the user the agent runs as.
	AllowedUsers []string `json:"allowed_users"`
	// RequestTimeout bounds one request.
	RequestTimeout Duration `json:"request_timeout"`
	// MaxConnections bounds the open connections.
	MaxConnections int `json:"max_connections"`
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
	// Path is the agent's SQLite file. It defaults to agent/agent.db in
	// the data directory.
	Path string `json:"path"`
}

// DiscoveryConfig configures read-only host discovery.
type DiscoveryConfig struct {
	// CommandTimeout bounds each external read-only command.
	CommandTimeout Duration `json:"command_timeout"`
	// CacheTTL is how long a discovery result is reused.
	CacheTTL Duration `json:"cache_ttl"`
}

// applyDefaults fills in the level and format, which depend on dev mode.
func (l *LogConfig) applyDefaults(devMode bool) {
	if l.Level == "" {
		l.Level = "info"
		if devMode {
			l.Level = "debug"
		}
	}
	if l.Format == "" {
		l.Format = "json"
		if devMode {
			l.Format = "text"
		}
	}
	l.Level = strings.ToLower(l.Level)
	l.Format = strings.ToLower(l.Format)
}

func (l LogConfig) validate() []error {
	var errs []error
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q must be one of debug, info, warn, error", l.Level))
	}
	switch l.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format %q must be json or text", l.Format))
	}
	return errs
}

// Overrides are values set on the command line. Zero values mean "not set".
type Overrides struct {
	DevMode    *bool
	SocketPath string
	DataDir    string
	LogLevel   string
	LogFormat  string
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
		lookup = osLookupEnv
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

func loadFile(path string, explicit bool, cfg any) error {
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
	if v := get(EnvSocket); v != "" {
		cfg.Socket.Path = v
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
	if o.SocketPath != "" {
		cfg.Socket.Path = o.SocketPath
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
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = Duration(10 * time.Second)
	}
	if c.Socket.Path == "" {
		if c.DevMode {
			c.Socket.Path = filepath.Join(c.DataDir, filepath.FromSlash(DevSocketPath))
		} else {
			c.Socket.Path = DefaultSocketPath
		}
	}
	if c.Socket.Group == "" && !c.DevMode {
		c.Socket.Group = DefaultSocketGroup
	}
	if c.Socket.AllowedUsers == nil {
		if c.DevMode {
			c.Socket.AllowedUsers = []string{strconv.Itoa(os.Geteuid())}
		} else {
			c.Socket.AllowedUsers = []string{"root", DefaultSocketGroup}
		}
	}
	if c.Socket.RequestTimeout == 0 {
		c.Socket.RequestTimeout = Duration(30 * time.Second)
	}
	if c.Socket.MaxConnections == 0 {
		c.Socket.MaxConnections = 64
	}
	c.Log.applyDefaults(c.DevMode)
	if c.Database.Path == "" {
		c.Database.Path = filepath.Join(c.DataDir, filepath.FromSlash(AgentDatabasePath))
	}
	if c.Discovery.CommandTimeout == 0 {
		c.Discovery.CommandTimeout = Duration(5 * time.Second)
	}
	if c.Discovery.CacheTTL == 0 {
		c.Discovery.CacheTTL = Duration(10 * time.Second)
	}
	if c.Tasks.Workers == 0 {
		c.Tasks.Workers = 4
	}
	if c.Tasks.QueueSize == 0 {
		c.Tasks.QueueSize = 64
	}
	c.applyConfinementDefaults()
	tasksDays := DefaultTaskRetentionDays
	// The agent's audit log is sealed and never removed unless the
	// operator opts in (ADR 0007, amendment a).
	c.Retention.applyDefaults(&tasksDays, 0)
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
	errs = append(errs, c.Log.validate()...)
	for name, d := range map[string]Duration{
		"shutdown_timeout":          c.ShutdownTimeout,
		"socket.request_timeout":    c.Socket.RequestTimeout,
		"discovery.command_timeout": c.Discovery.CommandTimeout,
		"discovery.cache_ttl":       c.Discovery.CacheTTL,
	} {
		if d <= 0 {
			add("%s must be positive", name)
		}
	}
	if c.Socket.Path == "" {
		add("socket.path must not be empty")
	} else if len(c.Socket.Path) > MaxSocketPath {
		add("socket.path %q is longer than %d bytes", c.Socket.Path, MaxSocketPath)
	}
	if len(c.Socket.AllowedUsers) == 0 {
		add("socket.allowed_users must not be empty: nobody could connect")
	}
	for _, u := range c.Socket.AllowedUsers {
		if !accountRE.MatchString(u) {
			add("socket.allowed_users: %q is not a user name or UID", u)
		}
	}
	if c.Socket.Group != "" && !accountRE.MatchString(c.Socket.Group) {
		add("socket.group %q is not a group name or GID", c.Socket.Group)
	}
	if c.Socket.MaxConnections < 1 || c.Socket.MaxConnections > 1024 {
		add("socket.max_connections must be between 1 and 1024")
	}
	if c.Tasks.Workers < 1 || c.Tasks.Workers > 64 {
		add("tasks.workers must be between 1 and 64")
	}
	if c.Tasks.QueueSize < 1 || c.Tasks.QueueSize > 4096 {
		add("tasks.queue_size must be between 1 and 4096")
	}
	errs = append(errs, c.Retention.validate()...)
	errs = append(errs, c.validateConfinement()...)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config: invalid configuration: %w", errors.Join(errs...))
}

// MaxSocketPath is the longest socket path accepted: sun_path holds 104
// bytes on FreeBSD, and the agent needs room for a temporary suffix.
const MaxSocketPath = 88

// accountRE is the accepted shape of a user or group name, or a numeric ID.
var accountRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`)

// IsLoopbackHost reports whether host is "localhost" or a loopback IP
// literal. An empty host means "all interfaces" and is not loopback.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// osLookupEnv is os.LookupEnv; a variable so both loaders share it.
var osLookupEnv = os.LookupEnv
