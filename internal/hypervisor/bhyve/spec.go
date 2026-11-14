package bhyve

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Spec is what the agent asks a runner to run, written to RunSpecFile in
// the VM's root-only runtime directory. Every path in it was derived by
// the agent from IDs and the confinement; nothing in it came from a
// request as a path.
type Spec struct {
	// Conf is the agent's host confinement, so the runner applies the
	// same rules without reading the agent's configuration.
	Conf        hostops.Confinement   `json:"confinement"`
	VMID        string                `json:"vm_id"`
	Name        string                `json:"name"`
	CPUs        int                   `json:"cpus"`
	Topology    *models.VMCPUTopology `json:"topology,omitempty"`
	MemoryBytes int64                 `json:"memory_bytes"`
	Disks       []SpecDisk            `json:"disks"`
	NICs        []SpecNIC             `json:"nics"`
}

// SpecDisk is a disk or an install medium with its backing path.
type SpecDisk struct {
	Slot     int    `json:"slot"`
	Bus      string `json:"bus"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
}

// SpecNIC is a network interface; its tap is created by the runner.
type SpecNIC struct {
	Slot   int    `json:"slot"`
	Model  string `json:"model"`
	MAC    string `json:"mac"`
	Bridge string `json:"bridge"`
}

// PCI slots on bus 0: the host bridge at 0, disks and media from 2, NICs
// from 10, the LPC bridge at 31.
const (
	pciHostBridge = 0
	pciFirstDisk  = 2
	pciFirstNIC   = 10
	pciLPC        = 31
)

// ConsoleDevice is one side of a VM's serial console pair.
func ConsoleDevice(vmID string, side byte) (string, error) { return hostops.ConsoleDevice(vmID, side) }

// Render returns the bhyve_config(5) file of a VM, with the taps the
// runner created for its NICs. Keys are emitted in a fixed order.
func Render(spec Spec, taps map[int]string, firmware, vars string) (string, error) {
	instance, err := hostops.Instance(spec.VMID)
	if err != nil {
		return "", err
	}
	if spec.CPUs < 1 || spec.MemoryBytes < models.MinVMMemoryBytes || spec.MemoryBytes%(1<<20) != 0 {
		return "", fmt.Errorf("bhyve: invalid CPU count or memory size")
	}
	com1, err := ConsoleDevice(spec.VMID, 'A')
	if err != nil {
		return "", err
	}
	var lines []string
	set := func(key, value string) { lines = append(lines, key+"="+value) }
	set("name", instance)
	set("uuid", spec.VMID)
	set("cpus", strconv.Itoa(spec.CPUs))
	if t := spec.Topology; t != nil {
		set("sockets", strconv.Itoa(t.Sockets))
		set("cores", strconv.Itoa(t.Cores))
		set("threads", strconv.Itoa(t.Threads))
	}
	set("memory.size", strconv.FormatInt(spec.MemoryBytes>>20, 10)+"M")
	set("acpi_tables", "true")
	// The guest's clock is UTC; idle and spinning vCPUs give the host its
	// CPU back; unknown MSRs do not stop a guest (bhyve -u -H -P -w).
	set("rtc.use_localtime", "false")
	set("x86.vmexit_on_hlt", "true")
	set("x86.vmexit_on_pause", "true")
	set("x86.strictmsr", "false")
	set("bootrom", firmware)
	set("bootvars", vars)
	set("lpc.com1.path", com1)
	set(pciNode(pciHostBridge)+".device", "hostbridge")
	set(pciNode(pciLPC)+".device", "lpc")

	disks := append([]SpecDisk(nil), spec.Disks...)
	sort.Slice(disks, func(i, j int) bool { return disks[i].Slot < disks[j].Slot })
	for _, d := range disks {
		if d.Slot < 0 || pciFirstDisk+d.Slot >= pciFirstNIC {
			return "", fmt.Errorf("bhyve: disk slot %d is out of range", d.Slot)
		}
		node := pciNode(pciFirstDisk + d.Slot)
		switch d.Bus {
		case models.BusVirtioBlk, models.BusNVMe:
			set(node+".device", d.Bus)
			set(node+".path", d.Path)
			if d.ReadOnly {
				set(node+".ro", "true")
			}
		case models.BusAHCIHD, models.BusAHCICD:
			set(node+".device", "ahci")
			kind := "hd"
			if d.Bus == models.BusAHCICD {
				kind = "cd"
			}
			set(node+".port.0.type", kind)
			set(node+".port.0.path", d.Path)
			if d.ReadOnly {
				set(node+".port.0.ro", "true")
			}
		default:
			return "", fmt.Errorf("bhyve: unknown disk bus %q", d.Bus)
		}
	}
	nics := append([]SpecNIC(nil), spec.NICs...)
	sort.Slice(nics, func(i, j int) bool { return nics[i].Slot < nics[j].Slot })
	for _, n := range nics {
		if n.Slot < 0 || pciFirstNIC+n.Slot >= pciLPC {
			return "", fmt.Errorf("bhyve: NIC slot %d is out of range", n.Slot)
		}
		tap, ok := taps[n.Slot]
		if !ok {
			return "", fmt.Errorf("bhyve: NIC %d has no tap", n.Slot)
		}
		node := pciNode(pciFirstNIC + n.Slot)
		switch n.Model {
		case models.NICVirtioNet, models.NICE1000:
			set(node+".device", n.Model)
		default:
			return "", fmt.Errorf("bhyve: unknown NIC model %q", n.Model)
		}
		set(node+".backend", tap)
		set(node+".mac", n.MAC)
	}
	for _, l := range lines {
		// One setting per line: a value can never start another.
		if strings.ContainsAny(l, "\n\r\x00") {
			return "", fmt.Errorf("bhyve: a configuration value contains a line break")
		}
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func pciNode(slot int) string { return "pci.0." + strconv.Itoa(slot) + ".0" }
