// Package hypervisor defines the interface every hypervisor engine
// implements. bhyve is the only adapter (ADR 0009 decision 1a); the
// interface must not acquire a method only one engine could implement.
//
// A method existing here does not mean an engine can do it on this host
// right now: Capabilities says what is possible, and every other method
// refuses with the same reason when something is missing.
package hypervisor

import (
	"context"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Hypervisor runs virtual machines with one engine. A VM is described by
// its API model: the adapter derives every name and path from the IDs in
// it.
type Hypervisor interface {
	// Engine is the engine's name, for example "bhyve".
	Engine() string
	// Capabilities reports what the engine can do on this host right now.
	// It changes nothing and loads nothing.
	Capabilities(ctx context.Context) models.EngineCapabilities
	// Validate checks a VM definition against the engine and the host
	// confinement. For a VM that is not provisioned yet it also checks
	// that its disks fit in their pools. The warnings are for the user;
	// they do not refuse the VM.
	Validate(ctx context.Context, vm models.VM) (warnings []string, err error)
	// Create provisions the storage and the persistent state of a VM. It
	// starts nothing.
	Create(ctx context.Context, vm models.VM) (Provisioned, error)
	// Delete removes the storage and the state of a VM that is not
	// running. What does not exist is not an error.
	Delete(ctx context.Context, vm models.VM) (Removed, error)
	// Inspect reports the state of one VM on the host.
	Inspect(ctx context.Context, vmID string) (Observation, error)
	// List reports every VM the engine knows something about on the
	// host, whether or not Skep's database has it.
	List(ctx context.Context) ([]Observation, error)
}

// Provisioned is what Create made.
type Provisioned struct {
	Datasets []hostops.Dataset
}

// Removed is what Delete destroyed.
type Removed struct {
	Datasets []hostops.Dataset
}

// Observation is the state of a VM on the host.
type Observation struct {
	VMID string
	// State is one of the models.VM* states.
	State  string
	Detail string
	// PID is the engine's process for the VM, 0 when there is none.
	PID int
	// Taps maps a NIC slot to the host interface that carries it.
	Taps map[int]string
	// Active is true while anything of the VM exists on the host: a
	// runner process or an instance. A VM that is not active can be
	// updated and deleted.
	Active bool
}
