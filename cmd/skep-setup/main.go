// Command skep-setup configures a FreeBSD host for Skep after the package
// is installed (ADR 0012, Milestone 10).
//
//	skep-setup check   [answers]   read-only: what is there, missing, refused
//	skep-setup plan    [answers]   read-only: the exact changes apply would make
//	skep-setup apply   [answers]   make them, with backups and a journal
//	skep-setup rollback [ID]       undo one apply where files are unchanged since
//	skep-setup upgrade             after pkg upgrade: back up, restart in order
//	skep-setup remove  [--plan]    before pkg delete: stop, disable, optionally revert
//
// The answers are flags (see "skep-setup plan -h"). Nothing here runs a
// shell; every command is an argument list.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/greenbeanorg/skep/internal/setup"
	"github.com/greenbeanorg/skep/pkg/version"
)

// Exit codes.
const (
	exitOK      = 0
	exitPending = 1 // check: there is work for apply, or something for the operator
	exitUsage   = 2
	exitRefused = 3 // a refusal: nothing was (or would be) done
)

// env is what the program takes from its surroundings. Tests replace it.
type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	prober         setup.Prober
	runner         setup.Runner
	now            func() time.Time
	euid           int
	// confirm asks the operator a yes/no question.
	confirm func(prompt string) bool
}

// exitError carries an exit code.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e := env{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		prober: setup.HostProber{}, runner: setup.HostRunner{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr},
		now: time.Now, euid: os.Geteuid(),
	}
	e.confirm = func(prompt string) bool { return askYes(e.stdin, e.stdout, prompt) }
	code := run(ctx, os.Args[1:], e)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, e env) int {
	if len(args) == 0 {
		usage(e.stderr)
		return exitUsage
	}
	var err error
	switch args[0] {
	case "-version", "--version", "version":
		fmt.Fprintln(e.stdout, "skep-setup", version.Get())
		return exitOK
	case "check":
		err = runCheck(ctx, args[1:], e)
	case "plan":
		err = runPlan(ctx, args[1:], e)
	case "apply":
		err = runApply(ctx, args[1:], e)
	case "rollback":
		err = runRollback(ctx, args[1:], e)
	case "upgrade":
		err = runUpgrade(ctx, args[1:], e)
	case "remove":
		err = runRemove(ctx, args[1:], e)
	case "-h", "-help", "--help", "help":
		usage(e.stdout)
		return exitOK
	default:
		fmt.Fprintf(e.stderr, "skep-setup: unknown command %q\n", args[0])
		usage(e.stderr)
		return exitUsage
	}
	if err == nil {
		return exitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(e.stderr, "skep-setup:", ee.msg)
		}
		return ee.code
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if errors.Is(err, fs.ErrPermission) {
		fmt.Fprintln(e.stderr, "skep-setup:", err)
		fmt.Fprintln(e.stderr, "skep-setup: it reads root-only files (agent.json); run it as root")
		return exitUsage
	}
	fmt.Fprintln(e.stderr, "skep-setup:", err)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: skep-setup COMMAND [flags]

  check     read-only: what is configured, missing, in conflict, or refused
  plan      read-only: the exact changes apply would make, and what it leaves to you
  apply     make the planned changes (backups and a journal in /var/db/skep/setup)
  rollback  undo one apply, only where files are unchanged since
  upgrade   after "pkg upgrade skep": check versions, back up, restart in order
  remove    before "pkg delete skep": stop and disable the services
  version   print the version

Run "skep-setup COMMAND -h" for the flags of a command.
`)
}

// listFlag collects a repeatable or comma-separated flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// answers registers the flags that make the options of check, plan and
// apply.
type answers struct {
	root, createBridge, uplink, listen, admin string
	mgmt, bridges, pools, dirPools            listFlag
	listenAll, noRACCT, start                 bool
}

func (a *answers) register(flags *flag.FlagSet) {
	flags.StringVar(&a.root, "root", "/", "directory standing in for / (for tests)")
	flags.Var(&a.mgmt, "mgmt", "management interface(s), never changed; the first one's address is the default listen address (required)")
	flags.Var(&a.bridges, "bridge", "existing guest bridge(s) to allow-list; setup changes nothing on them")
	flags.StringVar(&a.createBridge, "create-bridge", "", "create this new guest bridge (with -uplink), vlanfilter on")
	flags.StringVar(&a.uplink, "uplink", "", "spare, unaddressed interface for the new bridge")
	flags.Var(&a.pools, "pool", "ZFS pool(s) to allow-list (must exist and be ONLINE)")
	flags.Var(&a.dirPools, "dir-pool", "directory pool(s) for VM disks, NAME=/PATH")
	flags.StringVar(&a.listen, "listen", "", "API listen address, ADDRESS:PORT (default: the management address, port 8443)")
	flags.BoolVar(&a.listenAll, "listen-all", false, "allow listening on 0.0.0.0 or ::")
	flags.BoolVar(&a.noRACCT, "no-racct", false, "do not add kern.racct.enable (jail limits other than disk_quota are then refused)")
	flags.BoolVar(&a.start, "start", false, "start both services at the end of apply")
	flags.StringVar(&a.admin, "admin", "", "create the first administrator with this username (asks for the password)")
}

func (a *answers) options() (setup.Options, error) {
	o := setup.Options{
		Root: a.root, Mgmt: a.mgmt, Bridges: a.bridges, CreateBridge: a.createBridge, Uplink: a.uplink,
		Pools: a.pools, Listen: a.listen, ListenAll: a.listenAll, NoRACCT: a.noRACCT, Start: a.start, Admin: a.admin,
	}
	for _, d := range a.dirPools {
		name, path, ok := strings.Cut(d, "=")
		if !ok {
			return o, fmt.Errorf("-dir-pool %q: use NAME=/PATH", d)
		}
		o.DirPools = append(o.DirPools, setup.DirPool{Name: name, Path: path})
	}
	return o, setup.ValidateOptions(o)
}

// computePlan parses the answers, probes the host, and computes the plan.
func computePlan(ctx context.Context, name string, args []string, e env, extra func(*flag.FlagSet)) (setup.Options, setup.Plan, error) {
	flags := flag.NewFlagSet("skep-setup "+name, flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var a answers
	a.register(flags)
	if extra != nil {
		extra(flags)
	}
	if err := flags.Parse(args); err != nil {
		return setup.Options{}, setup.Plan{}, err
	}
	if flags.NArg() > 0 {
		return setup.Options{}, setup.Plan{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	o, err := a.options()
	if err != nil {
		return o, setup.Plan{}, err
	}
	f, err := e.prober.Probe(ctx)
	if err != nil {
		return o, setup.Plan{}, err
	}
	p, err := setup.Compute(ctx, o, f)
	return o, p, err
}

// printItems prints plan items; with all false only those that are not OK.
func printItems(w io.Writer, items []setup.Item, all bool) {
	for _, it := range items {
		if !all && it.Kind == setup.OK {
			continue
		}
		fmt.Fprintf(w, "  %-8s %-34s %s\n", it.Kind, it.Area, it.What)
		if it.Action != nil && it.Kind == setup.Add {
			fmt.Fprintf(w, "  %-8s %-34s   -> %s\n", "", "", it.Action)
		}
		if it.Detail != "" {
			for _, line := range strings.Split(strings.TrimRight(it.Detail, "\n"), "\n") {
				fmt.Fprintf(w, "  %-8s %-34s   %s\n", "", "", line)
			}
		}
		for _, s := range it.Steps {
			fmt.Fprintf(w, "  %-8s %-34s   $ %s\n", "", "", s)
		}
	}
}

func count(items []setup.Item, k setup.Kind) int {
	n := 0
	for _, it := range items {
		if it.Kind == k {
			n++
		}
	}
	return n
}

func summary(items []setup.Item) string {
	return fmt.Sprintf("%d ok, %d to add, %d warnings, %d conflicts, %d for you, %d refused",
		count(items, setup.OK), count(items, setup.Add), count(items, setup.Warn),
		count(items, setup.Conflict), count(items, setup.Manual), count(items, setup.Refuse))
}

func runCheck(ctx context.Context, args []string, e env) error {
	o, p, err := computePlan(ctx, "check", args, e, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "skep-setup check, root %s (read-only; nothing was changed)\n", o.Root)
	fmt.Fprintln(e.stdout, " ", summary(p.Items))
	printItems(e.stdout, p.Items, false)
	switch {
	case p.Refused():
		return &exitError{code: exitRefused, msg: "refused: apply would change nothing until the refused items are resolved"}
	case count(p.Items, setup.Add)+count(p.Items, setup.Manual)+count(p.Items, setup.Conflict) > 0:
		return &exitError{code: exitPending}
	}
	return nil
}

func runPlan(ctx context.Context, args []string, e env) error {
	o, p, err := computePlan(ctx, "plan", args, e, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "skep-setup plan, root %s (read-only; nothing was changed)\n", o.Root)
	printItems(e.stdout, p.Items, true)
	fmt.Fprintln(e.stdout, " ", summary(p.Items))
	if p.Refused() {
		return &exitError{code: exitRefused, msg: "refused: apply would change nothing until the refused items are resolved"}
	}
	return nil
}

// askYes reads one line and accepts "yes" only.
func askYes(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprint(out, prompt+" [type yes to continue] ")
	var b [64]byte
	n, _ := in.Read(b[:])
	return strings.TrimSpace(string(b[:n])) == "yes"
}
