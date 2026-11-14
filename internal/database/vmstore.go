package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/pkg/models"
)

// VMStore reads and writes the virtual machine tables: virtual_machines,
// vm_disks, vm_networks, storage_volumes, and storage_pools. It is the only
// code that knows them.
//
// The rule of the schema holds here: configuration and desired_* columns
// are written by the methods tasks call for a user (InsertTx, Update,
// SetDesiredState, SetProvisioned, Delete); observed_* columns are written
// by SaveObserved, SaveVolumes, and SavePools only.
type VMStore struct {
	db *sql.DB
}

// NewVMStore returns the VM store for the agent database.
func NewVMStore(db *sql.DB) *VMStore { return &VMStore{db: db} }

// ErrVMNameTaken is returned when a VM name is already in use.
var ErrVMNameTaken = &models.Error{
	Code:    models.CodeVMNameTaken,
	Message: "a virtual machine with this name already exists",
}

// VolumeName is the name of a disk's volume relative to its pool, as
// stored in storage_volumes.name.
func VolumeName(datasetRoot, vmID string, slot int) string {
	return fmt.Sprintf("%s/vm/%s/disk%d", datasetRoot, vmID, slot)
}

// poolID returns the row of a pool, creating it on first use.
func poolID(ctx context.Context, tx *sql.Tx, nodeID, name string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM storage_pools WHERE node_id = ? AND name = ?`, nodeID, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("database: read pool: %w", err)
	}
	if id, err = ids.NewUUID(); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO storage_pools (id, node_id, name, type) VALUES (?, ?, ?, 'zfs')`, id, nodeID, name); err != nil {
		return "", fmt.Errorf("database: insert pool: %w", err)
	}
	return id, nil
}

// InsertTx stores a new, unprovisioned VM with its disks, volumes, and
// NICs in an open transaction: the one that creates its vm.create task.
// datasetRoot names the volumes. It returns ErrVMNameTaken, or an
// invalid_request error for a MAC address that is already in use.
func (s *VMStore) InsertTx(ctx context.Context, tx *sql.Tx, vm models.VM, datasetRoot string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM virtual_machines WHERE name = ?`, vm.Name).Scan(&n); err != nil {
		return fmt.Errorf("database: check VM name: %w", err)
	}
	if n > 0 {
		return ErrVMNameTaken
	}
	config, err := json.Marshal(vm.Config)
	if err != nil {
		return fmt.Errorf("database: encode VM config: %w", err)
	}
	now := formatTime(vm.CreatedAt)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO virtual_machines (id, node_id, name, description, engine, cpu_count, memory_bytes,
			firmware, config, desired_state, provisioned, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'stopped', 0, ?, ?)`,
		vm.ID, vm.NodeID, vm.Name, vm.Description, vm.Engine, vm.CPUCount, vm.MemoryBytes,
		vm.Firmware, string(config), now, now); err != nil {
		return fmt.Errorf("database: insert VM: %w", err)
	}
	for _, d := range vm.Disks {
		if err := insertDisk(ctx, tx, vm, d, datasetRoot, now); err != nil {
			return err
		}
	}
	for _, nic := range vm.NICs {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM vm_networks WHERE mac_address = ?`, nic.MACAddress).Scan(&n); err != nil {
			return fmt.Errorf("database: check MAC address: %w", err)
		}
		if n > 0 {
			return models.Invalid("the MAC address " + nic.MACAddress + " is already used by another network interface")
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO vm_networks (id, vm_id, slot, model, mac_address, bridge, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			nic.ID, vm.ID, nic.Slot, nic.Model, nic.MACAddress, nic.Bridge, now); err != nil {
			return fmt.Errorf("database: insert NIC: %w", err)
		}
	}
	return nil
}

func insertDisk(ctx context.Context, tx *sql.Tx, vm models.VM, d models.VMDisk, datasetRoot, now string) error {
	if d.Bus == models.BusAHCICD {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO vm_disks (id, vm_id, slot, bus, read_only, media_name, created_at)
			VALUES (?, ?, ?, ?, 1, ?, ?)`, d.ID, vm.ID, d.Slot, d.Bus, d.Media, now); err != nil {
			return fmt.Errorf("database: insert media: %w", err)
		}
		return nil
	}
	pool, err := poolID(ctx, tx, vm.NodeID, d.Pool)
	if err != nil {
		return err
	}
	volumeID, err := ids.NewUUID()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO storage_volumes (id, pool_id, name, kind, desired_size_bytes, desired_thin,
			desired_volblocksize, created_at, updated_at)
		VALUES (?, ?, ?, 'zvol', ?, ?, ?, ?, ?)`,
		volumeID, pool, VolumeName(datasetRoot, vm.ID, d.Slot), d.SizeBytes, boolInt(d.Thin),
		d.VolBlockSize, now, now); err != nil {
		return fmt.Errorf("database: insert volume: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO vm_disks (id, vm_id, storage_volume_id, slot, bus, size_bytes, read_only, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, vm.ID, volumeID, d.Slot, d.Bus, d.SizeBytes, boolInt(d.ReadOnly), now); err != nil {
		return fmt.Errorf("database: insert disk: %w", err)
	}
	return nil
}

const vmColumns = `id, node_id, name, description, engine, cpu_count, memory_bytes, firmware, config,
	desired_state, provisioned, observed_state, COALESCE(observed_detail, ''), observed_pid, observed_at,
	created_at, updated_at`

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanVM(row rowScanner) (models.VM, error) {
	var (
		vm               models.VM
		config           string
		provisioned      int
		pid              sql.NullInt64
		observedAt       sql.NullString
		created, updated string
	)
	err := row.Scan(&vm.ID, &vm.NodeID, &vm.Name, &vm.Description, &vm.Engine, &vm.CPUCount, &vm.MemoryBytes,
		&vm.Firmware, &config, &vm.DesiredState, &provisioned, &vm.Observed.State, &vm.Observed.Detail,
		&pid, &observedAt, &created, &updated)
	if err != nil {
		return models.VM{}, err
	}
	if err := json.Unmarshal([]byte(config), &vm.Config); err != nil {
		return models.VM{}, fmt.Errorf("database: VM %s: config: %w", vm.ID, err)
	}
	vm.Provisioned = provisioned == 1
	if pid.Valid {
		p := int(pid.Int64)
		vm.Observed.PID = &p
	}
	if observedAt.Valid {
		at, err := parseTime(observedAt.String)
		if err != nil {
			return models.VM{}, err
		}
		vm.Observed.At = &at
	}
	if vm.CreatedAt, err = parseTime(created); err != nil {
		return models.VM{}, err
	}
	if vm.UpdatedAt, err = parseTime(updated); err != nil {
		return models.VM{}, err
	}
	// Drift: the VM should run and does not, or should not and does. A VM
	// on its way (starting, stopping) or not yet observed is not drift.
	switch vm.Observed.State {
	case models.VMRunning:
		vm.Drift = vm.DesiredState != models.VMRunning
	case models.VMStopped, models.VMFailed:
		vm.Drift = vm.DesiredState != models.VMStopped
	}
	vm.Disks, vm.NICs = []models.VMDisk{}, []models.VMNIC{}
	return vm, nil
}

// attach loads the disks and NICs of the given VMs.
func attach(ctx context.Context, q queryer, vms []models.VM) error {
	if len(vms) == 0 {
		return nil
	}
	index := make(map[string]*models.VM, len(vms))
	marks := make([]string, len(vms))
	args := make([]any, len(vms))
	for i := range vms {
		index[vms[i].ID] = &vms[i]
		marks[i], args[i] = "?", vms[i].ID
	}
	in := strings.Join(marks, ", ")

	rows, err := q.QueryContext(ctx, `
		SELECT d.vm_id, d.id, d.slot, d.bus, d.read_only, COALESCE(d.size_bytes, 0), COALESCE(d.media_name, ''),
			COALESCE(p.name, ''), COALESCE(v.name, ''), COALESCE(v.desired_thin, 0), COALESCE(v.desired_volblocksize, 0)
		FROM vm_disks d
		LEFT JOIN storage_volumes v ON v.id = d.storage_volume_id
		LEFT JOIN storage_pools p ON p.id = v.pool_id
		WHERE d.vm_id IN (`+in+`) ORDER BY d.vm_id, d.slot`, args...)
	if err != nil {
		return fmt.Errorf("database: read disks: %w", err)
	}
	for rows.Next() {
		var (
			vmID, volume   string
			d              models.VMDisk
			readOnly, thin int
		)
		if err := rows.Scan(&vmID, &d.ID, &d.Slot, &d.Bus, &readOnly, &d.SizeBytes, &d.Media,
			&d.Pool, &volume, &thin, &d.VolBlockSize); err != nil {
			rows.Close()
			return fmt.Errorf("database: read disks: %w", err)
		}
		d.ReadOnly, d.Thin = readOnly == 1, thin == 1
		if volume != "" {
			d.Dataset = d.Pool + "/" + volume
		}
		index[vmID].Disks = append(index[vmID].Disks, d)
	}
	if err := closeRows(rows); err != nil {
		return err
	}

	rows, err = q.QueryContext(ctx, `
		SELECT vm_id, id, slot, model, COALESCE(mac_address, ''), bridge, COALESCE(observed_tap, '')
		FROM vm_networks WHERE vm_id IN (`+in+`) ORDER BY vm_id, slot`, args...)
	if err != nil {
		return fmt.Errorf("database: read NICs: %w", err)
	}
	for rows.Next() {
		var vmID string
		var nic models.VMNIC
		if err := rows.Scan(&vmID, &nic.ID, &nic.Slot, &nic.Model, &nic.MACAddress, &nic.Bridge, &nic.Tap); err != nil {
			rows.Close()
			return fmt.Errorf("database: read NICs: %w", err)
		}
		index[vmID].NICs = append(index[vmID].NICs, nic)
	}
	return closeRows(rows)
}

func closeRows(rows *sql.Rows) error {
	if err := rows.Close(); err != nil {
		return err
	}
	return rows.Err()
}

// Get returns one VM with its disks and NICs, or models.ErrNotFound.
func (s *VMStore) Get(ctx context.Context, id string) (models.VM, error) {
	vm, err := scanVM(s.db.QueryRowContext(ctx, `SELECT `+vmColumns+` FROM virtual_machines WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return models.VM{}, fmt.Errorf("vm %q: %w", id, models.ErrNotFound)
	}
	if err != nil {
		return models.VM{}, fmt.Errorf("database: read VM: %w", err)
	}
	list := []models.VM{vm}
	if err := attach(ctx, s.db, list); err != nil {
		return models.VM{}, err
	}
	return list[0], nil
}

// List returns a page of VMs ordered by name, and the cursor of the next
// page. opts.Limit must be positive.
func (s *VMStore) List(ctx context.Context, opts models.VMListOptions) ([]models.VM, string, error) {
	where, args := []string{"1 = 1"}, []any{}
	if opts.State != "" {
		where, args = append(where, "observed_state = ?"), append(args, opts.State)
	}
	if opts.Cursor != "" {
		after, err := base64.RawURLEncoding.DecodeString(opts.Cursor)
		if err != nil || len(after) == 0 {
			return nil, "", models.Invalid("invalid cursor")
		}
		where, args = append(where, "name > ?"), append(args, string(after))
	}
	args = append(args, opts.Limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT `+vmColumns+` FROM virtual_machines WHERE `+
		strings.Join(where, " AND ")+` ORDER BY name LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("database: list VMs: %w", err)
	}
	list := []models.VM{}
	for rows.Next() {
		vm, err := scanVM(rows)
		if err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("database: list VMs: %w", err)
		}
		list = append(list, vm)
	}
	if err := closeRows(rows); err != nil {
		return nil, "", err
	}
	next := ""
	if len(list) > opts.Limit {
		list = list[:opts.Limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(list[len(list)-1].Name))
	}
	if err := attach(ctx, s.db, list); err != nil {
		return nil, "", err
	}
	return list, next, nil
}

// IDs returns the ID of every VM.
func (s *VMStore) IDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM virtual_machines ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("database: list VM IDs: %w", err)
	}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, id)
	}
	return out, closeRows(rows)
}

func (s *VMStore) inTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer func() {
		if err != nil {
			if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}

func mustAffect(res sql.Result, id string) error {
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("vm %q: %w", id, models.ErrNotFound)
	}
	return nil
}

// SetProvisioned marks a VM as provisioned: vm.create has finished.
func (s *VMStore) SetProvisioned(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE virtual_machines SET provisioned = 1, updated_at = ? WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("database: mark VM provisioned: %w", err)
	}
	return mustAffect(res, id)
}

// SetDesiredState records what an action asked for: "running" or
// "stopped".
func (s *VMStore) SetDesiredState(ctx context.Context, id, state string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE virtual_machines SET desired_state = ?, updated_at = ? WHERE id = ?`, state, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("database: set desired state: %w", err)
	}
	return mustAffect(res, id)
}

// VMChange is what vm.update changes. Nil fields are left alone.
type VMChange struct {
	Description *string
	CPUCount    *int
	MemoryBytes *int64
	Config      *models.VMConfig
	// Media, when not nil, replaces the VM's install media.
	Media *[]models.VMDisk
}

// Update applies a change to a VM's configuration.
func (s *VMStore) Update(ctx context.Context, id string, c VMChange, at time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		sets, args := []string{"updated_at = ?"}, []any{formatTime(at)}
		if c.Description != nil {
			sets, args = append(sets, "description = ?"), append(args, *c.Description)
		}
		if c.CPUCount != nil {
			sets, args = append(sets, "cpu_count = ?"), append(args, *c.CPUCount)
		}
		if c.MemoryBytes != nil {
			sets, args = append(sets, "memory_bytes = ?"), append(args, *c.MemoryBytes)
		}
		if c.Config != nil {
			doc, err := json.Marshal(*c.Config)
			if err != nil {
				return fmt.Errorf("database: encode VM config: %w", err)
			}
			sets, args = append(sets, "config = ?"), append(args, string(doc))
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE virtual_machines SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, id)...)
		if err != nil {
			return fmt.Errorf("database: update VM: %w", err)
		}
		if err := mustAffect(res, id); err != nil {
			return err
		}
		if c.Media == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM vm_disks WHERE vm_id = ? AND bus = 'ahci-cd'`, id); err != nil {
			return fmt.Errorf("database: detach media: %w", err)
		}
		for _, m := range *c.Media {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO vm_disks (id, vm_id, slot, bus, read_only, media_name, created_at)
				VALUES (?, ?, ?, 'ahci-cd', 1, ?, ?)`, m.ID, id, m.Slot, m.Media, formatTime(at)); err != nil {
				return fmt.Errorf("database: attach media: %w", err)
			}
		}
		return nil
	})
}

// Delete removes a VM with its disks, NICs, and volume rows. A VM that is
// already gone is not an error.
func (s *VMStore) Delete(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT storage_volume_id FROM vm_disks WHERE vm_id = ? AND storage_volume_id IS NOT NULL`, id)
		if err != nil {
			return fmt.Errorf("database: read volumes: %w", err)
		}
		var volumes []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			volumes = append(volumes, v)
		}
		if err := closeRows(rows); err != nil {
			return err
		}
		// Disks and NICs go with the VM (ON DELETE CASCADE); the volumes
		// they pointed at go after them.
		if _, err := tx.ExecContext(ctx, `DELETE FROM virtual_machines WHERE id = ?`, id); err != nil {
			return fmt.Errorf("database: delete VM: %w", err)
		}
		for _, v := range volumes {
			if _, err := tx.ExecContext(ctx, `DELETE FROM storage_volumes WHERE id = ?`, v); err != nil {
				return fmt.Errorf("database: delete volume: %w", err)
			}
		}
		return nil
	})
}

// Observation is what the agent saw of a VM on the host.
type Observation struct {
	State  string
	Detail string
	// PID is the bhyve process, 0 when there is none.
	PID int
	// Taps maps a NIC slot to its tap interface.
	Taps map[int]string
	At   time.Time
}

// SaveObserved records what the agent saw. It writes observed columns
// only, and nothing when the VM no longer exists.
func (s *VMStore) SaveObserved(ctx context.Context, id string, o Observation) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var pid any
		if o.PID > 0 {
			pid = o.PID
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE virtual_machines SET observed_state = ?, observed_detail = ?, observed_pid = ?, observed_at = ?
			WHERE id = ?`, o.State, nullable(o.Detail), pid, formatTime(o.At), id)
		if err != nil {
			return fmt.Errorf("database: save observed VM state: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE vm_networks SET observed_tap = NULL WHERE vm_id = ? AND observed_tap IS NOT NULL`, id); err != nil {
			return fmt.Errorf("database: clear observed taps: %w", err)
		}
		for slot, tap := range o.Taps {
			if _, err := tx.ExecContext(ctx,
				`UPDATE vm_networks SET observed_tap = ? WHERE vm_id = ? AND slot = ?`, tap, id, slot); err != nil {
				return fmt.Errorf("database: save observed tap: %w", err)
			}
		}
		return nil
	})
}

// VolumeObservation is one volume as zfs(8) listed it.
type VolumeObservation struct {
	// Pool and Name identify the volume: Name is relative to the pool.
	Pool, Name      string
	VolSizeBytes    int64
	UsedBytes       int64
	ReferencedBytes int64
}

// SaveVolumes records what was observed of volumes.
func (s *VMStore) SaveVolumes(ctx context.Context, nodeID string, list []VolumeObservation, at time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, v := range list {
			if _, err := tx.ExecContext(ctx, `
				UPDATE storage_volumes SET observed_exists = 1, observed_used_bytes = ?, observed_volsize_bytes = ?,
					observed_referenced_bytes = ?, observed_at = ?
				WHERE name = ? AND pool_id = (SELECT id FROM storage_pools WHERE node_id = ? AND name = ?)`,
				v.UsedBytes, v.VolSizeBytes, v.ReferencedBytes, formatTime(at), v.Name, nodeID, v.Pool); err != nil {
				return fmt.Errorf("database: save observed volume: %w", err)
			}
		}
		return nil
	})
}

// SavePools records what was observed of the allow-listed pools, creating
// the row of a pool on first sight.
func (s *VMStore) SavePools(ctx context.Context, nodeID string, pools []models.StoragePool, at time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, p := range pools {
			id, err := poolID(ctx, tx, nodeID, p.Name)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE storage_pools SET observed_health = ?, observed_size_bytes = ?, observed_allocated_bytes = ?,
					observed_free_bytes = ?, observed_available_bytes = ?, observed_error = ?, observed_at = ?
				WHERE id = ?`,
				nullable(p.Health), p.SizeBytes, p.AllocatedBytes, p.FreeBytes, p.AvailableBytes,
				nullable(p.Error), formatTime(at), id); err != nil {
				return fmt.Errorf("database: save observed pool: %w", err)
			}
		}
		return nil
	})
}

// ActiveMemory returns the memory of the VMs observed running or starting,
// leaving out one VM.
func (s *VMStore) ActiveMemory(ctx context.Context, exceptID string) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT SUM(memory_bytes) FROM virtual_machines
		WHERE observed_state IN ('running', 'starting', 'stopping') AND id <> ?`, exceptID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("database: sum VM memory: %w", err)
	}
	return total.Int64, nil
}
