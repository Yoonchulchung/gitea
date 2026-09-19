// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	files_service "gitea.dev/services/repository/files"

	"go.yaml.in/yaml/v4"
)

// The company blocks some sites at its own edge, and an app on this
// platform must not become the way round that. The block list is the one
// outbound rule above every other: an entry here refuses the destination
// for every app, in every network mode, whatever its own allow list or an
// approval says. It lives at the top of apps.yml — one file, one history —
// and every request the broker refuses on it is in the app's outbound log.
//
// An entry is a host ("example.com"), a domain and everything under it
// ("*.example.com"), a host with a port ("example.com:8443"), or a port
// alone (":25"). The broker always dials 443, so a port entry other than
// 443 refuses nothing today; it is kept for the day the broker learns
// another port, and so the list can say what the company means.

// normalizeBlockEntry checks and canonicalises one entry.
func normalizeBlockEntry(raw string) (string, error) {
	entry := strings.ToLower(strings.TrimSpace(raw))
	entry = strings.TrimPrefix(strings.TrimPrefix(entry, "https://"), "http://")
	entry = strings.TrimSuffix(entry, "/")
	if entry == "" {
		return "", errors.New("empty")
	}
	host, port := entry, ""
	if i := strings.LastIndexByte(entry, ':'); i >= 0 {
		host, port = entry[:i], entry[i+1:]
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return "", fmt.Errorf("%q is not a port", port)
		}
	}
	if host == "" && port == "" {
		return "", errors.New("empty")
	}
	if host != "" {
		bare := strings.TrimPrefix(host, "*.")
		if bare == "" || strings.ContainsAny(bare, " /\\?#@*") {
			return "", fmt.Errorf("%q is not a host name", host)
		}
	}
	if port == "" {
		return host, nil
	}
	return host + ":" + port, nil
}

// blockedBy reports the entry that refuses host:port, or "".
func blockedBy(entries []string, host string, port int) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, entry := range entries {
		eHost, ePort := entry, ""
		if i := strings.LastIndexByte(entry, ':'); i >= 0 {
			eHost, ePort = entry[:i], entry[i+1:]
		}
		if ePort != "" && ePort != strconv.Itoa(port) {
			continue
		}
		if eHost == "" {
			return entry // a port alone
		}
		if domain, ok := strings.CutPrefix(eHost, "*."); ok {
			if host == domain || strings.HasSuffix(host, "."+domain) {
				return entry
			}
			continue
		}
		if host == eHost {
			return entry
		}
	}
	return ""
}

// Blocklist is the list in force, from the loaded policy.
func Blocklist() []string {
	cfg, _, _ := AppsConfigSnapshot()
	if cfg == nil {
		return nil
	}
	return cfg.Blocked
}

// setBlocklist commits a changed list, the way every policy change lands.
func setBlocklist(ctx context.Context, doer *user_model.User, entries []string, subject string) error {
	centralOwner, centralName, err := centralDeployOwnerName()
	if err != nil {
		return err
	}
	central, err := repo_model.GetRepositoryByOwnerAndName(ctx, centralOwner, centralName)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := range commitRetries {
		current, existed, err := readAppsConfigFile(ctx, central)
		if err != nil {
			return err
		}
		current.Blocked = entries
		body, err := yaml.Marshal(current)
		if err != nil {
			return err
		}
		operation := "update"
		if !existed {
			operation = "create"
		}
		message := fmt.Sprintf("chore(apps): %s\n\nChanged by %s.", subject, doer.Name)
		if attempt > 0 {
			message += fmt.Sprintf("\n\n(retry %d after a concurrent update)", attempt)
		}
		if _, lastErr = files_service.ChangeRepoFiles(ctx, central, doer, &files_service.ChangeRepoFilesOptions{
			OldBranch: central.DefaultBranch,
			NewBranch: central.DefaultBranch,
			Message:   message,
			Files:     []*files_service.ChangeRepoFile{{Operation: operation, TreePath: appsConfigPath, ContentReader: bytes.NewReader(body)}},
		}); lastErr == nil {
			if parsed, perr := ParseAppsConfig(body); perr == nil {
				SetAppsConfig(parsed)
			}
			return nil
		}
	}
	return lastErr
}

// BlockDestination adds one entry; UnblockDestination removes one.
func BlockDestination(ctx context.Context, doer *user_model.User, raw string) (string, error) {
	entry, err := normalizeBlockEntry(raw)
	if err != nil {
		return "", userKeyError("company.err.bad_block_entry", raw)
	}
	entries := slices.Clone(Blocklist())
	if slices.Contains(entries, entry) {
		return entry, nil
	}
	entries = append(entries, entry)
	slices.Sort(entries)
	return entry, setBlocklist(ctx, doer, entries, "block "+entry+" for every app")
}

func UnblockDestination(ctx context.Context, doer *user_model.User, entry string) error {
	entries := slices.DeleteFunc(slices.Clone(Blocklist()), func(e string) bool { return e == entry })
	if len(entries) == len(Blocklist()) {
		return userKeyError("company.err.not_blocked", entry)
	}
	return setBlocklist(ctx, doer, entries, "unblock "+entry)
}

// hostAndPort splits what the app asked the broker for; the broker dials
// https, so a missing port is 443.
func hostAndPort(hostport string) (string, int) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, 443
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return host, 443
	}
	return host, p
}
