package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// skep-cluster's files in the package layout (ADR 0013 B12, step 8a).
const (
	clusterBin      = "/usr/local/sbin/skep-cluster"
	clusterPID      = "/var/run/skep/cluster.pid"
	clusterJSONPath = "/usr/local/etc/skep/cluster.json"
	clusterDataDir  = "/var/db/skep/cluster"
	// ClusterAccountID is the fixed UID and GID of the skep-cluster account.
	ClusterAccountID = "410"
	clusterAccount   = "skep-cluster"
)

// maxCommandOutput is the most setup reads of what skep-cluster or
// skep-api prints (D1 of step 8c-2).
const maxCommandOutput = 1 << 20

// Clean replaces control characters (newlines too) and Unicode format
// characters (category Cf) with "?": every string a root-run skep-setup
// prints that came from skep-cluster or from skep-api's commands goes
// through it. Such text is shown, never acted on except to refuse.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
}

// ClusterView is the part of "skep-cluster status -json" setup reads,
// decoded into setup's own struct: skep-setup never links the cluster
// packages (D1). A test keeps the tags in line with localapi.Status.
type ClusterView struct {
	NodeID        string `json:"node_id"`
	NodeName      string `json:"node_name"`
	ClusterID     string `json:"cluster_id"`
	ListenAddress string `json:"listen_address"`
	Build         string `json:"build"`
	Raft          struct {
		State    string `json:"state"`
		LeaderID string `json:"leader_id"`
	} `json:"raft"`
	StateMachine struct {
		Stalled *struct {
			Index  uint64 `json:"index"`
			Reason string `json:"reason"`
		} `json:"stalled"`
	} `json:"state_machine"`
	Members []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"members"`
	Pendings []struct {
		NodeID string `json:"node_id"`
		Name   string `json:"name"`
	} `json:"pendings"`
	Join *struct {
		Phase     string `json:"phase"`
		ClusterID string `json:"cluster_id"`
	} `json:"join"`
	Removed *struct {
		ClusterID string `json:"cluster_id"`
	} `json:"removed"`
	Rotation *struct {
		Phase string `json:"phase"`
	} `json:"rotation"`
	// Health is the service's health status, from the health document
	// printed with the status.
	Health string `json:"-"`
}

// Alone reports whether this node is a cluster of one: one member, itself;
// nothing pending; not joining; not removed.
func (v ClusterView) Alone() bool {
	return len(v.Members) == 1 && v.Members[0].ID == v.NodeID && len(v.Pendings) == 0 && v.Join == nil && v.Removed == nil
}

// LeaderKnown reports whether the status names a leader.
func (v ClusterView) LeaderKnown() bool { return v.Raft.LeaderID != "" }

// Summary is the one line check prints, cleaned.
func (v ClusterView) Summary() string {
	if v.Removed != nil {
		return Clean(fmt.Sprintf("removed from cluster %s; health %s", v.Removed.ClusterID, v.Health))
	}
	leader := "no leader known"
	if v.LeaderKnown() {
		leader = "leader known"
	}
	s := fmt.Sprintf("cluster %s, %d member(s), %s, health %s", v.ClusterID, len(v.Members), leader, v.Health)
	if v.StateMachine.Stalled != nil {
		s += fmt.Sprintf(", state machine STOPPED at %d", v.StateMachine.Stalled.Index)
	}
	if v.Join != nil {
		s += ", joining (" + v.Join.Phase + ")"
	}
	return Clean(s)
}

// Cluster is what setup asks skep-cluster, through its commands.
type Cluster interface {
	// Build is the installed skep-cluster's build (version+commit).
	Build(ctx context.Context) (string, error)
	// Initialised runs "skep-cluster init -check" (read-only).
	Initialised(ctx context.Context) (bool, error)
	// Init runs "skep-cluster init" (no -address: no port is opened).
	Init(ctx context.Context) error
	// Status runs "skep-cluster status -json" against the running service.
	Status(ctx context.Context) (ClusterView, error)
}

// HostCluster runs the installed skep-cluster as root, by absolute path,
// with setup's fixed environment, reading at most 1 MiB of its output.
type HostCluster struct {
	Root   string
	Runner Runner
}

func (h HostCluster) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := h.Runner.Run(cctx, Cmd{Name: hostPath(h.Root, clusterBin), Args: args, Limit: maxCommandOutput})
	if err != nil {
		return out, errors.New(Clean(err.Error()))
	}
	return out, nil
}

// clusterVersionRE is "skep-cluster version": skep-cluster V (C, built D).
var clusterVersionRE = regexp.MustCompile(`^skep-cluster (\S+) \((\S+), built `)

// Build implements Cluster.
func (h HostCluster) Build(ctx context.Context) (string, error) {
	out, err := h.run(ctx, commandTimeout, "version")
	if err != nil {
		return "", err
	}
	m := clusterVersionRE.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", fmt.Errorf("%s version printed %q", clusterBin, Clean(strings.TrimSpace(out)))
	}
	return m[1] + "+" + m[2], nil
}

// Initialised implements Cluster.
func (h HostCluster) Initialised(ctx context.Context) (bool, error) {
	out, err := h.run(ctx, 30*time.Second, "init", "-check")
	if err != nil {
		return false, fmt.Errorf("skep-cluster init -check: %w", err)
	}
	switch strings.TrimSpace(out) {
	case "initialised":
		return true, nil
	case "not-initialised":
		return false, nil
	}
	return false, fmt.Errorf("skep-cluster init -check printed %q", Clean(strings.TrimSpace(out)))
}

// clusterInitTimeout bounds "skep-cluster init": its agent call has its
// own 10 s limit; making the key and the store takes seconds.
const clusterInitTimeout = 2 * time.Minute

// Init implements Cluster.
func (h HostCluster) Init(ctx context.Context) error {
	if _, err := h.run(ctx, clusterInitTimeout, "init"); err != nil {
		return fmt.Errorf("skep-cluster init: %w", err)
	}
	return nil
}

// Status implements Cluster.
func (h HostCluster) Status(ctx context.Context) (ClusterView, error) {
	out, err := h.run(ctx, 30*time.Second, "status", "-json")
	if err != nil {
		return ClusterView{}, fmt.Errorf("skep-cluster status: %w", err)
	}
	return parseClusterStatus(out)
}

// parseClusterStatus decodes "skep-cluster status -json": {"status": …,
// "health": {"status": …}}.
func parseClusterStatus(out string) (ClusterView, error) {
	var doc struct {
		Status ClusterView `json:"status"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return ClusterView{}, fmt.Errorf("skep-cluster status -json: %v", err)
	}
	doc.Status.Health = doc.Health.Status
	return doc.Status, nil
}

// waitForLeader polls the status every second for up to wait until it
// shows a leader and a state machine that is not stopped (D8, D11 step 6).
func waitForLeader(ctx context.Context, c Cluster, wait time.Duration, sleep func(time.Duration)) (ClusterView, error) {
	var last error
	for waited := time.Duration(0); ; waited += time.Second {
		v, err := c.Status(ctx)
		switch {
		case err != nil:
			last = err
		case v.StateMachine.Stalled != nil:
			last = fmt.Errorf("the cluster's state machine is stopped at log index %d: %s", v.StateMachine.Stalled.Index, Clean(v.StateMachine.Stalled.Reason))
		case !v.LeaderKnown():
			last = errors.New("skep-cluster's status shows no leader")
		default:
			return v, nil
		}
		if waited >= wait {
			return ClusterView{}, fmt.Errorf("within %s: %v", wait, last)
		}
		if err := ctx.Err(); err != nil {
			return ClusterView{}, err
		}
		sleep(time.Second)
	}
}

// clusterAccountItem checks the skep-cluster account like the skep
// account (D3): missing or half there refuses, other numbers warn.
func clusterAccountItem(root string) Item {
	what := clusterAccount
	users, err := ReadAccounts(hostPath(root, "/etc/passwd"))
	if err != nil {
		return Item{Kind: Refuse, Area: "cluster", What: what, Detail: "cannot read /etc/passwd: " + err.Error()}
	}
	groups, err := ReadAccounts(hostPath(root, "/etc/group"))
	if err != nil {
		return Item{Kind: Refuse, Area: "cluster", What: what, Detail: "cannot read /etc/group: " + err.Error()}
	}
	u, hasU := findAccount(users, clusterAccount)
	g, hasG := findAccount(groups, clusterAccount)
	switch {
	case hasU != hasG:
		return Item{Kind: Refuse, Area: "cluster", What: what, Detail: "only one of the skep-cluster user and group exists; something else made it, resolve it by hand"}
	case !hasU:
		detail := "the skep-cluster account is missing; the package's pre-install creates it (UID/GID " + ClusterAccountID + "): reinstall the package"
		if o, taken := findAccountID(users, ClusterAccountID); taken {
			detail = "the skep-cluster account is missing and UID " + ClusterAccountID + " belongs to " + o.Name
		} else if o, taken := findAccountID(groups, ClusterAccountID); taken {
			detail = "the skep-cluster account is missing and GID " + ClusterAccountID + " belongs to " + o.Name
		}
		return Item{Kind: Refuse, Area: "cluster", What: what, Detail: detail}
	case (u.Shell != "/usr/sbin/nologin" && u.Shell != "/sbin/nologin") || u.Home != "/nonexistent":
		return Item{Kind: Refuse, Area: "cluster", What: what, Detail: fmt.Sprintf("the skep-cluster user has shell %q and home %q; the service account has no login shell and no home, so something else made it", u.Shell, u.Home)}
	case u.ID != ClusterAccountID || g.ID != ClusterAccountID:
		return Item{Kind: Warn, Area: "cluster", What: what, Detail: fmt.Sprintf("exists with UID %s and GID %s, not %s; it works on a single host, a cluster will need %s on every node. Setup changes nothing", u.ID, g.ID, ClusterAccountID, ClusterAccountID)}
	}
	return Item{Kind: OK, Area: "cluster", What: what, Detail: "UID and GID " + ClusterAccountID + ", no login shell, no home"}
}
