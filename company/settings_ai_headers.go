// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// userSettingAIHeaders holds extra request headers, stored one "Name: value"
// per line, for a gateway that wants more than a bearer key. Stored as a secret:
// these headers are usually credentials too.
const userSettingAIHeaders = "company.ai.headers"

const aiHeadersMax = 20

// aiHeadersReserved are set by the transport or by the request itself;
// overriding them breaks the request rather than authenticating it.
var aiHeadersReserved = map[string]bool{
	"Host": true, "Content-Length": true, "Content-Type": true, "Accept": true,
	"Transfer-Encoding": true, "Connection": true, "Upgrade": true, "Te": true, "Trailer": true,
	"Keep-Alive": true, "Proxy-Connection": true,
}

// parseAIHeaders reads the stored form. Blank lines are skipped; anything else
// must be a valid "Name: value".
func parseAIHeaders(text string) (http.Header, error) {
	headers := http.Header{}
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return nil, fmt.Errorf("line %d: expected \"Name: value\"", i+1)
		}
		name = http.CanonicalHeaderKey(name)
		if aiHeadersReserved[name] {
			return nil, fmt.Errorf("line %d: %s cannot be set", i+1, name)
		}
		headers.Add(name, value)
	}
	if len(headers) > aiHeadersMax {
		return nil, fmt.Errorf("at most %d headers", aiHeadersMax)
	}
	return headers, nil
}

// formAIHeaders turns the settings page's name/value rows into the stored form.
// Values are never sent back to the page, so a saved header's row comes back
// with a blank value, which keeps what was saved; a row that is gone is removed.
func formAIHeaders(names, values []string, saved http.Header) (string, error) {
	var b strings.Builder
	for i, name := range names {
		name = strings.TrimSpace(name)
		value := ""
		if i < len(values) {
			value = strings.TrimSpace(values[i])
		}
		if name == "" && value == "" {
			continue
		}
		if name == "" {
			return "", fmt.Errorf("row %d: a value needs a name", i+1)
		}
		kept := []string{value}
		if value == "" {
			if kept = saved.Values(name); len(kept) == 0 {
				return "", fmt.Errorf("%s: enter a value", name)
			}
		}
		for _, v := range kept {
			if strings.ContainsAny(name, ":\r\n") || strings.ContainsAny(v, "\r\n") {
				return "", fmt.Errorf("row %d: invalid header", i+1)
			}
			fmt.Fprintf(&b, "%s: %s\n", name, v)
		}
	}
	if _, err := parseAIHeaders(b.String()); err != nil {
		return "", err
	}
	return b.String(), nil
}

// applyAIHeaders runs after the provider's own auth headers, so a gateway
// that wants its key under Authorization in another form can have it.
func applyAIHeaders(req *http.Request, headers http.Header) {
	maps.Copy(req.Header, headers)
}

// aiHeaderNames is what the settings page shows of a saved set: the names,
// never the values.
func aiHeaderNames(headers http.Header) []string {
	return slices.Sorted(maps.Keys(headers))
}
