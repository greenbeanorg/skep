package hostops

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/pkg/models"
)

// tapNameRE is the shape of a tap the kernel names: tap followed by its
// unit number.
var tapNameRE = regexp.MustCompile(`^tap[0-9]{1,5}$`)

// TapDescription is the description Skep gives a VM's tap, so ifconfig
// output can be traced to the VM (ADR 0009 decision 5a).
func TapDescription(vmID string, slot int) string {
	return "skep:" + vmID + ":" + strconv.Itoa(slot)
}

// CreateTap implements Net.
func (h *host) CreateTap(ctx context.Context, vmID string, slot int) (string, error) {
	if err := checkVMID(vmID); err != nil {
		return "", err
	}
	if slot < 0 || slot > MaxSlot {
		return "", invalid("invalid NIC slot")
	}
	out, err := h.run.Run(ctx, ifconfigBinary, "tap", "create")
	if err != nil {
		return "", fmt.Errorf("hostops: create a tap: %w", err)
	}
	tap := strings.TrimSpace(string(out))
	if !tapNameRE.MatchString(tap) {
		return "", fmt.Errorf("hostops: ifconfig created an interface with an unexpected name %q", printable(tap))
	}
	// Brought up by Skep: net.link.tap.up_on_open is 0 by default.
	if _, err := h.run.Run(ctx, ifconfigBinary, tap, "description", TapDescription(vmID, slot), "up"); err != nil {
		if _, derr := h.run.Run(ctx, ifconfigBinary, tap, "destroy"); derr != nil {
			h.log.Error("hostops: could not destroy a tap after a failed setup", "tap", tap, "error", derr)
		}
		return "", fmt.Errorf("hostops: set up %s: %w", tap, err)
	}
	return tap, nil
}

// AddBridgeMember implements Net.
func (h *host) AddBridgeMember(ctx context.Context, bridge, tap string) error {
	if !tapNameRE.MatchString(tap) {
		return invalid("invalid tap name")
	}
	b, err := h.Bridge(ctx, bridge)
	if err != nil {
		return err
	}
	if err := checkBridgeUsable(h.conf, b); err != nil {
		return err
	}
	if _, err := h.run.Run(ctx, ifconfigBinary, bridge, "addm", tap); err != nil {
		return fmt.Errorf("hostops: add %s to %s: %w", tap, bridge, err)
	}
	return nil
}

// checkBridgeUsable refuses a bridge that does not exist or that has a
// management interface among its members (ADR 0009 decision 5a).
func checkBridgeUsable(conf Confinement, b Bridge) error {
	if !b.Exists {
		return &models.Error{Code: models.CodeBridgeNotAllowed,
			Message: "the bridge " + b.Name + " does not exist on this host; an administrator creates it, Skep does not",
			Details: map[string]any{"bridge": b.Name}}
	}
	for _, m := range b.Members {
		if conf.IsManagement(m) {
			return &models.Error{Code: models.CodeBridgeNotAllowed,
				Message: "the bridge " + b.Name + " has the management interface " + m + " as a member; Skep puts no guest on it",
				Details: map[string]any{"bridge": b.Name, "member": m}}
		}
	}
	return nil
}

// CheckBridgeUsable is checkBridgeUsable for callers outside the package.
func CheckBridgeUsable(conf Confinement, b Bridge) error { return checkBridgeUsable(conf, b) }

// tapDescription reads the description of a tap. exists is false when the
// tap is gone.
func (h *host) tapDescription(ctx context.Context, tap string) (desc string, exists bool, err error) {
	out, err := h.run.Run(ctx, ifconfigBinary, tap)
	if err != nil {
		if stderrContains(err, "does not exist") {
			return "", false, nil
		}
		return "", false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "description: "); ok {
			return strings.TrimSpace(rest), true, nil
		}
	}
	return "", true, nil
}

// DestroyTap implements Net.
func (h *host) DestroyTap(ctx context.Context, vmID, tap string) error {
	if err := checkVMID(vmID); err != nil {
		return err
	}
	if !tapNameRE.MatchString(tap) {
		return invalid("invalid tap name")
	}
	desc, exists, err := h.tapDescription(ctx, tap)
	if err != nil {
		return fmt.Errorf("hostops: read %s: %w", tap, err)
	}
	if !exists {
		return nil
	}
	if !strings.HasPrefix(desc, "skep:"+vmID+":") {
		return fmt.Errorf("hostops: refusing to destroy %s: its description %q does not name VM %s", tap, printable(desc), vmID)
	}
	if _, err := h.run.Run(ctx, ifconfigBinary, tap, "destroy"); err != nil && !stderrContains(err, "does not exist") {
		return fmt.Errorf("hostops: destroy %s: %w", tap, err)
	}
	return nil
}

// VMTaps implements Net.
func (h *host) VMTaps(ctx context.Context, vmID string) ([]string, error) {
	if err := checkVMID(vmID); err != nil {
		return nil, err
	}
	// ifconfig -g tap lists the members of the interface group "tap",
	// one per line.
	out, err := h.run.Run(ctx, ifconfigBinary, "-g", "tap")
	if err != nil {
		return nil, fmt.Errorf("hostops: list taps: %w", err)
	}
	var taps []string
	for _, name := range strings.Fields(string(out)) {
		if !tapNameRE.MatchString(name) {
			continue
		}
		desc, exists, err := h.tapDescription(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("hostops: read %s: %w", name, err)
		}
		if exists && strings.HasPrefix(desc, "skep:"+vmID+":") {
			taps = append(taps, name)
		}
	}
	return taps, nil
}
