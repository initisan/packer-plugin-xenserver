// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/mitchellh/go-vnc"
)

const (
	// The console proxy answers long before the guest's VNC server does, so an attempt
	// made moments after the VM is unpaused can fail for reasons that clear on their
	// own. Before CA-430515 a single failure ended the build.
	consoleConnectAttempts = 5
	consoleConnectBackoff  = 3 * time.Second

	// consoleHandshakeTimeout caps every part of establishing the session: a proxy that
	// accepts the connection and then sends nothing must fail rather than block.
	consoleHandshakeTimeout = 30 * time.Second

	// consoleWriteTimeout only guards a socket that never drains: typing has no retry, so
	// a write that times out ends the build. Ctrl-C is handled by closing the connection.
	consoleWriteTimeout = 5 * time.Minute
)

// permanentError marks a failure that retrying cannot fix. It stops the retry
// loop, so the build reports the real reason once instead of five times.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }

func (e permanentError) Unwrap() error { return e.err }

// withCloseWatchdog runs fn and closes conn if fn has not returned within timeout,
// reporting whether it had to. It closes rather than setting a deadline because the
// deadline belongs to crypto/tls, which arms one of its own around the closeNotify alert
// in Close without holding anything this could interlock with.
func withCloseWatchdog(conn net.Conn, timeout time.Duration, fn func()) bool {
	var mu sync.Mutex
	done := false
	expired := false

	watchdog := time.AfterFunc(timeout, func() {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return
		}
		expired = true
		conn.Close()
	})

	fn()

	mu.Lock()
	done = true
	fired := expired
	mu.Unlock()
	watchdog.Stop()

	return fired
}

// bufferedConn is the console connection as the VNC client sees it. Reads come from br, so
// bytes the CONNECT framing pulled off the socket early are still delivered first.
// writeTimeout is a test seam; leaving it unset takes consoleWriteTimeout.
type bufferedConn struct {
	net.Conn
	br           *bufio.Reader
	writeTimeout time.Duration
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.br.Read(p)
}

// writeBound is consoleWriteTimeout unless the test seam overrides it. Unset means the
// default, never "no cap at all".
func (c *bufferedConn) writeBound() time.Duration {
	if c.writeTimeout <= 0 {
		return consoleWriteTimeout
	}
	return c.writeTimeout
}

func (c *bufferedConn) Write(p []byte) (int, error) {
	bound := c.writeBound()

	var n int
	var err error
	if withCloseWatchdog(c.Conn, bound, func() { n, err = c.Conn.Write(p) }) {
		return n, fmt.Errorf("the VM console accepted no data for %s: %w", bound, os.ErrDeadlineExceeded)
	}

	return n, err
}

// parseHTTPStatusCode reads the numeric code out of a status line such as
// "HTTP/1.1 200 OK".
func parseHTTPStatusCode(statusLine string) (int, error) {
	fields := strings.Fields(statusLine)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, fmt.Errorf("the xapi console proxy did not answer with an HTTP status line: %q", statusLine)
	}

	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("the xapi console proxy returned an unreadable HTTP status: %q", statusLine)
	}

	return code, nil
}

// readHTTPHeader consumes one HTTP header block from br, up to and including the blank
// line that ends it, and returns the status line. Whatever follows stays buffered in br.
func readHTTPHeader(br *bufio.Reader) (string, error) {
	tp := textproto.NewReader(br)

	statusLine, err := tp.ReadLine()
	if err != nil {
		return "", fmt.Errorf("failed to read the console CONNECT response: %w", err)
	}

	for {
		line, err := tp.ReadLine()
		if err != nil {
			return statusLine, fmt.Errorf("failed to read the console CONNECT response header: %w", err)
		}
		if line == "" {
			return statusLine, nil
		}
	}
}

// connectConsoleProxy performs the HTTP CONNECT handshake for consoleLocation and returns
// a reader positioned on the first byte after the response header, plus the status line.
//
// The framing is the whole point. xapi writes the header block and then copies the guest's
// raw RFB stream onto the same socket, so when both arrive in one TLS record a single Read
// swallows the 12-byte "RFB 003.00x\n" banner too and the VNC client reports "error
// parsing ProtocolVersion" (CA-430515). Reading a line at a time stops on the blank line.
func connectConsoleProxy(conn io.ReadWriter, consoleHost, consoleLocation, sessionRef string) (*bufio.Reader, string, error) {
	// The Cookie header carries the session reference, so the request itself is
	// a credential and is never printed.
	request := fmt.Sprintf("CONNECT %s HTTP/1.0\r\nHost: %s\r\nCookie: session_id=%s\r\n\r\n",
		consoleLocation, consoleHost, sessionRef)

	if _, err := io.WriteString(conn, request); err != nil {
		return nil, "", fmt.Errorf("failed to send the console CONNECT request: %w", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := readHTTPHeader(br)
	if err != nil {
		return nil, statusLine, err
	}

	code, err := parseHTTPStatusCode(statusLine)
	if err != nil {
		return nil, statusLine, err
	}
	if code < 200 || code > 299 {
		return nil, statusLine, fmt.Errorf("the xapi console proxy refused the connection: %s", statusLine)
	}

	return br, statusLine, nil
}

// handshakeVNC runs the RFB handshake under a watchdog and hands back the connected
// client. The watchdog closes the connection rather than setting a read deadline, which
// would later expire under the goroutine vnc.Client leaves reading it.
func handshakeVNC(conn net.Conn, timeout time.Duration) (*vnc.ClientConn, error) {
	var vncClient *vnc.ClientConn
	var err error

	timedOut := withCloseWatchdog(conn, timeout, func() {
		vncClient, err = vnc.Client(conn, &vnc.ClientConfig{Exclusive: true})
	})

	if timedOut {
		// The watchdog closed the connection. Whether or not vnc.Client noticed
		// in time, the session is not usable.
		if err == nil {
			vncClient.Close()
		}
		return nil, fmt.Errorf("timed out after %s waiting for the VM console VNC handshake", timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("error establishing VNC session: %w", err)
	}

	return vncClient, nil
}

// dialConsoleVNC makes one attempt at a VNC session on the VM console: TCP, TLS, HTTP
// CONNECT, then the RFB handshake. The caller owns and must close the returned client.
func dialConsoleVNC(ctx context.Context, ui packer.Ui, c *Connection, consoleHost, consoleLocation string) (*vnc.ClientConn, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(consoleHost, "443"), consoleHandshakeTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the VM console: %w", err)
	}

	connected := false
	defer func() {
		if !connected {
			conn.Close()
		}
	}()

	// Nothing reads this connection in the background yet, so a deadline is
	// safe here. It is cleared before the VNC client starts its read loop.
	if err := conn.SetDeadline(time.Now().Add(consoleHandshakeTimeout)); err != nil {
		return nil, fmt.Errorf("failed to set a deadline on the VM console connection: %w", err)
	}

	// The console is another channel to the same pool, so it gets the same TLS policy as
	// the XenAPI session. tls.Client has no URL to take the name from, so ServerName has
	// to be set explicitly -- and it may be a different host in the pool than
	// xenserver_host, which is why ca_cert_path is documented as taking a bundle.
	tlsConfig := c.TLSConfig()
	tlsConfig.ServerName = consoleHost
	tlsConn := tls.Client(conn, tlsConfig)

	// Handshake here rather than on the first write, so a certificate problem is
	// reported as one. Only a trust failure is permanent; a reset or a timeout here
	// is what the retry exists for.
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		wrapped := describeTLSError(consoleHost,
			fmt.Errorf("failed to establish a TLS connection to the VM console: %w", err))
		var verifyErr *tls.CertificateVerificationError
		if errors.As(err, &verifyErr) {
			return nil, permanentError{wrapped}
		}
		return nil, wrapped
	}

	ui.Say(fmt.Sprintf("Making HTTP request to initiate VNC connection: CONNECT %s", consoleLocation))
	br, statusLine, err := connectConsoleProxy(tlsConn, consoleHost, consoleLocation, string(c.GetSessionRef()))
	if err != nil {
		return nil, err
	}
	ui.Say(fmt.Sprintf("Received response: %s", statusLine))

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("failed to clear the deadline on the VM console connection: %w", err)
	}

	vncClient, err := handshakeVNC(&bufferedConn{Conn: tlsConn, br: br}, consoleHandshakeTimeout)
	if err != nil {
		return nil, err
	}

	connected = true
	return vncClient, nil
}

// retryConsoleConnect runs attempt until it succeeds, until it fails permanently, until
// the attempts run out, or until the build is cancelled. It waits backoff between attempts.
func retryConsoleConnect(state multistep.StateBag, ui packer.Ui, attempts int, backoff time.Duration, attempt func() error) error {
	var lastErr error

	for i := 1; i <= attempts; i++ {
		if _, cancelled := state.GetOk(multistep.StateCancelled); cancelled {
			return InterruptedError{}
		}

		err := attempt()
		if err == nil {
			return nil
		}

		var permanent permanentError
		if errors.As(err, &permanent) {
			return err
		}
		lastErr = err

		if i == attempts {
			break
		}

		ui.Say(fmt.Sprintf("VM console VNC attempt %d of %d failed (%s). Retrying in %s...",
			i, attempts, err, backoff))
		if waitErr := (InterruptibleWait{Timeout: backoff}).Wait(state); waitErr != nil {
			return waitErr
		}
	}

	return fmt.Errorf("could not establish a VNC session with the VM console after %d attempts: %w",
		attempts, lastErr)
}

// establishConsoleVNC connects to the VM console over VNC, retrying transient
// failures. The caller owns the returned client and must close it.
func establishConsoleVNC(ctx context.Context, state multistep.StateBag, ui packer.Ui, c *Connection, consoleHost, consoleLocation string) (*vnc.ClientConn, error) {
	var vncClient *vnc.ClientConn

	err := retryConsoleConnect(state, ui, consoleConnectAttempts, consoleConnectBackoff, func() error {
		var attemptErr error
		vncClient, attemptErr = dialConsoleVNC(ctx, ui, c, consoleHost, consoleLocation)
		return attemptErr
	})
	if err != nil {
		return nil, err
	}

	return vncClient, nil
}
