// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"

	"xenapi"
)

// The other_config keys describing a template that carries no disks of its own.
// XenServer provisions disks from them and then deletes both (xapi_templates.ml:92-95,
// :166-180).
const (
	provisionDisksKey  = "disks"
	provisionScriptKey = "postinstall"

	// primaryDiskDevice is the VBD userdevice of a VM's system disk. The provisioning
	// XML's device attribute is passed straight through as the userdevice
	// (xapi_templates.ml:140), so the same value selects the disk either way.
	primaryDiskDevice = "0"
)

// provisionXML is the <provision> document other_config:disks holds. Only the primary
// disk's size is read here; the full contract is xapi_templates.ml:63-92.
type provisionXML struct {
	XMLName xml.Name `xml:"provision"`
	Disks   []struct {
		Device string `xml:"device,attr"`
		Size   string `xml:"size,attr"`
	} `xml:"disk"`
}

// TemplateDiskSize reports, in bytes, the size clone_template says its primary disk
// should be, for a build that set no disk_size_gb.
//
// A disk is not inherited the way vCPUs and memory are: VM.clone copies those, while the
// disk is one the iso builder creates, so the size has to be read off the template
// instead. Which mirrors XenCenter's New VM wizard, reading the provisioning XML when the
// template carries one and otherwise falling back to the template's own disks
// (Page_Storage.cs:106-186).
func TemplateDiskSize(c *Connection, src CloneSource, vm xenapi.VMRef, record xenapi.VMRecord) (int, error) {
	return templateDiskSize(xapiPrimaryDisk{session: c.GetSession(), vm: vm}, src, record.OtherConfig)
}

func templateDiskSize(api primaryDiskAPI, src CloneSource, otherConfig map[string]string) (int, error) {
	size, err := provisionedDiskSize(otherConfig[provisionDisksKey])
	if err != nil {
		return 0, fmt.Errorf("could not read the provisioning disks of %s: %w", src, err)
	}
	if size > 0 {
		return size, nil
	}

	// XenCenter stops at the XML and never looks further, but it is asking which disks to
	// create rather than how big one of them is: a document listing no device 0 answers
	// its question and not this one.
	vdi, found, err := primaryDiskVDI(api)
	if err != nil {
		return 0, fmt.Errorf("could not read the disks of %s: %w", src, err)
	}
	if found {
		if size, err = api.VirtualSize(vdi); err != nil {
			return 0, fmt.Errorf("could not read the primary disk's size: %w", err)
		}
		if size > 0 {
			return size, nil
		}
	}

	return 0, fmt.Errorf("%s supplies no primary disk size: it describes no disk to provision "+
		"and carries none of its own. Set disk_size_gb, which is then the size of the disk this "+
		"builder installs onto.", src)
}

// provisionedDiskSize is the size of the device-0 disk a <provision> document describes,
// or zero when there is no document or it describes no such disk.
func provisionedDiskSize(doc string) (int, error) {
	if strings.TrimSpace(doc) == "" {
		return 0, nil
	}

	var provision provisionXML
	if err := xml.Unmarshal([]byte(doc), &provision); err != nil {
		return 0, err
	}

	for _, disk := range provision.Disks {
		if disk.Device != primaryDiskDevice {
			continue
		}

		// Bytes, not GB: xapi reads this attribute with Int64.of_string and hands it
		// straight to VDI.create (xapi_templates.ml:72, :134).
		size, err := strconv.ParseInt(disk.Size, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("the disk on device %s has an unreadable size %q",
				primaryDiskDevice, disk.Size)
		}
		if size <= 0 {
			return 0, fmt.Errorf("the disk on device %s has a size of %d bytes",
				primaryDiskDevice, size)
		}

		return int(size), nil
	}

	return 0, nil
}

// EnsureCloneSourceHasPrimaryDisk refuses a disk_size_gb its source cannot take. The
// clone builder only ever grows the disk the source already has, so a diskless source --
// "Other install media" above all -- otherwise fails at the sizing step, once the clone
// has been made and the CD uploaded (CP-314723).
func EnsureCloneSourceHasPrimaryDisk(c *Connection, src CloneSource, vm xenapi.VMRef) error {
	return ensureCloneSourceHasPrimaryDisk(xapiPrimaryDisk{session: c.GetSession(), vm: vm}, src)
}

func ensureCloneSourceHasPrimaryDisk(api primaryDiskAPI, src CloneSource) error {
	if _, found, err := primaryDiskVDI(api); err != nil || found {
		return err
	}

	return fmt.Errorf("disk_size_gb is set, but %s has no disk on device %s: this builder grows the "+
		"disk its source already carries and never creates one. Remove disk_size_gb, or name a source "+
		"that has an installed disk.", src, primaryDiskDevice)
}

// StripProvisioningKeys takes the provisioning keys off a VM about to be delivered as a
// template.
//
// They describe disks to create, and this VM already carries the disk the build installed
// onto, so left in place `xe vm-install` against the artifact fails with
// DEVICE_ALREADY_EXISTS (xapi_templates.ml:123-148, xapi_vbd.ml:258-272) and XenCenter's
// New VM wizard shows the disks the XML describes in place of the real one
// (Page_Storage.cs:111-141). xapi drops both keys once it has provisioned from them
// (xapi_templates.ml:166-180), as does XenCenter before provisioning its own
// (CreateVMAction.cs:378-388).
func StripProvisioningKeys(c *Connection, vm xenapi.VMRef) ([]string, error) {
	otherConfig, err := xenapi.VM.GetOtherConfig(c.GetSession(), vm)
	if err != nil {
		return nil, fmt.Errorf("could not read the VM's other_config: %w", err)
	}

	var stripped []string
	for _, key := range []string{provisionDisksKey, provisionScriptKey} {
		if _, ok := otherConfig[key]; !ok {
			continue
		}
		if err := xenapi.VM.RemoveFromOtherConfig(c.GetSession(), vm, key); err != nil {
			return stripped, fmt.Errorf("could not remove other_config:%s from the VM: %w", key, err)
		}
		stripped = append(stripped, key)
	}

	return stripped, nil
}
