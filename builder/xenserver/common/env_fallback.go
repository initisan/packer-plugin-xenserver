// Copyright (c) 2026. Citrix Systems, Inc.

package common

import "os"

// envFallback pairs a config field with the environment variable it falls back to.
type envFallback struct {
	field *string
	env   string
}

// applyEnvFallbacks fills in each field the template left unset from its environment
// variable, so that a credential need never be written into a .pkr.hcl at all.
//
// A value present in the template always wins: an exported XENSERVER_PASSWORD must not
// silently redirect a build that named its own. The empty string is the only spelling of
// "unset" these options have -- they are plain strings, with no way to tell an explicit
// `""` from an absent one -- and an empty credential is not a usable value either way, so
// the environment fills it in.
func applyEnvFallbacks(fallbacks []envFallback) {
	for _, f := range fallbacks {
		if *f.field == "" {
			*f.field = os.Getenv(f.env)
		}
	}
}
