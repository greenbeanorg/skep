package hostops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// The device node of a zvol (ADR 0010 amendment z). On FreeBSD 15.1 a
// zvol created, cloned, or rolled back with volmode=dev sometimes never
// gets its /dev/zvol node; setting volmode to none and back to dev makes
// ZFS create it, without ever exposing the zvol to GEOM. The repair here
// is that and nothing else: it never writes a value other than none and
// dev, and it never repairs a zvol it did not find at volmode=dev.

// Default timings of EnsureVolumeDevice.
const (
	// deviceLateWait is how long a missing node is given to appear on its
	// own before anything is changed.
	deviceLateWait = 5 * time.Second
	// deviceSettleWait is how long a repair round waits for the node.
	deviceSettleWait = 10 * time.Second
	// devicePoll is how often the node is looked at while waiting.
	devicePoll = 100 * time.Millisecond
	// deviceRestoreAttempts bounds the tries of the second set
	// (volmode=dev), which runs even when the caller's context ends.
	deviceRestoreAttempts = 5
	// deviceRestoreBackoff is the pause between those tries.
	deviceRestoreBackoff = time.Second
	// maxDeviceRepairRounds is the most none→dev rounds one call makes.
	maxDeviceRepairRounds = 2
)

// deviceTimings are the waits of EnsureVolumeDevice; tests shorten them.
type deviceTimings struct {
	late, settle, poll, backoff time.Duration
	attempts                    int
}

func defaultDeviceTimings() deviceTimings {
	return deviceTimings{late: deviceLateWait, settle: deviceSettleWait, poll: devicePoll,
		backoff: deviceRestoreBackoff, attempts: deviceRestoreAttempts}
}

// StoppedCheck reports whether the VM that owns a disk is stopped: nil when
// it is, an error saying why not otherwise. EnsureVolumeDevice calls it
// right before each repair round. The caller holds the VM's task, the
// per-VM lock vm.start takes, so the answer cannot change in between.
type StoppedCheck func(ctx context.Context) error

// DeviceResult says what EnsureVolumeDevice found and did.
type DeviceResult struct {
	// Path is the device node.
	Path string
	// Rounds is the number of none→dev repair rounds made; 0 when the
	// node was there, or appeared on its own.
	Rounds int
}

// deviceError is the disk_device_missing error, with the dataset and the
// reason in its details.
func deviceError(dataset, reason, message string, extra map[string]any) error {
	details := map[string]any{"dataset": dataset, "reason": reason}
	for k, v := range extra {
		details[k] = v
	}
	return &models.Error{Code: models.CodeDiskDeviceMissing, Message: message, Details: details}
}

// adminCommand is the command an administrator runs to put a zvol back to
// the mode Skep needs. Skep prints it and never runs it on its own.
func adminCommand(dataset string) string { return "zfs set volmode=dev " + dataset }

// deviceNode reports whether path is a character device. A path that does
// not exist is (false, nil); one that exists as anything else is an error.
func (h *host) deviceNode(path string) (bool, error) {
	mode, err := h.sys.LstatMode(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if mode&fs.ModeCharDevice == 0 || mode&fs.ModeDevice == 0 {
		return false, fmt.Errorf("hostops: %s exists but is not a character device (%v)", path, mode.Type())
	}
	return true, nil
}

// waitDevice looks at path until it is a character device or wait has
// passed. It returns early, with the context's error, when ctx ends.
func (h *host) waitDevice(ctx context.Context, path string, wait time.Duration) (bool, error) {
	deadline := time.Now().Add(wait)
	for {
		ok, err := h.deviceNode(path)
		if ok || err != nil {
			return ok, err
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		t := time.NewTimer(h.dev.poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return false, ctx.Err()
		case <-t.C:
		}
	}
}

// EnsureVolumeDevice implements ZFS.
func (h *host) EnsureVolumeDevice(ctx context.Context, spec VolumeSpec, stopped StoppedCheck) (DeviceResult, error) {
	// The name is derived from the agent's own record (pool, VM ID,
	// slot), inside the allow-listed pool's dataset root.
	name, err := h.conf.DiskDataset(spec.Pool, spec.VMID, spec.Slot)
	if err != nil {
		return DeviceResult{}, err
	}
	res := DeviceResult{Path: "/dev/zvol/" + name}
	if ok, err := h.deviceNode(res.Path); ok || err != nil {
		return res, h.refuseIf(name, "not_a_device", err)
	}
	if ok, err := h.waitDevice(ctx, res.Path, h.dev.late); ok || err != nil {
		if ok {
			h.log.Info("hostops: a zvol device node appeared late", "dataset", name)
		}
		return res, err
	}
	if stopped == nil {
		return res, h.refuse(name, "no_stopped_check", deviceError(name, "no_stopped_check",
			"the device node of "+name+" is missing, and this caller cannot show the VM is stopped", nil))
	}

	// Every condition of a repair, before anything is changed.
	_, d, err := h.ownedVolume(ctx, spec)
	if err != nil {
		return res, h.refuse(name, "volume_check", err)
	}
	if d.Type != "volume" {
		return res, h.refuse(name, "not_a_volume", deviceError(name, "not_a_volume", name+" is not a volume", nil))
	}
	if d.KeyStatus != "" && d.KeyStatus != "available" {
		return res, h.refuse(name, "key_not_available", &models.Error{Code: models.CodeStorageLocked,
			Message: "the encryption key of " + name + " is not loaded; an administrator loads it, Skep does not",
			Details: map[string]any{"dataset": name, "keystatus": d.KeyStatus}})
	}
	switch d.VolMode {
	case "dev":
	case "none":
		return res, h.refuse(name, "volmode_none", deviceError(name, "volmode_none",
			name+" has volmode=none, so it has no device node; Skep does not change it on its own. An administrator who wants this disk usable again runs: "+adminCommand(name),
			map[string]any{"volmode": "none", "command": adminCommand(name)}))
	default:
		return res, h.refuse(name, "volmode_not_dev", deviceError(name, "volmode_not_dev",
			name+" has volmode="+printable(d.VolMode)+", not the dev Skep sets; Skep does not change it on its own. An administrator runs: "+adminCommand(name),
			map[string]any{"volmode": printable(d.VolMode), "command": adminCommand(name)}))
	}

	start := time.Now()
	for round := 1; round <= maxDeviceRepairRounds; round++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := h.checkStopped(ctx, spec.VMID, stopped); err != nil {
			return res, h.refuse(name, "vm_not_stopped", deviceError(name, "vm_not_stopped",
				"the device node of "+name+" is missing, and the virtual machine is not stopped; the next start repairs it",
				map[string]any{"error": err.Error()}))
		}
		res.Rounds = round
		if err := h.toggleVolMode(ctx, name); err != nil {
			h.log.Error("hostops: zvol device repair failed", "dataset", name, "round", round,
				"duration", time.Since(start), "error", err)
			return res, err
		}
		ok, err := h.waitDevice(ctx, res.Path, h.dev.settle)
		if err != nil {
			return res, err
		}
		if ok {
			h.log.Info("hostops: zvol device node repaired", "dataset", name, "rounds", round,
				"duration", time.Since(start))
			return res, nil
		}
	}
	h.log.Error("hostops: zvol device node still missing after repair", "dataset", name,
		"rounds", res.Rounds, "duration", time.Since(start))
	return res, deviceError(name, "repair_failed", fmt.Sprintf(
		"the device node of %s is still missing after %d repairs (volmode none, then dev)", name, res.Rounds),
		map[string]any{"rounds": res.Rounds})
}

// checkStopped is the stopped check of a repair: the caller's, and the
// host's own view that no vmm instance of the VM exists.
func (h *host) checkStopped(ctx context.Context, vmID string, stopped StoppedCheck) error {
	if err := stopped(ctx); err != nil {
		return err
	}
	if h.InstanceExists(vmID) {
		return errors.New("a vmm instance of the virtual machine exists")
	}
	return nil
}

// toggleVolMode sets volmode=none, then volmode=dev. Once the first set
// has been tried, the second is always tried too, with retries and
// whatever happens to ctx: a zvol must never be left at none by Skep.
// (If the agent dies in between, the zvol is at none, and the next
// EnsureVolumeDevice refuses it with the command for the administrator.)
func (h *host) toggleVolMode(ctx context.Context, name string) error {
	_, firstErr := h.run.Run(ctx, zfsBinary, "set", "volmode=none", name)
	restore := context.WithoutCancel(ctx)
	var err error
	for attempt := 1; attempt <= h.dev.attempts; attempt++ {
		if _, err = h.run.Run(restore, zfsBinary, "set", "volmode=dev", name); err == nil {
			break
		}
		h.log.Warn("hostops: setting volmode=dev back failed", "dataset", name, "attempt", attempt, "error", err)
		if attempt < h.dev.attempts {
			time.Sleep(h.dev.backoff)
		}
	}
	if err != nil {
		return deviceError(name, "restore_failed", fmt.Sprintf(
			"setting volmode=dev back on %s failed %d times; it may be left at volmode=none. An administrator runs: %s",
			name, h.dev.attempts, adminCommand(name)), map[string]any{"command": adminCommand(name), "error": err.Error()})
	}
	if firstErr != nil {
		return fmt.Errorf("hostops: zfs set volmode=none %s: %w", name, firstErr)
	}
	return nil
}

// refuse logs a refusal to repair a zvol's device node and returns err.
func (h *host) refuse(name, reason string, err error) error {
	h.log.Warn("hostops: zvol device repair refused", "dataset", name, "reason", reason, "error", err)
	return err
}

// refuseIf is refuse for an error that may be nil.
func (h *host) refuseIf(name, reason string, err error) error {
	if err == nil {
		return nil
	}
	return h.refuse(name, reason, err)
}
