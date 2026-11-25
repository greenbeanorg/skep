// Command skep-cluster is the Skep cluster service (ADR 0013): it holds
// the node key, the cluster membership and the replicated store, as the
// unprivileged account "skep-cluster". It never talks to skep-agent.
//
// Without a command it runs the service. The commands are local, for the
// host's administrator:
//
//	skep-cluster init [-address ADDR]        create a one-node cluster (root)
//	skep-cluster listen -address ADDR        open the node-to-node listener (root)
//	skep-cluster join-request -address ADDR  print this host's join request (root)
//	skep-cluster admit REQUEST               admit a host, print its admission (root)
//	skep-cluster admit --withdraw NAME       withdraw a pending admission (root)
//	skep-cluster join ADMISSION              join a cluster (root)
//	skep-cluster status [-json]              show the node and its cluster
//	skep-cluster health                      show the service's health
//	skep-cluster version
//
// Every host is a one-node cluster until it joins another (ADR 0013, step
// 8b: only a host without guests joins; removal comes with the next run).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/greenbeanorg/skep/internal/privdrop"
	"github.com/greenbeanorg/skep/internal/sysaccount"
	"github.com/greenbeanorg/skep/pkg/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Args[1:], realEnv())
	var exitErr *exitCodeError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		if exitErr.msg != "" {
			fmt.Fprintln(os.Stderr, "skep-cluster:", termSafeLines(exitErr.msg))
		}
		os.Exit(exitErr.code)
	case errors.Is(err, flag.ErrHelp):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "skep-cluster:", termSafeLines(err.Error()))
		os.Exit(1)
	}
}

// exitCodeError ends the program with a given exit code.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

// env is what the program takes from its surroundings. Tests replace it.
type env struct {
	stdout, stderr io.Writer
	euid           int
	// lookup returns the UID and primary GID of an account.
	lookup func(name string) (sysaccount.Account, error)
	// drop gives up root for the given user and group, for good.
	drop func(uid, gid uint32) error
	// chown changes a path's owner and group.
	chown func(path string, uid, gid int) error
	// owner returns the UID that owns a file.
	owner func(info fs.FileInfo) (uint32, bool)
	// hostname returns the host's name.
	hostname func() (string, error)
	// localAddrs returns the addresses assigned to this host.
	localAddrs func() ([]netip.Addr, error)
	// agentNodeID reads the node ID from skep-agent on its socket.
	agentNodeID func(ctx context.Context, socket string) (string, error)
	// configOwner returns the owner and group cluster.json gets: root and
	// the service account's group. Tests return -1, -1 (leave as is).
	configOwner func(accountGID uint32) (uid, gid int)
	// restartService restarts the skep_cluster service.
	restartService func(ctx context.Context) error
	// serviceCtl runs "service skep_cluster stop" or "start".
	serviceCtl func(ctx context.Context, action string) error
	// runAsAccount runs this program with args as the given user and
	// group (no supplementary groups) and returns its stdout.
	runAsAccount func(ctx context.Context, uid, gid uint32, args []string) ([]byte, error)
	// agentGuests counts the host's VMs and jails from skep-agent.
	agentGuests func(ctx context.Context, socket string) (vms, jails int, err error)
	// openTTY opens the controlling terminal.
	openTTY func() (io.ReadWriteCloser, error)
}

func realEnv() env {
	return env{
		stdout: os.Stdout, stderr: os.Stderr,
		euid:   os.Geteuid(),
		lookup: sysaccount.Lookup,
		drop:   privdrop.To,
		chown:  os.Lchown,
		owner: func(info fs.FileInfo) (uint32, bool) {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, false
			}
			return st.Uid, true
		},
		configOwner: func(gid uint32) (int, int) { return 0, int(gid) },
		hostname:    os.Hostname,
		localAddrs:  interfaceAddrs,
		agentNodeID: agentNodeID,
		restartService: func(ctx context.Context) error {
			// A fixed program and fixed arguments; nothing from
			// the command line reaches it.
			cmd := exec.CommandContext(ctx, "/usr/sbin/service", "skep_cluster", "restart")
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			return cmd.Run()
		},
		serviceCtl: func(ctx context.Context, action string) error {
			if action != "stop" && action != "start" {
				return fmt.Errorf("unknown service action %q", action)
			}
			// A fixed program and fixed arguments.
			cmd := exec.CommandContext(ctx, "/usr/sbin/service", "skep_cluster", action)
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			return cmd.Run()
		},
		runAsAccount: runAsAccount,
		agentGuests:  agentGuests,
		openTTY: func() (io.ReadWriteCloser, error) {
			return os.OpenFile("/dev/tty", os.O_RDWR, 0)
		},
	}
}

// runAsAccount runs this same executable, with a fixed environment, as the
// service account. Its arguments come from the root-run command, which
// validated them; the admission line is passed as one argument, never
// through a shell.
func runAsAccount(ctx context.Context, uid, gid uint32, args []string) ([]byte, error) {
	if uid == 0 {
		return nil, errors.New("refusing to run the stage as root")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	cmd.Dir = "/"
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{}}}
	return cmd.Output()
}

func interfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

// errNeedsRoot is returned when a root-run command is started by anyone
// else.
func errNeedsRoot(cmd string) error {
	return fmt.Errorf("\"skep-cluster %s\" must be run as root", cmd)
}

func run(ctx context.Context, args []string, e env) error {
	if len(args) > 0 {
		switch args[0] {
		case "init":
			return runInit(ctx, args[1:], e)
		case "listen":
			return runListen(ctx, args[1:], e)
		case "join-request":
			return runJoinRequest(ctx, args[1:], e)
		case "admit":
			return runAdmit(ctx, args[1:], e)
		case "join":
			return runJoin(ctx, args[1:], e)
		case stageCommand:
			return runStageCmd(ctx, args[1:], e)
		case "status":
			return runStatus(ctx, args[1:], e)
		case "health":
			return runHealth(ctx, args[1:], e)
		case "version":
			v := version.Get()
			fmt.Fprintf(e.stdout, "skep-cluster %s (%s, built %s)\n", v.Version, v.Commit, v.BuildDate)
			return nil
		case "help", "-h", "-help", "--help":
			fmt.Fprint(e.stdout, usage)
			return nil
		}
		if len(args[0]) > 0 && args[0][0] != '-' {
			return fmt.Errorf("unknown command %q\n%s", args[0], usage)
		}
	}
	return runServe(ctx, args, e)
}

const usage = `usage:
  skep-cluster [-config FILE] [-data-dir DIR] [-socket PATH]   run the service (as the skep-cluster account)
  skep-cluster init [-address ADDR] [-name NAME]               create a one-node cluster (root)
  skep-cluster listen -address ADDR                            open the node-to-node listener on ADDR:8444 (root)
  skep-cluster join-request -address ADDR                      print this host's join request; opens ADDR:8444 (root)
  skep-cluster admit REQUEST                                   admit a host; prints its admission (root, on a member)
  skep-cluster admit -withdraw NAME                            withdraw a pending admission (root, on a member)
  skep-cluster join [-cluster-id ID] ADMISSION                 join the cluster of an admission (root)
  skep-cluster status [-json]                                  show the node and its cluster (root or skep)
  skep-cluster health                                          show the service's health (root or skep)
  skep-cluster version
`
