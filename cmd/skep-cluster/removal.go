package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/service"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// confirmClusterID asks the administrator to type the cluster ID back on
// the terminal, or checks -cluster-id (decisions D7c and D8 of step 8b-2a,
// as "join" confirms). want is the ID shown; there is no "-yes".
func confirmClusterID(e env, what, want, given string) error {
	if given != "" {
		if given != want {
			return fmt.Errorf("refused: -cluster-id %s is not this node's cluster %s; nothing was changed", given, want)
		}
		return nil
	}
	tty, err := e.openTTY()
	if err != nil {
		return fmt.Errorf("refused: there is no terminal to confirm on (%v); give -cluster-id %s to confirm without one", err, want)
	}
	defer tty.Close()
	fmt.Fprintf(tty, "%s\nType the cluster ID (%s) to confirm: ", what, want)
	answer, _ := bufio.NewReader(tty).ReadString('\n')
	if strings.TrimSpace(answer) != want {
		return errors.New("not confirmed; nothing was changed")
	}
	return nil
}

// runRemove is "skep-cluster remove NAME" (ADR 0013 B1, removal; decision
// D3 of step 8b-2a): root only, on a member. It asks the service to
// propose the first entry (removing) and then waits and reports; the
// leader loop does the other two steps. No decision with a root effect is
// taken here: the service does everything, and this command only reads
// its answers to print them.
func runRemove(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster remove", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		socket          = flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the service")
		dataDir         = flags.String("data-dir", clusterconf.DefaultDataDir, "directory of the node key and the cluster store")
		allowUnanswered = flags.Bool("allow-unanswered", false, "remove the node although too few of the remaining voters answer (the cluster then accepts no changes until they return); needs the cluster ID confirmed")
		clusterID       = flags.String("cluster-id", "", "with -allow-unanswered, confirm the cluster without a terminal")
		wait            = flags.Duration("wait", 2*time.Minute, "how long to wait for the removal to finish")
		forgetGuests    = flags.Bool("forget-guests", false, "remove a member that owns guests or name reservations: the cluster forgets them (the guests keep running on that host)")
		forgetCount     = flags.Int("forget-count", 0, "with --forget-guests, confirm the number of entries to forget without a terminal")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	countGiven := false
	flags.Visit(func(f *flag.Flag) { countGiven = countGiven || f.Name == "forget-count" })
	if countGiven && !*forgetGuests {
		return errors.New("-forget-count is only for --forget-guests")
	}
	if e.euid != 0 {
		return errNeedsRoot("remove")
	}
	if flags.NArg() != 1 {
		return errors.New("remove needs the name of the member to remove, as one argument")
	}
	name := flags.Arg(0)
	if !clusterstate.ValidName(name) {
		return fmt.Errorf("%q is not a valid node name", termSafe(name))
	}
	if *clusterID != "" && !*allowUnanswered {
		return errors.New("-cluster-id is only for -allow-unanswered")
	}
	client := localapi.NewClient(*socket)
	st, err := client.Status(ctx)
	if err != nil {
		return err
	}
	req := localapi.RemoveRequest{Name: name}
	if *forgetGuests {
		if err := confirmForget(ctx, e, client, st, &req, countGiven, *forgetCount); err != nil {
			return err
		}
	}
	if *allowUnanswered {
		what := fmt.Sprintf("Removing %s without an answer from enough of the remaining voters: the cluster may accept no changes until they return.", termSafe(name))
		if err := confirmClusterID(e, what, st.ClusterID, *clusterID); err != nil {
			return err
		}
		req.AllowUnanswered, req.ClusterID = true, st.ClusterID
	}
	res, err := client.Remove(ctx, req)
	if err != nil {
		return errors.Join(err, checkNoRootFiles(e, *dataDir))
	}
	if res.HandedOver {
		fmt.Fprintf(e.stdout, "This node was the leader: it handed the leadership over first.\n")
	}
	fmt.Fprintf(e.stdout, "Removing %s (node ID %s) from cluster %s (operation %s): the first entry is committed.\n",
		termSafe(res.Name), termSafe(res.NodeID), termSafe(res.ClusterID), termSafe(res.OpID))
	if len(res.Unanswered) > 0 {
		fmt.Fprintf(e.stdout, "No answer from: %s.\n", termSafe(strings.Join(res.Unanswered, ", ")))
	}
	fmt.Fprintf(e.stdout, "The leader removes it from the Raft configuration, then removes its member entry; it does that by itself.\n")
	rootErr := checkNoRootFiles(e, *dataDir)
	err = waitRemoved(ctx, e, client, res, st.NodeID, *wait)
	return errors.Join(err, rootErr)
}

// confirmForget is "--forget-guests" (step 8d-2, D20): it counts what the
// member owns in this node's inventory, shows it, and has the operator
// confirm the number of entries the cluster forgets (on the terminal, or
// -forget-count). The confirmed counts go in the request; the leader
// compares them with what the member owns then. A member that owns
// nothing is removed as a plain removal; a name that is not a member's is
// left to the service, which gives its usual error.
func confirmForget(ctx context.Context, e env, client *localapi.Client, st localapi.Status, req *localapi.RemoveRequest, countGiven bool, count int) error {
	var id string
	for _, m := range st.Members {
		if m.Name == req.Name {
			id = m.ID
		}
	}
	if id == "" {
		return nil
	}
	inv, err := client.GuestsRead(ctx)
	if err != nil {
		return err
	}
	if inv.Joining || inv.Removed {
		return nil // the service refuses the removal on this node
	}
	var guests []guestapi.GuestInfo
	var reservations []guestapi.ReservationInfo
	for _, g := range inv.Guests {
		if g.Owner == id {
			guests = append(guests, g)
		}
	}
	for _, r := range inv.Reservations {
		if r.Owner == id {
			reservations = append(reservations, r)
		}
	}
	total := len(guests) + len(reservations)
	if total == 0 {
		fmt.Fprintf(e.stdout, "%s owns no guests and no name reservations in the inventory: it is removed as a plain removal.\n", termSafe(req.Name))
		return nil
	}
	fmt.Fprintf(e.stdout, "%s owns %d guests and %d name reservations in the cluster's inventory:\n", termSafe(req.Name), len(guests), len(reservations))
	for i, g := range guests {
		if i == 50 {
			fmt.Fprintf(e.stdout, "  and %d more guests\n", len(guests)-50)
			break
		}
		fmt.Fprintf(e.stdout, "  %-9s %s  %s\n", termSafe(g.Kind), termSafe(g.Name), termSafe(g.ID))
	}
	for i, r := range reservations {
		if i == 20 {
			fmt.Fprintf(e.stdout, "  and %d more name reservations\n", len(reservations)-20)
			break
		}
		fmt.Fprintf(e.stdout, "  %-9s %s  (name reservation)\n", termSafe(r.Kind), termSafe(r.Name))
	}
	fmt.Fprintf(e.stdout, "The cluster forgets these entries. The guests themselves are not touched: they keep running on %s.\n", termSafe(req.Name))
	fmt.Fprintf(e.stdout, "After \"skep-cluster leave --wipe\" there, that host lists them in its own inventory again.\n")
	if !countGiven {
		tty, err := e.openTTY()
		if err != nil {
			return fmt.Errorf("refused: there is no terminal to confirm on (%v); give -forget-count %d to confirm without one; nothing was changed", err, total)
		}
		defer tty.Close()
		fmt.Fprintf(tty, "Type the number of entries to forget (%d) to confirm: ", total)
		answer, _ := bufio.NewReader(tty).ReadString('\n')
		if count, err = strconv.Atoi(strings.TrimSpace(answer)); err != nil {
			count = -1
		}
	}
	if count != total {
		return fmt.Errorf("refused: the number confirmed is not the %d entries %s owns; nothing was changed", total, termSafe(req.Name))
	}
	req.ForgetGuests, req.Guests, req.Reservations = true, len(guests), len(reservations)
	return nil
}

// waitRemoved waits until the member entry is gone (or this node itself is
// removed) and prints who has not yet applied the removal.
func waitRemoved(ctx context.Context, e env, client *localapi.Client, res localapi.RemoveResult, self string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	var last localapi.Status
	for {
		st, err := client.Status(ctx)
		if err == nil {
			last = st
			if st.Removed != nil {
				fmt.Fprintf(e.stdout, "This node is the removed node: it has shut its Raft instance and its listener.\n")
				fmt.Fprintf(e.stdout, "\"skep-cluster health\" on a member names the members that have not yet applied the removal.\n")
				printWipeHint(e, res.Name)
				return nil
			}
			gone := true
			for _, m := range st.Members {
				if m.ID == res.NodeID {
					gone = false
				}
			}
			if gone {
				break
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(e.stdout, "After %s the removal has not finished.\n", wait)
			for _, r := range last.Removals {
				fmt.Fprintf(e.stdout, "  %s\n", termSafe(r))
			}
			fmt.Fprintf(e.stdout, "It finishes by itself once that is done; \"skep-cluster health\" on any member says what it waits for.\n")
			return &exitCodeError{code: 3}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	fmt.Fprintf(e.stdout, "%s is removed: no member pins its key any more once each has applied the removal.\n", termSafe(res.Name))
	// The status round follows the removal at once; give it a moment.
	var names []string
	for i := 0; i < 10; i++ {
		st, err := client.Status(ctx)
		if err == nil {
			names = st.UnappliedRemoval
			if len(names) == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if len(names) == 0 {
		fmt.Fprintf(e.stdout, "Every member has applied the removal.\n")
	} else {
		fmt.Fprintf(e.stdout, "Members that have not yet applied the removal: %s.\n", termSafe(strings.Join(names, ", ")))
		fmt.Fprintf(e.stdout, "Rule: if %s was removed because it is compromised, a member that was down during the removal must be wiped\n", termSafe(res.Name))
		fmt.Fprintf(e.stdout, "(\"skep-cluster leave --wipe\" on it) before it returns, and joined again. \"skep-cluster health\" names them until each has applied it.\n")
	}
	if res.NodeID != self {
		printWipeHint(e, res.Name)
	}
	return nil
}

func printWipeHint(e env, name string) {
	fmt.Fprintf(e.stdout, "On %s, as root, before it can join a cluster again:\n  skep-cluster leave --wipe\n", termSafe(name))
}

// runLeave is "skep-cluster leave --wipe" (ADR 0013 B1; decision D8 of
// step 8b-2a): root, local. It confirms the cluster ID (read from the
// files with the service running when they tell it, so that a refusal
// stops nothing), stops the service, and, as the service account, makes
// this host a one-node cluster with
// a new cluster ID and a new node key, the same node ID, no listen address
// and no open port. A marker written first and removed last makes it safe
// to run again after a crash at any point; the second run does not ask.
func runLeave(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster leave", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		wipe       = flags.Bool("wipe", false, "required: wipe this node's cluster state and key")
		clusterID  = flags.String("cluster-id", "", "confirm the cluster without a terminal: must equal this node's cluster ID")
		configPath = flags.String("config", clusterconf.DefaultPath, "path to cluster.json")
		dataDir    = flags.String("data-dir", clusterconf.DefaultDataDir, "directory of the node key and the cluster store")
		socket     = flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the service")
		account    = flags.String("account", clusterconf.Account, "the service account")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if !*wipe {
		return errors.New("leave needs --wipe: a node leaves its cluster only by wiping its cluster state and key")
	}
	if e.euid != 0 {
		return errNeedsRoot("leave --wipe")
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
	stage := func(action string) (stageResult, error) {
		res, err := runStage(ctx, e, acct.UID, acct.GID, []string{action, "-config", *configPath, "-data-dir", *dataDir}, nil)
		if err == nil && res.Outcome != "ok" {
			err = errors.New(termSafe(res.Message))
		}
		return res, err
	}
	fail := func(err error) error {
		return errors.Join(err, checkNoRootFiles(e, *dataDir),
			errors.New("skep_cluster is left stopped; run \"skep-cluster leave --wipe\" again, or start it with \"service skep_cluster start\""))
	}
	restart := func(err error) error {
		if serr := startAndWait(ctx, e, *socket); serr != nil {
			fmt.Fprintf(e.stderr, "skep-cluster: %v\n", serr)
		}
		return errors.Join(err, checkNoRootFiles(e, *dataDir))
	}
	// The cluster ID is read from the files with the service running, so
	// that a wrong or missing confirmation stops nothing. The service is
	// stopped first only when the store alone can tell, or for a resume.
	info, err := stage("wipe-info-files")
	if err != nil {
		return errors.Join(err, checkNoRootFiles(e, *dataDir))
	}
	stopped := false
	if info.ClusterID == "" || info.Resume {
		if err := stopForStore(ctx, e, *socket); err != nil {
			return err
		}
		stopped = true
		if info, err = stage("wipe-info"); err != nil {
			return restart(err)
		}
	}
	if info.Resume {
		fmt.Fprintf(e.stdout, "A wipe of this node was begun and did not finish; finishing it (new cluster %s).\n", termSafe(info.ClusterID))
	} else {
		fmt.Fprintf(e.stdout, "This wipes the cluster state of this node (%s, node ID %s):\n", cfg.NodeName, cfg.NodeID)
		pin := termSafe(info.Pin)
		if pin == "" {
			pin = "none (node.key cannot be loaded; the wipe replaces it)"
		}
		fmt.Fprintf(e.stdout, "  cluster %s, node key pin %s\n", termSafe(info.ClusterID), pin)
		fmt.Fprintf(e.stdout, "It becomes a one-node cluster with a new cluster ID and a new node key, and no listener.\n")
		if err := confirmClusterID(e, "Wipe this node's cluster state?", info.ClusterID, *clusterID); err != nil {
			if !stopped {
				return errors.Join(err, checkNoRootFiles(e, *dataDir))
			}
			return restart(err)
		}
	}
	if !stopped {
		if err := stopForStore(ctx, e, *socket); err != nil {
			return err
		}
		// What was confirmed must still be what is there.
		again, err := stage("wipe-info")
		if err == nil && (again.ClusterID != info.ClusterID || again.Resume) {
			err = fmt.Errorf("refused: the cluster ID changed from %s to %s while the confirmation was asked; nothing was changed",
				termSafe(info.ClusterID), termSafe(again.ClusterID))
		}
		if err != nil {
			return restart(err)
		}
	}
	if _, err := stage("wipe-begin"); err != nil {
		return fail(err)
	}
	// Root's own step: no listen address in cluster.json, so that the
	// service starts without a listener. Repeating it changes nothing.
	if cfg.ListenAddress != "" {
		cfg.ListenAddress = ""
		ownerUID, ownerGID := e.configOwner(acct.GID)
		if err := clusterconf.Write(*configPath, cfg, ownerUID, ownerGID); err != nil {
			return fail(err)
		}
	}
	done, err := stage("wipe-finish")
	if err != nil {
		return fail(err)
	}
	startErr := startAndWait(ctx, e, *socket)
	rootErr := checkNoRootFiles(e, *dataDir)
	fmt.Fprintf(e.stdout, "This node is a one-node cluster again.\n")
	fmt.Fprintf(e.stdout, "  Cluster ID:   %s\n  Node key pin: %s (new)\n  Listener:     none (no network port)\n", termSafe(done.ClusterID), termSafe(done.Pin))
	fmt.Fprintf(e.stdout, "If it has not been removed on a member of its old cluster yet, remove it there, as root:\n  skep-cluster remove %s\n", cfg.NodeName)
	fmt.Fprintf(e.stdout, "To join a cluster again: skep-cluster join-request -address ADDR\n")
	// D21 of step 8c-1: the new cluster has no users, and skep-api's copy
	// follows it.
	fmt.Fprintf(e.stdout, "This host has no users now: nobody can log in to skep-api until an administrator is created, as root:\n  skep-api admin create --username NAME\n")
	return errors.Join(startErr, rootErr)
}

// runAbandon is "skep-cluster join --abandon" (ADR 0013 B1 step 3.7;
// decision D9 of step 8b-2a): root, local, only with a join in progress.
// A first stage, as the service account with the service running, refuses
// what it can know already (no join, nothing to put back, a node that
// accepts the key, no answer), and then nothing is stopped. Otherwise the
// service is stopped; the stage asks the nodes again and does the file
// steps. This command believes the stages only for what it prints and for
// leaving the service alone; stopping and starting it is its only root
// effect.
func runAbandon(ctx context.Context, e env, configPath, dataDir, socket, account string) error {
	if e.euid != 0 {
		return errNeedsRoot("join --abandon")
	}
	acct, err := e.lookup(account)
	if err != nil {
		return fmt.Errorf("the service account: %w", err)
	}
	if acct.UID == 0 {
		return errors.New("the service account must not be root")
	}
	if _, err := clusterconf.Load(configPath); err != nil {
		return err
	}
	// A refusal that can be known with the service running stops nothing:
	// no join.json, nothing to put back, or a node that accepts the key or
	// no answer at all (the stage writes nothing).
	pre, err := runStage(ctx, e, acct.UID, acct.GID, []string{"abandon-check", "-config", configPath, "-data-dir", dataDir}, nil)
	if err != nil {
		return errors.Join(err, checkNoRootFiles(e, dataDir))
	}
	if pre.Outcome != "proceed" {
		return abandonOutcome(e, pre, nil, checkNoRootFiles(e, dataDir))
	}
	if err := stopForStore(ctx, e, socket); err != nil {
		return err
	}
	res, serr := runStage(ctx, e, acct.UID, acct.GID, []string{"abandon", "-config", configPath, "-data-dir", dataDir}, nil)
	startErr := startAndWait(ctx, e, socket)
	rootErr := checkNoRootFiles(e, dataDir)
	if serr != nil {
		return errors.Join(serr, startErr, rootErr)
	}
	return abandonOutcome(e, res, startErr, rootErr)
}

// abandonOutcome prints a stage's outcome of "join --abandon"; startErr
// and rootErr are joined to the result.
func abandonOutcome(e env, res stageResult, startErr, rootErr error) error {
	for _, p := range res.Attempts {
		fmt.Fprintf(e.stdout, "  %s\n", termSafe(p))
	}
	switch res.Outcome {
	case "restored":
		if res.Resume {
			fmt.Fprintf(e.stdout, "The decision to give the join up was already taken; its last steps are done now.\n")
		}
		fmt.Fprintf(e.stdout, "The join is given up: this host's one-node cluster is back (pin %s).\n", termSafe(res.Pin))
		return errors.Join(startErr, rootErr)
	case "no_join", "no_standalone":
		return errors.Join(fmt.Errorf("refused: %s", termSafe(res.Message)), startErr, rootErr)
	case "accepted":
		fmt.Fprintf(e.stdout, "Refused: %s still accept(s) this host's key, so the cluster still counts this host.\n", termSafe(strings.Join(res.Members, ", ")))
		fmt.Fprintf(e.stdout, "On %s, as root, remove this host first:\n  skep-cluster remove %s\nthen run \"skep-cluster join --abandon\" here again.\n",
			termSafe(res.Members[0]), termSafe(res.Name))
		return errors.Join(errors.New("nothing was changed"), startErr, rootErr)
	case "no_answer":
		fmt.Fprintf(e.stdout, "Refused: no node of the admission or of the join answer answered, so it cannot be known whether the cluster still counts this host.\n")
		fmt.Fprintf(e.stdout, "Run it again when at least one member is reachable.\n")
		return errors.Join(errors.New("nothing was changed"), startErr, rootErr)
	}
	return errors.Join(fmt.Errorf("join --abandon: %s", termSafe(res.Message)), startErr, rootErr)
}

// runListenNone is "skep-cluster listen -none" (decision D13 of step
// 8b-2a): root, local. A refusal the running service's status shows is
// given without a stop. Otherwise it is refused unless this node is alone
// in its cluster with nothing pending, read from the store with the
// service stopped (a decision with a root effect is not taken on the
// running service's word). Then it removes the listen address from cluster.json
// and starts the service, which updates its own records and opens no port.
func runListenNone(ctx context.Context, e env, configPath, dataDir, socket, account string) error {
	cfg, err := clusterconf.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.ListenAddress == "" {
		fmt.Fprintf(e.stdout, "This node has no listen address; nothing to do.\n")
		return checkNoRootFiles(e, dataDir)
	}
	acct, err := e.lookup(account)
	if err != nil {
		return fmt.Errorf("the service account: %w", err)
	}
	if acct.UID == 0 {
		return errors.New("the service account must not be root")
	}
	// A refusal the running service can show stops nothing (believing the
	// socket for a refusal has no root effect). Only a node it shows alone
	// with nothing pending, or a socket that does not answer, goes on to
	// the store, read with the service stopped.
	if st, err := localapi.NewClient(socket).Status(ctx); err == nil {
		if why := notAlone(st); why != "" {
			return errors.Join(errors.New("refused: "+why+"; nothing was changed"), checkNoRootFiles(e, dataDir))
		}
	}
	if err := stopForStore(ctx, e, socket); err != nil {
		return err
	}
	res, err := runStage(ctx, e, acct.UID, acct.GID, []string{"read-state", "-config", configPath, "-data-dir", dataDir}, nil)
	if err == nil && res.Outcome == "not_alone" {
		err = errors.New("refused: " + termSafe(res.Message) + "; nothing was changed")
	} else if err == nil && res.Outcome != "ok" {
		err = errors.New(termSafe(res.Message))
	}
	if err != nil {
		if serr := startAndWait(ctx, e, socket); serr != nil {
			fmt.Fprintf(e.stderr, "skep-cluster: %v\n", serr)
		}
		return errors.Join(err, checkNoRootFiles(e, dataDir))
	}
	old := cfg.PeerAddress()
	cfg.ListenAddress = ""
	ownerUID, ownerGID := e.configOwner(acct.GID)
	if err := clusterconf.Write(configPath, cfg, ownerUID, ownerGID); err != nil {
		_ = startAndWait(ctx, e, socket)
		return err
	}
	if err := startAndWait(ctx, e, socket); err != nil {
		return err
	}
	client := localapi.NewClient(socket)
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := client.Status(ctx)
		if err == nil && !st.Listening && st.ListenAddress == "" && len(st.Members) == 1 && st.Members[0].Address == "" {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("skep_cluster runs without a listen address, but after 30 s its member entry still carries an address; see \"skep-cluster health\"")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err := checkNoRootFiles(e, dataDir); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "skep-cluster no longer listens on %s: no network port is open, and \"skep-cluster admit\" is refused.\n", old)
	fmt.Fprintf(e.stdout, "To listen again: skep-cluster listen -address ADDR\n")
	return nil
}

// notAlone says why the running service's status refuses "listen -none",
// or "" when it does not.
func notAlone(st localapi.Status) string {
	switch {
	case st.Removed != nil:
		return "this node was removed from its cluster; run \"skep-cluster leave --wipe\""
	case st.Join != nil:
		return "this node is joining a cluster"
	case len(st.Members) > 1:
		return fmt.Sprintf("the cluster has %d members", len(st.Members))
	case len(st.Pendings) > 0:
		return fmt.Sprintf("%d admission(s) are pending; withdraw them first (\"skep-cluster admit --withdraw NAME\")", len(st.Pendings))
	}
	return ""
}

// stageReadState is the stage of "listen -none": alone, one member,
// nothing pending, not joining.
func stageReadState(configPath, dataDir string) stageResult {
	cfg, err := clusterconf.Load(configPath)
	if err != nil {
		return fail(err.Error())
	}
	if err := layout.CheckOldLayout(dataDir); err != nil {
		return fail(err.Error())
	}
	if js, err := joinstate.Read(dataDir); err != nil || js != nil {
		if err != nil {
			return fail(err.Error())
		}
		return stageResult{Outcome: "not_alone", Message: "this node is joining a cluster"}
	}
	if r, err := service.ReadRemoved(dataDir); err != nil || r != nil {
		if err != nil {
			return fail(err.Error())
		}
		return stageResult{Outcome: "not_alone", Message: "this node was removed from its cluster; run \"skep-cluster leave --wipe\""}
	}
	view, servers, err := store.ReadState(layout.State(dataDir), cfg.NodeID)
	if err != nil {
		if len(servers) > 1 {
			return stageResult{Outcome: "not_alone", Servers: len(servers), Message: fmt.Sprintf("the Raft configuration has %d servers", len(servers))}
		}
		return fail(err.Error())
	}
	switch {
	case view.Stalled != nil:
		return fail("the state machine stops at log index " + fmt.Sprint(view.Stalled.Index) + ": " + view.Stalled.Reason)
	case len(view.Members) != 1 || view.Members[0].ID != cfg.NodeID:
		return stageResult{Outcome: "not_alone", Servers: len(servers), Message: fmt.Sprintf("the cluster has %d members", len(view.Members))}
	case len(view.Pendings) > 0:
		return stageResult{Outcome: "not_alone", Servers: len(servers),
			Message: fmt.Sprintf("%d admission(s) are pending; withdraw them first (\"skep-cluster admit --withdraw NAME\")", len(view.Pendings))}
	}
	return stageResult{Outcome: "ok", Servers: len(servers), ClusterID: view.ClusterID}
}

// stageWipeInfo is the "wipe-info" stage, and with filesOnly the
// "wipe-info-files" stage that runs while the service runs (an empty
// cluster ID: only the store can tell).
func stageWipeInfo(configPath, dataDir string, filesOnly bool) stageResult {
	cfg, err := clusterconf.Load(configPath)
	if err != nil {
		return fail(err.Error())
	}
	// Either the files or the store, never both: the store's lock is
	// held by a running service, and opening it would wait for it.
	var id string
	var resume bool
	if filesOnly {
		id, resume, err = service.WipeInfoFiles(dataDir)
	} else {
		id, resume, err = service.WipeInfo(dataDir, cfg.NodeID)
	}
	if err != nil {
		return fail(err.Error())
	}
	res := stageResult{Outcome: "ok", ClusterID: id, Resume: resume}
	if k, err := nodekey.Load(dataDir); err == nil {
		res.Pin = k.Pin()
	}
	return res
}

func stageWipeBegin(configPath, dataDir string) stageResult {
	cfg, err := clusterconf.Load(configPath)
	if err != nil {
		return fail(err.Error())
	}
	m, err := service.WipeBegin(dataDir, cfg.NodeID, time.Now())
	if err != nil {
		return fail(err.Error())
	}
	return stageResult{Outcome: "ok", ClusterID: m.NewClusterID}
}

func stageWipeFinish(ctx context.Context, configPath, dataDir string) stageResult {
	cfg, err := clusterconf.Load(configPath)
	if err != nil {
		return fail(err.Error())
	}
	id, pin, err := service.WipeFinish(ctx, dataDir, cfg, time.Now())
	if err != nil {
		return fail(err.Error())
	}
	return stageResult{Outcome: "ok", ClusterID: id, Pin: pin}
}

// stageAbandon is the "abandon" stage, and with check the
// "abandon-check" stage that runs while the service runs and writes
// nothing.
func stageAbandon(configPath, dataDir string, check bool) stageResult {
	cfg, err := clusterconf.Load(configPath)
	if err != nil {
		return fail(err.Error())
	}
	key, err := nodekey.Load(dataDir)
	if err != nil {
		return fail(err.Error())
	}
	run := service.StageAbandon
	if check {
		run = service.CheckAbandon
	}
	out := run(dataDir, cfg, key, 10*time.Second)
	return stageResult{Outcome: out.Outcome, Message: out.Message, Attempts: out.Probes, Members: out.AcceptedBy,
		Standalone: out.Standalone, Resume: out.Resumed, Pin: key.Pin(), Name: cfg.NodeName}
}
