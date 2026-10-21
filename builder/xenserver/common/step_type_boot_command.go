// Copyright (c) 2026. Citrix Systems, Inc.

package common

/* Heavily borrowed from hashicorp/packer/builder/qemu/step_type_boot_command.go */

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"

	"xenapi"
)

const KeyLeftShift uint = 0xFFE1

// What a UEFI guest booting an installer CD gets when the build names no count of its
// own. The "Press any key to boot from CD" window is only open for about six seconds, at
// an offset that depends on how long the firmware takes to reach the CD, so the press is
// swept across it rather than aimed at it. These are the values examples/windows-iso has
// used since CP-314049, and the only pair with a passing run behind it.
const (
	uefiCDPromptGroups   = 20
	uefiCDPromptInterval = time.Second
)

type bootCommandTemplateData struct {
	Name     string
	HTTPIP   string
	HTTPPort uint
}

type StepTypeBootCommand struct {
	Ctx interpolate.Context
	// SweepUEFICDPrompt is set by the builder whose guest boots an installer CD, which
	// is the only case where repeating the boot command is right by default.
	SweepUEFICDPrompt bool
}

// dismissKeys answer a prompt that asks for any key without selecting anything, committing
// anything, or moving a cursor. Every other key in specialKeys does one of those: <enter>
// at a boot menu is a choice, not a dismissal, and the arrows move the choice it would
// commit. Adding to this is a deliberate act -- a key here is one the build may press
// twenty times into whatever the first press started.
var dismissKeys = map[string]bool{
	"<spacebar>": true,
}

// sweepableBootCommand reports whether a boot command may be repeated to cover a boot
// prompt that cannot be aimed at: exactly one key, and one that only dismisses.
//
// Nothing wider is safe. A command that types characters is entering data, and the second
// pass replays it into whatever the first pass started. A command that navigates or
// commits does the same with keys -- ["<down><enter>"] picks a boot menu entry, and the
// nineteen repeats land in the installer it booted. A command carrying a <wait> pays that
// wait on every pass, and since vncSendString spends 250 ms a key on top, the period
// between presses is the command's own cost plus the interval, which no fixed sweep can
// aim with. All three are CA-432255.
func sweepableBootCommand(commands []string) bool {
	return len(commands) == 1 && dismissKeys[commands[0]]
}

// bootKeyGroups decides how many times to send the boot command and how long to leave
// between sends. Zero means the build named no count -- Prepare leaves it alone so the
// two cases can still be told apart here -- and only then does the builder choose.
func bootKeyGroups(config CommonConfig, firmware string, sweepUEFICDPrompt bool, commands []string) (int, time.Duration) {
	if config.BootKeyGroups != 0 {
		return config.BootKeyGroups, config.BootKeyGroupInterval
	}

	if sweepUEFICDPrompt && firmware == firmwareUEFI && sweepableBootCommand(commands) {
		// The interval is a default, not part of the sweep: a build that named one and
		// left the count alone meant it.
		if config.BootKeyGroupInterval != 0 {
			return uefiCDPromptGroups, config.BootKeyGroupInterval
		}
		return uefiCDPromptGroups, uefiCDPromptInterval
	}

	return 1, config.BootKeyGroupInterval
}

func (self *StepTypeBootCommand) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("commonconfig").(CommonConfig)
	ui := state.Get("ui").(packer.Ui)
	c := state.Get("client").(*Connection)
	httpPort := state.Get("http_port").(int)

	if len(config.BootCommand) == 0 {
		return multistep.ActionContinue
	}

	uuid := state.Get("instance_uuid").(string)
	vmRef, err := xenapi.VM.GetByUUID(c.session, uuid)
	if err != nil {
		err = fmt.Errorf("unable to get VM from UUID '%s': %w", uuid, err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	consoles, err := xenapi.VM.GetConsoles(c.session, vmRef)
	if err != nil {
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	if len(consoles) != 1 {
		err := fmt.Errorf("expected to find a VM console, instead found %d; ensure there is only one console", len(consoles))
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	location, err := xenapi.Console.GetLocation(c.session, consoles[0])
	if err != nil {
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	trimmed := strings.TrimSpace(location)
	consoleURL, parseErr := url.Parse(trimmed)
	if parseErr != nil || consoleURL.Hostname() == "" {
		// url.Parse quotes the location in its own message, so only the other one does.
		err := fmt.Errorf("could not read a host and path out of the console location %q", trimmed)
		if parseErr != nil {
			err = fmt.Errorf("could not read a host and path out of the console location: %w", parseErr)
		}
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	consoleHost := consoleURL.Hostname()
	consoleLocation := consoleURL.RequestURI()

	ui.Say(fmt.Sprintf("Connecting to the VM console VNC over xapi via %s", consoleHost))
	vncClient, err := establishConsoleVNC(ctx, state, ui, c, consoleHost, consoleLocation)
	if err != nil {
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	defer vncClient.Close()

	// Closing the console is what unblocks a write parked on it. Without this Run never
	// returns, so BasicRunner never reaches any step's Cleanup and the VM is left behind.
	stopOnCancel := context.AfterFunc(ctx, func() { vncClient.Close() })
	defer stopOnCancel()

	log.Printf("Connected to the VNC console: %s", vncClient.DesktopName)

	// StepHTTPIPDiscover resolves this from http_bind_address, http_interface or the
	// routing table, so the boot command can be templated without a dom0 login.
	localIp, ok := state.Get("http_ip").(string)
	if !ok || localIp == "" {
		err := fmt.Errorf("no HTTP server address is known for this build; StepHTTPIPDiscover must run before the boot command")
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	ui.Message(fmt.Sprintf("Boot command will point the VM at %s", localIp))

	commands, err := renderBootCommand(config.BootCommand, self.Ctx, bootCommandTemplateData{
		Name:     config.Name(),
		HTTPIP:   localIp,
		HTTPPort: uint(httpPort),
	})
	if err != nil {
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	firmware, _ := state.Get("firmware").(string)
	groups, interval := bootKeyGroups(config, firmware, self.SweepUEFICDPrompt, commands)
	switch {
	case config.BootKeyGroups != 0:
	case groups > 1:
		ui.Message(fmt.Sprintf("UEFI guest: sending the boot command %d times, at least %s apart, "+
			"to cover the \"Press any key to boot from CD\" window. Set boot_key_groups = 1 to "+
			"send it once.", groups, interval))
	case self.SweepUEFICDPrompt && firmware == firmwareUEFI:
		ui.Message("UEFI guest: sending the boot command once. Only boot_command = " +
			"[\"<spacebar>\"] is repeated by default, because every repeat lands in whatever the " +
			"first one started and <spacebar> is the one key that cannot choose anything. Set " +
			"boot_key_groups and boot_key_group_interval to sweep this command anyway.")
	}

	ui.Say("Typing boot commands over VNC...")
	action := self.typeCommands(ctx, state, ui, groups, interval, commands, func(cmd string) error {
		return vncSendString(state, vncClient, cmd)
	})

	if action == multistep.ActionContinue {
		ui.Say("Finished typing.")
	}
	return action
}

// typeCommands sends the already-rendered boot commands groups times, waiting interval
// between groups. send is called once per command string; in production it wraps
// vncSendString, in tests it is a spy.
func (self *StepTypeBootCommand) typeCommands(
	ctx context.Context,
	state multistep.StateBag,
	ui packer.Ui,
	groups int,
	interval time.Duration,
	commands []string,
	send func(string) error,
) multistep.StepAction {
	// ctx.Err() rather than StateCancelled alone: cancel sets it before releasing anyone,
	// including the AfterFunc that closes the console, while StateCancelled is put by a
	// separate goroutine that may not have run yet.
	cancelled := func() bool {
		if ctx.Err() != nil {
			return true
		}
		_, ok := state.GetOk(multistep.StateCancelled)
		return ok
	}

	for group := 0; group < groups; group++ {
		if cancelled() {
			return multistep.ActionHalt
		}

		for _, command := range commands {
			if cancelled() {
				return multistep.ActionHalt
			}

			// Halt rather than carry on: the guest is left part-way through a boot
			// command, and the install wait above would otherwise sit out its whole
			// timeout before the build failed for the wrong reason.
			if err := send(command); err != nil {
				// Ctrl-C closes the console under the keystroke, so send fails with the
				// close rather than with anything the guest did. Halting without an error
				// leaves the builder to report the cancellation.
				if cancelled() {
					return multistep.ActionHalt
				}
				state.Put("error", err)
				ui.Error(err.Error())
				return multistep.ActionHalt
			}
		}

		if group < groups-1 && interval > 0 {
			ui.Message(fmt.Sprintf("Waiting %s before next boot command group...", interval))
			if err := (InterruptibleWait{Timeout: interval}).Wait(state); err != nil {
				ui.Error(err.Error())
				return multistep.ActionHalt
			}
		}
	}
	return multistep.ActionContinue
}

// renderBootCommand interpolates every boot command up-front, so a template error is
// reported before any of it reaches the guest: a half-entered boot command leaves an
// installer in a state no later step can recover from.
//
// ctx is taken by value so the caller's interpolation context, which carries the build's
// user variables, is not left pointing at this step's data.
func renderBootCommand(commands []string, ctx interpolate.Context, data bootCommandTemplateData) ([]string, error) {
	ctx.Data = &data

	rendered := make([]string, 0, len(commands))
	for i, command := range commands {
		out, err := interpolate.Render(command, &ctx)
		if err != nil {
			return nil, fmt.Errorf("error preparing boot command %d of %d: %w", i+1, len(commands), err)
		}
		rendered = append(rendered, out)
	}

	return rendered, nil
}

func (self *StepTypeBootCommand) Cleanup(multistep.StateBag) {}

// parseWaitToken reports whether s starts with a <wait...> token and, if so, how long it
// asks for and how many bytes it spans. A bare <wait> is one second; anything else is a
// Go duration, with a plain number read as seconds. This is the spelling the other Packer
// builders use.
func parseWaitToken(s string) (time.Duration, int, bool) {
	if !strings.HasPrefix(s, "<wait") {
		return 0, 0, false
	}

	end := strings.Index(s, ">")
	if end < 0 {
		return 0, 0, false
	}

	arg := s[len("<wait"):end]
	if arg == "" {
		return time.Second, end + 1, true
	}
	// A bare number means seconds; time.ParseDuration insists on a unit.
	if _, err := strconv.Atoi(arg); err == nil {
		arg += "s"
	}

	d, err := time.ParseDuration(arg)
	if err != nil || d < 0 {
		return 0, 0, false
	}
	return d, end + 1, true
}

// validateBootCommandWaits rejects <wait...> tokens parseWaitToken will not take, so a
// mistyped one is reported by `packer validate` rather than typed in as literal text.
func validateBootCommandWaits(commands []string) (errs []error) {
	for i, command := range commands {
		for rest := command; ; {
			at := strings.Index(rest, "<wait")
			if at < 0 {
				break
			}
			rest = rest[at:]

			if _, length, ok := parseWaitToken(rest); ok {
				rest = rest[length:]
				continue
			}

			end := strings.Index(rest, ">")
			if end < 0 {
				break
			}
			errs = append(errs, fmt.Errorf("boot_command %d: '%s' is not a valid wait token and would be "+
				"typed into the guest as text. Use <wait>, a number of seconds such as <wait5>, or a "+
				"duration such as <wait500ms>, <wait2m> or <wait1m30s>", i+1, rest[:end+1]))
			rest = rest[end+1:]
		}
	}
	return errs
}

// keyTyper is the part of *vnc.ClientConn that typing a boot command uses, so the token
// handling below can be tested without a VNC server.
type keyTyper interface {
	KeyEvent(keysym uint32, down bool) error
}

// specialKeys are the tokens that become one keypress. Everything else in a boot command
// is typed character by character. Package-level so that whether a command only presses
// keys is decided from the same table that does the pressing.
//
// Keysym reference: https://github.com/qemu/qemu/blob/master/ui/vnc_keysym.h
var specialKeys = map[string]uint32{
	"<bs>":       0xFF08,
	"<del>":      0xFFFF,
	"<enter>":    0xFF0D,
	"<esc>":      0xFF1B,
	"<f1>":       0xFFBE,
	"<f2>":       0xFFBF,
	"<f3>":       0xFFC0,
	"<f4>":       0xFFC1,
	"<f5>":       0xFFC2,
	"<f6>":       0xFFC3,
	"<f7>":       0xFFC4,
	"<f8>":       0xFFC5,
	"<f9>":       0xFFC6,
	"<f10>":      0xFFC7,
	"<f11>":      0xFFC8,
	"<f12>":      0xFFC9,
	"<return>":   0xFF0D,
	"<tab>":      0xFF09,
	"<up>":       0xFF52,
	"<down>":     0xFF54,
	"<left>":     0xFF51,
	"<right>":    0xFF53,
	"<spacebar>": 0x020,
	"<insert>":   0xFF63,
	"<home>":     0xFF50,
	"<end>":      0xFF57,
	"<pageUp>":   0xFF55,
	"<pageDown>": 0xFF56,
}

// Taken from qemu's builder plugin - not an exported function.
func vncSendString(state multistep.StateBag, c keyTyper, original string) error {
	special := specialKeys

	shiftedChars := "~!@#$%^&*()_+{}|:\"<>?"

	// KeyEvent writes to the console socket, so a dropped session has to stop the typing
	// and reach the caller: nothing above notices before install_timeout otherwise.
	send := func(keysym uint32, down bool) error {
		if err := c.KeyEvent(keysym, down); err != nil {
			// go-vnc's reader closes the connection on any read failure and keeps the
			// reason to itself, so the console going away and a message it would not
			// parse are indistinguishable from here. Naming only one sends the reader of
			// this message after the wrong thing.
			if errors.Is(err, net.ErrClosed) {
				return fmt.Errorf("the console connection closed part-way through the boot "+
					"command: either the VM console channel went away or the VNC client's "+
					"reader rejected a message from it. The guest did not refuse the "+
					"keystroke: %w", err)
			}

			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return fmt.Errorf("the VM console stopped accepting data part-way through the "+
					"boot command; the cap that ended it is this plugin's own rather than "+
					"anything the guest reported: %w", err)
			}

			return fmt.Errorf("stopped typing the boot command: %w", err)
		}
		return nil
	}

	for len(original) > 0 {
		var keyCode uint32
		keyShift := false

		if d, length, ok := parseWaitToken(original); ok {
			log.Printf("Special code '%s' found, sleeping %s", original[:length], d)
			// A boot command may ask for minutes, and a wait that ignores
			// Ctrl-C would hold the whole build for that long.
			if err := (InterruptibleWait{Timeout: d}).Wait(state); err != nil {
				return fmt.Errorf("stopped typing the boot command: %w", err)
			}
			original = original[length:]
			continue
		}

		for specialCode, specialValue := range special {
			if strings.HasPrefix(original, specialCode) {
				log.Printf("Special code '%s' found, replacing with: %d", specialCode, specialValue)
				keyCode = specialValue
				original = original[len(specialCode):]
				break
			}
		}

		// Nothing typed is logged: a boot command carries the unattended
		// installer's answer file, passwords included.
		if keyCode == 0 {
			r, size := utf8.DecodeRuneInString(original)
			original = original[size:]
			keyCode = uint32(r)
			keyShift = unicode.IsUpper(r) || strings.ContainsRune(shiftedChars, r)
		}

		if keyShift {
			if err := send(uint32(KeyLeftShift), true); err != nil {
				return err
			}
		}

		if err := send(keyCode, true); err != nil {
			return err
		}
		time.Sleep(time.Second / 10)
		if err := send(keyCode, false); err != nil {
			return err
		}
		time.Sleep(time.Second / 10)

		if keyShift {
			if err := send(uint32(KeyLeftShift), false); err != nil {
				return err
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	return nil
}
