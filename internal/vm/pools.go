package vm

import (
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// poolTypes records the kind of pool each new disk is in, from the
// agent's configuration, and refuses the volume options that only a ZFS
// pool has (ADR 0010 decision 6). specs[i] is the request for disks[i];
// field names the request's list in error messages.
func (s *Service) poolTypes(disks []models.VMDisk, specs []models.VMDiskSpec, field string) error {
	for i := range disks {
		d := &disks[i]
		if d.Bus == models.BusAHCICD {
			continue
		}
		kind, err := s.host.Conf.PoolKind(d.Pool)
		if err != nil {
			return err
		}
		d.PoolType = kind
		if kind != hostops.PoolKindDirectory {
			continue
		}
		if i < len(specs) && specs[i].VolBlockSize != 0 {
			return invalidf("%s[%d].volblocksize applies to ZFS pools only; %s is a directory pool", field, i, d.Pool)
		}
		d.VolBlockSize = 0
	}
	return nil
}
