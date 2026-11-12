// Command skepctl is the command-line client for Skep. It uses only the
// public REST API, exactly as the web UI does, through pkg/client.
//
// It logs in with a bearer session whose token it keeps in a 0600 file,
// and it trusts a server by pinning the SHA-256 of the server's public key
// on first use, after the user has confirmed it (ADR 0006 decisions 3 and
// 6). There is no way to skip that check.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/greenbeanorg/skep/internal/ctlstate"
	"github.com/greenbeanorg/skep/internal/termpass"
	"github.com/greenbeanorg/skep/pkg/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stateDir, err := ctlstate.DefaultDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "skepctl:", err)
		os.Exit(1)
	}
	err = run(ctx, os.Args[1:], env{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		stateDir:     stateDir,
		readPassword: termpass.Read,
		confirm:      confirmOnTerminal,
		now:          time.Now,
	})
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "skepctl:", err)
		os.Exit(1)
	}
}

// env is what the program takes from its surroundings. Tests replace it.
type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	// stateDir holds the pins and the session tokens.
	stateDir string
	// readPassword asks for a password on the terminal, without echo.
	readPassword func(ctx context.Context, prompt string) (string, error)
	// confirm asks a yes/no question on the terminal.
	confirm func(ctx context.Context, prompt string) (bool, error)
	now     func() time.Time
}

const usage = `usage: skepctl <command> [flags]

Session:
  login     --server HOST[:PORT] --username NAME [--password-stdin] [--trust-key PIN]
            log in; on first contact, confirm the server's public key
  logout    [--server HOST[:PORT]]
            end the session on the server and forget its token

Read-only:
  system    [--json]                         the node's system document
  hardware  [--node ID] [--json]             hardware discovery (default: this node)
  tasks list [--status S] [--limit N] [--cursor C] [--json]
  tasks show ID [--json]                     one task with its events
  audit list [--limit N] [--cursor C] [--json]
                                             the access audit log, newest first
  version                                    print skepctl's version

Every command takes --server; without it, the server of the last login is used.
skepctl keeps its state in $SKEPCTL_CONFIG_DIR or ~/.config/skepctl.`

// run is the whole program; main only maps its error to an exit code.
func run(ctx context.Context, args []string, e env) error {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, usage)
		return errors.New("missing command")
	}
	switch args[0] {
	case "login":
		return cmdLogin(ctx, args[1:], e)
	case "logout":
		return cmdLogout(ctx, args[1:], e)
	case "system":
		return cmdSystem(ctx, args[1:], e)
	case "hardware":
		return cmdHardware(ctx, args[1:], e)
	case "tasks":
		return cmdTasks(ctx, args[1:], e)
	case "audit":
		return cmdAudit(ctx, args[1:], e)
	case "version", "-version", "--version":
		fmt.Fprintln(e.stdout, "skepctl", version.Get())
		return nil
	case "help", "-h", "-help", "--help":
		fmt.Fprintln(e.stdout, usage)
		return nil
	default:
		fmt.Fprintln(e.stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// userAgent identifies skepctl to the server.
func userAgent() string { return "skepctl/" + version.Version }

// parseInterspersed parses flags that may come before or after the
// positional arguments, and returns the positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}
