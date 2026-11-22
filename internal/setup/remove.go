package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// RemoveOptions are the answers of skep-setup remove (ADR 0012
// Decision 6, answer 11: data, host lines and the account are kept unless
// asked).
type RemoveOptions struct {
	Root string
	// KeepGuests removes Skep although guests exist; it stops nothing.
	KeepGuests bool
	// RevertHost removes the lines setup added to rc.conf and loader.conf
	// where they are unchanged since.
	RevertHost bool
	// PurgeData deletes Skep's data, its configuration and the account.
	PurgeData bool
}

// dataRoot is Skep's data directory; purging keeps only setup's journals.
const dataRoot = "/var/db/skep"

// guestCounts reads how many VMs and jails the agent has, read-only, from
// the database its configuration names. A database that does not exist
// means an agent that never started, unless the agent answers on its
// socket (errAgentWithoutDB). A table that is missing is an error: the
// database is not what this build expects.
func guestCounts(ctx context.Context, root string, ag Agent) (vms, jails int, err error) {
	path, err := AgentDBPath(root)
	if err != nil {
		return 0, 0, fmt.Errorf("the agent's configuration: %w", err)
	}
	if !exists(path) {
		return 0, 0, agentWithoutDB(ctx, ag, root, path)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	count := func(table string) (int, error) {
		var n int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n)
		return n, err
	}
	if vms, err = count("virtual_machines"); err != nil {
		return 0, 0, err
	}
	jails, err = count("containers")
	return vms, jails, err
}

// ComputeRemove plans skep-setup remove. It reads files and the agent's
// database, asks the agent's socket whether it answers, and runs zfs list
// and zfs get (read-only) to name the datasets it never destroys and to
// find encryption keys kept under /var/db/skep.
func ComputeRemove(ctx context.Context, o RemoveOptions, f Facts, r Runner, ag Agent) (Plan, error) {
	if o.Root == "" {
		return Plan{}, errors.New("root must not be empty")
	}
	journals, err := LoadJournals(o.Root)
	if err != nil {
		return Plan{}, err
	}
	b := &builder{o: Options{Root: o.Root}, f: f, journals: journals}
	if j := incompleteJournal(journals); j != nil {
		b.refuse("journal", j.Command+" "+j.ID, "an earlier run did not finish; run skep-setup rollback "+j.ID+" first")
	}
	rc, err := ReadRCConf(o.Root)
	if err != nil {
		return Plan{}, err
	}
	loader, err := readLoader(o.Root)
	if err != nil {
		return Plan{}, err
	}
	b.rc, b.loader = rc, loader
	b.editability()

	// Guests.
	vms, jails, err := guestCounts(ctx, o.Root, ag)
	var noDB *errAgentWithoutDB
	if errors.As(err, &noDB) {
		b.refuse("guests", "the agent's database", noDB.Error())
	} else if err != nil {
		return Plan{}, fmt.Errorf("read the agent's guests: %w", err)
	}
	rj, rv := f.SkepGuests()
	if o.PurgeData && len(rj)+len(rv) > 0 {
		b.refuse("data", "--purge-data", fmt.Sprintf("Skep guests are running (jails %v, VMs %v): purging would delete the agent's state for them while they run. Stop them first; --keep-guests does not change this", rj, rv))
	}
	switch {
	case vms+jails+len(rj)+len(rv) == 0:
		b.ok("guests", "no Skep guests", "")
	case !o.KeepGuests:
		b.refuse("guests", fmt.Sprintf("%d VM(s) and %d jail(s) exist (%d running)", vms, jails, len(rj)+len(rv)),
			"delete them first, or use --keep-guests to leave them unmanaged")
	default:
		b.warn("guests", fmt.Sprintf("%d VM(s) and %d jail(s) are kept (%d running)", vms, jails, len(rj)+len(rv)),
			"nothing is stopped; their datasets stay and nothing will manage them. pkg delete refuses while any of them runs")
	}

	// Stop the services, then disable them.
	b.add("services", "stop skep_api", "", Action{Type: ActServiceStop, Service: "skep_api"})
	b.add("services", "stop skep_agent", "running guests keep running", Action{Type: ActServiceStop, Service: "skep_agent"})
	reported := map[string]bool{}
	if o.RevertHost {
		reported = b.revertHost()
	} else {
		b.ok("host", "lines setup added to rc.conf and loader.conf", "kept (use --revert-host to remove them where unchanged)")
	}
	for _, v := range []string{"skep_agent_enable", "skep_api_enable"} {
		// revertHost has said what happens to it already, unless it left
		// a changed value that still enables the service.
		if reported[v] {
			continue
		}
		b.disable(v)
	}

	if o.PurgeData {
		if err := b.purge(); err != nil {
			return Plan{}, err
		}
	} else {
		b.ok("data", dataRoot+", agent.json, api.json, the skep account", "kept (use --purge-data to delete them)")
	}
	b.datasets(ctx, r, o.PurgeData)
	b.item(Item{Kind: Manual, Area: "package", What: "then remove the files", Steps: []string{"pkg delete skep"}})
	return b.plan, nil
}

// disable sets an rc.conf enable variable to NO when it is YES.
func (b *builder) disable(name string) {
	if over := b.rc.OverriddenIn(name); over != "" {
		b.item(Item{Kind: Manual, Area: rcConfPath, What: name + `="NO"`,
			Detail: "set in " + strings.TrimPrefix(over, b.o.Root) + "; change it there"})
		return
	}
	v, ok := b.rc.Main.Vals[name]
	if !ok || !strings.EqualFold(v, "YES") {
		b.ok(rcConfPath, name, "not enabled")
		return
	}
	a := Action{Type: ActSysrc, File: rcConfPath, Name: name, Op: "=", Value: "NO", Expect: &v}
	if b.rcOK {
		b.add(rcConfPath, name+`="NO"`, "", a)
	} else {
		b.item(Item{Kind: Manual, Area: rcConfPath, What: name + `="NO"`, Steps: []string{a.String()}})
	}
}

type addedVar struct {
	file, name, op, value string
	// created: the variable was not in the file before that apply (read
	// from the apply's backup of the file), so setup made it.
	created bool
}

// setBefore reports whether name was set in file before journal j first
// changed it, from j's backup copy. No backup (the file did not exist)
// means it was not set.
func setBefore(root string, j Journal, file, name string) bool {
	for _, e := range j.Entries {
		if e.File != file || e.Backup == "" {
			continue
		}
		v, err := ReadShVars(filepath.Join(JournalDir(root, j.ID), e.Backup))
		if err != nil {
			return true // unknown: treat as the operator's, never delete it
		}
		_, ok := v.Vals[name]
		return ok
	}
	return false
}

// revertHost plans removing what setup's applies added, where unchanged,
// and returns the variables it has settled: removed, gone, or reported in
// a way that needs nothing more.
func (b *builder) revertHost() map[string]bool {
	reported := map[string]bool{}
	var added []addedVar
	seen := map[addedVar]bool{}
	for _, j := range b.journals {
		if j.RolledBack != nil || j.Command != "apply" {
			continue
		}
		for _, e := range j.Entries {
			a := e.Action
			if !e.Done || a.Type != ActSysrc || (a.Op != "=" && a.Op != "+=") {
				continue
			}
			v := addedVar{file: a.File, name: a.Name, op: a.Op, value: a.Value}
			if !seen[v] {
				seen[v] = true
				v.created = a.Op == "+=" && !setBefore(b.o.Root, j, a.File, a.Name)
				added = append(added, v)
			}
		}
	}
	if len(added) == 0 {
		b.ok("host", "lines setup added", "none recorded")
		return reported
	}
	bridge := false
	for _, v := range added {
		conf, editable := b.rc, b.rcOK
		if v.file == loaderConfPath {
			conf, editable = b.loader, b.loaderOK
		}
		cur, ok := conf.Main.Vals[v.name]
		reported[v.name] = true
		var acts []Action
		switch {
		case v.file == rcConfPath && v.name == "zfs_enable" && ok && len(b.f.Pools) > 0:
			pools := make([]string, 0, len(b.f.Pools))
			for p := range b.f.Pools {
				pools = append(pools, p)
			}
			sort.Strings(pools)
			b.ok(v.file, v.name, fmt.Sprintf("kept although setup added it: pools %v are imported, and without it they are not imported at boot", pools))
		case !ok:
			b.ok(v.file, v.name, "already gone")
		case v.op == "=" && cur != v.value:
			b.item(Item{Kind: Conflict, Area: v.file, What: v.name,
				Detail: fmt.Sprintf("is %q, setup set %q; changed since, left alone", cur, v.value)})
			// A changed skep_*_enable that still says yes is disabled.
			reported[v.name] = !strings.EqualFold(cur, "YES")
		case v.op == "=":
			acts = []Action{{Type: ActSysrc, File: v.file, Name: v.name, Op: "-x", Expect: &cur}}
		default:
			var mine []string
			for _, w := range strings.Fields(v.value) {
				if slices.Contains(strings.Fields(cur), w) {
					mine = append(mine, w)
				}
			}
			rest := len(strings.Fields(cur)) - len(mine)
			switch {
			case v.created && rest == 0:
				// sysrc NAME-= leaves NAME="" behind (FreeBSD 15.1, smith,
				// 2026-11-22); a list setup created and that holds only
				// setup's words, or nothing, is deleted, so the file
				// returns to its bytes before apply.
				acts = []Action{{Type: ActSysrc, File: v.file, Name: v.name, Op: "-x", Expect: &cur}}
			case len(mine) == 0:
				b.ok(v.file, v.name, "setup's words are gone already")
			default:
				// One word per sysrc call: how sysrc -= treats several
				// words at once is not something setup relies on.
				for _, w := range mine {
					acts = append(acts, Action{Type: ActSysrc, File: v.file, Name: v.name, Op: "-=", Value: w})
				}
			}
		}
		if strings.HasPrefix(v.name, "ifconfig_bridge") || v.name == "cloned_interfaces" {
			bridge = true
		}
		for _, act := range acts {
			if editable {
				b.add(v.file, act.String(), "added by skep-setup and unchanged since", act)
			} else {
				b.item(Item{Kind: Manual, Area: v.file, What: act.String(), Steps: []string{act.String()}})
			}
		}
	}
	b.item(Item{Kind: Manual, Area: "host", What: "the running system",
		Detail: "reverting boot-time lines unloads no module and destroys no interface; they go at the next boot" +
			map[bool]string{true: ", or now with ifconfig BRIDGE destroy once nothing uses the bridge", false: ""}[bridge]})
	return reported
}

// purge plans deleting Skep's data (keeping setup's journals), its
// configuration files, and the account.
func (b *builder) purge() error {
	b.item(Item{Kind: Warn, Area: "data", What: "--purge-data",
		Detail: "deletes the node identity, both databases, every audit log and archive, the database backups, the TLS key and the install media in " +
			dataRoot + ", and agent.json and api.json. It cannot be undone; setup's journals in " + dataRoot + "/setup are kept. " +
			"Skep keeps no ZFS encryption key (ADR 0010: the operator holds them); setup checks below that no guest dataset's keylocation is under " + dataRoot})
	entries, err := os.ReadDir(hostPath(b.o.Root, dataRoot))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.Name() != "setup" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		p := dataRoot + "/" + n
		b.add("data", "delete "+p, "", Action{Type: ActRemovePath, File: p})
	}
	for _, p := range []string{agentJSONPath, apiJSONPath} {
		if _, err := os.Lstat(hostPath(b.o.Root, p)); err == nil {
			b.add("config", "delete "+p, "a copy is kept in the journal", Action{Type: ActRemovePath, File: p})
		}
	}
	users, _ := ReadAccounts(hostPath(b.o.Root, "/etc/passwd"))
	groups, _ := ReadAccounts(hostPath(b.o.Root, "/etc/group"))
	_, u := findAccount(users, skepAccount)
	_, g := findAccount(groups, skepAccount)
	if u || g {
		b.add("account", "delete the skep user and group", "files still owned by it would show a bare number",
			Action{Type: ActPwDelete, Name: skepAccount})
	}
	return nil
}

// datasets lists, never destroys, Skep's datasets under each allow-listed
// pool, and finds the encryption keys of those datasets that are files
// under /var/db/skep, which --purge-data would delete: refused with
// --purge-data, a warning without.
func (b *builder) datasets(ctx context.Context, r Runner, purge bool) {
	raw, err := os.ReadFile(hostPath(b.o.Root, agentJSONPath))
	if err != nil {
		return
	}
	var c struct {
		Storage struct {
			Pools       []string `json:"pools"`
			DatasetRoot string   `json:"dataset_root"`
		} `json:"storage"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return
	}
	if c.Storage.DatasetRoot == "" {
		c.Storage.DatasetRoot = "skep"
	}
	for _, p := range c.Storage.Pools {
		if !poolNameRE.MatchString(p) {
			continue
		}
		root := p + "/" + c.Storage.DatasetRoot
		out, err := r.Run(ctx, Cmd{Name: cmdZfs, Args: []string{"list", "-H", "-o", "name", "-r", root}})
		if err != nil {
			continue
		}
		names := lines(out)
		if len(names) > 0 {
			b.item(Item{Kind: Manual, Area: "storage", What: "datasets under " + root,
				Detail: "setup never destroys a dataset; destroy them yourself once nothing uses them:\n" + strings.Join(names, "\n"),
				Steps:  []string{"zfs destroy -r " + root}})
		}
		b.keysUnderData(ctx, r, root, purge)
	}
}

// keysUnderData looks up the encryption roots of the datasets under root
// and their keylocation; a key file under /var/db/skep is reported.
func (b *builder) keysUnderData(ctx context.Context, r Runner, root string, purge bool) {
	out, err := r.Run(ctx, Cmd{Name: cmdZfs, Args: []string{"get", "-H", "-o", "value", "-r", "encryptionroot", root}})
	if err != nil {
		b.warn("storage", "encryption keys under "+root, "zfs get encryptionroot failed ("+err.Error()+"); whether a key file is under "+dataRoot+" is unknown")
		return
	}
	var encRoots []string
	for _, v := range lines(out) {
		if v != "-" && v != "" && !slices.Contains(encRoots, v) {
			encRoots = append(encRoots, v)
		}
	}
	if len(encRoots) == 0 {
		return
	}
	out, err = r.Run(ctx, Cmd{Name: cmdZfs, Args: append([]string{"get", "-H", "-o", "name,value", "keylocation"}, encRoots...)})
	if err != nil {
		b.warn("storage", "encryption keys under "+root, "zfs get keylocation failed ("+err.Error()+"); whether a key file is under "+dataRoot+" is unknown")
		return
	}
	var under []string
	for _, l := range lines(out) {
		name, loc, ok := strings.Cut(l, "\t")
		if ok && strings.HasPrefix(loc, "file://"+dataRoot+"/") {
			under = append(under, name+" ("+loc+")")
		}
	}
	switch {
	case len(under) == 0:
		b.ok("storage", "encryption keys of "+root, "encryption roots "+strings.Join(encRoots, ", ")+"; no key file under "+dataRoot)
	case purge:
		b.refuse("data", "--purge-data", "the encryption key of "+strings.Join(under, ", ")+" is under "+dataRoot+" and would be deleted, making the data unreadable; move it and change keylocation first")
	default:
		b.warn("storage", "encryption keys under "+dataRoot, strings.Join(under, ", ")+": the key is under "+dataRoot+"; --purge-data would refuse")
	}
}
