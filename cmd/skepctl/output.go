package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Text output is for people; --json is for scripts. Times are shown in
// UTC, as the API returns them.

const timeFormat = "2006-01-02 15:04:05Z"

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(timeFormat)
}

func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return fmtTime(*t)
}

// fmtBytes shows a byte count in binary units.
func fmtBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// clean keeps a value from the server on one table cell: control
// characters, which could also move the terminal's cursor, become spaces.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
}

func capability(c models.Capability) string {
	if c.Reason == "" {
		return string(c.Status)
	}
	return string(c.Status) + ": " + clean(c.Reason)
}

func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func printSystem(w io.Writer, s models.System) error {
	t := newTable(w)
	platform := s.Platform
	switch {
	case s.ProductionHost:
		platform += " (production host)"
	case s.DevMode:
		platform += " (development mode)"
	}
	fmt.Fprintf(t, "Node ID:\t%s\n", clean(s.NodeID))
	fmt.Fprintf(t, "Hostname:\t%s\n", clean(s.Hostname))
	fmt.Fprintf(t, "Platform:\t%s\n", clean(platform))
	fmt.Fprintf(t, "OS:\t%s %s (%s)\n", clean(s.OS.Name), clean(dash(s.OS.Release)), clean(s.Architecture))
	fmt.Fprintf(t, "Skep:\t%s (commit %s)\n", clean(s.Skep.Version), clean(s.Skep.Commit))
	fmt.Fprintf(t, "Agent started:\t%s (up %s)\n", fmtTime(s.StartedAt), time.Duration(s.UptimeSeconds)*time.Second)
	fmt.Fprintf(t, "bhyve:\t%s\n", capability(s.Capabilities.Bhyve))
	fmt.Fprintf(t, "QEMU:\t%s\n", capability(s.Capabilities.QEMU))
	fmt.Fprintf(t, "ZFS:\t%s\n", capability(s.Capabilities.ZFS))
	if s.API != nil {
		fmt.Fprintf(t, "TLS certificate:\t%s, valid until %s\n", clean(s.API.TLS.Source), fmtTime(s.API.TLS.NotAfter))
		fmt.Fprintf(t, "TLS public key:\t%s\n", clean(s.API.TLS.PublicKeySHA256))
	}
	return t.Flush()
}

func printHardware(w io.Writer, h models.Hardware) error {
	t := newTable(w)
	fmt.Fprintf(t, "Node ID:\t%s\n", clean(h.NodeID))
	fmt.Fprintf(t, "Hostname:\t%s\n", clean(h.Host.Hostname))
	fmt.Fprintf(t, "Platform:\t%s\n", clean(h.Platform))
	fmt.Fprintf(t, "OS:\t%s %s (%s)\n", clean(h.Host.OS.Name), clean(dash(h.Host.OS.Release)), clean(h.Host.Architecture))
	fmt.Fprintf(t, "CPU:\t%s, %d cores, %d threads\n", clean(dash(h.CPU.Model)), h.CPU.Cores, h.CPU.Threads)
	fmt.Fprintf(t, "Memory:\t%s total, %s available\n", fmtBytes(h.Memory.TotalBytes), fmtBytes(h.Memory.AvailableBytes))
	b := h.Hypervisors.Bhyve
	fmt.Fprintf(t, "bhyve:\t%s\n", capability(b.Capability))
	fmt.Fprintf(t, "  vmm:\tloaded %t, loadable %t\n", b.VMMLoaded, b.VMMLoadable)
	q := h.Hypervisors.QEMU
	fmt.Fprintf(t, "QEMU:\t%s\n", capability(q.Capability))
	for _, bin := range q.Binaries {
		v := bin.Version
		if v == "" {
			v = "version unknown: " + bin.Error
		}
		fmt.Fprintf(t, "  %s:\t%s (%s)\n", clean(bin.Name), clean(bin.Path), clean(v))
	}
	z := h.Storage.ZFS
	fmt.Fprintf(t, "ZFS:\t%s\n", capability(z.Capability))
	if z.PoolsError != "" {
		fmt.Fprintf(t, "  pools:\tnot read: %s\n", clean(z.PoolsError))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	if len(z.Pools) > 0 {
		fmt.Fprintln(w)
		t = newTable(w)
		fmt.Fprintln(t, "POOL\tHEALTH\tSIZE\tALLOCATED\tFREE\tCAPACITY")
		for _, p := range z.Pools {
			capacity := "-"
			if p.CapacityPercent != nil {
				capacity = fmt.Sprintf("%d%%", *p.CapacityPercent)
			}
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", clean(p.Name), clean(p.Health),
				fmtBytes(p.SizeBytes), fmtBytes(p.AllocatedBytes), fmtBytes(p.FreeBytes), capacity)
		}
		if err := t.Flush(); err != nil {
			return err
		}
	}
	if len(h.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, warn := range h.Warnings {
			fmt.Fprintf(w, "  %s\n", clean(warn))
		}
	}
	fmt.Fprintf(w, "\nCollected at %s\n", fmtTime(h.CollectedAt))
	return nil
}

func printTasks(w io.Writer, l models.TaskList) error {
	if len(l.Items) == 0 {
		fmt.Fprintln(w, "No tasks.")
		return nil
	}
	t := newTable(w)
	fmt.Fprintln(t, "ID\tTYPE\tSTATUS\tACTOR\tCREATED\tFINISHED")
	for _, task := range l.Items {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", clean(task.ID), clean(task.Type), clean(string(task.Status)),
			clean(dash(task.Actor.Name)), fmtTime(task.CreatedAt), fmtTimePtr(task.FinishedAt))
	}
	return t.Flush()
}

func printTask(w io.Writer, d models.TaskDetail) error {
	t := newTable(w)
	fmt.Fprintf(t, "ID:\t%s\n", clean(d.ID))
	fmt.Fprintf(t, "Type:\t%s\n", clean(d.Type))
	fmt.Fprintf(t, "Status:\t%s\n", clean(string(d.Status)))
	fmt.Fprintf(t, "Node:\t%s\n", clean(d.NodeID))
	if d.ResourceType != "" {
		fmt.Fprintf(t, "Resource:\t%s %s\n", clean(d.ResourceType), clean(d.ResourceID))
	}
	fmt.Fprintf(t, "Actor:\t%s\n", clean(dash(d.Actor.Name)))
	fmt.Fprintf(t, "Request ID:\t%s\n", clean(dash(d.RequestID)))
	fmt.Fprintf(t, "Cancellable:\t%t\n", d.Cancellable)
	if d.CancelRequestedAt != nil {
		fmt.Fprintf(t, "Cancel requested:\t%s\n", fmtTimePtr(d.CancelRequestedAt))
	}
	fmt.Fprintf(t, "Created:\t%s\n", fmtTime(d.CreatedAt))
	fmt.Fprintf(t, "Started:\t%s\n", fmtTimePtr(d.StartedAt))
	fmt.Fprintf(t, "Finished:\t%s\n", fmtTimePtr(d.FinishedAt))
	if d.Error != nil {
		fmt.Fprintf(t, "Error:\t%s: %s\n", clean(d.Error.Code), clean(d.Error.Message))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	if len(d.Events) > 0 {
		fmt.Fprintln(w, "\nEvents:")
		t = newTable(w)
		for _, ev := range d.Events {
			fmt.Fprintf(t, "  %s\t%s\t%s\t%s\n", fmtTime(ev.CreatedAt), clean(ev.Kind), clean(ev.Level), clean(ev.Message))
		}
		return t.Flush()
	}
	return nil
}

// fmtDetails shows audit details as sorted key=value pairs.
func fmtDetails(d map[string]any) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, d[k]))
	}
	return clean(strings.Join(parts, " "))
}

func printAudit(w io.Writer, l models.AuditList) error {
	if len(l.Items) == 0 {
		fmt.Fprintln(w, "No entries.")
		return nil
	}
	t := newTable(w)
	fmt.Fprintln(t, "ID\tTIME\tACTOR\tACTION\tOUTCOME\tSOURCE\tDETAILS")
	for _, a := range l.Items {
		fmt.Fprintf(t, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, fmtTime(a.OccurredAt), clean(dash(a.ActorName)),
			clean(a.Action), clean(a.Outcome), clean(dash(a.SourceIP)), fmtDetails(a.Details))
	}
	return t.Flush()
}

// printNextPage tells how to get the next page, on stderr, so that the
// table on stdout stays clean for other tools.
func printNextPage(w io.Writer, command, cursor string) {
	if cursor != "" {
		fmt.Fprintf(w, "More entries: skepctl %s --cursor %s\n", command, clean(cursor))
	}
}
