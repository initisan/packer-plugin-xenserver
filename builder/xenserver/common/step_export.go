// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"

	"xenapi"
)

type StepExport struct{}

// minExportSize is the size below which an export cannot be an artifact.
const minExportSize = 512

// An XVA is a tar archive, and the plugin always asks for use_compression=true, so the
// download is a gzip stream. The plain tar header is accepted too, in case a pool ignores
// the compression parameter. "ustar" sits at a fixed offset in the first tar header block.
const tarMagicOffset = 257

// previewBytes is how much of a rejected file the error message quotes -- enough to
// recognise an XML or HTML error document at a glance.
const previewBytes = 8

// downloadBufferBytes is how much of an export is moved per read/write pair.
const downloadBufferBytes = 1 << 20

// progressMarkerPercent is the granularity of the "Downloading... n%" messages.
const progressMarkerPercent = 5

var (
	gzipMagic = []byte{0x1f, 0x8b}
	tarMagic  = []byte("ustar")
)

// exportXVAURL builds the URL for XAPI's /export handler. The session reference
// authenticates the request, so it has to be redacted before the URL is printed.
func exportXVAURL(host, compressOption, vmUUID string, sessionRef xenapi.SessionRef) string {
	return fmt.Sprintf("https://%s/export?%suuid=%s&session_id=%s", host, compressOption, vmUUID, sessionRef)
}

// exportVDIURL builds the URL for XAPI's /export_raw_vdi handler. It authenticates with
// session_id rather than HTTP Basic userinfo, which the redirect XAPI issues for a VDI on
// another host would drop.
func exportVDIURL(host string, sessionRef xenapi.SessionRef, vdiUUID, extraURI string) string {
	return fmt.Sprintf("https://%s/export_raw_vdi?session_id=%s&vdi=%s%s", host, sessionRef, vdiUUID, extraURI)
}

// xeExportArgs builds the argv for `xe vm-export`, separately so that the argv and the
// redacted form printed alongside it (redactArgs) cannot drift apart.
func xeExportArgs(host, username, password, vmUUID, compressOption, filename string) []string {
	return []string{
		"-s", host,
		"-p", "443",
		"-u", username,
		"-pw", password,
		"vm-export",
		"vm=" + vmUUID,
		compressOption,
		"filename=" + filename,
	}
}

// exportLogLine and xeExportLogLine build the two lines the export writes to the build
// log. They are functions so a test asserts on the line itself: the failure guarded
// against is a log line that stops calling redactURL, which testing redactURL cannot see.
func exportLogLine(exportURL string) string {
	return "Getting XVA " + redactURL(exportURL)
}

func xeExportLogLine(path string, args []string) string {
	return fmt.Sprintf("Getting XVA %+v %+v", path, redactArgs(args))
}

// validateExportedFile rejects a file that a working export cannot have written.
//
// A 200 is not evidence of success: XAPI writes the status line and headers for both
// /export and /export_raw_vdi before the body, so a later failure still reaches the client
// as a 200 with a short or empty body, and `xe vm-export` reports it as exit 0. The file
// on disk is the only remaining evidence.
func validateExportedFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("the export wrote no file: %w", err)
	}
	if info.Size() < minExportSize {
		return fmt.Errorf("the export wrote %d bytes to %s, which is too small to be an artifact", info.Size(), path)
	}
	return nil
}

// validateExportedXVA also reads the start of the file: a plausibly sized file that is not
// an archive would otherwise fail at import time, long after the build reported success.
func validateExportedXVA(path string) error {
	if err := validateExportedFile(path); err != nil {
		return err
	}

	fh, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not open the exported XVA: %w", err)
	}
	defer fh.Close()

	header := make([]byte, tarMagicOffset+len(tarMagic))
	if _, err := io.ReadFull(fh, header); err != nil {
		return fmt.Errorf("could not read the start of %s: %w", path, err)
	}

	if bytes.HasPrefix(header, gzipMagic) {
		return nil
	}
	if bytes.Equal(header[tarMagicOffset:], tarMagic) {
		return nil
	}

	return fmt.Errorf("%s is not an XVA: it starts with %x, which is neither a gzip nor a tar header", path, header[:previewBytes])
}

// downloadFile streams url into filename. The client comes from the caller so the download
// verifies the pool exactly as the XenAPI session did -- see Connection.TLSConfig.
//
// ctx is the build's: an export runs to tens of gigabytes with no timeout above it, so
// without one a host that stops sending mid-transfer holds the build in Read and Ctrl-C
// cannot reach it.
func downloadFile(ctx context.Context, client *http.Client, url, filename string, ui packer.Ui) (err error) {

	fh, err := os.Create(filename)
	if err != nil {
		return err
	}
	// A failed Close can lose the tail of the artifact, which no later check can see.
	defer func() {
		if cerr := fh.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return redactURLError(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		// net/url only redacts userinfo, and the session reference is in the query
		// string, so the URL this error carries has to be rewritten first.
		return redactURLError(err)
	}

	defer resp.Body.Close()

	// Without this the error document XAPI sends with a failure is written into the
	// artifact in place of the export.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("export request got non-200 status code: %s", resp.Status)
	}

	var progress, percentage uint
	// ContentLength is -1 when the export arrives chunked: no total, no percentage.
	total := uint(resp.ContentLength)
	showProgress := resp.ContentLength > 0

	// A VM export runs to tens of gigabytes, so a small buffer would cost a syscall
	// pair per few KiB and hold the socket drain rate down with it.
	var buffer [downloadBufferBytes]byte
	for {
		n, err := resp.Body.Read(buffer[:])
		if err != nil && err != io.EOF {
			return err
		}

		progress += uint(n)

		if _, write_err := fh.Write(buffer[:n]); write_err != nil {
			return write_err
		}

		if err == io.EOF {
			break
		}

		if showProgress {
			cur_percentage := ((progress * 100 / total) / progressMarkerPercent) * progressMarkerPercent
			if cur_percentage > percentage {
				percentage = cur_percentage
				ui.Message(fmt.Sprintf("Downloading... %d%%", percentage))
			}
		}
	}

	return nil
}

func (StepExport) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("commonconfig").(CommonConfig)
	ui := state.Get("ui").(packer.Ui)
	c := state.Get("client").(*Connection)
	instance_uuid := state.Get("instance_uuid").(string)

	ui.Say("Step: export artifact")

	switch config.Format {
	case "none":
		ui.Say("Skipping export")
		return multistep.ActionContinue

	case "xva":
		// Always gzip-compressed: 40-50% smaller, and both import paths detect gzip
		// transparently. CommonConfig.Prepare normalizes "xva_compressed" to "xva".
		compress_option_xe := "compress=true"
		compress_option_url := "use_compression=true&"

		export_filename := fmt.Sprintf("%s/%s.xva", config.OutputDir, config.Name())

		var err error
		use_xe := os.Getenv("USE_XE") == "1"
		if xe, e := exec.LookPath("xe"); e == nil && use_xe {
			// `xe` takes the password as an argument, so this opt-in path puts it
			// in the argv for anything on the machine to read. Keeping it out of
			// the build log at least stops it being archived with that log.
			args := xeExportArgs(c.Host, c.Username, c.Password, instance_uuid, compress_option_xe, export_filename)
			cmd := exec.CommandContext(ctx, xe, args...)

			// Without this a failure reaches the build log as "exit status 1" and
			// nothing xe said about it.
			var stderr bytes.Buffer
			cmd.Stderr = &stderr

			ui.Say(xeExportLogLine(cmd.Path, cmd.Args))

			if err = cmd.Run(); err != nil && stderr.Len() > 0 {
				err = fmt.Errorf("%w: %s", err, stderr.String())
			}
		} else {
			export_url := exportXVAURL(c.Host, compress_option_url, instance_uuid, c.GetSessionRef())

			ui.Say(exportLogLine(export_url))
			err = downloadFile(ctx, c.HTTPClient(), export_url, export_filename, ui)
		}

		if err != nil {
			ui.Error(fmt.Sprintf("Could not download XVA: %s", err.Error()))
			return multistep.ActionHalt
		}

		// Both branches above report success without looking at what they wrote.
		// Last chance to fail the build rather than someone's `xe vm-import`.
		if err := validateExportedXVA(export_filename); err != nil {
			ui.Error(fmt.Sprintf("The XVA export did not produce a usable artifact: %s", err.Error()))
			return multistep.ActionHalt
		}

	case "vdi_raw", "vdi_vhd":
		// Both spellings drive XAPI's /export_raw_vdi handler; the format=
		// query parameter is the only difference, and its default is raw.
		suffix, extrauri := ".raw", ""
		if config.Format == "vdi_vhd" {
			suffix, extrauri = ".vhd", "&format=vhd"
		}

		// Only this branch needs the VM reference.
		instance, err := xenapi.VM.GetByUUID(c.session, instance_uuid)
		if err != nil {
			ui.Error(fmt.Sprintf("Could not get VM with UUID '%s': %s", instance_uuid, err.Error()))
			return multistep.ActionHalt
		}

		disks, err := GetDisks(c, instance)
		if err != nil {
			ui.Error(fmt.Sprintf("Could not get VM disks: %s", err.Error()))
			return multistep.ActionHalt
		}
		for _, disk := range disks {
			disk_uuid, err := xenapi.VDI.GetUUID(c.session, disk)
			if err != nil {
				ui.Error(fmt.Sprintf("Could not get the UUID of disk '%s': %s", disk, err.Error()))
				return multistep.ActionHalt
			}

			disk_export_url := exportVDIURL(c.Host, c.GetSessionRef(), disk_uuid, extrauri)

			disk_export_filename := fmt.Sprintf("%s/%s%s", config.OutputDir, disk_uuid, suffix)

			ui.Say(fmt.Sprintf("Getting VDI %s", disk_uuid))
			err = downloadFile(ctx, c.HTTPClient(), disk_export_url, disk_export_filename, ui)
			if err != nil {
				ui.Error(fmt.Sprintf("Could not download VDI: %s", err.Error()))
				return multistep.ActionHalt
			}

			// /export_raw_vdi writes its 200 before the copy too, so a disk image
			// gets the same size check. There is no magic number to look for.
			if err := validateExportedFile(disk_export_filename); err != nil {
				ui.Error(fmt.Sprintf("The VDI export did not produce a usable artifact: %s", err.Error()))
				return multistep.ActionHalt
			}

		}

	default:
		panic(fmt.Sprintf("Unknown export format '%s'", config.Format))
	}

	ui.Say("Download completed: " + config.OutputDir)

	return multistep.ActionContinue
}

func (StepExport) Cleanup(state multistep.StateBag) {}
