package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/pkg/client"
	"github.com/greenbeanorg/skep/pkg/models"
)

const clusterUsage = `usage:
  skepctl cluster show [--json] [--server HOST]
  skepctl cluster guests [--json] [--server HOST]
        the cluster's guest inventory: every VM and jail, its owner node, and
        the names reserved by creates and clones under way
  skepctl cluster admit REQUEST [--json] [--server HOST]
        admit a host: REQUEST is the line "skep-cluster join-request" printed on it
  skepctl cluster withdraw NAME [--json] [--server HOST]
        withdraw the pending admission of a host
  skepctl cluster remove NAME [--cluster-id ID] [--json] [--server HOST]
        remove a member; without --cluster-id, shows the member and asks for
        the cluster ID on the terminal`

// cmdCluster is "skepctl cluster" (step 8c-2, D28): the public API only.
func cmdCluster(ctx context.Context, args []string, e env) error {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, clusterUsage)
		return errors.New("missing cluster command")
	}
	switch args[0] {
	case "show":
		return clusterShow(ctx, args[1:], e)
	case "guests":
		return clusterGuests(ctx, args[1:], e)
	case "admit":
		return clusterAdmit(ctx, args[1:], e)
	case "withdraw":
		return clusterWithdraw(ctx, args[1:], e)
	case "remove":
		return clusterRemove(ctx, args[1:], e)
	case "help", "-h", "-help", "--help":
		fmt.Fprintln(e.stdout, clusterUsage)
		return nil
	}
	fmt.Fprintln(e.stderr, clusterUsage)
	return fmt.Errorf("unknown cluster command %q", args[0])
}

func clusterShow(ctx context.Context, args []string, e env) error {
	f := newReadFlags("skepctl cluster show", e)
	if _, err := f.parse(args, 0); err != nil {
		return err
	}
	s, err := connect(e, *f.server)
	if err != nil {
		return err
	}
	defer s.client.Close()
	c, err := s.client.Cluster(ctx)
	if err != nil {
		return s.explain(err)
	}
	return output(e, *f.json, c, func(w io.Writer) error { printCluster(w, c); return nil })
}

// clusterGuests is "skepctl cluster guests" (step 8d-1, D35).
func clusterGuests(ctx context.Context, args []string, e env) error {
	f := newReadFlags("skepctl cluster guests", e)
	if _, err := f.parse(args, 0); err != nil {
		return err
	}
	s, err := connect(e, *f.server)
	if err != nil {
		return err
	}
	defer s.client.Close()
	l, err := s.client.ClusterGuests(ctx)
	if err != nil {
		return s.explain(err)
	}
	return output(e, *f.json, l, func(w io.Writer) error { printGuests(w, l); return nil })
}

// printGuests prints the inventory; every value from the server is
// cleaned.
func printGuests(w io.Writer, l models.ClusterGuestList) {
	fmt.Fprintf(w, "Cluster %s, guest inventory at index %d (%d entries)\n", clean(l.ClusterID), l.GuestsIndex, len(l.Items))
	if len(l.Items) == 0 {
		return
	}
	fmt.Fprintf(w, "%-9s %-24s %-36s %-8s %-24s %-36s %s\n", "KIND", "NAME", "ID", "STATE", "OWNER", "TASK", "SINCE")
	for _, g := range l.Items {
		id, task, state := g.ID, g.TaskID, g.State
		if id == "" {
			id = "-"
		}
		if task == "" {
			task = "-"
		}
		if g.Flagged {
			state += " FLAGGED"
		}
		fmt.Fprintf(w, "%-9s %-24s %-36s %-8s %-24s %-36s %s\n", clean(g.Kind), clean(g.Name), clean(id), clean(state),
			clean(g.Owner.NodeName), clean(task), g.Time.UTC().Format(time.RFC3339))
	}
	for _, g := range l.Items {
		if g.Flagged {
			fmt.Fprintln(w, "FLAGGED: another guest of the same kind and name was added first; rename or delete one of them.")
			break
		}
	}
}

// printCluster prints the view; every value from the server is cleaned.
func printCluster(w io.Writer, c models.Cluster) {
	if c.Removed != nil {
		fmt.Fprintf(w, "This node (%s, %s) was REMOVED from cluster %s (%s, %s).\n", clean(c.NodeName), clean(c.NodeID),
			clean(c.Removed.ClusterID), clean(c.Removed.How), clean(c.Removed.At))
		return
	}
	leader := "no leader known"
	if c.LeaderKnown {
		leader = "leader known"
	}
	fmt.Fprintf(w, "Cluster %s, seen from %s (%s): %s, health %s\n", clean(c.ClusterID), clean(c.NodeName), clean(c.NodeID), leader, clean(c.Health.Status))
	if c.StateMachineStoppedAt > 0 {
		fmt.Fprintf(w, "The state machine is STOPPED at log index %d.\n", c.StateMachineStoppedAt)
	}
	if j := c.Joining; j != nil {
		fmt.Fprintf(w, "Joining cluster %s: %s %s\n", clean(j.ClusterID), clean(j.Phase), clean(j.Message))
	}
	fmt.Fprintf(w, "Members (%d):\n", len(c.Members))
	for _, m := range c.Members {
		role := "non-voter"
		if m.Voter {
			role = "voter"
		}
		addr := m.Address
		if addr == "" {
			addr = "-"
		}
		fmt.Fprintf(w, "  %s  %s  %s, %s  %s  %s\n", clean(m.Name), clean(m.ID), clean(m.State), role, clean(addr), clean(m.Pin))
		if m.NextPin != "" {
			fmt.Fprintf(w, "      next pin %s (rotating)\n", clean(m.NextPin))
		}
	}
	for _, p := range c.Pendings {
		fmt.Fprintf(w, "Pending admission: %s  %s  %s  %s  until %s\n", clean(p.Name), clean(p.NodeID), clean(p.Address), clean(p.Pin), p.ExpiresAt.Format(time.RFC3339))
	}
	for _, r := range c.Removals {
		fmt.Fprintf(w, "Removal: %s\n", clean(r))
	}
	for _, r := range c.Rotations {
		fmt.Fprintf(w, "Rotation: %s\n", clean(r))
	}
	if len(c.UnappliedRemoval) > 0 {
		fmt.Fprintf(w, "Not yet applied the last removal: %s\n", clean(strings.Join(c.UnappliedRemoval, ", ")))
	}
	for _, k := range c.Clocks {
		if !k.Known {
			fmt.Fprintf(w, "Clock of %s: not known (%s)\n", clean(k.Name), clean(k.Note))
			continue
		}
		fmt.Fprintf(w, "Clock of %s: %+.3f s ±%.3f s\n", clean(k.Name), float64(k.OffsetMillis)/1000, float64(k.UncertaintyMillis)/1000)
	}
}

func clusterAdmit(ctx context.Context, args []string, e env) error {
	f := newReadFlags("skepctl cluster admit", e)
	pos, err := f.parse(args, 1)
	if err != nil {
		return err
	}
	s, err := connect(e, *f.server)
	if err != nil {
		return err
	}
	defer s.client.Close()
	a, err := s.client.AdmitHost(ctx, strings.TrimSpace(pos[0]))
	if err != nil {
		return s.explain(err)
	}
	return output(e, *f.json, a, func(w io.Writer) error {
		fmt.Fprintf(w, "Admitted %s\n  Node ID:  %s\n  Address:  %s\n  Key pin:  %s\n  Until:    %s\n",
			clean(a.Name), clean(a.NodeID), clean(a.Address), clean(a.Pin), a.ExpiresAt.Format(time.RFC3339))
		fmt.Fprintln(w, "Compare this pin with the one the host shows on its own console (skep-cluster status there) before you join.")
		fmt.Fprintln(w, "If they differ, withdraw the admission: skepctl cluster withdraw "+clean(a.Name))
		fmt.Fprintln(w, "The admission, to paste on that host:")
		fmt.Fprintln(w, "  "+clean(a.Admission))
		fmt.Fprintln(w, "Then, as root on that host:")
		fmt.Fprintln(w, "  skep-cluster join "+clean(a.Admission))
		return nil
	})
}

func clusterWithdraw(ctx context.Context, args []string, e env) error {
	f := newReadFlags("skepctl cluster withdraw", e)
	pos, err := f.parse(args, 1)
	if err != nil {
		return err
	}
	s, err := connect(e, *f.server)
	if err != nil {
		return err
	}
	defer s.client.Close()
	res, err := s.client.WithdrawAdmission(ctx, pos[0])
	if err != nil {
		return s.explain(err)
	}
	return output(e, *f.json, res, func(w io.Writer) error {
		fmt.Fprintf(w, "Withdrew the admission of %s (%s).\n", clean(res.Name), clean(res.NodeID))
		return nil
	})
}

func clusterRemove(ctx context.Context, args []string, e env) error {
	f := newReadFlags("skepctl cluster remove", e)
	clusterID := f.set.String("cluster-id", "", "the cluster's ID, typed back; without it skepctl asks on the terminal")
	pos, err := f.parse(args, 1)
	if err != nil {
		return err
	}
	s, err := connectTimeout(e, *f.server, client.RemoveTimeout)
	if err != nil {
		return err
	}
	defer s.client.Close()
	name := pos[0]
	if *clusterID == "" {
		c, err := s.client.Cluster(ctx)
		if err != nil {
			return s.explain(err)
		}
		var m *models.ClusterMember
		for i := range c.Members {
			if c.Members[i].Name == name {
				m = &c.Members[i]
			}
		}
		if m == nil {
			return fmt.Errorf("%s is not a member of cluster %s", clean(name), clean(c.ClusterID))
		}
		fmt.Fprintf(e.stdout, "Remove %s (%s, %s, %s) from cluster %s.\n", clean(m.Name), clean(m.ID), clean(m.Address), clean(m.Pin), clean(c.ClusterID))
		fmt.Fprintln(e.stdout, "Its guests keep running; it stops when it sees itself removed, and needs skep-cluster leave --wipe before it can join again.")
		typed, err := e.readLine(ctx, "Type the cluster ID to confirm: ")
		if err != nil {
			return fmt.Errorf("%w; or pass --cluster-id", err)
		}
		if strings.TrimSpace(typed) != c.ClusterID {
			return errors.New("the cluster ID typed does not match; nothing was sent")
		}
		*clusterID = c.ClusterID
	}
	res, err := s.client.RemoveMember(ctx, name, *clusterID)
	if err != nil {
		return s.explain(err)
	}
	return output(e, *f.json, res, func(w io.Writer) error {
		fmt.Fprintf(w, "Removing %s (%s) from cluster %s; the leader finishes the removal by itself (operation %s).\n",
			clean(res.Name), clean(res.NodeID), clean(res.ClusterID), clean(res.OpID))
		if res.HandedOver {
			fmt.Fprintln(w, "It was the leader: it handed the leadership over first.")
		}
		return nil
	})
}
