// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/packer-plugin-sdk/packer"

	"xenapi"
)

const testExportHost = "10.0.0.1"

// The URL used to format the *xenapi.Session with %v, dumping the struct where the session
// reference belonged: malformed, and a leak.
func TestExportXVAURL_AuthenticatesWithTheSessionReference(t *testing.T) {
	raw := exportXVAURL(testExportHost, "use_compression=true&", "vm-uuid-123", xenapi.SessionRef(testSessionRef))

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the export URL does not parse: %s", err)
	}
	if got := parsed.Query().Get("session_id"); got != testSessionRef {
		t.Errorf("session_id = %q, want the session reference itself", got)
	}
	if got := parsed.Query().Get("uuid"); got != "vm-uuid-123" {
		t.Errorf("uuid = %q, want \"vm-uuid-123\"", got)
	}
	if got := parsed.Query().Get("use_compression"); got != "true" {
		t.Errorf("use_compression = %q, want \"true\"", got)
	}
	if parsed.Scheme != "https" {
		t.Errorf("scheme = %q, want https", parsed.Scheme)
	}
}

// /export_raw_vdi used to carry the pool password as HTTP Basic userinfo, which put it in
// any log line echoing the URL and in every error net/http built from it.
func TestExportVDIURL_CarriesNoPassword(t *testing.T) {
	raw := exportVDIURL(testExportHost, xenapi.SessionRef(testSessionRef), "vdi-uuid-456", "&format=vhd")

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the export URL does not parse: %s", err)
	}
	if parsed.User != nil {
		t.Errorf("the URL carries userinfo: %s", parsed.User)
	}
	if got := parsed.Query().Get("session_id"); got != testSessionRef {
		t.Errorf("session_id = %q, want the session reference", got)
	}
	if got := parsed.Query().Get("vdi"); got != "vdi-uuid-456" {
		t.Errorf("vdi = %q, want \"vdi-uuid-456\"", got)
	}
	if got := parsed.Query().Get("format"); got != "vhd" {
		t.Errorf("format = %q, want \"vhd\"", got)
	}
}

// The raw format passes no format at all, which is what XAPI defaults to.
func TestExportVDIURL_OmitsTheFormatForRaw(t *testing.T) {
	raw := exportVDIURL(testExportHost, xenapi.SessionRef(testSessionRef), "vdi-uuid-456", "")

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the export URL does not parse: %s", err)
	}
	if parsed.Query().Has("format") {
		t.Errorf("a raw export should name no format, got %q", parsed.Query().Get("format"))
	}
}

// Both URLs are printed to the build log, so both have to survive redactURL with nothing
// sensitive left in them.
func TestExportURLs_AreSafeToLog(t *testing.T) {
	for name, raw := range map[string]string{
		"xva": exportXVAURL(testExportHost, "", "vm-uuid-123", xenapi.SessionRef(testSessionRef)),
		"vdi": exportVDIURL(testExportHost, xenapi.SessionRef(testSessionRef), "vdi-uuid-456", ""),
	} {
		if redacted := redactURL(raw); strings.Contains(redacted, testSessionRef) {
			t.Errorf("%s: the session reference survived redaction: %s", name, redacted)
		}
	}
}

// The line, not the helper: what protects the session reference is this log line calling
// redactURL, and only a test that builds the line itself notices if it stops.
func TestExportLogLine_HidesTheSessionReference(t *testing.T) {
	exportURL := exportXVAURL(testExportHost, "use_compression=true&", "vm-uuid-123", xenapi.SessionRef(testSessionRef))

	line := exportLogLine(exportURL)

	if strings.Contains(line, testSessionRef) {
		t.Errorf("the session reference reached the build log: %s", line)
	}
	if !strings.Contains(line, "vm-uuid-123") {
		t.Errorf("the log line no longer says what is being exported: %s", line)
	}
}

func TestXeExportLogLine_HidesThePassword(t *testing.T) {
	args := xeExportArgs(testExportHost, "root", "hunter2", "vm-uuid-123", "compress=true", "out/vm.xva")

	line := xeExportLogLine("/usr/bin/xe", args)

	if strings.Contains(line, "hunter2") {
		t.Errorf("the pool password reached the build log: %s", line)
	}
	if !strings.Contains(line, "vm-export") {
		t.Errorf("the log line no longer says what is being run: %s", line)
	}
}

// XAPI answers an unauthorised or unsupported export with a short XML error document.
// Streaming that into the artifact writes a file that only fails at import time.
func TestDownloadFile_RejectsANonOKResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<error class=\"session_invalid\"/>")
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "export.xva")
	err := downloadFile(context.Background(), server.Client(), server.URL, target, packer.TestUi(t))

	if err == nil {
		t.Fatal("a 403 was reported as a successful export")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should report the status that failed", err)
	}
}

// CA-431816 restricts the upload to 302, because 301 and 303 license a change of method.
// It spares the export, which is a GET either way -- all three statuses have to be
// followed, so a CheckRedirect on Connection.HTTPClient modelled on the upload would
// break it. 302 alone cannot show that: it is the one the upload follows too.
func TestDownloadFile_FollowsARedirectToTheHostHoldingTheVDI(t *testing.T) {
	const exported = "exported bytes"

	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, exported)
	}))
	defer owner.Close()

	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		contacted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, owner.URL+"/export_raw_vdi", status)
		}))

		target := filepath.Join(t.TempDir(), "disk.raw")
		err := downloadFile(context.Background(), contacted.Client(), contacted.URL+"/export_raw_vdi", target, packer.TestUi(t))
		contacted.Close()
		if err != nil {
			t.Errorf("%d: unexpected error: %s", status, err)
			continue
		}

		written, err := os.ReadFile(target)
		if err != nil {
			t.Errorf("%d: unexpected error: %s", status, err)
			continue
		}
		if string(written) != exported {
			t.Errorf("%d: the export wrote %q, want the redirected host's body", status, written)
		}
	}
}

// writeExport puts content at a temporary path and returns the path.
func writeExport(t *testing.T, name string, content []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("could not write the test artifact: %s", err)
	}
	return path
}

// incompressible returns n bytes that gzip cannot shrink, so a real gzip stream built
// from them clears minExportSize. A repeating pattern would compress to a few bytes and
// fail the size check for the wrong reason.
func incompressible(t *testing.T, n int) []byte {
	t.Helper()

	buf := make([]byte, n)
	// A fixed seed keeps the test deterministic.
	_, _ = rand.New(rand.NewSource(1)).Read(buf)
	return buf
}

// XAPI writes the 200 for /export before the tar, so a later failure arrives as a short
// body under a successful status that downloadFile cannot see. Writing it to <vm>.xva and
// reporting success is the silent false-green CA-430513 reported.
func TestValidateExportedXVA_RejectsAnErrorDocument(t *testing.T) {
	path := writeExport(t, "export.xva", []byte("<error class=\"session_invalid\"/>"))

	err := validateExportedXVA(path)

	if err == nil {
		t.Fatal("an XML error document was accepted as an XVA")
	}
	if !strings.Contains(err.Error(), "too small") {
		t.Errorf("error %q should say the artifact is too small", err)
	}
}

// `xe vm-export` exits 0 for failures it handles after opening the output file, leaving
// nothing on disk. Only the artifact says so.
func TestValidateExportedXVA_RejectsAnEmptyFile(t *testing.T) {
	path := writeExport(t, "export.xva", nil)

	if err := validateExportedXVA(path); err == nil {
		t.Fatal("an empty file was accepted as an XVA")
	}
}

// A size floor alone is not enough: a body large enough to clear it and still not be an
// archive fails at import instead of at build time.
func TestValidateExportedXVA_RejectsAFileThatIsBigEnoughButNotAnArchive(t *testing.T) {
	body := []byte("<html><body>Internal Server Error</body></html>")
	body = append(body, bytes.Repeat([]byte(" "), minExportSize)...)
	path := writeExport(t, "export.xva", body)

	err := validateExportedXVA(path)

	if err == nil {
		t.Fatal("an HTML error page was accepted as an XVA")
	}
	if !strings.Contains(err.Error(), "not an XVA") {
		t.Errorf("error %q should say the artifact is not an XVA", err)
	}
}

// The plugin always asks for use_compression=true, so this is the shape a successful
// export arrives in.
func TestValidateExportedXVA_AcceptsAGzipStream(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(incompressible(t, 4096)); err != nil {
		t.Fatalf("could not build the gzip stream: %s", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("could not close the gzip stream: %s", err)
	}
	path := writeExport(t, "export.xva", buf.Bytes())

	if err := validateExportedXVA(path); err != nil {
		t.Errorf("a gzip stream was rejected: %s", err)
	}
}

// A pool that ignores use_compression returns the tar itself, which is a valid artifact.
func TestValidateExportedXVA_AcceptsAnUncompressedTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := incompressible(t, 2048)
	if err := tw.WriteHeader(&tar.Header{Name: "ova.xml", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatalf("could not build the tar header: %s", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("could not build the tar: %s", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("could not close the tar: %s", err)
	}
	path := writeExport(t, "export.xva", buf.Bytes())

	if err := validateExportedXVA(path); err != nil {
		t.Errorf("an uncompressed tar was rejected: %s", err)
	}
}

func TestValidateExportedXVA_RejectsAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-written.xva")

	if err := validateExportedXVA(path); err == nil {
		t.Fatal("a missing artifact was reported as a successful export")
	}
}

// /export_raw_vdi writes its 200 before the copy too, so the disk formats get the size
// check. A raw disk has no magic number to look for.
func TestValidateExportedFile_RejectsAnErrorDocumentButAcceptsADiskImage(t *testing.T) {
	tooSmall := writeExport(t, "disk.vhd", []byte("<error class=\"vdi_io_error\"/>"))
	if err := validateExportedFile(tooSmall); err == nil {
		t.Error("an XML error document was accepted as a disk image")
	}

	bigEnough := writeExport(t, "disk.raw", incompressible(t, minExportSize))
	if err := validateExportedFile(bigEnough); err != nil {
		t.Errorf("a disk image was rejected: %s", err)
	}
}

// The two halves together: a 200 satisfies downloadFile, so only the artifact check
// stands between a failed export and a build that reports success.
func TestDownloadFile_A200ThatIsNotAnExportIsCaughtByTheArtifactCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// XAPI has already written the status line by the time the export fails.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<error class=\"vdi_missing\"/>")
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "export.xva")

	if err := downloadFile(context.Background(), server.Client(), server.URL, target, packer.TestUi(t)); err != nil {
		t.Fatalf("the download itself should succeed on a 200: %s", err)
	}
	if err := validateExportedXVA(target); err == nil {
		t.Fatal("a 200 carrying an error document was reported as a successful export")
	}
}

// Connection.Host is pasted into every export URL, so a scheme left in it by
// NewXenAPIClient -- as a case-sensitive strip would do to the "HTTPS://pool" that
// CommonConfig.Prepare accepts -- only shows up here, as a URL pointing nowhere.
func TestExportXVAURL_FromAHostThatWasWrittenWithAScheme(t *testing.T) {
	host := stripScheme("HTTPS://xs.example.com:443")

	raw := exportXVAURL(host, "", "vm-uuid-123", xenapi.SessionRef(testSessionRef))

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the export URL does not parse: %s", err)
	}
	if parsed.Host != "xs.example.com:443" {
		t.Errorf("host = %q, want \"xs.example.com:443\"", parsed.Host)
	}
	if parsed.Path != "/export" {
		t.Errorf("path = %q, want \"/export\"", parsed.Path)
	}
}
