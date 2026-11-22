package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// ContainerStore reads and writes containers (ADR 0010 decision 8). The
// desired columns (hostname, config, desired_state) are written by the
// container tasks acting for a user; the observed_* columns are what the
// agent saw on the host. started_once only ever goes from 0 to 1 (the
// schema's trigger enforces it).
type ContainerStore struct {
	db *sql.DB
}

// NewContainerStore returns the container store for the agent database.
func NewContainerStore(db *sql.DB) *ContainerStore { return &ContainerStore{db: db} }

// containerConfig is the JSON of containers.config. The NICs have their
// own table, container_networks.
type containerConfig struct {
	Options          models.ContainerOptions `json:"options"`
	Limits           models.ContainerLimits  `json:"limits"`
	SSHAuthorizedKey string                  `json:"ssh_authorized_key,omitempty"`
	Nameservers      []string                `json:"nameservers,omitempty"`
}

func configJSON(c models.Container) (string, error) {
	raw, err := json.Marshal(containerConfig{Options: c.Options, Limits: c.Limits, SSHAuthorizedKey: c.SSHAuthorizedKey, Nameservers: c.Nameservers})
	return string(raw), err
}

// insertNICs stores a container's NICs. A MAC address another jail or VM
// already uses is refused with container_mac_in_use, as the containers
// service refuses it before (amendment u, finding 2); this check, in the
// writing transaction, is the last word.
func insertNICs(ctx context.Context, tx *sql.Tx, c models.Container, at string) error {
	for _, nic := range c.NICs {
		var kind string
		err := tx.QueryRowContext(ctx, `SELECT 'container' FROM container_networks WHERE mac_address = ? AND container_id <> ?
			UNION ALL SELECT 'vm' FROM vm_networks WHERE mac_address = ? LIMIT 1`, nic.MACAddress, c.ID, nic.MACAddress).Scan(&kind)
		switch {
		case err == nil:
			return &models.Error{Code: models.CodeContainerMACInUse,
				Message: fmt.Sprintf("the MAC address %s of NIC slot %d is already held by a %s; give the NIC another address", nic.MACAddress, nic.Slot, kind),
				Details: map[string]any{"slot": nic.Slot, "mac_address": nic.MACAddress, "held_by_kind": kind}}
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("database: check MAC address: %w", err)
		}
		var addr, gw any
		if nic.IPv4.Address != "" {
			addr = nic.IPv4.Address
		}
		if nic.IPv4.Gateway != "" {
			gw = nic.IPv4.Gateway
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO container_networks (id, container_id, slot, mac_address, bridge, ipv4_mode, ipv4_address, ipv4_gateway, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nic.ID, c.ID, nic.Slot, nic.MACAddress, nic.Bridge, nic.IPv4.Mode, addr, gw, at); err != nil {
			return fmt.Errorf("database: insert container NIC: %w", err)
		}
	}
	return nil
}

// loadNICs fills in the NICs of the containers.
func (s *ContainerStore) loadNICs(ctx context.Context, list []models.Container) error {
	if len(list) == 0 {
		return nil
	}
	index := map[string]*models.Container{}
	args := make([]any, 0, len(list))
	marks := make([]string, 0, len(list))
	for i := range list {
		list[i].NICs = []models.ContainerNIC{}
		index[list[i].ID] = &list[i]
		args = append(args, list[i].ID)
		marks = append(marks, "?")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT container_id, id, slot, COALESCE(mac_address, ''), bridge, ipv4_mode, COALESCE(ipv4_address, ''),
			COALESCE(ipv4_gateway, ''), COALESCE(observed_epair, '')
		FROM container_networks WHERE container_id IN (`+strings.Join(marks, ",")+`) ORDER BY container_id, slot`, args...)
	if err != nil {
		return fmt.Errorf("database: read container NICs: %w", err)
	}
	for rows.Next() {
		var id string
		var nic models.ContainerNIC
		if err := rows.Scan(&id, &nic.ID, &nic.Slot, &nic.MACAddress, &nic.Bridge, &nic.IPv4.Mode, &nic.IPv4.Address,
			&nic.IPv4.Gateway, &nic.HostInterface); err != nil {
			rows.Close()
			return fmt.Errorf("database: read container NICs: %w", err)
		}
		index[id].NICs = append(index[id].NICs, nic)
	}
	return closeRows(rows)
}

// IDPrefixTaken reports whether another container's ID, without hyphens,
// starts with prefix: the jail end of a NIC is named after it.
func (s *ContainerStore) IDPrefixTaken(ctx context.Context, prefix string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM containers WHERE substr(replace(id, '-', ''), 1, ?) = ?`,
		len(prefix), prefix).Scan(&n); err != nil {
		return false, fmt.Errorf("database: check container IDs: %w", err)
	}
	return n > 0, nil
}

// SaveEpairs records the host ends of a running jail's epairs by slot,
// and clears those of slots not listed. Observed columns only.
func (s *ContainerStore) SaveEpairs(ctx context.Context, id string, epairs map[int]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE container_networks SET observed_epair = NULL WHERE container_id = ?`, id); err != nil {
		return fmt.Errorf("database: clear observed epairs: %w", err)
	}
	for slot, name := range epairs {
		if _, err := tx.ExecContext(ctx, `UPDATE container_networks SET observed_epair = ? WHERE container_id = ? AND slot = ?`,
			name, id, slot); err != nil {
			return fmt.Errorf("database: record observed epair: %w", err)
		}
	}
	return tx.Commit()
}

// InsertTx stores a new, unprovisioned container in the transaction that
// creates its task. A name another container has is container_name_taken;
// the pool is the template's.
func (s *ContainerStore) InsertTx(ctx context.Context, tx *sql.Tx, nodeID string, c models.Container) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM containers WHERE name = ?`, c.Name).Scan(&n); err != nil {
		return fmt.Errorf("database: check container name: %w", err)
	}
	if n > 0 {
		return &models.Error{Code: models.CodeContainerNameTaken, Message: "another container already has this name"}
	}
	var pool string
	err := tx.QueryRowContext(ctx, `SELECT storage_pool_id FROM container_templates WHERE id = ?`, c.TemplateID).Scan(&pool)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("template %q: %w", c.TemplateID, models.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("database: read the container's template: %w", err)
	}
	cfg, err := configJSON(c)
	if err != nil {
		return err
	}
	at := formatTime(c.CreatedAt)
	// A linked clone starts with its source's started_once (its tree may
	// have run code) and names its origin snapshot (decision 3).
	var origin any
	if c.ClonedFromSnapshotID != "" {
		origin = c.ClonedFromSnapshotID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO containers (id, node_id, name, kind, hostname, vnet, config, desired_state, observed_state,
			template_id, storage_pool_id, provisioned, started_once, cloned_from_snapshot_id, created_at, updated_at)
		VALUES (?, ?, ?, 'jail', ?, 1, ?, 'stopped', 'unknown', ?, ?, 0, ?, ?, ?, ?)`,
		c.ID, nodeID, c.Name, c.Hostname, cfg, c.TemplateID, pool, boolInt(c.StartedOnce), origin, at, at); err != nil {
		return fmt.Errorf("database: insert container: %w", err)
	}
	return insertNICs(ctx, tx, c, at)
}

const containerColumns = `c.id, c.name, c.hostname, c.config, c.template_id, t.name, p.name,
	COALESCE(t.observed_release, ''), c.provisioned, c.started_once, c.desired_state, c.observed_state,
	c.observed_jid, COALESCE(c.observed_detail, ''), c.observed_at, c.created_at, c.updated_at,
	COALESCE(c.cloned_from_snapshot_id, '')`

const containerFrom = ` FROM containers c
	JOIN container_templates t ON t.id = c.template_id
	JOIN storage_pools p ON p.id = c.storage_pool_id`

func scanContainer(row rowScanner) (models.Container, error) {
	var (
		c                        models.Container
		cfg                      string
		provisioned, startedOnce int
		jid                      sql.NullInt64
		observedAt               sql.NullString
		created, updated         string
	)
	if err := row.Scan(&c.ID, &c.Name, &c.Hostname, &cfg, &c.TemplateID, &c.TemplateName, &c.Pool, &c.Release,
		&provisioned, &startedOnce, &c.DesiredState, &c.State, &jid, &c.StateDetail, &observedAt, &created, &updated,
		&c.ClonedFromSnapshotID); err != nil {
		return models.Container{}, err
	}
	var conf containerConfig
	if err := json.Unmarshal([]byte(cfg), &conf); err != nil {
		return models.Container{}, fmt.Errorf("database: container config: %w", err)
	}
	c.Options, c.Limits, c.SSHAuthorizedKey, c.Nameservers = conf.Options, conf.Limits, conf.SSHAuthorizedKey, conf.Nameservers
	c.Provisioned, c.StartedOnce = provisioned == 1, startedOnce == 1
	if jid.Valid {
		v := int(jid.Int64)
		c.JID = &v
	}
	var err error
	if observedAt.Valid {
		at, perr := parseTime(observedAt.String)
		if perr != nil {
			return models.Container{}, perr
		}
		c.ObservedAt = &at
	}
	if c.CreatedAt, err = parseTime(created); err != nil {
		return models.Container{}, err
	}
	c.UpdatedAt, err = parseTime(updated)
	return c, err
}

// StaticNIC is a NIC with a static IPv4 address, as Skep wrote it into its
// jail's rc.conf.
type StaticNIC struct {
	ContainerID   string
	ContainerName string
	Slot          int
	Bridge        string
	// Address is in CIDR form.
	Address string
}

// StaticNICs lists every container NIC with a static IPv4 address: the
// start's check that no running jail on the same bridge holds the address
// (amendment k).
func (s *ContainerStore) StaticNICs(ctx context.Context) ([]StaticNIC, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.container_id, c.name, n.slot, n.bridge, n.ipv4_address FROM container_networks n
		JOIN containers c ON c.id = n.container_id
		WHERE n.ipv4_mode = 'static' AND n.ipv4_address IS NOT NULL ORDER BY c.name, n.slot`)
	if err != nil {
		return nil, fmt.Errorf("database: list static NICs: %w", err)
	}
	var out []StaticNIC
	for rows.Next() {
		var n StaticNIC
		if err := rows.Scan(&n.ContainerID, &n.ContainerName, &n.Slot, &n.Bridge, &n.Address); err != nil {
			rows.Close()
			return nil, fmt.Errorf("database: list static NICs: %w", err)
		}
		out = append(out, n)
	}
	return out, closeRows(rows)
}

// MACHolder is a Skep jail or VM NIC that holds a MAC address.
type MACHolder struct {
	// Kind is "container" or "vm".
	Kind string
	ID   string
	Name string
	Slot int
}

// MACHolders lists the jail and VM NICs, other than containerID's, that
// hold a MAC address (amendment n, SR-03: checked again at every start).
func (s *ContainerStore) MACHolders(ctx context.Context, mac, containerID string) ([]MACHolder, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT 'container', c.id, c.name, n.slot FROM container_networks n JOIN containers c ON c.id = n.container_id
		WHERE n.mac_address = ? AND n.container_id <> ?
		UNION ALL
		SELECT 'vm', v.id, v.name, n.slot FROM vm_networks n JOIN virtual_machines v ON v.id = n.vm_id
		WHERE n.mac_address = ?
		ORDER BY 1, 3, 4`, mac, containerID, mac)
	if err != nil {
		return nil, fmt.Errorf("database: list MAC holders: %w", err)
	}
	var out []MACHolder
	for rows.Next() {
		var h MACHolder
		if err := rows.Scan(&h.Kind, &h.ID, &h.Name, &h.Slot); err != nil {
			rows.Close()
			return nil, fmt.Errorf("database: list MAC holders: %w", err)
		}
		out = append(out, h)
	}
	return out, closeRows(rows)
}

// Get returns one container, or models.ErrNotFound.
func (s *ContainerStore) Get(ctx context.Context, id string) (models.Container, error) {
	c, err := scanContainer(s.db.QueryRowContext(ctx, `SELECT `+containerColumns+containerFrom+` WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Container{}, fmt.Errorf("container %q: %w", id, models.ErrNotFound)
	}
	if err != nil {
		return models.Container{}, fmt.Errorf("database: get container: %w", err)
	}
	one := []models.Container{c}
	if err := s.loadNICs(ctx, one); err != nil {
		return models.Container{}, err
	}
	return one[0], nil
}

// List returns a page of containers ordered by name, and the cursor of the
// next page.
func (s *ContainerStore) List(ctx context.Context, limit int, cursor string) ([]models.Container, string, error) {
	after := ""
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(b) == 0 {
			return nil, "", models.Invalid("invalid cursor")
		}
		after = string(b)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+containerColumns+containerFrom+
		` WHERE c.name > ? ORDER BY c.name LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("database: list containers: %w", err)
	}
	list := []models.Container{}
	for rows.Next() {
		c, err := scanContainer(rows)
		if err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("database: list containers: %w", err)
		}
		list = append(list, c)
	}
	if err := closeRows(rows); err != nil {
		return nil, "", err
	}
	if err := s.loadNICs(ctx, list); err != nil {
		return nil, "", err
	}
	next := ""
	if len(list) > limit {
		list = list[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(list[limit-1].Name))
	}
	return list, next, nil
}

// IDs lists every container's ID.
func (s *ContainerStore) IDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM containers ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("database: list container IDs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, closeRows(rows)
}

func (s *ContainerStore) exec(ctx context.Context, what, id, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("database: %s: %w", what, err)
	}
	return oneRow(res, "container", id)
}

// SetProvisioned records a finished container.create.
func (s *ContainerStore) SetProvisioned(ctx context.Context, id string, at time.Time) error {
	return s.exec(ctx, "provision container", id,
		`UPDATE containers SET provisioned = 1, observed_state = 'stopped', observed_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(at), formatTime(at), id)
}

// MarkStarting records, before jail(8) runs, that the jail is about to
// run code for the first time or again: started_once becomes 1 for good
// and the desired state running (decision 13: from here on Skep never
// writes into the tree).
//
// It refuses, with container_address_in_use, when another jail recorded
// starting or running holds one of this jail's static IPv4 addresses on
// the same bridge (SR-07). The check and the write share one transaction,
// and writing transactions are serialised (_txlock immediate), so two
// starts of jails with the same address cannot both pass: the start
// service's earlier check against the host's running jails can race.
func (s *ContainerStore) MarkStarting(ctx context.Context, id string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: mark container starting: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := addressFreeTx(ctx, tx, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE containers SET started_once = 1, desired_state = 'running', observed_state = 'starting', observed_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(at), formatTime(at), id)
	if err != nil {
		return fmt.Errorf("database: mark container starting: %w", err)
	}
	if err := oneRow(res, "container", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: mark container starting: %w", err)
	}
	return nil
}

// addressFreeTx refuses when a jail other than id, recorded starting or
// running, holds one of id's static IPv4 addresses on the same bridge.
// Addresses are compared by IP, whatever their prefix lengths.
func addressFreeTx(ctx context.Context, tx *sql.Tx, id string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT n.container_id, c.name, c.observed_state, n.slot, n.bridge, n.ipv4_address
		FROM container_networks n JOIN containers c ON c.id = n.container_id
		WHERE n.ipv4_mode = 'static' AND n.ipv4_address IS NOT NULL
		  AND (n.container_id = ? OR c.observed_state IN ('starting', 'running'))
		ORDER BY c.name, n.slot`, id)
	if err != nil {
		return fmt.Errorf("database: check static addresses: %w", err)
	}
	type nic struct {
		StaticNIC
		state string
		ip    netip.Addr
	}
	var mine, others []nic
	for rows.Next() {
		var n nic
		if err := rows.Scan(&n.ContainerID, &n.ContainerName, &n.state, &n.Slot, &n.Bridge, &n.Address); err != nil {
			rows.Close()
			return fmt.Errorf("database: check static addresses: %w", err)
		}
		p, err := netip.ParsePrefix(n.Address)
		if err != nil {
			continue // validated on write; an unparsable address holds nothing
		}
		n.ip = p.Addr()
		if n.ContainerID == id {
			mine = append(mine, n)
		} else {
			others = append(others, n)
		}
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("database: check static addresses: %w", err)
	}
	for _, m := range mine {
		for _, o := range others {
			if o.Bridge != m.Bridge || o.ip != m.ip {
				continue
			}
			return &models.Error{Code: models.CodeContainerAddressInUse,
				Message: fmt.Sprintf("the %s jail %s holds %s on %s; stop it or give this jail another address",
					o.state, o.ContainerName, o.Address, o.Bridge),
				Details: map[string]any{"slot": m.Slot, "address": m.Address, "bridge": m.Bridge,
					"held_by_container_id": o.ContainerID, "held_by_container_name": o.ContainerName}}
		}
	}
	return nil
}

// SetDesired records the desired state a user asked for.
func (s *ContainerStore) SetDesired(ctx context.Context, id, state string, at time.Time) error {
	return s.exec(ctx, "set container desired state", id,
		`UPDATE containers SET desired_state = ?, updated_at = ? WHERE id = ?`, state, formatTime(at), id)
}

// SaveObserved records what the agent saw on the host. It writes observed
// columns only.
func (s *ContainerStore) SaveObserved(ctx context.Context, id, state string, jid *int, detail string, at time.Time) error {
	var j, d any
	if jid != nil {
		j = *jid
	}
	if detail != "" {
		d = detail
	}
	return s.exec(ctx, "save observed container", id,
		`UPDATE containers SET observed_state = ?, observed_jid = ?, observed_detail = ?, observed_at = ? WHERE id = ?`,
		state, j, d, formatTime(at), id)
}

// UpdateConfig records a changed hostname, options, SSH key, nameservers,
// or NICs (replaced whole).
func (s *ContainerStore) UpdateConfig(ctx context.Context, c models.Container, at time.Time) error {
	cfg, err := configJSON(c)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE containers SET hostname = ?, config = ?, updated_at = ? WHERE id = ?`,
		c.Hostname, cfg, formatTime(at), c.ID)
	if err != nil {
		return fmt.Errorf("database: update container: %w", err)
	}
	if err := oneRow(res, "container", c.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM container_networks WHERE container_id = ?`, c.ID); err != nil {
		return fmt.Errorf("database: replace container NICs: %w", err)
	}
	if err := insertNICs(ctx, tx, c, formatTime(at)); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete removes a container's row and, by trigger, its snapshot rows.
func (s *ContainerStore) Delete(ctx context.Context, id string) error {
	return s.exec(ctx, "delete container", id, `DELETE FROM containers WHERE id = ?`, id)
}
