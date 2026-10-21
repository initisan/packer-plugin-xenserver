// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"
	"strings"

	"xenapi"
)

// firstVGPUDevice is the device passed to VGPU.create. XAPI reads "0" as "take the next
// free device", so a clone that inherited a vGPU puts the build's on a later slot.
const firstVGPUDevice = "0"

// AttachedVGPU records the vGPU an attach put on the VM, with the type and GPU group it
// came from.
type AttachedVGPU struct {
	VGPU     xenapi.VGPURef
	VGPUType xenapi.VGPUTypeRef
	GPUGroup xenapi.GPUGroupRef
}

// vgpuAPI is the slice of XenAPI the vGPU attach drives. It is an interface only so the
// attach can be tested -- the SDK's functions are bound to a live session and CI has no
// GPU-bearing pool. Production code always uses xapiVGPU.
type vgpuAPI interface {
	// AllTypes returns every vGPU type record the pool holds.
	AllTypes() ([]xenapi.VGPUTypeRef, error)
	// ModelName reports VGPU_type.model_name, for example "GRID T4-2Q".
	ModelName(vgpuType xenapi.VGPUTypeRef) (string, error)
	// EnabledGroups reports VGPU_type.enabled_on_GPU_groups: the groups that can
	// actually hand out a vGPU of this type.
	EnabledGroups(vgpuType xenapi.VGPUTypeRef) ([]xenapi.GPUGroupRef, error)
	// Create attaches a vGPU of the type, taken from the group, to the VM.
	Create(group xenapi.GPUGroupRef, vgpuType xenapi.VGPUTypeRef) (xenapi.VGPURef, error)
}

// xapiVGPU drives one VM's vGPU attach over an established XenAPI session.
type xapiVGPU struct {
	session *xenapi.Session
	vm      xenapi.VMRef
}

func (v xapiVGPU) AllTypes() ([]xenapi.VGPUTypeRef, error) {
	return xenapi.VGPUType.GetAll(v.session)
}

func (v xapiVGPU) ModelName(vgpuType xenapi.VGPUTypeRef) (string, error) {
	return xenapi.VGPUType.GetModelName(v.session, vgpuType)
}

func (v xapiVGPU) EnabledGroups(vgpuType xenapi.VGPUTypeRef) ([]xenapi.GPUGroupRef, error) {
	return xenapi.VGPUType.GetEnabledOnGPUGroups(v.session, vgpuType)
}

func (v xapiVGPU) Create(group xenapi.GPUGroupRef, vgpuType xenapi.VGPUTypeRef) (xenapi.VGPURef, error) {
	return xenapi.VGPU.Create(v.session, v.vm, group, firstVGPUDevice, nil, vgpuType)
}

// AttachVGPU adds a vGPU of the profile the build asked for; it does not reconcile, so a
// clone that inherited a vGPU from its source can end up with two. profile is matched
// against VGPU_type.model_name as a case-insensitive substring, so "t4-2q" selects
// "GRID T4-2Q", and the first match wins. say receives the progress messages.
//
// Every failure is fatal, which is the point: the attach used to match a profile, find
// no GPU group with that type enabled and carry on in silence, so the build reported
// success and handed back a VM with no vGPU (CA-430134).
func AttachVGPU(c *Connection, vm xenapi.VMRef, profile string, say func(string)) (AttachedVGPU, error) {
	return attachVGPU(xapiVGPU{session: c.GetSession(), vm: vm}, profile, say)
}

func attachVGPU(api vgpuAPI, profile string, say func(string)) (AttachedVGPU, error) {
	vgpuType, modelName, err := findVGPUType(api, profile)
	if err != nil {
		return AttachedVGPU{}, err
	}
	say(fmt.Sprintf("Found profile with name: %s, ref: %s", modelName, string(vgpuType)))

	groups, err := api.EnabledGroups(vgpuType)
	if err != nil {
		return AttachedVGPU{}, fmt.Errorf("could not read the GPU groups that have the vGPU type %q enabled: %w", modelName, err)
	}

	// A type record survives on its own: XAPI keeps one per model the pool has ever
	// seen, so the name can match with the physical GPU absent or the type disabled
	// on the group holding it. No vGPU can be created either way.
	if len(groups) == 0 {
		return AttachedVGPU{}, fmt.Errorf(
			"the vGPU type %q matches the requested vgpu_profile %q, but no GPU group has that type enabled, "+
				"so no vGPU can be attached; enable the type on a GPU group that holds a physical GPU supporting it, "+
				"or ask for a vgpu_profile the pool can serve",
			modelName, profile)
	}

	// First enabled group wins: the build does not rank them by
	// GPU_group.get_remaining_capacity, so a full group fails only when the VM starts.
	group := groups[0]

	say("Attaching vGPU to VM")
	vgpu, err := api.Create(group, vgpuType)
	if err != nil {
		return AttachedVGPU{}, fmt.Errorf("could not attach a vGPU of type %q to the VM: %w", modelName, err)
	}

	return AttachedVGPU{VGPU: vgpu, VGPUType: vgpuType, GPUGroup: group}, nil
}

// findVGPUType returns the first vGPU type whose model name contains profile, and that
// model name.
func findVGPUType(api vgpuAPI, profile string) (xenapi.VGPUTypeRef, string, error) {
	types, err := api.AllTypes()
	if err != nil {
		return "", "", fmt.Errorf("could not list the pool's vGPU types: %w", err)
	}

	want := strings.ToLower(profile)
	for _, vgpuType := range types {
		modelName, err := api.ModelName(vgpuType)
		if err != nil {
			return "", "", fmt.Errorf("could not read a vGPU type's model name: %w", err)
		}

		if strings.Contains(strings.ToLower(modelName), want) {
			return vgpuType, modelName, nil
		}
	}

	return "", "", fmt.Errorf("no vGPU type on this pool has a model name matching the requested vgpu_profile %q", profile)
}
