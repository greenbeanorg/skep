package vm

import (
	"crypto/rand"
	"fmt"
	"net"
	"regexp"
	"sort"
	"unicode/utf8"

	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// nameRE is the shape of a VM name (ADR 0009 decision 9).
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

const mib = 1 << 20

func invalidf(format string, args ...any) error {
	return models.Invalid(fmt.Sprintf(format, args...))
}

func checkDescription(s string) error {
	if len(s) > models.MaxVMDescription || !utf8.ValidString(s) {
		return invalidf("the description must be at most %d bytes of UTF-8", models.MaxVMDescription)
	}
	for _, r := range s {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return invalidf("the description must not contain control characters")
		}
	}
	return nil
}

func checkCPUAndMemory(cpus int, memory int64, topology *models.VMCPUTopology) error {
	if cpus < 1 {
		return invalidf("cpu_count must be at least 1")
	}
	if memory < models.MinVMMemoryBytes || memory%mib != 0 {
		return invalidf("memory must be at least 128 MiB and a multiple of 1 MiB")
	}
	if t := topology; t != nil {
		if t.Sockets < 1 || t.Cores < 1 || t.Threads < 1 || t.Sockets > 1024 || t.Cores > 1024 || t.Threads > 1024 ||
			t.Sockets*t.Cores*t.Threads != cpus {
			return invalidf("config.cpu: sockets x cores x threads must equal cpu_count (%d)", cpus)
		}
	}
	return nil
}

// normalizeMAC checks a MAC address a request supplied: six bytes,
// unicast. It returns the lower-case colon form.
func normalizeMAC(s string) (string, error) {
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		return "", invalidf("mac_address must be six bytes, such as 02:00:00:aa:bb:cc")
	}
	if mac[0]&1 != 0 {
		return "", invalidf("mac_address must be a unicast address")
	}
	if mac.String() == "00:00:00:00:00:00" {
		return "", invalidf("mac_address must not be all zeroes")
	}
	return mac.String(), nil
}

// newMAC generates a locally administered unicast address: 02 followed by
// five random bytes. It is not the OUI bhyve itself uses.
func newMAC() (string, error) {
	b := make(net.HardwareAddr, 6)
	if _, err := rand.Read(b[1:]); err != nil {
		return "", err
	}
	b[0] = 0x02
	return b.String(), nil
}

// normalizeCreate turns a create request into a VM without IDs. Every
// value in the result is typed and bounded; nothing in it is a path.
func normalizeCreate(req models.VMCreate) (models.VM, error) {
	vm := models.VM{
		Name: req.Name, Description: req.Description, Engine: req.Engine, Firmware: req.Firmware,
		CPUCount: req.CPUCount, MemoryBytes: int64(req.Memory),
		DesiredState: models.VMStopped, Observed: models.VMObserved{State: models.VMUnknown},
		Disks: []models.VMDisk{}, NICs: []models.VMNIC{},
	}
	if !nameRE.MatchString(vm.Name) {
		return vm, invalidf("the name must be 1 to 63 characters: a letter or digit, then letters, digits, '.', '_' or '-'")
	}
	if err := checkDescription(vm.Description); err != nil {
		return vm, err
	}
	switch vm.Engine {
	case "":
		vm.Engine = models.EngineBhyve
	case models.EngineBhyve, models.EngineQEMU:
	default:
		return vm, invalidf("engine must be \"bhyve\"")
	}
	switch vm.Firmware {
	case "":
		vm.Firmware = models.FirmwareUEFI
	case models.FirmwareUEFI, models.FirmwareBIOS:
	default:
		return vm, invalidf("firmware must be \"uefi\"")
	}
	if req.Config != nil {
		vm.Config = *req.Config
	}
	if err := checkCPUAndMemory(vm.CPUCount, vm.MemoryBytes, vm.Config.CPU); err != nil {
		return vm, err
	}

	if len(req.Disks) > models.MaxVMDisks {
		return vm, invalidf("a VM has at most %d disks and install media together", models.MaxVMDisks)
	}
	for slot, spec := range req.Disks {
		d, err := normalizeDisk(slot, spec)
		if err != nil {
			return vm, err
		}
		vm.Disks = append(vm.Disks, d)
	}

	if len(req.NICs) > models.MaxVMNICs {
		return vm, invalidf("a VM has at most %d network interfaces", models.MaxVMNICs)
	}
	seen := map[string]bool{}
	for slot, spec := range req.NICs {
		nic := models.VMNIC{Slot: slot, Model: spec.Model, Bridge: spec.Bridge}
		switch nic.Model {
		case "":
			nic.Model = models.NICVirtioNet
		case models.NICVirtioNet, models.NICE1000:
		default:
			return vm, invalidf("nics[%d].model must be \"virtio-net\" or \"e1000\"", slot)
		}
		if !config.ValidInterfaceName(nic.Bridge) {
			return vm, invalidf("nics[%d].bridge must be the name of a bridge, such as bridge0", slot)
		}
		if spec.MACAddress != "" {
			mac, err := normalizeMAC(spec.MACAddress)
			if err != nil {
				return vm, err
			}
			if seen[mac] {
				return vm, invalidf("nics[%d].mac_address is used twice in this request", slot)
			}
			seen[mac], nic.MACAddress = true, mac
		}
		vm.NICs = append(vm.NICs, nic)
	}
	return vm, nil
}

func normalizeDisk(slot int, spec models.VMDiskSpec) (models.VMDisk, error) {
	d := models.VMDisk{Slot: slot, Bus: spec.Bus}
	if spec.Media != "" || spec.Bus == models.BusAHCICD {
		if d.Bus == "" {
			d.Bus = models.BusAHCICD
		}
		if d.Bus != models.BusAHCICD {
			return d, invalidf("disks[%d]: install media uses the bus \"ahci-cd\"", slot)
		}
		if spec.Size != 0 || spec.Pool != "" || spec.Thin || spec.VolBlockSize != 0 {
			return d, invalidf("disks[%d]: install media has a media name and no size, pool, or volume options", slot)
		}
		if !hostops.ValidMediaName(spec.Media) {
			return d, invalidf("disks[%d].media must be the name of an ISO file in the media directory", slot)
		}
		d.Media, d.ReadOnly = spec.Media, true
		return d, nil
	}
	switch d.Bus {
	case "":
		d.Bus = models.BusVirtioBlk
	case models.BusVirtioBlk, models.BusNVMe, models.BusAHCIHD:
	default:
		return d, invalidf("disks[%d].bus must be \"virtio-blk\", \"nvme\", \"ahci-hd\", or \"ahci-cd\"", slot)
	}
	if !config.ValidPoolName(spec.Pool) {
		return d, invalidf("disks[%d].pool must name a storage pool", slot)
	}
	size := int64(spec.Size)
	if size < models.MinDiskBytes || size%mib != 0 {
		return d, invalidf("disks[%d].size must be at least 16 MiB and a multiple of 1 MiB", slot)
	}
	block := int64(spec.VolBlockSize)
	if block == 0 {
		block = models.DefaultVolBlockSize
	}
	if block < 4<<10 || block > 128<<10 || block&(block-1) != 0 {
		return d, invalidf("disks[%d].volblocksize must be a power of two from 4K to 128K", slot)
	}
	d.Pool, d.SizeBytes, d.Thin, d.VolBlockSize = spec.Pool, size, spec.Thin, int(block)
	return d, nil
}

// diskPlan is what an update does to a VM's zvol-backed disks (ADR 0010
// decision 4), in the order the task does it: remove, grow, add.
type diskPlan struct {
	Remove []models.VMDisk
	Grow   []diskGrow
	Add    []models.VMDisk
}

type diskGrow struct {
	Disk     models.VMDisk
	NewBytes int64
}

func (p diskPlan) empty() bool { return len(p.Remove)+len(p.Grow)+len(p.Add) == 0 }

// planDisks validates the disk changes of an update against the VM and
// returns the VM's disks as they will be, and the plan. New disks have no
// ID yet.
func planDisks(vm models.VM, upd models.VMUpdate) ([]models.VMDisk, diskPlan, error) {
	var plan diskPlan
	bySlot := map[int]models.VMDisk{}
	used := map[int]bool{}
	for _, d := range vm.Disks {
		used[d.Slot] = true
		if d.Bus != models.BusAHCICD {
			bySlot[d.Slot] = d
		}
	}
	removed := map[int]bool{}
	for i, slot := range upd.RemoveDisks {
		d, ok := bySlot[slot]
		switch {
		case !ok:
			return nil, plan, invalidf("remove_disks[%d]: the VM has no disk in slot %d", i, slot)
		case removed[slot]:
			return nil, plan, invalidf("remove_disks[%d]: slot %d is listed twice", i, slot)
		}
		removed[slot] = true
		plan.Remove = append(plan.Remove, d)
	}
	grown := map[int]bool{}
	for i, r := range upd.ResizeDisks {
		d, ok := bySlot[r.Slot]
		size := int64(r.Size)
		switch {
		case !ok:
			return nil, plan, invalidf("resize_disks[%d]: the VM has no disk in slot %d", i, r.Slot)
		case removed[r.Slot]:
			return nil, plan, invalidf("resize_disks[%d]: slot %d is also being removed", i, r.Slot)
		case grown[r.Slot]:
			return nil, plan, invalidf("resize_disks[%d]: slot %d is listed twice", i, r.Slot)
		case size <= d.SizeBytes:
			return nil, plan, invalidf("resize_disks[%d]: a disk only grows; slot %d has %d bytes", i, r.Slot, d.SizeBytes)
		case size%mib != 0 || (d.VolBlockSize > 0 && size%int64(d.VolBlockSize) != 0):
			return nil, plan, invalidf("resize_disks[%d].size must be a multiple of 1 MiB and of the disk's block size", i)
		}
		grown[r.Slot] = true
		plan.Grow = append(plan.Grow, diskGrow{Disk: d, NewBytes: size})
	}
	if len(vm.Disks)-len(plan.Remove)+len(upd.AddDisks) > models.MaxVMDisks {
		return nil, plan, invalidf("a VM has at most %d disks and install media together", models.MaxVMDisks)
	}
	// New disks take the lowest slots nobody uses now. A slot freed in
	// the same update is not reused by it.
	next := 0
	for i, spec := range upd.AddDisks {
		for used[next] {
			next++
		}
		if next > hostops.MaxSlot {
			return nil, plan, invalidf("add_disks[%d]: no free slot is left", i)
		}
		if spec.Media != "" || spec.Bus == models.BusAHCICD {
			return nil, plan, invalidf("add_disks[%d]: install media is attached with media, not add_disks", i)
		}
		d, err := normalizeDisk(next, spec)
		if err != nil {
			return nil, plan, invalidf("add_disks[%d]: %s", i, err.(*models.Error).Message)
		}
		used[next] = true
		plan.Add = append(plan.Add, d)
	}
	disks := []models.VMDisk{}
	for _, d := range vm.Disks {
		if removed[d.Slot] {
			continue
		}
		for _, g := range plan.Grow {
			if g.Disk.Slot == d.Slot {
				d.SizeBytes = g.NewBytes
			}
		}
		disks = append(disks, d)
	}
	disks = append(disks, plan.Add...)
	sort.Slice(disks, func(i, j int) bool { return disks[i].Slot < disks[j].Slot })
	return disks, plan, nil
}

// applyUpdate returns the VM as an update would leave it, the names of
// the fields that change, and what happens to its disks. NICs are fixed.
func applyUpdate(vm models.VM, upd models.VMUpdate) (models.VM, []string, diskPlan, error) {
	var changed []string
	disks, plan, err := planDisks(vm, upd)
	if err != nil {
		return vm, nil, plan, err
	}
	vm.Disks = disks
	if len(plan.Add) > 0 {
		changed = append(changed, "disks_added")
	}
	if len(plan.Grow) > 0 {
		changed = append(changed, "disks_grown")
	}
	if len(plan.Remove) > 0 {
		changed = append(changed, "disks_removed")
	}
	if upd.Description != nil && *upd.Description != vm.Description {
		if err := checkDescription(*upd.Description); err != nil {
			return vm, nil, plan, err
		}
		vm.Description, changed = *upd.Description, append(changed, "description")
	}
	if upd.CPUCount != nil && *upd.CPUCount != vm.CPUCount {
		vm.CPUCount, changed = *upd.CPUCount, append(changed, "cpu_count")
		if upd.Config == nil && vm.Config.CPU != nil {
			// The old topology no longer multiplies to the new count.
			vm.Config.CPU = nil
			changed = append(changed, "config")
		}
	}
	if upd.Memory != nil && int64(*upd.Memory) != vm.MemoryBytes {
		vm.MemoryBytes, changed = int64(*upd.Memory), append(changed, "memory")
	}
	if upd.Config != nil {
		vm.Config, changed = *upd.Config, append(changed, "config")
	}
	if err := checkCPUAndMemory(vm.CPUCount, vm.MemoryBytes, vm.Config.CPU); err != nil {
		return vm, nil, plan, err
	}
	if upd.Media != nil {
		disks := []models.VMDisk{}
		next := 0
		for _, d := range vm.Disks {
			if d.Bus != models.BusAHCICD {
				disks = append(disks, d)
				if d.Slot >= next {
					next = d.Slot + 1
				}
			}
		}
		if len(disks)+len(*upd.Media) > models.MaxVMDisks {
			return vm, nil, plan, invalidf("a VM has at most %d disks and install media together", models.MaxVMDisks)
		}
		seen := map[string]bool{}
		for i, name := range *upd.Media {
			if !hostops.ValidMediaName(name) {
				return vm, nil, plan, invalidf("media[%d] must be the name of an ISO file in the media directory", i)
			}
			if seen[name] {
				return vm, nil, plan, invalidf("media[%d] is attached twice", i)
			}
			seen[name] = true
			disks = append(disks, models.VMDisk{Slot: next + i, Bus: models.BusAHCICD, ReadOnly: true, Media: name})
		}
		vm.Disks, changed = disks, append(changed, "media")
	}
	return vm, changed, plan, nil
}

// media returns the install media of a VM.
func media(vm models.VM) []models.VMDisk {
	out := []models.VMDisk{}
	for _, d := range vm.Disks {
		if d.Bus == models.BusAHCICD {
			out = append(out, d)
		}
	}
	return out
}
