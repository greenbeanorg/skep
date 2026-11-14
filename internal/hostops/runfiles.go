package hostops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The fixed files of a VM's runtime directory, under RunDir (gone after a
// reboot), and of its persistent directory, under StateDir. No other
// name is ever written there, and none comes from a request.
const (
	// RunSpecFile is what the agent asks the runner to run.
	RunSpecFile = "spec.json"
	// RunStateFile is what the runner reports: a RunState.
	RunStateFile = "state.json"
	// RunnerPIDFile holds the runner's process ID.
	RunnerPIDFile = "runner.pid"
	// RunnerLogFile is the runner's own standard error.
	RunnerLogFile = "runner.log"

	// BhyveConfigFile is the bhyve_config(5) file of the last start.
	BhyveConfigFile = "bhyve.conf"
	// BhyveLogFile is bhyve's output since the runner last started it.
	BhyveLogFile = "bhyve.log"
)

var runFiles = map[string]bool{RunSpecFile: true, RunStateFile: true, RunnerPIDFile: true, RunnerLogFile: true}
var stateFiles = map[string]bool{BhyveConfigFile: true, BhyveLogFile: true, VMVarsFile: true}

// RunState is what a runner reports about its VM in RunStateFile. The
// runner is its only writer; the agent reads it.
type RunState struct {
	// State is starting, running, stopping, stopped, or failed.
	State  string    `json:"state"`
	Detail string    `json:"detail,omitempty"`
	Since  time.Time `json:"since"`
	// RunnerPID is the runner; BhyvePID is bhyve while it runs.
	RunnerPID int `json:"runner_pid"`
	BhyvePID  int `json:"bhyve_pid,omitempty"`
	// Taps maps a NIC slot to its tap.
	Taps map[int]string `json:"taps,omitempty"`
	// Boots counts the times bhyve was started in this run: 1, then one
	// more for every guest reboot.
	Boots int `json:"boots"`
	// ExitStatus is bhyve's last exit status.
	ExitStatus *int `json:"exit_status,omitempty"`
	// StopRequested is set when the agent asked for a stop.
	StopRequested bool `json:"stop_requested,omitempty"`
	// Final is set by the runner's last write: the run is over.
	Final bool `json:"final,omitempty"`
}

// ReadRunState reads the RunState of a VM. ok is false when there is none.
func ReadRunState(f Files, vmID string) (st RunState, ok bool, err error) {
	raw, err := f.ReadRunFile(vmID, RunStateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return RunState{}, false, nil
	}
	if err != nil {
		return RunState{}, false, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return RunState{}, false, fmt.Errorf("hostops: the run state of VM %s is not valid: %w", vmID, err)
	}
	return st, true, nil
}

// WriteRunState writes the RunState of a VM.
func WriteRunState(f Files, vmID string, st RunState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return f.WriteRunFile(vmID, RunStateFile, raw)
}

func (h *host) runFile(vmID, name string) (string, error) {
	if !runFiles[name] {
		return "", fmt.Errorf("hostops: %q is not a runtime file", name)
	}
	dir, err := h.conf.VMRunDir(vmID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// StateFilePath implements Files.
func (h *host) StateFilePath(vmID, name string) (string, error) {
	if !stateFiles[name] {
		return "", fmt.Errorf("hostops: %q is not a VM state file", name)
	}
	dir, err := h.conf.VMStateDir(vmID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// PrepareRun implements Files.
func (h *host) PrepareRun(_ context.Context, vmID string) error {
	dir, err := h.conf.VMRunDir(vmID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.conf.RunDir, 0o700); err != nil {
		return fmt.Errorf("hostops: create the VM runtime directory: %w", err)
	}
	if err := mkdirDurable(dir); err != nil {
		return fmt.Errorf("hostops: create the VM runtime directory: %w", err)
	}
	return nil
}

// writeAtomic writes a root-only file so that, after a crash or a hard
// reset, the file holds either its old content or the new one, never
// nothing: the data goes to a temporary file, which is fsynced, renamed
// over the old one, and the directory is fsynced so the rename itself is
// on disk. A reader never sees half a file.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir fsyncs a directory, which makes the creation, renaming, or
// removal of an entry in it durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// mkdirDurable creates a directory, if it does not exist, and makes its
// entry in the parent durable.
func mkdirDurable(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return syncDir(filepath.Dir(dir))
}

// WriteRunFile implements Files.
func (h *host) WriteRunFile(vmID, name string, data []byte) error {
	path, err := h.runFile(vmID, name)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, data); err != nil {
		return fmt.Errorf("hostops: write %s of VM %s: %w", name, vmID, err)
	}
	return nil
}

// maxRunFile bounds what is read of a runtime file.
const maxRunFile = 1 << 20

// ReadRunFile implements Files.
func (h *host) ReadRunFile(vmID, name string) ([]byte, error) {
	path, err := h.runFile(vmID, name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxRunFile))
}

// RemoveRunFile implements Files.
func (h *host) RemoveRunFile(vmID, name string) error {
	path, err := h.runFile(vmID, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("hostops: remove %s of VM %s: %w", name, vmID, err)
	}
	return nil
}

// RunVMs implements Files.
func (h *host) RunVMs() ([]string, error) {
	entries, err := os.ReadDir(h.conf.RunDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hostops: read the VM runtime directory: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// WriteStateFile implements Files.
func (h *host) WriteStateFile(vmID, name string, data []byte) error {
	path, err := h.StateFilePath(vmID, name)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, data); err != nil {
		return fmt.Errorf("hostops: write %s of VM %s: %w", name, vmID, err)
	}
	return nil
}

// LogTail implements Files.
func (h *host) LogTail(vmID string, maxBytes int) string {
	path, err := h.StateFilePath(vmID, BhyveLogFile)
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > int64(maxBytes) {
		_, _ = f.Seek(fi.Size()-int64(maxBytes), io.SeekStart)
	}
	raw, _ := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
	return string(raw)
}
