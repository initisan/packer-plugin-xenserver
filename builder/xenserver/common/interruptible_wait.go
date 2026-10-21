// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
)

type InterruptibleWait struct {
	Timeout time.Duration

	// optional; PredicateInterval is required whenever Predicate is set, or the poll never pauses.
	Predicate         func() (result bool, err error)
	PredicateInterval time.Duration
}

type TimeoutError struct{}

func (err TimeoutError) Error() string {
	return "Timed out"
}

type InterruptedError struct{}

func (err InterruptedError) Error() string {
	return "Interrupted"
}

// Wait blocks for up to Timeout, polling Predicate every PredicateInterval starting immediately.
// It returns InterruptedError if the build is cancelled, the Predicate's own error if one fails,
// nil once the Predicate succeeds, and TimeoutError on timeout. With no Predicate a timeout
// returns nil, which makes Wait an interruptible sleep.
func (wait InterruptibleWait) Wait(state multistep.StateBag) error {
	predicateResult := make(chan error, 1)
	stopWaiting := make(chan struct{})
	defer close(stopWaiting)

	if wait.Predicate != nil {
		go func() {
			for {
				if complete, err := wait.Predicate(); err != nil || complete {
					predicateResult <- err
					return
				}

				select {
				case <-time.After(wait.PredicateInterval):
					continue
				case <-stopWaiting:
					return
				}
			}
		}()
	}

	timeout := time.After(wait.Timeout)
	for {
		if _, ok := state.GetOk(multistep.StateCancelled); ok {
			return InterruptedError{}
		}

		select {
		case err := <-predicateResult:
			return err

		case <-time.After(1 * time.Second):
			continue

		case <-timeout:
			if wait.Predicate != nil {
				return TimeoutError{}
			} else {
				return nil
			}
		}
	}
}
