// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"

	"xenapi"
)

const (
	guestShutdownPollInterval = 5 * time.Second
	guestShutdownReportEvery  = 2 * time.Minute
)

// InstallWaitStep returns the step that bounds the unattended installation: with a
// communicator, the wait for an address to connect to; with `communicator = "none"`,
// the wait for the guest to power itself off, since no step downstream dials an address.
func InstallWaitStep(commType string, httpReqChan <-chan string, timeout time.Duration) multistep.Step {
	if commType == "none" {
		return &StepWaitForGuestShutdown{Timeout: timeout}
	}

	return &StepWaitForIP{
		Chan:    httpReqChan,
		Timeout: timeout,
	}
}

// StepWaitForGuestShutdown waits for the guest to power itself off, which is how a build
// with no communicator learns that its unattended installation has finished. It never
// asks XAPI to shut the VM down: the point is to observe the guest finishing, and xapi
// rejects VM.clean_shutdown with VM_LACKS_FEATURE on a tools-less guest anyway.
type StepWaitForGuestShutdown struct {
	VmCleanup
	Timeout time.Duration

	// Zero means the package defaults; a test sets them to keep its own runtime short.
	pollInterval time.Duration
	reportEvery  time.Duration
}

func (self *StepWaitForGuestShutdown) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packer.Ui)
	c := state.Get("client").(*Connection)

	ui.Say("Step: Wait for the guest to shut itself down.")

	uuid := state.Get("instance_uuid").(string)
	instance, err := xenapi.VM.GetByUUID(c.session, uuid)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to get VM from UUID '%s': %s", uuid, err.Error()))
		return multistep.ActionHalt
	}

	pollInterval := cmp.Or(self.pollInterval, guestShutdownPollInterval)
	reportEvery := cmp.Or(self.reportEvery, guestShutdownReportEvery)

	ui.Message(fmt.Sprintf("The build has no communicator, so it waits up to %s (install_timeout) "+
		"for the VM to reach the Halted power state.", self.Timeout))
	ui.Message(fmt.Sprintf("Every %s it reports the VM's power state and how many times the "+
		"guest's domain has started. A count that stops rising well short of install_timeout is "+
		"worth checking on the VM's console.", reportEvery))

	progress := newShutdownProgress(reportEvery, self.Timeout)
	started := time.Now()

	err = InterruptibleWait{
		Timeout:           self.Timeout,
		PredicateInterval: pollInterval,
		Predicate: func() (bool, error) {
			powerState, err := xenapi.VM.GetPowerState(c.session, instance)
			if err != nil {
				return false, err
			}

			if line := progress.observe(time.Since(started), powerState, domainStartTime(c, instance)); line != "" {
				ui.Message(line)
			}

			return powerState == xenapi.VMPowerStateHalted, nil
		},
	}.Wait(state)

	switch err.(type) {
	case nil:
		ui.Say("The guest has shut itself down")
		return multistep.ActionContinue
	case InterruptedError:
		// Cancelled, not a guest failure. Leave the VM to the cleanup path, which
		// honours keep_vm.
		ui.Say("Interrupted while waiting for the guest to shut itself down")
		return multistep.ActionHalt
	case TimeoutError:
		ui.Error(fmt.Sprintf("The guest did not shut itself down within %s (install_timeout): %s",
			self.Timeout, progress.domainSummary()))
		ui.Error("With communicator = \"none\" the guest powering itself off is the only signal " +
			"the build has that the installation finished. Make the answer file shut the guest " +
			"down at the end of the install, or raise install_timeout.")
		ui.Error(keepVMAdvice(state))
		return multistep.ActionHalt
	default:
		ui.Error(fmt.Sprintf("Could not read the VM's power state: %s", err.Error()))
		return multistep.ActionHalt
	}
}

// keepVMAdvice says what is about to become of the VM carrying the only evidence of where
// the install stopped. keep_vm defaults to never, so by default it is destroyed moments
// after the timeout is reported (CA-431815).
func keepVMAdvice(state multistep.StateBag) string {
	// Only "always" survives the halt this timeout causes: "on_success" reads the state
	// bag, which has no halt recorded in it until this step returns.
	if config, ok := state.Get("commonconfig").(CommonConfig); ok && config.KeepVM == "always" {
		return "keep_vm = \"always\" leaves this VM in place: look at its console to see what " +
			"the installer is waiting on."
	}

	return "This VM is about to be destroyed. Set keep_vm = \"always\" and run the build again " +
		"to keep it, then look at its console to see what the installer is waiting on."
}

// shutdownProgress turns the shutdown poll into a progress report. CA-432008: the wait
// printed nothing between its opening message and install_timeout, so a guest stopped on
// an installer dialog was indistinguishable from a slow install for up to 3h20m.
//
// InterruptibleWait polls from its own goroutine but reports the timeout from the
// caller's, and a predicate already in flight when the deadline passes runs to
// completion, so both reach a shutdownProgress at once.
type shutdownProgress struct {
	every   time.Duration
	timeout time.Duration

	mu         sync.Mutex
	nextReport time.Duration
	starts     int
	lastStart  time.Time
}

func newShutdownProgress(every, timeout time.Duration) *shutdownProgress {
	return &shutdownProgress{every: every, timeout: timeout, nextReport: every}
}

// observe records one poll and returns the line to print, or "" on the polls in between.
// domainStart is the zero time when XAPI cannot say.
func (p *shutdownProgress) observe(elapsed time.Duration, powerState xenapi.VMPowerState, domainStart time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !domainStart.IsZero() && !domainStart.Equal(p.lastStart) {
		p.lastStart = domainStart
		p.starts++
	}

	if elapsed < p.nextReport {
		return ""
	}
	for p.nextReport <= elapsed {
		p.nextReport += p.every
	}

	// Past the deadline the step is printing its own timeout message, which carries the
	// same count. A progress line arriving behind it would read as a later observation.
	if elapsed >= p.timeout {
		return ""
	}

	return fmt.Sprintf("Waited %s of %s: power state %s, %s.",
		elapsed.Round(time.Second), p.timeout, powerState, p.summary())
}

func (p *shutdownProgress) domainSummary() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.summary()
}

// summary reports the domain starts seen so far; the caller holds p.mu.
func (p *shutdownProgress) summary() string {
	switch p.starts {
	case 0:
		return "the domain's start time is unknown"
	case 1:
		return "the domain has started once"
	default:
		return fmt.Sprintf("the domain has started %d times", p.starts)
	}
}

// xapiEpoch is what XAPI reports for a domain that has not started. Go's zero time is
// year 1, so IsZero does not catch it.
var xapiEpoch = time.Unix(0, 0)

// domainStartTime reports when the VM's current domain started. It answers a failure with
// the zero time rather than an error: this is only wanted for a log line, and must not be
// what ends the wait.
func domainStartTime(c *Connection, instance xenapi.VMRef) time.Time {
	metrics, err := xenapi.VM.GetMetrics(c.session, instance)
	if err != nil || metrics == "" || metrics == nullRef {
		return time.Time{}
	}

	started, err := xenapi.VMMetrics.GetStartTime(c.session, metrics)
	if err != nil || started.Equal(xapiEpoch) {
		return time.Time{}
	}

	return started
}
