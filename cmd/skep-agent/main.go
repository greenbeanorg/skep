// Command skep-agent is the Skep host agent. It runs on every node as
// root, discovers the host, keeps local state in SQLite, and runs tasks.
//
// It has no network listener. Its only interface is the internal API on a
// Unix socket, for skep-api, which checks the kernel-reported peer of every
// connection (docs/decisions/0004-privilege-boundary.md).
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/greenbeanorg/skep/internal/agent"
	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auditarchive"
	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/console"
	"github.com/greenbeanorg/skep/internal/containers"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hardware"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hypervisor/bhyve"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/internal/logging"
	"github.com/greenbeanorg/skep/internal/monitoring"
	"github.com/greenbeanorg/skep/internal/orphans"
	"github.com/greenbeanorg/skep/internal/portcheck"
	"github.com/greenbeanorg/skep/internal/retention"
	"github.com/greenbeanorg/skep/internal/storage"
	"github.com/greenbeanorg/skep/internal/sysaccount"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/internal/templates"
	"github.com/greenbeanorg/skep/internal/vm"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
	"github.com/greenbeanorg/skep/pkg/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "skep-agent:", err)
		os.Exit(1)
	}
}

// run is the whole program; main only maps its error to an exit code.
// Without a command it runs the agent; "audit" is a local command for the
// host's administrator.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "audit" {
		return runAudit(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == hostops.RunnerCommand {
		return runRunner(args[1:], stderr)
	}
	flags := flag.NewFlagSet("skep-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		configPath  = flags.String("config", "", "path to the JSON configuration file (default "+config.DefaultConfigPath+" if it exists)")
		dev         = flags.Bool("dev", false, "development mode: local data directory, text logs, debug level")
		socketPath  = flags.String("socket", "", "Unix socket of the internal API (default "+config.DefaultSocketPath+")")
		dataDir     = flags.String("data-dir", "", "state directory")
		logLevel    = flags.String("log-level", "", "debug, info, warn, or error")
		logFormat   = flags.String("log-format", "", "json or text")
		showVersion = flags.Bool("version", false, "print the version and exit")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *showVersion {
		fmt.Fprintln(stdout, "skep-agent", version.Get())
		if testTasksBuilt {
			fmt.Fprintln(stdout, "TEST BUILD: contains test-only task types; not for installation")
		}
		return nil
	}

	overrides := config.Overrides{
		SocketPath: *socketPath,
		DataDir:    *dataDir,
		LogLevel:   *logLevel,
		LogFormat:  *logFormat,
	}
	// Only an explicit -dev overrides the file and environment.
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "dev" {
			overrides.DevMode = dev
		}
	})
	cfg, err := config.Load(config.LoadOptions{Path: *configPath, Overrides: overrides})
	if err != nil {
		return err
	}

	log, err := logging.New(stderr, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	log.Info("starting skep-agent",
		"version", version.Version, "commit", version.Commit,
		"dev_mode", cfg.DevMode, "data_dir", cfg.DataDir)

	db, err := database.Open(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := database.Migrate(ctx, db, migrations.Agent())
	if err != nil {
		return err
	}
	schema, err := database.SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	log.Info("database ready", "path", cfg.Database.Path, "schema_version", schema, "migrations_applied", len(applied))

	disc := hardware.New(hardware.Options{CommandTimeout: cfg.Discovery.CommandTimeout.Std()})
	hostname, _ := os.Hostname()
	nodeID, err := database.EnsureLocalNode(ctx, db, hostname)
	if err != nil {
		return err
	}
	if runtime.GOOS != "freebsd" {
		log.Warn("this is not a FreeBSD host: running as a development build only, never as a production host")
	}

	// Everything that needs root goes through hostops, bounded by the
	// confinement in the root-owned configuration (ADR 0009 decision 1).
	host, err := newHostOps(confinement(cfg), log)
	if err != nil {
		return err
	}
	log.Info("host confinement", "pools", cfg.Storage.Pools, "dataset_root", cfg.Storage.DatasetRoot,
		"bridges", cfg.Network.Bridges, "management_interfaces", cfg.Network.ManagementInterfaces,
		"media_dir", cfg.Storage.MediaDir, "firmware_dir", cfg.Hypervisor.Bhyve.FirmwareDir)
	if len(cfg.Storage.Pools) == 0 {
		log.Warn("storage.pools is empty: no VM can be created until the configuration names a pool")
	}

	taskStore := database.NewTaskStore(db)
	engine, err := tasks.New(tasks.Options{
		Store:     taskStore,
		Audit:     database.NewAuditLog(db),
		NodeID:    nodeID,
		Workers:   cfg.Tasks.Workers,
		QueueSize: cfg.Tasks.QueueSize,
		Logger:    log,
	})
	if err != nil {
		return err
	}
	// Retention (ADR 0007): the audit archive, its health check, and the
	// task that maintains it and deletes old tasks.
	archive, err := newArchive(db, cfg, nodeID, log)
	if err != nil {
		return err
	}
	checks := map[string]models.HealthProbe{}
	if archive != nil {
		// Degraded when the archive fails verification, and when last
		// month is still unsealed 7 days into this one.
		checks[models.CheckAuditArchive] = archive.Health
	}
	// The VM service: every write is a task, run against the bhyve
	// adapter, which reaches the host through hostops only.
	engineBhyve := bhyve.New(bhyve.Options{
		Host:                   host,
		HostMemoryReserveBytes: *cfg.Hypervisor.Bhyve.HostMemoryReserveBytes,
		MaxVCPUsPerVM:          cfg.Hypervisor.Bhyve.MaxVCPUsPerVM,
		Logger:                 log,
	})
	consoles := console.New(console.Options{Open: host.VMM.OpenConsole, Audit: database.NewAuditLog(db), Logger: log})
	vms, err := vm.New(vm.Options{
		Console:      consoles,
		NodeID:       nodeID,
		Store:        database.NewVMStore(db),
		Snapshots:    database.NewSnapshotStore(db),
		MaxSnapshots: cfg.Storage.MaxSnapshotsPerResource,
		Engine:       engine,
		Hypervisor:   engineBhyve,
		Host:         host,
		Logger:       log,
	})
	if err != nil {
		return err
	}
	for _, def := range vms.Definitions() {
		if err := engine.Register(def); err != nil {
			return err
		}
	}
	// Jail templates (ADR 0010 decision 7): built by a task, as admin
	// only, from the configured packages or the dist directory.
	templateSvc, err := templates.New(templates.Options{
		NodeID:   nodeID,
		Store:    database.NewTemplateStore(db),
		Engine:   engine,
		Host:     host,
		Packages: cfg.Storage.TemplatePackages,
		Logger:   log,
	})
	if err != nil {
		return err
	}
	for _, def := range templateSvc.Definitions() {
		if err := engine.Register(def); err != nil {
			return err
		}
	}
	// Jails (ADR 0010 decisions 8, 9, and 13): clones of a template, run
	// by jail(8) from a generated jail.conf. The host's total of locked
	// memory defaults to a quarter of hw.physmem, read once here
	// (amendment o, SR-04).
	physMem := host.VMM.Capabilities(ctx).HostMemoryBytes
	maxLocked := containers.MaxLockedMemory(cfg.Containers.MaxLockedMemoryBytes, physMem)
	if maxLocked == 0 {
		log.Warn("jails may lock no memory: physical memory could not be read and containers.max_locked_memory_bytes is not set")
	}
	log.Info("jail locked-memory total", "max_locked_memory_bytes", maxLocked, "physical_memory_bytes", physMem,
		"configured", cfg.Containers.MaxLockedMemoryBytes > 0)
	containerSvc, err := containers.New(containers.Options{
		NodeID:          nodeID,
		Store:           database.NewContainerStore(db),
		Templates:       database.NewTemplateStore(db),
		Snapshots:       database.NewSnapshotStore(db),
		MaxSnapshots:    cfg.Storage.MaxSnapshotsPerResource,
		Engine:          engine,
		Host:            host,
		StopTimeout:     time.Duration(cfg.Containers.StopTimeoutSeconds) * time.Second,
		StartTimeout:    time.Duration(cfg.Containers.StartTimeoutSeconds) * time.Second,
		MaxLockedMemory: cfg.Containers.MaxLockedMemoryBytes,
		PhysicalMemory:  physMem,
		Logger:          log,
	})
	if err != nil {
		return err
	}
	for _, def := range containerSvc.Definitions() {
		if err := engine.Register(def); err != nil {
			return err
		}
	}
	// The storage inventory reads the host and the database; it changes
	// nothing.
	storageSvc, err := storage.New(storage.Options{
		Host:   host,
		Owners: database.NewStorageStore(db),
		Logger: log,
	})
	if err != nil {
		return err
	}
	// Current resource use (ADR 0010 decision 15), which also feeds the
	// two storage health checks.
	sampler, err := monitoring.New(monitoring.Options{
		NodeID:      nodeID,
		Host:        host,
		VMs:         vms,
		Containers:  containerSvc,
		WarnPercent: cfg.Storage.CapacityWarnPercent,
		FailPercent: cfg.Storage.CapacityFailPercent,
		MaxAge:      2 * monitoring.DefaultInterval,
		Logger:      log,
	})
	if err != nil {
		return err
	}
	checks[models.CheckStorageCapacity] = sampler.CapacityProbe
	checks[models.CheckStorageLocked] = sampler.LockedProbe
	// Interfaces Skep made whose owner is not running (ADR 0010 decision
	// 10): reported at start and by a health check, destroyed by the
	// owner's next start, stop, or delete.
	orphanChecker, err := orphans.New(orphans.Options{
		Jails:     host.Jails,
		VMM:       host.VMM,
		Container: orphans.ContainerOwner(database.NewContainerStore(db).Get),
		VM:        orphans.VMOwner(database.NewVMStore(db).Get),
		Logger:    log,
	})
	if err != nil {
		return err
	}
	checks[models.CheckOrphanedInterfaces] = orphanChecker.Probe
	// The anti-spoofing filters of running guests' ports (ADR 0011
	// decision 5): checked at start, before the socket is served, and
	// every 10 seconds; repaired, or the port detached, when one is
	// missing or wrong.
	portChecker, err := portcheck.New(portcheck.Options{
		Host:   host,
		Ports:  runningPorts(containerSvc, vms, log),
		Audit:  database.NewAuditLog(db),
		Logger: log,
	})
	if err != nil {
		return err
	}
	checks[models.CheckPortFilters] = portChecker.Probe
	checks[models.CheckBridgeVLANUnfiltered] = portChecker.VLANProbe
	svc, err := agent.NewService(agent.Options{
		Bhyve:      vms.Capabilities,
		Containers: containerSvc.Capabilities,
		NodeID:     nodeID,
		DevMode:    cfg.DevMode,
		Discoverer: disc,
		Database:   db,
		Nodes:      database.NewNodeStore(db),
		Tasks:      engine,
		Checks:     checks,
		CacheTTL:   cfg.Discovery.CacheTTL.Std(),
		Logger:     log,
	})
	if err != nil {
		return err
	}
	if err := engine.Register(svc.RefreshDefinition()); err != nil {
		return err
	}
	keeper, err := retention.New(retention.Options{
		Tasks:         taskStore,
		TasksDays:     *cfg.Retention.TasksDays,
		Archive:       archive,
		AuditLiveDays: cfg.Retention.AuditLiveDaysOrZero(),
		Logger:        log,
	})
	if err != nil {
		return err
	}
	if err := engine.Register(keeper.Definition()); err != nil {
		return err
	}
	// Nothing in a shipped build; see testtasks.go.
	if err := registerTestTasks(engine); err != nil {
		return err
	}

	// Start marks tasks left unfinished by a previous run as interrupted.
	// It must finish before anything can be submitted or served.
	if err := engine.Start(ctx); err != nil {
		return err
	}

	// Refresh the node inventory once at startup, as a task, so it leaves
	// the same history and audit trail as any other operation. A refusal
	// here is not fatal: the agent must stay up.
	if _, _, err := engine.Submit(ctx, tasks.Request{
		Type:         agent.TaskDiscoveryRefresh,
		ResourceType: agent.NodeResource,
		ResourceID:   nodeID,
		Actor:        models.TaskActor{ID: audit.System, Name: audit.System},
		PeerUID:      uint32(os.Geteuid()),
	}); err != nil {
		log.Warn("could not submit the initial discovery task", "error", err)
	}
	submitTestTasks(ctx, engine, log)

	// Retention runs once now, after the startup sweep above, and then
	// once a day. It is a plain timer; the work itself is a task.
	log.Info("retention configured", "tasks_days", *cfg.Retention.TasksDays,
		"audit_archive", cfg.Retention.ArchiveEnabled(), "audit_live_days", cfg.Retention.AuditLiveDaysOrZero(),
		"audit_archive_dir", cfg.AuditArchiveDir())
	go retention.Schedule(ctx, retention.Interval, func(ctx context.Context) error {
		_, _, err := engine.Submit(ctx, tasks.Request{
			Type:         retention.TaskType,
			ResourceType: retention.ResourceType,
			ResourceID:   retention.ResourceID,
			Actor:        models.TaskActor{ID: audit.System, Name: audit.System},
			PeerUID:      uint32(os.Geteuid()),
		})
		return err
	}, log)

	// A build jail outlives a crashed agent (it is persistent); remove any
	// before the socket is served (the step 6 decision, item 2).
	if err := templateSvc.SweepBuildJails(ctx); err != nil {
		log.Error("could not remove every leftover template build jail", "error", err)
	}

	// A jail outlives the agent (it is persistent): look at every one
	// before anything can ask about it.
	if err := containerSvc.ObserveAll(ctx); err != nil {
		log.Warn("could not observe the jails", "error", err)
	}

	// Look at every VM before anything can ask about one: a restarted
	// agent knows what is running before it serves its socket.
	if err := vms.ObserveAll(ctx); err != nil {
		log.Warn("could not observe the virtual machines", "error", err)
	}

	// After both observations, which reset the starting and stopping a
	// crash left: report every epair and tap whose owner is not running.
	_ = orphanChecker.Report(ctx)
	// Every running guest's port, before anything can ask about one: an
	// agent restarted, or upgraded from a Skep without filters, installs
	// what is missing (decision 4).
	if err := portChecker.Check(ctx); err != nil {
		log.Error("could not check the guest port filters", "error", err)
	}
	go portChecker.Run(ctx, portcheck.Interval)
	go vms.Watch(ctx, cfg.Hypervisor.Bhyve.WatchInterval.Std())
	go sampler.Run(ctx, monitoring.DefaultInterval)
	if caps := vms.Capabilities(ctx); caps.Available {
		log.Info("bhyve is available", "reason", caps.Reason, "max_vcpus_per_vm", caps.MaxVCPUsPerVM,
			"guest_memory_bytes", caps.GuestMemoryBytes)
	} else {
		log.Warn("bhyve is not available: no VM can be started", "reason", caps.Reason)
	}

	// The internal API on the Unix socket is the agent's only listener:
	// skep-api reaches the agent through it, and nothing else does.
	socket, err := listenSocket(cfg, log)
	if err != nil {
		return err
	}
	internal := internalapi.NewHandler(svc, internalapi.Options{
		VMs:            vms,
		Console:        consoles,
		Storage:        storageSvc,
		Templates:      templateSvc,
		Containers:     containerSvc,
		Metrics:        sampler,
		RequestTimeout: cfg.Socket.RequestTimeout.Std(),
		Logger:         log,
	})
	serveErr := serve(ctx, cfg, socket, internal, log)

	// Stop the engine after the listener: running tasks are told to stop
	// and are recorded as interrupted.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout.Std())
	defer cancel()
	if err := engine.Shutdown(shutdownCtx); err != nil {
		log.Warn("task engine did not stop cleanly; unfinished tasks will be marked interrupted at the next start", "error", err)
	}
	if serveErr == nil {
		log.Info("stopped")
	}
	return serveErr
}

// confinement builds what Skep may touch on this host from the agent's
// configuration, and from nothing else.
func confinement(cfg config.Config) hostops.Confinement {
	dirs := make([]hostops.DirPool, 0, len(cfg.Storage.Directories))
	for _, d := range cfg.Storage.Directories {
		dirs = append(dirs, hostops.DirPool{Name: d.Name, Path: d.Path})
	}
	return hostops.Confinement{
		Pools:                cfg.Storage.Pools,
		Directories:          dirs,
		DatasetRoot:          cfg.Storage.DatasetRoot,
		MediaDir:             cfg.Storage.MediaDir,
		Bridges:              cfg.Network.Bridges,
		ManagementInterfaces: cfg.Network.ManagementInterfaces,
		FirmwareDir:          cfg.Hypervisor.Bhyve.FirmwareDir,
		RunDir:               cfg.VMRunDir(),
		StateDir:             cfg.VMStateDir(),
		JailRunDir:           cfg.JailRunDir(),
		DistDir:              cfg.Storage.DistDir,
	}
}

// quietLogger is the logger of local commands: they report on stdout and
// stderr themselves, and only problems are logged.
func quietLogger(stderr io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// syslogTag is the tag of the agent's own syslog lines.
const syslogTag = "skep-agent"

// newArchive returns the agent's audit archive, or nil when archiving is
// turned off: then nothing is sealed and no audit entry is ever removed.
func newArchive(db *sql.DB, cfg config.Config, nodeID string, log *slog.Logger) (*auditarchive.Archiver, error) {
	if !cfg.Retention.ArchiveEnabled() {
		return nil, nil
	}
	opts := auditarchive.Options{
		DB:     db,
		Log:    auditarchive.AgentLog,
		Dir:    cfg.AuditArchiveDir(),
		NodeID: func(context.Context) (string, error) { return nodeID, nil },
		Logger: log,
	}
	if !cfg.DevMode {
		// Each seal also goes to syslog at notice: on a host that forwards
		// syslog that is an off-host anchor for the hash chain.
		opts.Notify = auditarchive.SyslogNotifier(syslogTag, auditarchive.AgentLog.Name, log)
	}
	return auditarchive.New(opts)
}

// listenSocket creates the agent socket with the configured group and
// peer allow-list.
func listenSocket(cfg config.Config, log *slog.Logger) (*internalapi.Listener, error) {
	uids, err := sysaccount.UIDs(cfg.Socket.AllowedUsers)
	if err != nil {
		return nil, fmt.Errorf("socket.allowed_users: %w", err)
	}
	gid, mode := -1, fs.FileMode(0o600)
	if cfg.Socket.Group != "" {
		g, err := sysaccount.GID(cfg.Socket.Group)
		if err != nil {
			return nil, fmt.Errorf("socket.group: %w", err)
		}
		gid, mode = int(g), 0o660
	}
	if cfg.DevMode {
		// In production the rc.d script creates the run directory as
		// root:skep 0750. In dev mode it lives in the data directory.
		if err := os.MkdirAll(filepath.Dir(cfg.Socket.Path), 0o700); err != nil {
			return nil, fmt.Errorf("create the socket directory: %w", err)
		}
	}
	ln, err := internalapi.Listen(internalapi.ListenOptions{
		Path:           cfg.Socket.Path,
		Mode:           mode,
		GID:            gid,
		AllowedUIDs:    uids,
		MaxConnections: cfg.Socket.MaxConnections,
		Logger:         log,
	})
	if err != nil {
		return nil, err
	}
	log.Info("internal API listening", "socket", cfg.Socket.Path, "base_path", agentproto.Prefix,
		"mode", fmt.Sprintf("%04o", mode), "group", cfg.Socket.Group, "allowed_uids", uids)
	return ln, nil
}

// serve runs the internal API on the agent socket until ctx is cancelled
// or the server fails, then shuts it down gracefully.
func serve(ctx context.Context, cfg config.Config, socket *internalapi.Listener, handler http.Handler, log *slog.Logger) error {
	defer socket.Close()
	srv := internalapi.NewServer(handler, cfg.Socket.RequestTimeout.Std(), log)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(socket) }()

	select {
	case err := <-errc:
		return fmt.Errorf("internal API server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout.Std())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("internal API server shutdown: %w", err)
	}
	return nil
}
