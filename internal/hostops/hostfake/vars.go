package hostfake

import (
	"os"
	"path/filepath"

	"github.com/greenbeanorg/skep/internal/hostops"
)

// writeEmptyVars creates the VM's state directory with an empty variable
// store, so the real PrepareVM finds one and copies no firmware file.
func writeEmptyVars(conf hostops.Confinement, vmID string) error {
	path, err := conf.VMVarsPath(vmID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}
