// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"errors"
	"strings"
	"testing"
)

// fakeSecureBoot stands in for one VM's platform:secureboot. It records every
// write, so a test can tell "wrote the value it already had" from "was left
// alone" -- the distinction the whole thing turns on.
type fakeSecureBoot struct {
	err    error
	writes []string
}

func (f *fakeSecureBoot) Set(value string) error {
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, value)
	return nil
}

// The regression this exists for: a build that says nothing about secure_boot
// must not write it. The value it would otherwise write is "false", which turns
// off the Secure Boot the clone inherited.
func TestReconcileSecureBoot_WritesNothingWhenTheBuildSaidNothing(t *testing.T) {
	api := &fakeSecureBoot{}

	outcome, err := reconcileSecureBoot(api, nil)

	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if outcome != SecureBootKept {
		t.Errorf("outcome = %d, want SecureBootKept", outcome)
	}
	if len(api.writes) != 0 {
		t.Errorf("wrote platform:secureboot %v, want no write at all", api.writes)
	}
}

// "auto", not "true": XAPI resolves it at VM.start against the pool's UEFI
// certificates. Writing "true" would claim an enforcement the pool may not have.
func TestReconcileSecureBoot_EnablesWithAuto(t *testing.T) {
	api := &fakeSecureBoot{}

	outcome, err := reconcileSecureBoot(api, boolPtr(true))

	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if outcome != SecureBootAuto {
		t.Errorf("outcome = %d, want SecureBootAuto", outcome)
	}
	if len(api.writes) != 1 || api.writes[0] != "auto" {
		t.Errorf("wrote %v, want exactly one write of \"auto\"", api.writes)
	}
}

// An explicit false is a real instruction and must reach the VM: it is the only
// way to turn off Secure Boot a clone inherited.
func TestReconcileSecureBoot_DisablesOnAnExplicitFalse(t *testing.T) {
	api := &fakeSecureBoot{}

	outcome, err := reconcileSecureBoot(api, boolPtr(false))

	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if outcome != SecureBootDisabled {
		t.Errorf("outcome = %d, want SecureBootDisabled", outcome)
	}
	if len(api.writes) != 1 || api.writes[0] != "false" {
		t.Errorf("wrote %v, want exactly one write of \"false\"", api.writes)
	}
}

// Unlike the pre-start checks, this one writes: a failure means the VM does not
// have the Secure Boot state the build asked for, so the build must not carry on
// and produce an image that quietly differs from what was requested.
func TestReconcileSecureBoot_FailsTheBuildWhenTheWriteFails(t *testing.T) {
	api := &fakeSecureBoot{err: errors.New("API error: code 1, message SESSION_INVALID")}

	outcome, err := reconcileSecureBoot(api, boolPtr(true))

	if err == nil {
		t.Fatal("a failed write leaves the VM in a state the build did not ask for")
	}
	if outcome != SecureBootKept {
		t.Errorf("outcome = %d, want SecureBootKept reported when nothing was written", outcome)
	}
	// The value is the part the user has to act on, and the wrapped cause names
	// the pool problem underneath it.
	if !strings.Contains(err.Error(), "auto") || !strings.Contains(err.Error(), "SESSION_INVALID") {
		t.Errorf("the error must name the value and the cause, got: %s", err)
	}
}

// The build log is the only place the user learns that an inherited setting was
// kept rather than overwritten, so the kept case has to name where it came from.
func TestSecureBootOutcome_Describe(t *testing.T) {
	kept := SecureBootKept.Describe("Windows 11")
	if !strings.Contains(kept, "Windows 11") {
		t.Errorf("a kept setting must name the source it came from, got: %s", kept)
	}

	for outcome, want := range map[SecureBootOutcome]string{
		SecureBootDisabled: "Disabled",
		SecureBootAuto:     "Auto",
	} {
		if got := outcome.Describe("Windows 11"); !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to mention %q", got, want)
		}
	}
}
