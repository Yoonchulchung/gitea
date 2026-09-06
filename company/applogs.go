// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// journalctl is blocked on the deploy server, and it was never needed: Gitea
// is the app's parent and owns its stdout/stderr, so the logs are already
// plain rotated files it wrote itself (appproc.go).
//
// These are admin-only. A department sees a classified cause instead — "the
// app was stopped for using too much memory" — because raw output contains
// whatever the app happened to print, which can include the data it handles.
// See docs/company/app-platform.md.

const (
	// logSearchMaxResults bounds what one query can return. A dashboard that
	// tries to render a million lines hangs the browser, not the server, but
	// it is just as unusable either way.
	logSearchMaxResults = 2000

	// logSearchTimeout stops a pathological query from occupying a request
	// goroutine indefinitely.
	logSearchTimeout = 5 * time.Second
)

// LogLine is one line of app output.
type LogLine struct {
	Text string
	File string
}

// LogQuery is a search over an app's logs.
type LogQuery struct {
	// Substring match by default. Regexp is opt-in because a user-supplied
	// pattern is a denial-of-service surface (a catastrophically backtracking
	// expression), which the timeout bounds but does not remove.
	Text   string
	Regexp bool
	Limit  int
}

// ReadAppLogs returns the most recent matching lines, newest last.
//
// Files are streamed rather than read whole: an app's log can be tens of
// megabytes, and loading it into memory to grep it would make the log page
// the most expensive request on the instance.
func ReadAppLogs(owner, repo string, query LogQuery) ([]LogLine, bool, error) {
	limit := query.Limit
	if limit <= 0 || limit > logSearchMaxResults {
		limit = logSearchMaxResults
	}

	var matcher func(string) bool
	switch {
	case query.Text == "":
		matcher = func(string) bool { return true }
	case query.Regexp:
		re, err := regexp.Compile(query.Text)
		if err != nil {
			return nil, false, err
		}
		matcher = re.MatchString
	default:
		needle := strings.ToLower(query.Text)
		matcher = func(line string) bool { return strings.Contains(strings.ToLower(line), needle) }
	}

	deadline := time.Now().Add(logSearchTimeout)
	// Oldest rotated file first, so the result reads in chronological order.
	files := rotatedLogFiles(appPathsFor(owner, repo).logs)

	// A ring buffer keeps the *newest* matches without holding every match:
	// the interesting lines are almost always at the end.
	ring := make([]LogLine, 0, limit)
	truncated := false
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue // a file rotated away mid-read is not an error
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20) // a stack trace on one line is normal
		name := filepath.Base(file)
		for scanner.Scan() {
			if time.Now().After(deadline) {
				truncated = true
				break
			}
			line := scanner.Text()
			if !matcher(line) {
				continue
			}
			if len(ring) == limit {
				ring = ring[1:]
				truncated = true
			}
			ring = append(ring, LogLine{Text: line, File: name})
		}
		_ = f.Close()
		if truncated && time.Now().After(deadline) {
			break
		}
	}
	return ring, truncated, nil
}

// rotatedLogFiles lists an app's log files oldest first: app.log.5 … app.log.1,
// then the live app.log.
func rotatedLogFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var rotated []string
	live := ""
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "app.log":
			live = filepath.Join(dir, name)
		case strings.HasPrefix(name, "app.log."):
			rotated = append(rotated, filepath.Join(dir, name))
		}
	}
	// Higher suffix means older, so descending order puts the oldest first.
	slices.SortFunc(rotated, func(a, b string) int { return strings.Compare(b, a) })
	if live != "" {
		rotated = append(rotated, live)
	}
	return rotated
}
