// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bufio"
	"compress/gzip"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gitea.dev/modules/setting"
)

// The platform's own log, as opposed to a deployed app's (company/applogs.go).
//
// It existed only on the server's disk, which meant that answering "did the
// login fail, or did it never arrive" required someone with shell access on
// the deploy host — and on this instance MODE=file, so it is not even on a
// terminal somebody might have left open. Every other operational question
// this platform answers has a screen; this one sent an administrator to ssh.
//
// Read-only and admin-only. The file is never written, never rotated and
// never deleted from here: log retention is the server's business, and a
// screen that could delete the audit trail would defeat the reason the trail
// is written to a file in the first place.

const (
	// serverLogMaxResults bounds one query. The log of a busy instance is
	// hundreds of megabytes; the browser is what falls over first.
	serverLogMaxResults = 2000

	// serverLogTimeout stops a pathological search from occupying a request
	// goroutine while it walks every rotated file.
	serverLogTimeout = 5 * time.Second
)

// ServerLogLine is one line of Gitea's own log.
type ServerLogLine struct {
	Text string
	// At is when the line was written, 0 when the line carries no parsable
	// timestamp — a continuation line of a stack trace, or a log format the
	// instance configured differently.
	At int64
	// Level is the lower-case level name Gitea stamped ("error"), empty when
	// the line has none. Kept as the name rather than a number so a template
	// can use it as a class without a mapping table.
	Level string
	File  string
}

// ServerLogQuery is a search over the instance's log files.
type ServerLogQuery struct {
	// Substring match by default; Regexp is opt-in for the same reason as in
	// an app's logs — a supplied pattern is a backtracking risk that the
	// timeout bounds but does not remove.
	Text   string
	Regexp bool
	// File limits the search to one log file, by base name. Validated against
	// the enumerated set, never joined as a path: this handler reads files for
	// an administrator, and a name from a query string must not be able to
	// name custom/conf/app.ini.
	File string
	// MinLevel drops anything below it ("warn" keeps warn/error/fatal). Empty
	// keeps everything.
	MinLevel string
	Limit    int
}

// serverLogLevels is the ordering MinLevel compares against, lowest first.
var serverLogLevels = []string{"trace", "debug", "info", "warn", "error", "fatal"}

// serverLogPrefix matches Gitea's default line prefix: a date, an optional
// logger or caller field, and a level in brackets — either the initial the
// default flags write ("[I]") or the whole name ("[INFO]"), since the flags
// are configurable per writer. A line that does not match is still shown,
// just without a time or a level: dropping it would hide exactly the lines a
// stack trace is made of.
var serverLogPrefix = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) (?:(\S+) )?\[([A-Z]+)\] `)

// serverLogName matches a log file and captures the logger it belongs to.
// Anchored and without a path separator, so an entry that is not plainly a
// log file is skipped rather than opened.
var serverLogName = regexp.MustCompile(`^([A-Za-z0-9_-]+)\.log(\.[A-Za-z0-9._-]+)?$`)

// ServerLogFiles lists the instance's log files, oldest first within each
// logger, loggers in alphabetical order.
//
// Rotated names sort the opposite way to an app's: Gitea names them by date
// and sequence ("gitea.log.2026-09-18.001.gz"), so ascending order is
// chronological and the live file, having no suffix at all, is the newest and
// belongs last.
func ServerLogFiles() []string {
	root := setting.Log.RootPath
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	live := map[string]string{}
	rotated := map[string][]string{}
	loggers := map[string]struct{}{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := serverLogName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		loggers[m[1]] = struct{}{}
		if m[2] == "" {
			live[m[1]] = e.Name()
		} else {
			rotated[m[1]] = append(rotated[m[1]], e.Name())
		}
	}

	var files []string
	for _, logger := range slices.Sorted(maps.Keys(loggers)) {
		names := rotated[logger]
		slices.Sort(names)
		files = append(files, names...)
		if name := live[logger]; name != "" {
			files = append(files, name)
		}
	}
	return files
}

// ReadServerLogs returns the most recent matching lines, newest last.
//
// Files are streamed, and the rotated ones decompressed on the way through,
// rather than read whole: this is the largest thing on the disk that a web
// request is allowed to touch.
func ReadServerLogs(query ServerLogQuery) ([]ServerLogLine, bool, error) {
	limit := query.Limit
	if limit <= 0 || limit > serverLogMaxResults {
		limit = serverLogMaxResults
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

	minLevel := slices.Index(serverLogLevels, query.MinLevel)

	files := ServerLogFiles()
	if query.File != "" {
		// Membership, not a path join: the only names this will open are ones
		// the directory listing produced.
		if !slices.Contains(files, query.File) {
			return nil, false, nil
		}
		files = []string{query.File}
	}

	deadline := time.Now().Add(serverLogTimeout)
	// A ring buffer keeps the newest matches without holding every match.
	ring := make([]ServerLogLine, 0, limit)
	truncated := false
	for _, name := range files {
		if scanServerLog(filepath.Join(setting.Log.RootPath, name), name, matcher, minLevel, limit, deadline, &ring, &truncated) {
			truncated = true
			break
		}
	}
	return ring, truncated, nil
}

// scanServerLog appends one file's matches into the ring, reporting whether
// it stopped on the deadline rather than at end of file.
func scanServerLog(path, name string, matcher func(string) bool, minLevel, limit int, deadline time.Time, ring *[]ServerLogLine, truncated *bool) bool {
	f, err := os.Open(path)
	if err != nil {
		return false // a file rotated away mid-read is not an error
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return false // still being written by the rotator
		}
		defer gz.Close()
		r = gz
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20) // a stack trace arrives as one line
	// The level of the last line that carried one, so an unstamped
	// continuation is filtered with the message it belongs to instead of
	// being dropped out from under it.
	carried := ""
	for scanner.Scan() {
		if time.Now().After(deadline) {
			return true
		}
		line := scanner.Text()
		at, level, text := splitServerLogLine(line)
		if level != "" {
			carried = level
		}
		if minLevel >= 0 && slices.Index(serverLogLevels, carried) < minLevel {
			continue
		}
		if !matcher(line) { // matched raw, so searching for a date or a level works
			continue
		}
		if len(*ring) == limit {
			*ring = (*ring)[1:]
			*truncated = true
		}
		*ring = append(*ring, ServerLogLine{Text: text, At: at, Level: level, File: name})
	}
	return false
}

// splitServerLogLine pulls the time and level out for display as columns,
// returning the rest of the line. An unrecognised line is returned whole.
func splitServerLogLine(line string) (at int64, level, text string) {
	m := serverLogPrefix.FindStringSubmatch(line)
	if m == nil {
		return 0, "", line
	}
	// The instance's own clock wrote it and its own clock reads it back, so
	// local time is the right frame here — unlike an app's log, which is
	// stamped with an offset because it can be read on a different machine.
	if t, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.Local); err == nil {
		at = t.Unix()
	}
	for _, name := range serverLogLevels {
		if strings.EqualFold(string(name[0]), string(m[3][0])) {
			level = name
			break
		}
	}
	// The caller field stays in the text rather than becoming a third column:
	// it is what says whether a line came from the router, the mailer or a
	// migration, and it is only ever read alongside the message.
	rest := strings.TrimPrefix(line, m[0])
	if m[2] != "" {
		rest = m[2] + " " + rest
	}
	return at, level, rest
}
