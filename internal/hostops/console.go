package hostops

// ConsoleDevice is one side of a VM's nmdm(4) serial console pair, named
// after the vmm instance: bhyve holds A (lpc.com1.path), the agent B.
func ConsoleDevice(vmID string, side byte) (string, error) {
	name, err := Instance(vmID)
	if err != nil {
		return "", err
	}
	if side != 'A' && side != 'B' {
		return "", invalid("an nmdm side is A or B")
	}
	return "/dev/nmdm-" + name + ".1" + string(side), nil
}
