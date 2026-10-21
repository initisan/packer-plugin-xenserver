// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"errors"
	"strings"
	"testing"

	"xenapi"
)

// fakeVTPM stands in for one VM's vTPM state, counting every create, destroy and
// declaration clear so a test can assert a matching VM was not written to at all.
type fakeVTPM struct {
	current     []xenapi.VTPMRef
	declared    bool
	listErr     error
	createErr   error
	destroyErr  error
	declaredErr error
	clearErr    error
	created     int
	destroyed   []xenapi.VTPMRef
	cleared     int
}

func (f *fakeVTPM) List() ([]xenapi.VTPMRef, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.current, nil
}

func (f *fakeVTPM) Create() (xenapi.VTPMRef, error) {
	f.created++
	if f.createErr != nil {
		return "", f.createErr
	}
	ref := xenapi.VTPMRef("OpaqueRef:new-vtpm")
	f.current = append(f.current, ref)
	return ref, nil
}

func (f *fakeVTPM) Destroy(ref xenapi.VTPMRef) error {
	f.destroyed = append(f.destroyed, ref)
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.current = nil
	return nil
}

func (f *fakeVTPM) Declared() (bool, error) {
	if f.declaredErr != nil {
		return false, f.declaredErr
	}
	return f.declared, nil
}

func (f *fakeVTPM) ClearDeclaration() error {
	f.cleared++
	if f.clearErr != nil {
		return f.clearErr
	}
	f.declared = false
	return nil
}

// The case the ticket exists for: cloning a Windows Server or Windows 10
// template, which has no vTPM, and asking for one so the image can run
// BitLocker or Credential Guard. VM.clone alone leaves the VM without one.
func TestReconcileVTPM_CreatesOneWhenTheSourceHadNone(t *testing.T) {
	api := &fakeVTPM{}

	outcome, err := reconcileVTPM(api, true)

	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if outcome != VTPMCreated {
		t.Errorf("outcome = %v, want VTPMCreated", outcome)
	}
	if api.created != 1 {
		t.Errorf("created %d vTPMs, want exactly 1", api.created)
	}
}

// The other half of the defect: a clone inherits the source's vTPM, and an
// explicit vtpm = false has to be able to remove it.
func TestReconcileVTPM_DestroysAnInheritedOneWhenAskedTo(t *testing.T) {
	inherited := xenapi.VTPMRef("OpaqueRef:inherited")
	api := &fakeVTPM{current: []xenapi.VTPMRef{inherited}}

	outcome, err := reconcileVTPM(api, false)

	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if outcome != VTPMDestroyed {
		t.Errorf("outcome = %v, want VTPMDestroyed", outcome)
	}
	if len(api.destroyed) != 1 || api.destroyed[0] != inherited {
		t.Errorf("destroyed = %v, want exactly the inherited ref", api.destroyed)
	}
}

// The guard that matters most, for two separate reasons. Creating a second vTPM
// raises VTPM_MAX_AMOUNT_REACHED, and destroying one deletes its secret with no
// way back -- so a VM that already matches must not be written to at all. A
// "just do it every time" implementation passes both tests above and fails this.
func TestReconcileVTPM_LeavesAVMThatAlreadyMatchesAlone(t *testing.T) {
	t.Run("wants one and already has one", func(t *testing.T) {
		api := &fakeVTPM{current: []xenapi.VTPMRef{"OpaqueRef:inherited"}}

		outcome, err := reconcileVTPM(api, true)

		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if outcome != VTPMUnchanged {
			t.Errorf("outcome = %v, want VTPMUnchanged", outcome)
		}
		if api.created != 0 {
			t.Errorf("created %d vTPMs on a VM that already had one", api.created)
		}
		if len(api.destroyed) != 0 {
			t.Errorf("destroyed %v on a VM that already matched", api.destroyed)
		}
	})

	t.Run("wants none and already has none", func(t *testing.T) {
		api := &fakeVTPM{}

		outcome, err := reconcileVTPM(api, false)

		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if outcome != VTPMUnchanged {
			t.Errorf("outcome = %v, want VTPMUnchanged", outcome)
		}
		if len(api.destroyed) != 0 {
			t.Errorf("destroyed %v on a VM that had no vTPM", api.destroyed)
		}
		if api.cleared != 0 {
			t.Error("wrote platform:vtpm on a VM that did not declare one")
		}
	})
}

// CA-431619. Destroying the object is not enough: platform:vtpm comes with the
// clone, and XAPI makes a fresh vTPM from it at the next start.
func TestReconcileVTPM_ClearsTheDeclarationItWouldBeRemadeFrom(t *testing.T) {
	inherited := xenapi.VTPMRef("OpaqueRef:inherited")
	api := &fakeVTPM{current: []xenapi.VTPMRef{inherited}, declared: true}

	outcome, err := reconcileVTPM(api, false)

	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if outcome != VTPMDestroyed {
		t.Errorf("outcome = %v, want VTPMDestroyed", outcome)
	}
	if len(api.destroyed) != 1 {
		t.Errorf("destroyed = %v, want the inherited vTPM", api.destroyed)
	}
	if api.declared {
		t.Error("platform:vtpm survived, so VM.start will hand the VM a fresh vTPM")
	}
}

// The same key with no object to go with it: nothing to destroy, and a VM that
// still boots with a vTPM unless the key goes. VM.clone copies platform without
// copying a vTPM the source does not have.
func TestReconcileVTPM_ClearsADeclarationWithNoVTPMOfItsOwn(t *testing.T) {
	api := &fakeVTPM{declared: true}

	outcome, err := reconcileVTPM(api, false)

	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if outcome != VTPMDeclarationCleared {
		t.Errorf("outcome = %v, want VTPMDeclarationCleared", outcome)
	}
	if len(api.destroyed) != 0 {
		t.Errorf("destroyed %v on a VM that had no vTPM object", api.destroyed)
	}
	if api.cleared != 1 {
		t.Errorf("cleared platform:vtpm %d times, want exactly 1", api.cleared)
	}
}

// A vTPM the build asked for is attached whatever platform:vtpm says, so that
// key is not read on the way in -- and must not be written either.
func TestReconcileVTPM_LeavesTheDeclarationAloneWhenAVTPMIsWanted(t *testing.T) {
	api := &fakeVTPM{declaredErr: errors.New("platform:vtpm must not be read here")}

	if _, err := reconcileVTPM(api, true); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if api.cleared != 0 {
		t.Error("wrote platform:vtpm while adding a vTPM")
	}
}

// The destroy has already landed by the time the key is written, so the failure
// has to reach the user: what is left is a VM that boots with a blank vTPM.
func TestReconcileVTPM_ReportsAFailureToClearTheDeclaration(t *testing.T) {
	clearFailed := errors.New("API error: code 1, message VM_BAD_POWER_STATE")
	api := &fakeVTPM{current: []xenapi.VTPMRef{"OpaqueRef:inherited"}, declared: true, clearErr: clearFailed}

	outcome, err := reconcileVTPM(api, false)

	if err == nil {
		t.Fatal("a vTPM that comes back at start must not be reported as removed")
	}
	if !errors.Is(err, clearFailed) {
		t.Errorf("error should wrap the XAPI failure, got: %s", err)
	}
	if !strings.Contains(err.Error(), "platform:vtpm") {
		t.Errorf("error should name the key that was left behind, got: %s", err)
	}
	if outcome != VTPMDestroyed {
		t.Errorf("outcome = %v, want the destroy that did land reported", outcome)
	}
}

// The platform read is as load-bearing as the vTPM list read below: an unknown
// state must stop the step, not fall through to a blind destroy.
func TestReconcileVTPM_ReportsAFailureToReadTheDeclaration(t *testing.T) {
	readFailed := errors.New("API error: code 1, message SESSION_INVALID")
	api := &fakeVTPM{current: []xenapi.VTPMRef{"OpaqueRef:inherited"}, declaredErr: readFailed}

	outcome, err := reconcileVTPM(api, false)

	if err == nil {
		t.Fatal("a failed read must be reported")
	}
	if !errors.Is(err, readFailed) {
		t.Errorf("error should wrap the XAPI failure, got: %s", err)
	}
	if outcome != VTPMUnchanged {
		t.Errorf("outcome = %v after a failed read", outcome)
	}
	if len(api.destroyed) != 0 {
		t.Error("destroyed a vTPM without knowing whether the VM would make another")
	}
}

// A read failure must stop the step rather than fall through to a blind write:
// the safety property above rests entirely on knowing the current state.
func TestReconcileVTPM_ReportsAReadFailureAndWritesNothing(t *testing.T) {
	readFailed := errors.New("API error: code 1, message SESSION_INVALID")
	api := &fakeVTPM{listErr: readFailed}

	outcome, err := reconcileVTPM(api, true)

	if err == nil {
		t.Fatal("a failed read must be reported")
	}
	if !errors.Is(err, readFailed) {
		t.Errorf("error should wrap the XAPI failure, got: %s", err)
	}
	if !strings.Contains(err.Error(), "vtpm") {
		t.Errorf("error should name the option the user can act on, got: %s", err)
	}
	if outcome != VTPMUnchanged {
		t.Errorf("outcome = %v after a failed read", outcome)
	}
	if api.created != 0 || len(api.destroyed) != 0 {
		t.Error("wrote despite not knowing the current state")
	}
}

// FEATURE_RESTRICTED (the pool's restrict_vtpm gate) and VM_BAD_POWER_STATE both
// surface here, and both have to fail the build rather than pass silently.
func TestReconcileVTPM_ReportsACreateFailure(t *testing.T) {
	createFailed := errors.New("API error: code 1, message FEATURE_RESTRICTED")
	api := &fakeVTPM{createErr: createFailed}

	outcome, err := reconcileVTPM(api, true)

	if err == nil {
		t.Fatal("a failed create must be reported")
	}
	if !errors.Is(err, createFailed) {
		t.Errorf("error should wrap the XAPI failure, got: %s", err)
	}
	if !strings.Contains(err.Error(), "vtpm") {
		t.Errorf("error should name the option the user can act on, got: %s", err)
	}
	if outcome != VTPMUnchanged {
		t.Errorf("outcome = %v after a failed create", outcome)
	}
}

func TestReconcileVTPM_ReportsADestroyFailure(t *testing.T) {
	destroyFailed := errors.New("API error: code 1, message VM_BAD_POWER_STATE")
	api := &fakeVTPM{current: []xenapi.VTPMRef{"OpaqueRef:inherited"}, destroyErr: destroyFailed}

	outcome, err := reconcileVTPM(api, false)

	if err == nil {
		t.Fatal("a failed destroy must be reported")
	}
	if !errors.Is(err, destroyFailed) {
		t.Errorf("error should wrap the XAPI failure, got: %s", err)
	}
	// The destroy did not land, so the caller must not be told the VM changed.
	if outcome != VTPMUnchanged {
		t.Errorf("outcome = %v after a failed destroy", outcome)
	}
}

// CA-431619 turns on reading the key the way XAPI does. Vm_platform.is_true
// lowercases the value and takes "1" as well as "true", so a source spelling it
// that way gets a vTPM manufactured at start like any other -- and vtpm = false
// has to clear it, not walk past it.
func TestPlatformDeclaresVTPM(t *testing.T) {
	for value, want := range map[string]bool{
		"true": true, "True": true, "TRUE": true, "1": true,
		"false": false, "0": false, "": false, "yes": false,
	} {
		if got := platformDeclaresVTPM(map[string]string{"vtpm": value}); got != want {
			t.Errorf("platform:vtpm = %q: got %t, want %t", value, got, want)
		}
	}
	if platformDeclaresVTPM(map[string]string{"nx": "true"}) {
		t.Error("an absent vtpm key is not a declaration")
	}
}
