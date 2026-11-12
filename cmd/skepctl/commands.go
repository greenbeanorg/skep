package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/greenbeanorg/skep/pkg/client"
	"github.com/greenbeanorg/skep/pkg/models"
)

// readFlags are the flags of every read-only command.
type readFlags struct {
	set    *flag.FlagSet
	server *string
	json   *bool
}

func newReadFlags(name string, e env) *readFlags {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(e.stderr)
	return &readFlags{
		set:    set,
		server: set.String("server", "", "address of skep-api (default: the server of the last login)"),
		json:   set.Bool("json", false, "print the API response as JSON"),
	}
}

// parse parses args and checks the number of positional arguments.
func (f *readFlags) parse(args []string, positional int) ([]string, error) {
	pos, err := parseInterspersed(f.set, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > positional {
		return nil, fmt.Errorf("unexpected argument %q", pos[positional])
	}
	if len(pos) < positional {
		return nil, errors.New("missing argument")
	}
	return pos, nil
}

// printJSON writes v as indented JSON.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// output prints v as JSON or with its text printer.
func output(e env, asJSON bool, v any, text func(io.Writer) error) error {
	if asJSON {
		return printJSON(e.stdout, v)
	}
	return text(e.stdout)
}

func cmdSystem(ctx context.Context, args []string, e env) error {
	f := newReadFlags("skepctl system", e)
	if _, err := f.parse(args, 0); err != nil {
		return err
	}
	s, err := connect(e, *f.server)
	if err != nil {
		return err
	}
	defer s.client.Close()
	sys, err := s.client.System(ctx)
	if err != nil {
		return s.explain(err)
	}
	return output(e, *f.json, sys, func(w io.Writer) error { return printSystem(w, sys) })
}

func cmdHardware(ctx context.Context, args []string, e env) error {
	f := newReadFlags("skepctl hardware", e)
	node := f.set.String("node", "", "node ID (default: the node skep-api runs on)")
	if _, err := f.parse(args, 0); err != nil {
		return err
	}
	s, err := connect(e, *f.server)
	if err != nil {
		return err
	}
	defer s.client.Close()
	nodeID := *node
	if nodeID == "" {
		sys, err := s.client.System(ctx)
		if err != nil {
			return s.explain(err)
		}
		nodeID = sys.NodeID
	}
	hw, err := s.client.Hardware(ctx, nodeID)
	if err != nil {
		return s.explain(err)
	}
	return output(e, *f.json, hw, func(w io.Writer) error { return printHardware(w, hw) })
}

const tasksUsage = `usage:
  skepctl tasks list [--status S] [--limit N] [--cursor C] [--json] [--server HOST]
  skepctl tasks show ID [--json] [--server HOST]`

func cmdTasks(ctx context.Context, args []string, e env) error {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, tasksUsage)
		return errors.New("missing tasks command")
	}
	switch args[0] {
	case "list":
		f := newReadFlags("skepctl tasks list", e)
		status := f.set.String("status", "", "only tasks in this status: pending, running, succeeded, failed, cancelled, or interrupted")
		limit := f.set.Int("limit", 0, "number of tasks per page (default: the server's)")
		cursor := f.set.String("cursor", "", "the cursor printed with the previous page")
		if _, err := f.parse(args[1:], 0); err != nil {
			return err
		}
		s, err := connect(e, *f.server)
		if err != nil {
			return err
		}
		defer s.client.Close()
		list, err := s.client.Tasks(ctx, models.TaskListOptions{Status: models.TaskStatus(*status), Limit: *limit, Cursor: *cursor})
		if err != nil {
			return s.explain(err)
		}
		return output(e, *f.json, list, func(w io.Writer) error {
			if err := printTasks(w, list); err != nil {
				return err
			}
			printNextPage(e.stderr, "tasks list", list.NextCursor)
			return nil
		})
	case "show":
		f := newReadFlags("skepctl tasks show", e)
		pos, err := f.parse(args[1:], 1)
		if err != nil {
			return err
		}
		s, err := connect(e, *f.server)
		if err != nil {
			return err
		}
		defer s.client.Close()
		task, err := s.client.Task(ctx, pos[0])
		if err != nil {
			return s.explain(err)
		}
		return output(e, *f.json, task, func(w io.Writer) error { return printTask(w, task) })
	default:
		fmt.Fprintln(e.stderr, tasksUsage)
		return fmt.Errorf("unknown tasks command %q", args[0])
	}
}

const auditUsage = `usage:
  skepctl audit list [--limit N] [--cursor C] [--json] [--server HOST]`

func cmdAudit(ctx context.Context, args []string, e env) error {
	if len(args) == 0 || args[0] != "list" {
		fmt.Fprintln(e.stderr, auditUsage)
		if len(args) == 0 {
			return errors.New("missing audit command")
		}
		return fmt.Errorf("unknown audit command %q", args[0])
	}
	f := newReadFlags("skepctl audit list", e)
	limit := f.set.Int("limit", 0, "number of entries per page (default: the server's)")
	cursor := f.set.String("cursor", "", "the cursor printed with the previous page")
	if _, err := f.parse(args[1:], 0); err != nil {
		return err
	}
	s, err := connect(e, *f.server)
	if err != nil {
		return err
	}
	defer s.client.Close()
	list, err := s.client.Audit(ctx, client.AuditOptions{Limit: *limit, Cursor: *cursor})
	if err != nil {
		return s.explain(err)
	}
	return output(e, *f.json, list, func(w io.Writer) error {
		if err := printAudit(w, list); err != nil {
			return err
		}
		printNextPage(e.stderr, "audit list", list.NextCursor)
		return nil
	})
}
