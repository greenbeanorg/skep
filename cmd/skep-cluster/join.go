package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// joinActor is the actor "skep-cluster join-request" and "join" name on
// the agent's socket.
const joinActor = "skep-cluster-join"

// stageCommand is the hidden command the root-run commands start, as the
// service account, for everything they do inside the data directory.
const stageCommand = "internal-stage"

// stageResult is what the service-account stage reports to the root-run
// command, as one JSON line on its stdout.
type stageResult struct {
	// Outcome is "ok" for a read; for a join one of the service.Join*
	// results, or "not_alone".
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
	Pin     string `json:"pin,omitempty"`
	Servers int    `json:"servers"`
	// Standalone is the directory the one-node state was moved to.
	Standalone string   `json:"standalone,omitempty"`
	Attempts   []string `json:"attempts,omitempty"`
	ClusterID  string   `json:"cluster_id,omitempty"`
	Members    []string `json:"members,omitempty"`
	// Resume: a wipe or an abandon that was begun earlier is finished.
	Resume bool `json:"resume,omitempty"`
	// Name is this node's name (abandon).
	Name string `json:"name,omitempty"`
}

// stopForStore stops skep_cluster so that its store can be read: bbolt
// holds an exclusive lock while the service runs.
func stopForStore(ctx context.Context, e env, socket string) error {
	if err := e.serviceCtl(ctx, "stop"); err != nil {
		// Not running is fine; anything still holding the store
		// makes the stage fail on bbolt's lock.
		fmt.Fprintf(e.stderr, "note: \"service skep_cluster stop\": %v\n", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := localapi.NewClient(socket).Health(ctx); err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("skep_cluster is still answering on its socket after \"service skep_cluster stop\"")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// startAndWait starts skep_cluster and waits until it answers.
func startAndWait(ctx context.Context, e env, socket string) error {
	if err := e.serviceCtl(ctx, "start"); err != nil {
		return fmt.Errorf("starting skep_cluster: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := localapi.NewClient(socket).Status(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("skep_cluster does not answer on its socket 30 s after its start; see \"skep-cluster health\"")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// runStage runs a stage as the service account and decodes its result.
// Only the join stage gets a standard input (its guest list, D13); every
// other stage gets none (nil).
func runStage(ctx context.Context, e env, uid, gid uint32, args []string, stdin []byte) (stageResult, error) {
	out, err := e.runAsAccount(ctx, uid, gid, append([]string{stageCommand}, args...), stdin)
	var res stageResult
	line := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(line, '\n'); i >= 0 {
		line = line[i+1:]
	}
	if derr := json.Unmarshal([]byte(line), &res); derr != nil {
		if err == nil {
			err = derr
		}
		return stageResult{}, fmt.Errorf("the part of the command that runs as the service account failed: %w (output: %q)", err, strings.TrimSpace(string(out)))
	}
	return res, nil
}

// checkNoRootFiles is the end of every root-run command: nothing in the
// data directory may belong to root (ADR 0013 B4, B6).
func checkNoRootFiles(e env, dataDir string) error {
	if _, err := os.Lstat(dataDir); errors.Is(err, fs.ErrNotExist) {
		return nil // nothing there, nothing root-owned
	}
	rootOwned, err := store.RootOwned(dataDir, e.owner)
	if err != nil {
		return err
	}
	if len(rootOwned) > 0 {
		return fmt.Errorf("root-owned files are in %s: %v", dataDir, rootOwned)
	}
	return nil
}

func runJoinRequest(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster join-request", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		address    = flags.String("address", "", "this host's management address for the node-to-node listener (required)")
		configPath = flags.String("config", clusterconf.DefaultPath, "path to cluster.json")
		dataDir    = flags.String("data-dir", clusterconf.DefaultDataDir, "directory of the node key and the cluster store")
		socket     = flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the service")
		account    = flags.String("account", clusterconf.Account, "the service account")
	)
	// Step 8d-2, D9: join-request no longer asks the agent about guests;
	// the flag is still accepted, so that scripts that pass it keep
	// working.
	_ = flags.String("agent-socket", clusterconf.DefaultAgentSocket, "not used since step 8d-2 (accepted for compatibility)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if e.euid != 0 {
		return errNeedsRoot("join-request")
	}
	if err := refuseDuringRotation(*dataDir, "join-request"); err != nil {
		return err
	}
	if *address == "" {
		return errors.New("join-request needs -address: there is no default address")
	}
	addr, err := clusterconf.ParseListenAddress(*address)
	if err != nil {
		return err
	}
	if err := checkLocalAddress(e, addr); err != nil {
		return err
	}
	cfg, err := clusterconf.Load(*configPath)
	if err != nil {
		return err
	}
	acct, err := e.lookup(*account)
	if err != nil {
		return fmt.Errorf("the service account: %w", err)
	}
	if acct.UID == 0 {
		return errors.New("the service account must not be root")
	}

	// "Is this node alone" is read from the store with the service
	// stopped, by a process running as the service account.
	if err := stopForStore(ctx, e, *socket); err != nil {
		return err
	}
	res, err := runStage(ctx, e, acct.UID, acct.GID, []string{"read", "-config", *configPath, "-data-dir", *dataDir}, nil)
	if err == nil && res.Outcome == "not_alone" {
		err = errors.New("refused: " + termSafe(res.Message))
	} else if err == nil && res.Outcome != "ok" {
		err = errors.New(termSafe(res.Message))
	}
	if err != nil {
		if serr := startAndWait(ctx, e, *socket); serr != nil {
			fmt.Fprintf(e.stderr, "skep-cluster: %v\n", serr)
		}
		return errors.Join(err, checkNoRootFiles(e, *dataDir))
	}
	cfg.ListenAddress = addr.String()
	ownerUID, ownerGID := e.configOwner(acct.GID)
	if err := clusterconf.Write(*configPath, cfg, ownerUID, ownerGID); err != nil {
		_ = startAndWait(ctx, e, *socket)
		return err
	}
	if err := startAndWait(ctx, e, *socket); err != nil {
		return err
	}
	// Wait for the listener and the member entry's address.
	client := localapi.NewClient(*socket)
	deadline := time.Now().Add(30 * time.Second)
	var st localapi.Status
	for {
		st, err = client.Status(ctx)
		if err == nil && st.Listening && len(st.Members) == 1 && st.Members[0].Address == cfg.PeerAddress() {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("skep_cluster runs, but after 30 s its listener or its member entry does not carry the new address; see \"skep-cluster health\"")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if st.Pin != res.Pin {
		return fmt.Errorf("the running service has pin %s, the key on disk %s; nothing was printed", st.Pin, res.Pin)
	}
	line, err := joincode.EncodeRequest(joincode.Request{Node: joincode.Node{NodeID: cfg.NodeID, Name: cfg.NodeName, Address: cfg.PeerAddress(), Pin: st.Pin}})
	if err != nil {
		return err
	}
	if err := checkNoRootFiles(e, *dataDir); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Join request for this host:\n")
	fmt.Fprintf(e.stdout, "  Node ID:      %s\n  Node name:    %s\n  Address:      %s\n  Node key pin: %s\n", cfg.NodeID, cfg.NodeName, cfg.PeerAddress(), st.Pin)
	fmt.Fprintf(e.stdout, "\n%s\n\n", line)
	fmt.Fprintf(e.stdout, "On a member of the cluster, compare the pin above with the one this host shows on its own console\n")
	fmt.Fprintf(e.stdout, "(\"skep-cluster status\", line \"Node key pin\"), then admit it, as root:\n")
	fmt.Fprintf(e.stdout, "  skep-cluster admit %s\n", line)
	fmt.Fprintf(e.stdout, "The admission it prints is valid for 30 minutes.\n")
	return nil
}

func runAdmit(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster admit", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		socket   = flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the service")
		dataDir  = flags.String("data-dir", clusterconf.DefaultDataDir, "directory of the node key and the cluster store")
		withdraw = flags.String("withdraw", "", "withdraw the pending admission of the host with this name")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if e.euid != 0 {
		return errNeedsRoot("admit")
	}
	client := localapi.NewClient(*socket)
	if *withdraw != "" {
		if flags.NArg() > 0 {
			return errors.New("admit --withdraw takes a name and no request")
		}
		res, err := client.Withdraw(ctx, *withdraw)
		if err != nil {
			return errors.Join(err, checkNoRootFiles(e, *dataDir))
		}
		if err := checkNoRootFiles(e, *dataDir); err != nil {
			return err
		}
		fmt.Fprintf(e.stdout, "Withdrawn: the pending admission of %s (node ID %s) is removed (operation %s).\n", termSafe(res.Name), termSafe(res.NodeID), termSafe(res.OpID))
		return nil
	}
	if flags.NArg() != 1 {
		return errors.New("admit needs the request line that \"skep-cluster join-request\" printed, as one argument")
	}
	req, err := joincode.ParseRequest(flags.Arg(0))
	if err != nil {
		return err
	}
	if err := joincode.CheckPorts(req.Node); err != nil {
		return fmt.Errorf("refused: %w", err)
	}
	res, err := client.Admit(ctx, flags.Arg(0))
	if err != nil {
		return errors.Join(err, checkNoRootFiles(e, *dataDir))
	}
	if err := checkNoRootFiles(e, *dataDir); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Admitted %s (node ID %s, address %s)\n  pin %s\n", termSafe(req.Name), termSafe(req.NodeID), termSafe(req.Address), termSafe(req.Pin))
	fmt.Fprintf(e.stdout, "  until %s (operation %s).\n", time.Unix(res.ExpiresAt, 0).UTC().Format(time.RFC3339), termSafe(res.OpID))
	fmt.Fprintf(e.stdout, "\nAdmission (nothing in it is secret):\n\n%s\n\n", termSafe(res.Admission))
	fmt.Fprintf(e.stdout, "On %s, as root:\n  skep-cluster join %s\n", termSafe(req.Name), termSafe(res.Admission))
	fmt.Fprintf(e.stdout, "To withdraw it before it is used:\n  skep-cluster admit --withdraw %s\n", termSafe(req.Name))
	return nil
}

func printAdmission(w io.Writer, adm joincode.Admission, guests int) {
	fmt.Fprintf(w, "This host will join the cluster\n  Cluster ID: %s\nwith the members\n", adm.ClusterID)
	for _, m := range adm.Members {
		fmt.Fprintf(w, "  %-20s %s  %s\n                       pin %s\n", m.Name, m.NodeID, m.Address, m.Pin)
	}
	if guests > 0 {
		// Step 8d-2, D17.
		fmt.Fprintf(w, "%d guests of this host join with it; their names must be free in the cluster (a name already taken there refuses the join).\n", guests)
	}
	fmt.Fprintf(w, "Its own one-node cluster is moved aside.\n")
}

// printHostUsers lists the host's own users (D21 of step 8c-1): the
// cluster's users replace them once the join has finished.
func printHostUsers(ctx context.Context, e env, w io.Writer) {
	lines, err := e.hostUsers(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(w, "This host's users could not be read (%s); whatever they are, the cluster's users replace them.\n", termSafe(err.Error()))
	case len(lines) == 0:
		fmt.Fprintf(w, "This host has no users of its own; it takes the cluster's users.\n")
	default:
		fmt.Fprintf(w, "This host's users (name, role):\n")
		for _, l := range lines {
			fmt.Fprintf(w, "  %s\n", termSafe(l))
		}
		fmt.Fprintf(w, "The cluster's users replace them: these accounts exist here after the join only if the cluster has them, and every session on this host ends.\n")
	}
}

// refusePendingImport refuses the join of a host whose 0.1 users have not
// been imported (F5 of the 8c-1 review): its users would be lost. An
// api.db older than the installed skep-api ("unmigrated", D18 of step
// 8c-2) is refused the same way: it may hold such users. A state that
// cannot be read does not stop the join; it is said.
func refusePendingImport(ctx context.Context, e env) error {
	state, err := e.hostImportState(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(e.stdout, "The state of this host's user import could not be read (%s); the join goes on.\n", termSafe(err.Error()))
	case state == "pending":
		return errors.New("refused: this host's users have not been imported: run \"skep-api admin import-users\" first; nothing was changed")
	case state == "unmigrated":
		return errors.New("refused: this host's api.db is older than the installed skep-api, so its users may not have been imported: run skep-setup upgrade first; nothing was changed")
	}
	return nil
}

// confirmJoin asks on the terminal, or checks -cluster-id (decision D4 of
// the 8b-1 run).
func confirmJoin(e env, adm joincode.Admission, clusterID string) error {
	if clusterID != "" {
		if clusterID != adm.ClusterID {
			return fmt.Errorf("refused: -cluster-id %s is not the admission's cluster %s; nothing was changed", clusterID, adm.ClusterID)
		}
		return nil
	}
	tty, err := e.openTTY()
	if err != nil {
		return fmt.Errorf("refused: there is no terminal to confirm on (%v); give -cluster-id %s to confirm without one", err, adm.ClusterID)
	}
	defer tty.Close()
	fmt.Fprintf(tty, "Join cluster %s? Type yes to continue: ", adm.ClusterID)
	answer, _ := bufio.NewReader(tty).ReadString('\n')
	if strings.TrimSpace(answer) != "yes" {
		return errors.New("not confirmed; nothing was changed")
	}
	return nil
}

func runJoin(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster join", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		clusterID   = flags.String("cluster-id", "", "confirm the cluster without a terminal: must equal the admission's cluster ID")
		configPath  = flags.String("config", clusterconf.DefaultPath, "path to cluster.json")
		dataDir     = flags.String("data-dir", clusterconf.DefaultDataDir, "directory of the node key and the cluster store")
		socket      = flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the service")
		agentSocket = flags.String("agent-socket", clusterconf.DefaultAgentSocket, "Unix socket of skep-agent")
		account     = flags.String("account", clusterconf.Account, "the service account")
		wait        = flags.Duration("wait", 90*time.Second, "how long to wait for this node to apply its member entry")
		taskWait    = flags.Duration("task-wait", 60*time.Second, "how long to wait for skep-agent's unfinished tasks once its drain is set")
		abandon     = flags.Bool("abandon", false, "give up a join that is past its point of no return (after removing this host on a member)")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *abandon {
		if flags.NArg() > 0 || *clusterID != "" {
			return errors.New("join --abandon takes no admission and no -cluster-id")
		}
		return runAbandon(ctx, e, *configPath, *dataDir, *socket, *account)
	}
	if e.euid != 0 {
		return errNeedsRoot("join")
	}
	if err := refuseDuringRotation(*dataDir, "join"); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("join needs the admission line that \"skep-cluster admit\" printed, as one argument")
	}
	line := flags.Arg(0)
	adm, err := joincode.ParseAdmission(line)
	if err != nil {
		return err
	}
	if err := joincode.CheckPorts(append([]joincode.Node{adm.For}, adm.Members...)...); err != nil {
		return fmt.Errorf("refused: %w; nothing was changed", err)
	}
	cfg, err := clusterconf.Load(*configPath)
	if err != nil {
		return err
	}
	if adm.For.NodeID != cfg.NodeID || adm.For.Name != cfg.NodeName || adm.For.Address != cfg.PeerAddress() {
		return fmt.Errorf("refused: the admission is for %s (%s, %s), this host is %s (%s, %s); nothing was changed",
			adm.For.Name, adm.For.NodeID, adm.For.Address, cfg.NodeName, cfg.NodeID, cfg.PeerAddress())
	}
	acct, err := e.lookup(*account)
	if err != nil {
		return fmt.Errorf("the service account: %w", err)
	}
	if acct.UID == 0 {
		return errors.New("the service account must not be root")
	}
	// Step 8d-2, D10 step 2: the host's guests, read without a drain to
	// show them. Nothing is changed by a refusal here.
	shown, err := e.agentGuestList(ctx, *agentSocket)
	if err != nil {
		return fmt.Errorf("refused: join needs the guest list from skep-agent: %s; nothing was changed", termSafe(err.Error()))
	}
	if err := checkHostGuests(shown); err != nil {
		return fmt.Errorf("refused: %s; nothing was changed", termSafe(err.Error()))
	}
	if err := refusePendingImport(ctx, e); err != nil {
		return err
	}
	printAdmission(e.stdout, adm, len(shown))
	printGuests(e.stdout, shown)
	printHostUsers(ctx, e, e.stdout)
	if err := confirmJoin(e, adm, *clusterID); err != nil {
		return err
	}

	// D10 step 5 (D11): the drain, then the agent's unfinished tasks.
	// From here every path clears the drain (drain.clear, once); the
	// deferred call is the backstop of a path that returns early.
	drain, err := setJoinDrain(ctx, e, *agentSocket)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := drain.clear(ctx); cerr != nil {
			fmt.Fprintf(e.stderr, "skep-cluster: %v\n", cerr)
		}
	}()
	refuseDrained := func(why string) error {
		if cerr := drain.clear(ctx); cerr != nil {
			return fmt.Errorf("refused: %s; the drain was set and clearing it failed: %v; nothing else was changed", why, cerr)
		}
		return fmt.Errorf("refused: %s; the drain was set and cleared; nothing else was changed", why)
	}
	if err := drain.waitTasks(ctx, *taskWait); err != nil {
		return refuseDrained(err.Error())
	}
	// D10 step 6: the list that joins, read with the drain set.
	guests, err := e.agentGuestList(ctx, *agentSocket)
	if err != nil {
		return refuseDrained("reading the guest list from skep-agent again failed: " + termSafe(err.Error()))
	}
	if err := checkHostGuests(guests); err != nil {
		return refuseDrained(termSafe(err.Error()))
	}
	if !sameGuests(shown, guests) {
		vms, jails := countKinds(guests)
		fmt.Fprintf(e.stdout, "The host's guests changed since they were shown: %d VMs and %d jails join with it.\n", vms, jails)
	}
	if rerr := drain.renewFailed(); rerr != nil {
		return refuseDrained("renewing skep-agent's drain failed: " + termSafe(rerr.Error()))
	}
	input, err := json.Marshal(stageInput{Guests: append([]clusterstate.JoinGuest{}, guests...)})
	if err != nil {
		return refuseDrained(err.Error())
	}

	// The join call is made by the stage that runs as the service
	// account, with the service stopped: it reads "is this node alone"
	// from the store, moves the one-node state aside, calls, and puts
	// the state back only when it knows join step 1 was not applied.
	// This command believes only the stage's outcome, and its own root
	// effects (stopping and starting the service) do not depend on it.
	// The guest list goes to the stage on its standard input (D13).
	drain.serviceStopped()
	if err := stopForStore(ctx, e, *socket); err != nil {
		return err
	}
	res, serr := runStage(ctx, e, acct.UID, acct.GID, []string{"join", "-config", *configPath, "-data-dir", *dataDir, "-admission", line}, input)
	startErr := startAndWait(ctx, e, *socket)
	// The drain is cleared here, whatever the stage's outcome and whether
	// or not the service started, not after waitJoined: it is needed only
	// until the join call is made. With join.json the service refuses
	// guest writes ("this node is joining"), a guest deleted on the host
	// leaves the inventory by its reconciliation once it is a member, and
	// a join that failed before step 1 leaves the host its own one-node
	// cluster. The deferred call stays the backstop of earlier returns.
	if cerr := drain.clear(ctx); cerr != nil {
		fmt.Fprintf(e.stderr, "skep-cluster: %v\n", cerr)
	}
	rootErr := checkNoRootFiles(e, *dataDir)
	if serr != nil {
		return errors.Join(serr, startErr, rootErr,
			errors.New("the join's outcome is not known; see \"skep-cluster status\" before doing anything else"))
	}
	switch res.Outcome {
	case "not_alone":
		return errors.Join(fmt.Errorf("refused: %s; nothing was changed", termSafe(res.Message)), startErr, rootErr)
	case "refused", "unreached":
		fmt.Fprintf(e.stdout, "\nThe join failed before the cluster added this node:\n")
		for _, a := range res.Attempts {
			fmt.Fprintf(e.stdout, "  %s\n", termSafe(a))
		}
		fmt.Fprintf(e.stdout, "Nothing was written in the cluster. This host's one-node cluster is back (pin %s).\n", termSafe(res.Pin))
		return errors.Join(fmt.Errorf("join failed: %s", termSafe(res.Message)), startErr, rootErr)
	case "accepted", "unknown":
	default:
		return errors.Join(fmt.Errorf("the join stage reported %q: %s", res.Outcome, termSafe(res.Message)), startErr, rootErr)
	}
	if res.Outcome == "accepted" {
		fmt.Fprintf(e.stdout, "\nThe cluster accepted this node (cluster %s).\n", termSafe(res.ClusterID))
	} else {
		fmt.Fprintf(e.stdout, "\nThe join request was sent and no answer came: its outcome is unknown.\n")
		for _, a := range res.Attempts {
			fmt.Fprintf(e.stdout, "  %s\n", termSafe(a))
		}
	}
	fmt.Fprintf(e.stdout, "The join is past its point of no return: the one-node state stays moved aside (%s)\n", termSafe(res.Standalone))
	fmt.Fprintf(e.stdout, "and skep-cluster keeps working with the admission's members until it has joined.\n")
	removeCmd := "skep-cluster remove " + cfg.NodeName
	if len(guests) > 0 {
		removeCmd = "skep-cluster remove --forget-guests " + cfg.NodeName
	}
	fmt.Fprintf(e.stdout, "To give it up: on a member, \"%s\"; then here \"skep-cluster join --abandon\".\n", removeCmd)
	if startErr != nil || rootErr != nil {
		return errors.Join(startErr, rootErr)
	}
	return waitJoined(ctx, e, *socket, cfg.NodeID, *wait)
}

// waitJoined waits until the node has applied its member entry and prints
// where the join stands.
func waitJoined(ctx context.Context, e env, socket, nodeID string, wait time.Duration) error {
	client := localapi.NewClient(socket)
	deadline := time.Now().Add(wait)
	for {
		st, err := client.Status(ctx)
		if err == nil {
			for _, m := range st.Members {
				if m.ID == nodeID {
					fmt.Fprintf(e.stdout, "This node has applied its member entry: state %s.\n", m.State)
					if m.State != "member" {
						fmt.Fprintf(e.stdout, "It is a non-voter until it has caught up and every member has applied its entry;\n")
						fmt.Fprintf(e.stdout, "\"skep-cluster health\" on any member names the member being waited for.\n")
					}
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(e.stdout, "After %s this node has not yet applied its member entry; it keeps trying.\n", wait)
			fmt.Fprintf(e.stdout, "See \"skep-cluster status\" and \"skep-cluster health\".\n")
			return &exitCodeError{code: 3}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
