// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"net/url"
)

// redactedValue stands in for a credential in anything the plugin prints.
const redactedValue = "***"

// sensitiveQueryParams are the query-string keys that carry a credential. XAPI's HTTP
// handlers authenticate with the session reference, which is a bearer token for the whole
// pool: as sensitive as the password it was obtained with.
var sensitiveQueryParams = []string{"session_id", "password", "pwd"}

// redactURL returns urlString with every credential replaced and the rest left readable:
// the reason to log a XAPI URL at all is to see which handler was called for which
// object. net/url's own redaction covers the userinfo password only, so it leaves a
// session_id in the query string untouched.
func redactURL(urlString string) string {
	u, err := url.Parse(urlString)
	if err != nil {
		// Echoing back an unparseable string could print the very credential
		// this exists to hide.
		return "<unparseable URL>"
	}

	if u.User != nil {
		u.User = url.UserPassword(redactedValue, redactedValue)
	}

	query := u.Query()
	for _, key := range sensitiveQueryParams {
		if query.Has(key) {
			query.Set(key, redactedValue)
		}
	}
	u.RawQuery = query.Encode()

	return u.String()
}

// redactURLError rewrites the URL carried by an *url.Error. net/http builds one of those
// out of the request URL for every failed request, and that error goes to ui.Error, which
// is the build log.
func redactURLError(err error) error {
	urlErr, ok := err.(*url.Error)
	if !ok {
		return err
	}

	return &url.Error{Op: urlErr.Op, URL: redactURL(urlErr.URL), Err: urlErr.Err}
}

// redactArgs returns a copy of an argv with every credential-bearing flag's value
// replaced, for logging. `xe` takes the password on the command line, so the argv cannot
// avoid holding it; the log line can.
func redactArgs(args []string) []string {
	redacted := make([]string, len(args))
	copy(redacted, args)

	for i, arg := range redacted {
		if arg == "-pw" && i+1 < len(redacted) {
			redacted[i+1] = redactedValue
		}
	}

	return redacted
}
