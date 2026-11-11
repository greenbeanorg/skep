package hardware

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/greenbeanorg/skep/pkg/models"
)

// qemuBinaries are the QEMU executables discovery looks for.
var qemuBinaries = []string{"qemu-system-x86_64", "qemu-system-aarch64", "qemu-img"}

var qemuVersionRE = regexp.MustCompile(`(?m)^(?:QEMU emulator|qemu-img) version (\d+\.\d+(?:\.\d+)?)`)

// parseQEMUVersion extracts the version from "<binary> --version" output,
// for example "QEMU emulator version 9.2.0" or "qemu-img version 9.2.0".
func parseQEMUVersion(out []byte) (string, error) {
	m := qemuVersionRE.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unrecognised version output %q", firstLine(out))
	}
	return string(m[1]), nil
}

// discoverQEMU finds QEMU binaries and asks each one for its version.
// Running "--version" is read-only.
func discoverQEMU(ctx context.Context, find pathFinder, run CommandRunner) models.QEMUInfo {
	info := models.QEMUInfo{Binaries: []models.QEMUBinary{}}
	emulators := 0
	for _, name := range qemuBinaries {
		path, ok := find(name)
		if !ok {
			continue
		}
		bin := models.QEMUBinary{Name: name, Path: path}
		out, err := run.Run(ctx, path, "--version")
		if err == nil {
			bin.Version, err = parseQEMUVersion(out)
		}
		if err != nil {
			bin.Error = err.Error()
		}
		if strings.HasPrefix(name, "qemu-system-") {
			emulators++
		}
		info.Binaries = append(info.Binaries, bin)
	}

	switch {
	case emulators > 0:
		info.Status = models.StatusAvailable
		info.Acceleration = "tcg"
		info.Reason = fmt.Sprintf("%d QEMU system emulator(s) found; software emulation (TCG) only, no hardware acceleration", emulators)
	case len(info.Binaries) > 0:
		info.Status = models.StatusUnavailable
		info.Reason = "only QEMU image utilities were found; no qemu-system emulator is installed"
	default:
		info.Status = models.StatusUnavailable
		info.Reason = "no QEMU binaries found in " + strings.Join(searchDirs, ", ")
	}
	return info
}
