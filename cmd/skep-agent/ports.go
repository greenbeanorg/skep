package main

import (
	"context"
	"log/slog"

	"github.com/greenbeanorg/skep/internal/containers"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hypervisor/bhyve"
	"github.com/greenbeanorg/skep/internal/portcheck"
	"github.com/greenbeanorg/skep/pkg/models"
)

// containerLister and vmLister page through the jails and VMs.
type containerLister interface {
	Containers(ctx context.Context, opts models.ContainerListOptions) (models.ContainerList, error)
}

type vmLister interface {
	VMs(ctx context.Context, opts models.VMListOptions) (models.VMList, error)
}

// runningPorts lists the NICs of every running jail and VM with the
// filter each must carry (ADR 0011 decision 2): from the NIC as recorded,
// the same derivation as at start.
func runningPorts(cs containerLister, vs vmLister, log *slog.Logger) portcheck.PortLister {
	return func(ctx context.Context) ([]portcheck.Port, error) {
		var ports []portcheck.Port
		copts := models.ContainerListOptions{Limit: 200}
		for {
			page, err := cs.Containers(ctx, copts)
			if err != nil {
				return nil, err
			}
			for _, c := range page.Items {
				if c.State != models.ContainerRunning {
					continue
				}
				for _, n := range containers.JailNICs(c) {
					params, err := n.FilterParams()
					if err != nil {
						log.Error("portcheck: a running jail's NIC has no valid filter parameters", "container_id", c.ID, "slot", n.Slot, "error", err)
						continue
					}
					ports = append(ports, portcheck.Port{Kind: hostops.PortJail, OwnerID: c.ID, Slot: n.Slot, Bridge: n.Bridge, Params: params})
				}
			}
			if page.NextCursor == "" {
				break
			}
			copts.Cursor = page.NextCursor
		}
		vopts := models.VMListOptions{Limit: 200}
		for {
			page, err := vs.VMs(ctx, vopts)
			if err != nil {
				return nil, err
			}
			for _, vm := range page.Items {
				if vm.Observed.State != models.VMRunning {
					continue
				}
				for _, n := range vm.NICs {
					params, err := bhyve.VMFilterParams(n.MACAddress)
					if err != nil {
						log.Error("portcheck: a running VM's NIC has no valid filter parameters", "vm_id", vm.ID, "slot", n.Slot, "error", err)
						continue
					}
					ports = append(ports, portcheck.Port{Kind: hostops.PortVM, OwnerID: vm.ID, Slot: n.Slot, Bridge: n.Bridge, Params: params})
				}
			}
			if page.NextCursor == "" {
				break
			}
			vopts.Cursor = page.NextCursor
		}
		return ports, nil
	}
}
