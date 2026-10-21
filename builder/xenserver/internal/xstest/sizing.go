// Copyright (c) 2026. Citrix Systems, Inc.

package xstest

import "testing"

// WantUint fails unless the option is set to want. The sizing options are pointers
// so that an explicit 0 can be told apart from the option being absent (CA-431825),
// which makes "set to n" and "unset" two different assertions.
func WantUint(t *testing.T, option string, got *uint, want uint) {
	t.Helper()
	switch {
	case got == nil:
		t.Errorf("%s is unset, want %d", option, want)
	case *got != want:
		t.Errorf("%s = %d, want %d", option, *got, want)
	}
}

// WantUnset fails unless the option is absent, which is how a build says "keep
// whatever the source template has".
func WantUnset(t *testing.T, option string, got *uint) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %d, want it unset so the source template's value is kept", option, *got)
	}
}
