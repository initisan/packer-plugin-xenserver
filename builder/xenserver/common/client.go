// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"xenapi"

	version "github.com/xenserver/packer-plugin-xenserver/version"
)

// GetDisks returns every disk-type VBD's VDI, root disk included (CDs and floppies are
// skipped).
func GetDisks(c *Connection, vmRef xenapi.VMRef) (vdis []xenapi.VDIRef, err error) {
	vdis = make([]xenapi.VDIRef, 0)
	vbds, err := xenapi.VM.GetVBDs(c.session, vmRef)
	if err != nil {
		return nil, err
	}

	for _, vbd := range vbds {
		rec, err := xenapi.VBD.GetRecord(c.session, vbd)
		if err != nil {
			return nil, err
		}
		if rec.Type == xenapi.VbdTypeDisk {
			vdis = append(vdis, rec.VDI)
		}
	}
	return vdis, nil
}

// ConnectVdi attaches a VDI to a VM. Callers must do so before VM.Start: XAPI plugs VBDs
// at boot, so there is deliberately no VBD.plug here and attaching to an already-running
// VM would silently not reach the guest.
func ConnectVdi(c *Connection, vmRef xenapi.VMRef, vdiRef xenapi.VDIRef, vbdType xenapi.VbdType) error {
	mode, bootable := xenapi.VbdModeRW, false
	if vbdType == xenapi.VbdTypeCD {
		mode, bootable = xenapi.VbdModeRO, true
	}

	// other-config:owner is what makes xe vm-uninstall and XenCenter delete the VDI along
	// with the VM. Data disks only, as xapi itself marks them (xapi_vm_clone.ml, xe
	// vm-disk-add): a CD here holds an ISO, and vm-uninstall applies no type filter before
	// destroying what it is handed.
	var otherConfig map[string]string
	if vbdType == xenapi.VbdTypeDisk {
		otherConfig = map[string]string{"owner": ""}
	}

	_, err := xenapi.VBD.Create(c.session, xenapi.VBDRecord{
		VM:               vmRef,
		VDI:              vdiRef,
		Userdevice:       "autodetect",
		Empty:            false,
		QosAlgorithmType: "",
		Mode:             mode,
		Unpluggable:      false,
		Bootable:         bootable,
		Type:             vbdType,
		OtherConfig:      otherConfig,
	})

	return err
}

// DisconnectVdi unplugs and destroys the VBD attaching vdi to vmRef. The VDI itself is
// left alone.
func DisconnectVdi(c *Connection, vmRef xenapi.VMRef, vdi xenapi.VDIRef) error {
	vbds, err := xenapi.VM.GetVBDs(c.session, vmRef)
	if err != nil {
		return fmt.Errorf("unable to get VM VBDs: %w", err)
	}

	for _, vbd := range vbds {
		rec, err := xenapi.VBD.GetRecord(c.session, vbd)
		if err != nil {
			return fmt.Errorf("could not get record for VBD '%s': %w", vbd, err)
		}
		if rec.VDI == vdi {
			_ = xenapi.VBD.Unplug(c.session, vbd)
			if err := xenapi.VBD.Destroy(c.session, vbd); err != nil {
				return fmt.Errorf("could not destroy VBD '%s': %w", vbd, err)
			}

			return nil
		}
	}

	return fmt.Errorf("could not find VBD for VDI '%s'", vdi)
}

func ConnectNetwork(c *Connection, networkRef xenapi.NetworkRef, vmRef xenapi.VMRef, device string) (*xenapi.VIFRef, error) {
	vif, err := xenapi.VIF.Create(c.session, xenapi.VIFRecord{
		Network:     networkRef,
		VM:          vmRef,
		Device:      device,
		LockingMode: xenapi.VifLockingModeNetworkDefault,
	})

	if err != nil {
		return nil, err
	}
	log.Printf("Created the following VIF: %s", vif)

	return &vif, nil
}

type Connection struct {
	session  *xenapi.Session
	ref      xenapi.SessionRef
	Host     string
	Username string
	Password string
	// TLS policy for the plugin's HTTP and VNC channels; the XenAPI session
	// gets its own from the goSDK.
	tlsConfig *tls.Config
}

func (c Connection) GetSession() *xenapi.Session {
	return c.session
}

// Logout drops the session on the pool master. Without it the record sits there until the
// server-side timeout, and a CI pool doing many builds a day accumulates them.
func (c Connection) Logout() {
	if c.session == nil {
		return
	}
	if err := c.session.Logout(); err != nil {
		log.Printf("Unable to log out of the XAPI session: %s", err.Error())
	}
}

// String keeps the connection out of any log line that formats it: it carries the
// password and the session reference, and %v is not how either should escape.
func (c Connection) String() string {
	return fmt.Sprintf("Connection{Host: %s, Username: %s}", c.Host, c.Username)
}

// GoString does the same for %#v.
func (c Connection) GoString() string {
	return c.String()
}

// TLSConfig returns a copy of the TLS policy the plugin uses on its own channels to the
// pool: a build also opens /export, /export_raw_vdi, /import_raw_vdi and /console, so all
// of them verify it the same way. The copy is what stops one caller's ServerName or
// NextProtos from reaching the others.
func (c *Connection) TLSConfig() *tls.Config {
	return c.tlsConfig.Clone()
}

// HTTPClient returns an http.Client carrying the policy newTLSConfig built, so it
// verifies the pool on the same terms as the XenAPI session. The export handlers are
// reached through it; the upload is not, because it has to read XAPI's answer before it
// sends anything, which net/http cannot express (see putFollowingRedirects).
func (c *Connection) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: c.TLSConfig()}}
}

// poolDialTimeout caps reaching a host in the pool, as consoleHandshakeTimeout does for
// the console: a host that accepts the connection and never finishes the handshake must
// not hold the build.
const poolDialTimeout = 30 * time.Second

// DialTLS opens one connection to a host in the pool, carrying the policy HTTPClient
// hands net/http. host is an authority, and after a redirect it names another host in
// the pool, which is why ca_cert_path takes a bundle. tls.DialWithDialer fills ServerName
// from it, whether it is a name or an address.
func (c *Connection) DialTLS(host string) (net.Conn, error) {
	if strings.TrimSpace(host) == "" {
		return nil, errors.New("no host to connect to")
	}

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: poolDialTimeout}, "tcp",
		poolHostAddress(host), c.TLSConfig())
	if err != nil {
		return nil, describeTLSError(host, err)
	}

	return conn, nil
}

func pluginUserAgent() string {
	return fmt.Sprintf("XenServerPacker/%s", version.PluginVersion.FormattedVersion())
}

// poolHostAddress gives an authority the XAPI port if it carries none. url.URL.Host keeps
// an IPv6 literal in its brackets and net.JoinHostPort adds its own; SplitHostPort accepts
// a trailing colon and calls the empty port a port.
func poolHostAddress(host string) string {
	if name, port, err := net.SplitHostPort(host); err == nil {
		if port == "" {
			port = "443"
		}
		return net.JoinHostPort(name, port)
	}

	return net.JoinHostPort(strings.Trim(host, "[]"), "443")
}

// loadCACertPool reads a PEM bundle into a pool. CommonConfig.Prepare calls it too, so a
// bad ca_cert_path is reported before the build starts rather than by the goSDK's
// log.Fatal partway through.
func loadCACertPool(caCertPath string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("could not read ca_cert_path '%s': %w", caCertPath, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("ca_cert_path '%s' contains no PEM-encoded certificate. "+
			"`xe host-get-server-certificate host=<uuid>` writes one out in the expected form", caCertPath)
	}

	return pool, nil
}

// newTLSConfig builds the TLS policy for the plugin's own HTTP and VNC channels. The
// goSDK builds a separate one for the JSON-RPC session (goSDK/jsonrpc_client.go); this
// deliberately matches it -- same minimum version, same cipher suites, and the same
// ca_cert_path or insecure choice -- so every channel the build opens to the pool is
// verified the same way.
func newTLSConfig(insecure bool, caCertPath string) (*tls.Config, error) {
	config := &tls.Config{
		MinVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},
	}

	if insecure {
		// #nosec G402 -- the build asked for this explicitly with `insecure`.
		config.InsecureSkipVerify = true
		return config, nil
	}

	pool, err := loadCACertPool(caCertPath)
	if err != nil {
		return nil, err
	}
	config.RootCAs = pool

	return config, nil
}

// describeTLSError turns the two x509 failures a XenServer build actually runs into
// something the user can act on. Every host ships a self-signed certificate issued to
// itself, so both are common first-run experiences.
func describeTLSError(host string, err error) error {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError

	switch {
	case errors.As(err, &unknownAuthority):
		return fmt.Errorf("TLS verification of '%s' failed: %w.\n"+
			"A XenServer host's default certificate is self-signed, so it has to be trusted explicitly: "+
			"write it out with `xe host-get-server-certificate host=<uuid>` and point ca_cert_path at it. "+
			"For a pool, concatenate every host's certificate into one PEM file — an export or console "+
			"connection can be served by any host in the pool, not just the one you connected to. "+
			"Setting insecure = true skips verification altogether and is for non-production use only",
			host, err)
	case errors.As(err, &hostname):
		return fmt.Errorf("TLS verification of '%s' failed: %w.\n"+
			"Reach the host by a name or address its certificate is issued for, or reissue the "+
			"certificate to cover the one you use", host, err)
	}

	return err
}

// normalizeXenAPIHost turns the free text of xenserver_host into the two forms a build
// needs: the URL the XenAPI session logs in to, and the bare host that step_export,
// step_upload_vdi and the console paste into URLs of their own.
//
// Producing both here is what stops them disagreeing about where the scheme ends. A
// scheme is case-insensitive, so "HTTPS://pool" is legal; recognising it that way but
// stripping it case-sensitively once left every later URL reading
// "https://HTTPS://pool/export".
func normalizeXenAPIHost(host string) (sessionURL, urlHost string, err error) {
	// Prepare rejects http:// and every other scheme, so this only has to supply a missing
	// one; testing for "://" rather than an "http" prefix keeps a host named "httpserver"
	// from looking like it already has one.
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	// A pasted trailing slash would otherwise reach the handlers as "...//export".
	host = strings.TrimSuffix(host, "/")

	if !strings.HasPrefix(strings.ToLower(host), "https://") {
		return "", "", fmt.Errorf("refusing to connect to '%s': the XenAPI session must be over https", host)
	}

	return host, stripScheme(host), nil
}

func NewXenAPIClient(host, username, password string, insecure bool, caCertPath string) (*Connection, error) {
	log.Printf("XenServer Packer Plugin Version: %s", version.PluginVersion.FormattedVersion())

	host, hostForURLs, err := normalizeXenAPIHost(host)
	if err != nil {
		return nil, err
	}
	if !insecure && caCertPath == "" {
		return nil, errors.New("refusing to connect: certificate verification is on but no ca_cert_path was given")
	}

	tlsConfig, err := newTLSConfig(insecure, caCertPath)
	if err != nil {
		return nil, err
	}

	opts := &xenapi.ClientOpts{
		URL: host,
		Headers: map[string]string{
			"User-Agent": pluginUserAgent(),
		},
	}
	if !insecure {
		// ServerCert is the goSDK's own field name; it takes the CA certificate path.
		opts.SecureOpts = &xenapi.SecureOpts{
			ServerCert: caCertPath,
		}
	}
	session := xenapi.NewSession(opts)

	ref, err := session.LoginWithPassword(username, password, "1.0", "packer")
	if err != nil {
		return nil, describeTLSError(host, err)
	}

	return &Connection{
		session:   session,
		ref:       ref,
		Host:      hostForURLs,
		Username:  username,
		Password:  password,
		tlsConfig: tlsConfig,
	}, nil
}

func (c *Connection) GetSessionRef() xenapi.SessionRef {
	return c.ref
}
