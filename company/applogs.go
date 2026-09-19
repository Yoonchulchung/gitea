// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	gitea_context "gitea.dev/services/context"
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

// LogLine is one line of output.
type LogLine struct {
	Text string
	// At is when the line was written, 0 for lines logged before the platform
	// started stamping them (company/applogtime.go).
	At   int64
	File string
	// Build marks a line that came from a deploy rather than from the running
	// app. Both matter and both are kept, but "pip could not find that
	// version" and "the app raised an exception" are different kinds of
	// answer and mixing them silently makes neither readable.
	Build bool
	// Repeats is set by ErrorLines on the first line of a failure: how many
	// times it occurred back to back.
	Repeats int
}

// LogQuery is a search over an app's logs.
type LogQuery struct {
	// Substring match by default. Regexp is opt-in because a user-supplied
	// pattern is a denial-of-service surface (a catastrophically backtracking
	// expression), which the timeout bounds but does not remove.
	Text   string
	Regexp bool
	Limit  int
	// File narrows the read to one of the rotated files, by base name; ""
	// reads them all. The files rotate by size, not by day, so the page
	// labels each with the span of time it holds (AppLogFiles).
	File string
	// Before skips this many of the newest matching lines, which is how the
	// page turns back past what one screen holds.
	Before int
}

// setLogPagingData gives a log page its period selector and its paging.
func setLogPagingData(ctx *gitea_context.Context, query LogQuery, files []LogFileInfo) {
	limit := query.Limit
	if limit <= 0 || limit > logSearchMaxResults {
		limit = logSearchMaxResults
	}
	ctx.Data["LogFiles"] = files
	ctx.Data["LogFile"] = query.File
	ctx.Data["LogBefore"] = max(query.Before, 0)
	ctx.Data["LogLimit"] = limit
	// Server local time, as the stamps in the file are, and as text: these
	// go inside <option>, which holds no markup.
	labels := make(map[string]string, len(files))
	for _, f := range files {
		labels[f.Name] = logPeriodLabel(f)
	}
	ctx.Data["LogFileLabels"] = labels
}

func logPeriodLabel(f LogFileInfo) string {
	const layout = "2006-01-02 15:04"
	if f.From == 0 {
		return f.Name
	}
	from := time.Unix(f.From, 0).Format(layout)
	to := time.Unix(f.To, 0).Format(layout)
	if from[:10] == to[:10] {
		to = to[11:]
	}
	return from + " – " + to
}

// LogFileInfo is one of an app's log files and the time it covers.
type LogFileInfo struct {
	Name  string
	Build bool // deploy output rather than the running app's
	Live  bool // the file being written now
	From  int64
	To    int64
	Bytes int64
}

// AppLogFiles lists an app's log files oldest first, each with the first
// and last time stamped in it, so a person can choose a period rather than
// a file name.
func AppLogFiles(owner, repo string) []LogFileInfo {
	var out []LogFileInfo
	for _, file := range rotatedLogFiles(appPathsFor(owner, repo).logs) {
		info, err := os.Stat(file)
		if err != nil {
			continue
		}
		name := filepath.Base(file)
		entry := LogFileInfo{Name: name, Bytes: info.Size(), Build: strings.HasPrefix(name, buildLogName), Live: name == appLogName || name == buildLogName}
		entry.From, entry.To = logFileSpan(file)
		out = append(out, entry)
	}
	return out
}

// logFileSpan is the first and last stamped time in a file, reading only
// its head and its tail.
func logFileSpan(file string) (from, to int64) {
	f, err := os.Open(file)
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()
	const window = 64 << 10
	head := make([]byte, window)
	n, _ := io.ReadFull(f, head)
	for line := range strings.SplitSeq(string(head[:n]), "\n") {
		if at := logLineTime(line); at != 0 {
			from = at
			break
		}
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return from, from
	}
	start := max(size-window, 0)
	tail := make([]byte, size-start)
	if _, err := f.ReadAt(tail, start); err != nil && err != io.EOF {
		return from, from
	}
	for _, line := range slices.Backward(strings.Split(string(tail), "\n")) {
		if at := logLineTime(line); at != 0 {
			return from, at
		}
	}
	return from, from
}

// logLineTime is when a line was written: the stamp the platform put on an
// app's line, or the header a deploy wrote into the build log.
func logLineTime(line string) int64 {
	if at, _ := splitLogTime(line); at != 0 {
		return at
	}
	if strings.HasPrefix(line, buildLogHeader) {
		if fields := strings.Fields(strings.Trim(line, "= ")); len(fields) > 0 {
			if at, err := time.Parse(time.RFC3339, fields[0]); err == nil {
				return at.Unix()
			}
		}
	}
	return 0
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
	if query.File != "" {
		// Membership, never a path join: the name came from a query string.
		files = slices.DeleteFunc(files, func(f string) bool { return filepath.Base(f) != query.File })
	}

	// A ring buffer keeps the *newest* matches without holding every match:
	// the interesting lines are almost always at the end. Turning back a page
	// keeps that many more and drops them off the end afterwards.
	before := max(query.Before, 0)
	limit += before
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
			// Matched against the raw line, so searching for a date works, but
			// split for display so the time is a column rather than noise.
			at, text := splitLogTime(line)
			if query.Text == "" && healthProbeLine.MatchString(text) {
				continue // the platform's own probes (applive.go); a search still finds them
			}
			ring = append(ring, LogLine{Text: text, At: at, File: name, Build: strings.HasPrefix(name, buildLogName)})
		}
		_ = f.Close()
		if truncated && time.Now().After(deadline) {
			break
		}
	}
	if before > 0 {
		ring = ring[:max(len(ring)-before, 0)]
	}
	return ring, truncated, nil
}

// rotatedLogFiles lists an app's log files oldest first.
//
// Build output comes first as a block, then the running app's. They are
// separate files with no shared clock, so interleaving them would imply an
// ordering that is not there; a reader looking for why a deploy failed wants
// the build half anyway, and it is at the top.
func rotatedLogFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var buildRotated, appRotated []string
	buildLive, appLive := "", ""
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(dir, name)
		switch {
		case name == buildLogName:
			buildLive = path
		case strings.HasPrefix(name, buildLogName+"."):
			buildRotated = append(buildRotated, path)
		case name == appLogName:
			appLive = path
		case strings.HasPrefix(name, appLogName+"."):
			appRotated = append(appRotated, path)
		}
	}
	// Higher suffix means older, so descending order puts the oldest first.
	older := func(a, b string) int { return strings.Compare(b, a) }
	slices.SortFunc(buildRotated, older)
	slices.SortFunc(appRotated, older)

	files := buildRotated
	if buildLive != "" {
		files = append(files, buildLive)
	}
	files = append(files, appRotated...)
	if appLive != "" {
		files = append(files, appLive)
	}
	return files
}
