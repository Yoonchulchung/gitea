// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

// An app's output went to the log file exactly as the app wrote it, which
// meant a log with no times in it. "It broke" and "it broke twenty minutes
// ago, right after the deploy" are different pieces of information, and the
// second one is the one that lets someone connect a failure to what caused
// it — without it the log answers what happened but never when, and a
// department comparing it against a deploy or a restart has nothing to line
// them up by.
//
// The timestamp is added on the way in rather than expected from the app,
// because the app is a department's own code: uvicorn timestamps its access
// lines and a bare `print()` does not, so leaving it to the app means the
// times are there only when someone happened to configure logging.

// logTimeLayout is fixed-width so a reader's eye can skip past it, and
// sortable so the file stays in order when read as text.
const logTimeLayout = "2006-01-02T15:04:05Z07:00"

// timestampWriter prefixes each complete line with the time it arrived.
//
// Buffers partial writes: a write from the child does not necessarily end on
// a line boundary, and prefixing every write would put a timestamp into the
// middle of a stack trace. Whatever is left without a newline is flushed on
// Close, so a final line is never dropped.
type timestampWriter struct {
	mu sync.Mutex // stdout and stderr are separate goroutines writing one file
	w  io.WriteCloser
	// buf holds the tail of a line that has not been terminated yet.
	buf  bytes.Buffer
	now  func() time.Time
	last time.Time
}

func newTimestampWriter(w io.WriteCloser) *timestampWriter {
	return &timestampWriter{w: w, now: time.Now}
}

func (t *timestampWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Reported back to the caller regardless of how the bytes are reframed:
	// exec treats a short write as an error, and the child's line lengths are
	// not this writer's to renegotiate.
	written := len(p)
	t.buf.Write(p)
	t.last = t.now()

	for {
		line, err := t.buf.ReadString('\n')
		if err != nil {
			// Not a whole line yet — put it back and wait for the rest.
			t.buf.Reset()
			t.buf.WriteString(line)
			return written, nil
		}
		if _, err := t.w.Write([]byte(t.last.Format(logTimeLayout) + " " + line)); err != nil {
			return written, err
		}
	}
}

// Close flushes a trailing partial line. An app killed mid-write usually has
// one, and it is often the most interesting line in the file.
func (t *timestampWriter) Close() error {
	t.mu.Lock()
	if rest := strings.TrimRight(t.buf.String(), "\r\n"); rest != "" {
		_, _ = t.w.Write([]byte(t.last.Format(logTimeLayout) + " " + rest + "\n"))
		t.buf.Reset()
	}
	t.mu.Unlock()
	return t.w.Close()
}

// splitLogTime separates the prefix this writer added.
//
// A line without one is returned whole with a zero time rather than guessed
// at: logs written before this existed are still in the rotation, and an app
// that prints something starting with a date should not have it silently
// eaten.
func splitLogTime(line string) (int64, string) {
	stamp, rest, found := strings.Cut(line, " ")
	if !found || len(stamp) < len("2006-01-02T15:04:05Z") {
		return 0, line
	}
	at, err := time.Parse(logTimeLayout, stamp)
	if err != nil {
		return 0, line
	}
	return at.Unix(), rest
}
