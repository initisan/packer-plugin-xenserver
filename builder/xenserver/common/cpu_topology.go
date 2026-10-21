// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"
	"strconv"

	"xenapi"
)

// coresPerSocketKey is the platform key XenServer divides VCPUs_max by to get the socket
// count.
const coresPerSocketKey = "cores-per-socket"

// maxSockets is the most sockets a VM may have: Xen's own hard limit, which XenCenter
// enforces as a topology validation rule (XenModel VM.cs:115, CA-198276). XAPI does not
// check it -- vm_platform.ml:196-214 checks only that the socket count comes out whole.
const maxSockets = 16

type topologyKind int

// topologyUntouched is the zero value: the VM was never read, so whether it has a topology
// at all is not known here.
const (
	topologyUntouched topologyKind = iota
	topologyKept
	topologyWritten
	topologyReset
	topologyAbsent
)

// TopologyOutcome says what writing the CPU topology actually did, so the caller can tell
// the user something true without reading the VM back.
type TopologyOutcome struct {
	kind      topologyKind
	cores     uint
	vcpus     uint
	inherited string
	// Separate from inherited: a key holding an empty value reads the same as no key.
	found     bool
	effective uint
}

// Describe reports the outcome for the build log. source names the clone_template, which
// is where a kept or a reset topology came from.
func (o TopologyOutcome) Describe(source string) string {
	switch o.kind {
	case topologyWritten:
		return fmt.Sprintf("Set the CPU topology to %d core(s) per socket", o.cores)

	case topologyKept:
		return fmt.Sprintf("Keeping the CPU topology of '%s': %s core(s) per socket", source, o.inherited)

	case topologyAbsent:
		return fmt.Sprintf("'%s' carries no CPU topology of its own, so the VM gets one core per socket", source)

	case topologyReset:
		switch {
		case !o.found:
			return fmt.Sprintf("'%s' carries no CPU topology of its own, and one core per socket would give "+
				"the %d vcpus this build asked for more than %d sockets, so the VM gets %d core(s) per "+
				"socket. Set cores_per_socket to choose the topology.", source, o.vcpus, maxSockets, o.cores)

		case o.effective == 0:
			return fmt.Sprintf("The cores per socket of '%s' is %q, which is not a usable number of cores, so "+
				"the VM gets %d core(s) per socket. Set cores_per_socket to choose the topology.",
				source, o.inherited, o.cores)
		}
		return fmt.Sprintf("The %s cores per socket of '%s' cannot be kept for the %d vcpus this build asked "+
			"for, so the VM gets %d core(s) per socket. Set cores_per_socket to choose the topology.",
			o.inherited, source, o.vcpus, o.cores)
	}
	return fmt.Sprintf("Leaving the CPU topology of '%s' as it is", source)
}

// topologyFits mirrors XenCenter's VM.ValidVCPUConfiguration (XenModel VM.cs:1547-1557):
// the socket count XenServer derives from the pair has to come out whole and stay within
// maxSockets. Both conditions, not just the first: XAPI enforces only the first, so a
// topology that passes it can still be one XenCenter would refuse to build.
func topologyFits(vcpus, coresPerSocket uint) bool {
	return coresPerSocket > 0 && vcpus%coresPerSocket == 0 && vcpus/coresPerSocket <= maxSockets
}

// fallbackCoresPerSocket is the topology to use when the source's cannot be kept: the
// fewest cores per socket that still fits, which is what XenCenter's New VM wizard falls
// back to -- the first entry of a combo box it builds in that order
// (CPUTopologyComboBox.cs:107-126).
//
// That combo box also caps cores per socket at the home server's physical count
// (CPUTopologyComboBox.cs:118), which is not mirrored: no host is chosen yet.
func fallbackCoresPerSocket(vcpus uint) uint {
	if vcpus == 0 {
		return 1
	}

	for cores := uint(1); cores < vcpus; cores++ {
		if topologyFits(vcpus, cores) {
			return cores
		}
	}

	return vcpus
}

// cpuTopologyAPI is the slice of XenAPI the topology handling drives.
//
// It is an interface only so the decision can be tested: the XenAPI SDK exposes
// package-level functions bound to a live session, and CI has no pool to call them
// against. Production code always uses xapiCPUTopology.
type cpuTopologyAPI interface {
	// CoresPerSocket reads platform:cores-per-socket, reporting found=false when the VM
	// has no such key.
	CoresPerSocket() (string, bool, error)
	// SetCoresPerSocket writes platform:cores-per-socket, replacing any value already
	// there.
	SetCoresPerSocket(cores uint) error
}

// xapiCPUTopology drives one VM's platform keys over an established XenAPI session.
type xapiCPUTopology struct {
	session *xenapi.Session
	vm      xenapi.VMRef
}

func (t xapiCPUTopology) CoresPerSocket() (string, bool, error) {
	platform, err := xenapi.VM.GetPlatform(t.session, t.vm)
	if err != nil {
		return "", false, err
	}

	value, found := platform[coresPerSocketKey]

	return value, found, nil
}

func (t xapiCPUTopology) SetCoresPerSocket(cores uint) error {
	return setPlatformKey(t.session, t.vm, coresPerSocketKey, strconv.FormatUint(uint64(cores), 10))
}

// ReconcileCPUTopology gives a freshly cloned VM the CPU topology the build asked for,
// and reports what it had to change.
//
// Both are nil when the build set neither, which means "keep what VM.clone copied from
// clone_template" -- and keeping it means writing nothing at all (CP-314723).
//
// The one case where an unset cores_per_socket still has to be written is a vcpus the
// inherited topology does not fit: XenServer refuses to start a VM whose vcpus its cores
// per socket does not divide (vm_platform.ml:196-214), and the build would fail hours
// later having produced nothing. Falling back to the fewest cores per socket that does
// fit is what XenCenter's New VM wizard does in exactly this case
// (CPUTopologyComboBox.cs:80-126).
//
// Call it while the VM is still halted, and after VCPUs_max has been written.
func ReconcileCPUTopology(c *Connection, vm xenapi.VMRef, vcpus, coresPerSocket *uint) (TopologyOutcome, error) {
	return reconcileCPUTopology(xapiCPUTopology{session: c.GetSession(), vm: vm}, vcpus, coresPerSocket)
}

func reconcileCPUTopology(api cpuTopologyAPI, vcpus, coresPerSocket *uint) (TopologyOutcome, error) {
	// cores_per_socket alone is ignored, as Prepare warns: XenServer derives the socket
	// count from the pair, so writing one over an inherited vcpus it does not divide
	// gives the very VM this function exists to avoid.
	if vcpus == nil {
		return TopologyOutcome{}, nil
	}

	if coresPerSocket != nil {
		if err := api.SetCoresPerSocket(*coresPerSocket); err != nil {
			return TopologyOutcome{}, fmt.Errorf("could not set the VM's cores per socket to %d: %w",
				*coresPerSocket, err)
		}

		return TopologyOutcome{kind: topologyWritten, cores: *coresPerSocket}, nil
	}

	inherited, found, err := api.CoresPerSocket()
	if err != nil {
		return TopologyOutcome{}, fmt.Errorf("could not read the VM's CPU topology: %w", err)
	}

	// A VM with no key of its own runs one core per socket, which XenServer applies
	// without storing it. A value it cannot parse is refused at start with INVALID_VALUE
	// rather than VCPU_MAX_NOT_CORES_PER_SOCKET_MULTIPLE, so it is replaced the same way.
	effective := uint(1)
	if found {
		cores, err := strconv.ParseUint(inherited, 10, 32)
		if err != nil {
			effective = 0
		} else {
			effective = uint(cores)
		}
	}

	if topologyFits(*vcpus, effective) {
		if !found {
			return TopologyOutcome{kind: topologyAbsent}, nil
		}
		return TopologyOutcome{kind: topologyKept, inherited: inherited}, nil
	}

	cores := fallbackCoresPerSocket(*vcpus)
	if err := api.SetCoresPerSocket(cores); err != nil {
		return TopologyOutcome{}, fmt.Errorf("could not set the VM's cores per socket to %d: %w", cores, err)
	}

	return TopologyOutcome{
		kind: topologyReset, cores: cores, vcpus: *vcpus,
		inherited: inherited, found: found, effective: effective,
	}, nil
}
