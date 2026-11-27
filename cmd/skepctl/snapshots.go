package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/greenbeanorg/skep/pkg/client"
	"github.com/greenbeanorg/skep/pkg/models"
)

const snapshotUsage = `usage:
  skepctl vm snapshot list VM [--json]
  skepctl vm snapshot create VM NAME [--description TEXT] [--wait] [--json]
  skepctl vm snapshot rollback VM SNAPSHOT [--destroy-newer] [--wait] [--json]
  skepctl vm snapshot delete VM SNAPSHOT [--wait] [--json]

VM is a VM's ID or name; SNAPSHOT is a snapshot's ID or name. A snapshot of
a running VM is crash-consistent. Rollback needs the VM stopped, and rolls
back to an older snapshot only with --destroy-newer, which destroys every
newer snapshot. Every command takes --server.`

// cloneNote is said wherever a clone is made (ADR 0010 decision 3).
const cloneNote = "Note: a clone is the same guest. It keeps the source's hostname, SSH host keys, and machine ID; change them inside the clone."

func cmdSnapshot(ctx context.Context, args []string, e env) error {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, snapshotUsage)
		return errors.New("missing vm snapshot command")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		f := newReadFlags("skepctl vm snapshot list", e)
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
		list, err := s.client.Snapshots(ctx, vm.ID)
		if err != nil {
			return s.explain(err)
		}
		return output(e, *f.json, list, func(w io.Writer) error { return printSnapshots(w, list) })
	case "create":
		f := newReadFlags("skepctl vm snapshot create", e)
		desc := f.set.String("description", "", "a description of the snapshot")
		wait := f.set.Bool("wait", false, "wait until the snapshot is taken")
		pos, err := f.parse(rest, 2)
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
		created, err := s.client.CreateSnapshot(ctx, vm.ID, models.SnapshotCreate{Name: pos[1], Description: *desc})
		if err != nil {
			return s.explain(err)
		}
		task := created.Task
		if *wait {
			if task, err = waitTask(ctx, s, task); err != nil {
				return err
			}
		}
		result := struct {
			Snapshot models.Snapshot `json:"snapshot"`
			Task     models.Task     `json:"task"`
		}{created.Snapshot, task}
		if err := output(e, *f.json, result, func(w io.Writer) error {
			fmt.Fprintf(w, "snapshot %s (%s) of %s, %s; task %s is %s\n", clean(created.Snapshot.Name), created.Snapshot.ID,
				clean(vm.Name), consistencyText(created.Snapshot.Consistency), task.ID, task.Status)
			return nil
		}); err != nil {
			return err
		}
		return taskFailure(task)
	case "rollback", "delete":
		f := newReadFlags("skepctl vm snapshot "+sub, e)
		wait := f.set.Bool("wait", false, "wait until the task ends")
		var destroyNewer *bool
		if sub == "rollback" {
			destroyNewer = f.set.Bool("destroy-newer", false, "roll back to an older snapshot, destroying every newer one")
		}
		pos, err := f.parse(rest, 2)
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
		snap, err := resolveSnapshot(ctx, s.client, vm, pos[1])
		if err != nil {
			return s.explain(err)
		}
		var task models.Task
		if sub == "rollback" {
			task, err = s.client.RollbackSnapshot(ctx, vm.ID, snap.ID, models.SnapshotRollback{DestroyNewer: *destroyNewer})
		} else {
			task, err = s.client.DeleteSnapshot(ctx, vm.ID, snap.ID)
		}
		if err != nil {
			return s.explain(err)
		}
		if *wait {
			if task, err = waitTask(ctx, s, task); err != nil {
				return err
			}
		}
		if err := output(e, *f.json, task, func(w io.Writer) error {
			fmt.Fprintf(w, "%s %s of %s: task %s is %s\n", task.Type, clean(snap.Name), clean(vm.Name), task.ID, task.Status)
			return nil
		}); err != nil {
			return err
		}
		return taskFailure(task)
	default:
		fmt.Fprintln(e.stderr, snapshotUsage)
		return fmt.Errorf("unknown vm snapshot command %q", sub)
	}
}

// cmdClone is "skepctl vm clone VM NAME [--snapshot SNAPSHOT]".
func cmdClone(ctx context.Context, args []string, e env) error {
	f := newReadFlags("skepctl vm clone", e)
	snapRef := f.set.String("snapshot", "", "the snapshot to clone, by ID or name (default: take one now, named after the clone)")
	desc := f.set.String("description", "", "a description of the clone")
	wait := f.set.Bool("wait", false, "wait until the clone is provisioned")
	pos, err := f.parse(args, 2)
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
	req := models.VMClone{Name: pos[1], Description: *desc}
	if *snapRef != "" {
		snap, err := resolveSnapshot(ctx, s.client, vm, *snapRef)
		if err != nil {
			return s.explain(err)
		}
		req.SnapshotID = snap.ID
	}
	created, err := s.client.CloneVM(ctx, vm.ID, req)
	if err != nil {
		return s.explain(err)
	}
	task := created.Task
	if *wait {
		if task, err = waitTask(ctx, s, task); err != nil {
			return err
		}
	}
	result := struct {
		VM   models.VM   `json:"vm"`
		Task models.Task `json:"task"`
	}{created.VM, task}
	if err := output(e, *f.json, result, func(w io.Writer) error {
		fmt.Fprintf(w, "VM %s (%s) cloned from %s; task %s is %s\n", clean(created.VM.Name), created.VM.ID, clean(vm.Name), task.ID, task.Status)
		fmt.Fprintln(w, cloneNote)
		return nil
	}); err != nil {
		return err
	}
	if *f.json {
		// Said on stderr so the JSON stays parseable.
		fmt.Fprintln(e.stderr, cloneNote)
	}
	return taskFailure(task)
}

// resolveSnapshot finds a VM's snapshot by ID or by name.
func resolveSnapshot(ctx context.Context, c *client.Client, vm models.VM, ref string) (models.Snapshot, error) {
	list, err := c.Snapshots(ctx, vm.ID)
	if err != nil {
		return models.Snapshot{}, err
	}
	for _, sn := range list.Items {
		if sn.ID == ref || sn.Name == ref {
			return sn, nil
		}
	}
	return models.Snapshot{}, fmt.Errorf("VM %s has no snapshot %q", clean(vm.Name), clean(ref))
}

func consistencyText(c string) string {
	if c == "crash_consistent" {
		return "crash-consistent (taken while running)"
	}
	return c
}

func printSnapshots(w io.Writer, l models.SnapshotList) error {
	t := newTable(w)
	fmt.Fprintln(t, "NAME\tCREATED\tCONSISTENCY\tUSED\tCLONES\tID")
	for _, sn := range l.Items {
		name := clean(sn.Name)
		if !sn.Provisioned {
			name += " (not provisioned)"
		}
		used := "-"
		if sn.Observed != nil {
			used = fmtBytes(uint64(sn.Observed.UsedBytes))
			if !sn.Observed.Exists {
				used = "missing"
			}
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%d\t%s\n", name, fmtTime(sn.CreatedAt), sn.Consistency, used, len(sn.Clones), sn.ID)
	}
	return t.Flush()
}
