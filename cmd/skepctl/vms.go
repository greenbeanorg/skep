package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/pkg/client"
	"github.com/greenbeanorg/skep/pkg/models"
)

const vmUsage = `usage:
  skepctl vm list [--json]
  skepctl vm show VM [--json]
  skepctl vm create -f FILE [--wait] [--json]      FILE is a JSON VM definition, - for stdin
  skepctl vm start VM [--wait] [--json]
  skepctl vm stop VM [--timeout SECONDS] [--wait] [--json]
  skepctl vm force-stop VM [--wait] [--json]
  skepctl vm restart VM [--timeout SECONDS] [--wait] [--json]
  skepctl vm delete VM [--wait] [--json]
  skepctl vm clone VM NAME [--snapshot SNAPSHOT] [--wait] [--json]
  skepctl vm snapshot list|create|rollback|delete ...   see "skepctl vm snapshot"

VM is a VM's ID or its name. --wait follows the task until it ends and fails
when the task fails. Every command takes --server.`

// uuidRE is the shape of a resource ID.
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// maxDefinitionBytes bounds a VM definition file.
const maxDefinitionBytes = 1 << 20

// taskPoll is how often --wait reads the task.
var taskPoll = 500 * time.Millisecond

func cmdVM(ctx context.Context, args []string, e env) error {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, vmUsage)
		return errors.New("missing vm command")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "snapshot":
		return cmdSnapshot(ctx, rest, e)
	case "clone":
		return cmdClone(ctx, rest, e)
	case "list":
		f := newReadFlags("skepctl vm list", e)
		if _, err := f.parse(rest, 0); err != nil {
			return err
		}
		s, err := connect(e, *f.server)
		if err != nil {
			return err
		}
		defer s.client.Close()
		list, err := allVMs(ctx, s.client)
		if err != nil {
			return s.explain(err)
		}
		return output(e, *f.json, list, func(w io.Writer) error { return printVMs(w, list) })
	case "show":
		f := newReadFlags("skepctl vm show", e)
		pos, err := f.parse(rest, 1)
		if err != nil {
			return err
		}
		s, err := connect(e, *f.server)
		if err != nil {
			return err
		}
		defer s.client.Close()
		vm, err := resolveVM(ctx, s.client, pos[0])
		if err != nil {
			return s.explain(err)
		}
		return output(e, *f.json, vm, func(w io.Writer) error { return printVM(w, vm) })
	case "create":
		f := newReadFlags("skepctl vm create", e)
		file := f.set.String("f", "", "the VM definition: a JSON file, or - for standard input")
		wait := f.set.Bool("wait", false, "wait until the VM is provisioned")
		if _, err := f.parse(rest, 0); err != nil {
			return err
		}
		if *file == "" {
			return errors.New("-f FILE is required")
		}
		req, err := readDefinition(e, *file)
		if err != nil {
			return err
		}
		s, err := connect(e, *f.server)
		if err != nil {
			return err
		}
		defer s.client.Close()
		created, err := s.client.CreateVM(ctx, req)
		if err != nil {
			return s.explain(err)
		}
		task := created.Task
		if *wait {
			if task, err = waitTask(ctx, s, task.ID); err != nil {
				return err
			}
		}
		result := struct {
			VM   models.VM   `json:"vm"`
			Task models.Task `json:"task"`
		}{created.VM, task}
		if err := output(e, *f.json, result, func(w io.Writer) error {
			fmt.Fprintf(w, "VM %s (%s) created; task %s is %s\n", clean(created.VM.Name), created.VM.ID, task.ID, task.Status)
			return nil
		}); err != nil {
			return err
		}
		return taskFailure(task)
	case "start", "stop", "force-stop", "restart", "delete":
		f := newReadFlags("skepctl vm "+sub, e)
		wait := f.set.Bool("wait", false, "wait until the task ends")
		var timeout *int
		if sub == "stop" || sub == "restart" {
			timeout = f.set.Int("timeout", 0, "seconds to wait for the guest to shut down (default 90, at most 300)")
		}
		pos, err := f.parse(rest, 1)
		if err != nil {
			return err
		}
		s, err := connect(e, *f.server)
		if err != nil {
			return err
		}
		defer s.client.Close()
		vm, err := resolveVM(ctx, s.client, pos[0])
		if err != nil {
			return s.explain(err)
		}
		var task models.Task
		if sub == "delete" {
			task, err = s.client.DeleteVM(ctx, vm.ID)
		} else {
			act := models.VMAction{Action: strings.ReplaceAll(sub, "-", "_")}
			if timeout != nil && *timeout != 0 {
				act.Timeout = timeout
			}
			task, err = s.client.VMAction(ctx, vm.ID, act)
		}
		if err != nil {
			return s.explain(err)
		}
		if *wait {
			if task, err = waitTask(ctx, s, task.ID); err != nil {
				return err
			}
		}
		if err := output(e, *f.json, task, func(w io.Writer) error {
			fmt.Fprintf(w, "%s %s: task %s is %s\n", task.Type, clean(vm.Name), task.ID, task.Status)
			return nil
		}); err != nil {
			return err
		}
		return taskFailure(task)
	default:
		fmt.Fprintln(e.stderr, vmUsage)
		return fmt.Errorf("unknown vm command %q", sub)
	}
}

// readDefinition reads a VM definition, refusing unknown fields so that a
// typo is an error and not a silently ignored setting.
func readDefinition(e env, file string) (models.VMCreate, error) {
	var r io.Reader = e.stdin
	if file != "-" {
		fh, err := os.Open(file)
		if err != nil {
			return models.VMCreate{}, err
		}
		defer fh.Close()
		r = fh
	}
	dec := json.NewDecoder(io.LimitReader(r, maxDefinitionBytes))
	dec.DisallowUnknownFields()
	var req models.VMCreate
	if err := dec.Decode(&req); err != nil {
		return models.VMCreate{}, fmt.Errorf("the VM definition is not valid: %w", err)
	}
	return req, nil
}

// allVMs reads every page of the VM list.
func allVMs(ctx context.Context, c *client.Client) (models.VMList, error) {
	var all models.VMList
	cursor := ""
	for {
		page, err := c.VMs(ctx, models.VMListOptions{Limit: 200, Cursor: cursor})
		if err != nil {
			return models.VMList{}, err
		}
		all.Items = append(all.Items, page.Items...)
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// resolveVM finds a VM by ID or by name.
func resolveVM(ctx context.Context, c *client.Client, ref string) (models.VM, error) {
	if uuidRE.MatchString(ref) {
		return c.VM(ctx, ref)
	}
	list, err := allVMs(ctx, c)
	if err != nil {
		return models.VM{}, err
	}
	for _, vm := range list.Items {
		if vm.Name == ref {
			return vm, nil
		}
	}
	return models.VM{}, fmt.Errorf("no VM is named %q", ref)
}

// waitTask polls a task until it ends.
func waitTask(ctx context.Context, s *session, id string) (models.Task, error) {
	for {
		d, err := s.client.Task(ctx, id)
		if err != nil {
			return models.Task{}, s.explain(err)
		}
		if d.Status.Terminal() {
			return d.Task, nil
		}
		select {
		case <-ctx.Done():
			return models.Task{}, ctx.Err()
		case <-time.After(taskPoll):
		}
	}
}

// taskFailure is the error of a task that ended without success.
func taskFailure(t models.Task) error {
	if !t.Status.Terminal() || t.Status == models.TaskSucceeded {
		return nil
	}
	if t.Error != nil {
		msg := fmt.Sprintf("task %s %s: %s: %s", t.ID, t.Status, t.Error.Code, clean(t.Error.Message))
		for _, kv := range detailLines(t.Error.Details) {
			msg += "\n  " + kv[0] + ": " + kv[1]
		}
		return errors.New(msg)
	}
	return fmt.Errorf("task %s %s", t.ID, t.Status)
}

func printVMs(w io.Writer, l models.VMList) error {
	t := newTable(w)
	fmt.Fprintln(t, "NAME\tSTATE\tDESIRED\tCPUS\tMEMORY\tID")
	for _, vm := range l.Items {
		state := vm.Observed.State
		if !vm.Provisioned {
			state += " (provisioning)"
		}
		if vm.Drift {
			state += " (drift)"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%d\t%s\t%s\n", clean(vm.Name), state, vm.DesiredState, vm.CPUCount,
			fmtBytes(uint64(vm.MemoryBytes)), vm.ID)
	}
	return t.Flush()
}

func printVM(w io.Writer, vm models.VM) error {
	t := newTable(w)
	fmt.Fprintf(t, "Name:\t%s\n", clean(vm.Name))
	fmt.Fprintf(t, "ID:\t%s\n", vm.ID)
	fmt.Fprintf(t, "Engine:\t%s, %s firmware\n", vm.Engine, vm.Firmware)
	fmt.Fprintf(t, "CPUs:\t%d\n", vm.CPUCount)
	fmt.Fprintf(t, "Memory:\t%s\n", fmtBytes(uint64(vm.MemoryBytes)))
	fmt.Fprintf(t, "Provisioned:\t%v\n", vm.Provisioned)
	fmt.Fprintf(t, "Desired state:\t%s\n", vm.DesiredState)
	fmt.Fprintf(t, "Observed state:\t%s\n", vm.Observed.State)
	if vm.Observed.Detail != "" {
		fmt.Fprintf(t, "Detail:\t%s\n", clean(vm.Observed.Detail))
	}
	if vm.Observed.PID != nil {
		fmt.Fprintf(t, "bhyve PID:\t%d\n", *vm.Observed.PID)
	}
	fmt.Fprintf(t, "Created:\t%s\n", fmtTime(vm.CreatedAt))
	if err := t.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nDisks:")
	t = newTable(w)
	fmt.Fprintln(t, "  SLOT\tBUS\tSIZE\tBACKING")
	for _, d := range vm.Disks {
		backing, size := d.Dataset, fmtBytes(uint64(d.SizeBytes))
		if d.Bus == models.BusAHCICD {
			backing, size = "media "+clean(d.Media), "-"
		} else if d.Thin {
			size += " thin"
		}
		fmt.Fprintf(t, "  %d\t%s\t%s\t%s\n", d.Slot, d.Bus, size, backing)
	}
	if err := t.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nNetwork:")
	t = newTable(w)
	fmt.Fprintln(t, "  SLOT\tMODEL\tMAC\tBRIDGE\tTAP")
	for _, n := range vm.NICs {
		fmt.Fprintf(t, "  %d\t%s\t%s\t%s\t%s\n", n.Slot, n.Model, n.MACAddress, n.Bridge, dash(n.Tap))
	}
	return t.Flush()
}
