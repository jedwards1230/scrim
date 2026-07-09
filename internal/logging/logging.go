// Package logging is scrim's sole sanctioned logging surface for the
// daemon (internal/server and internal/daemon). Every log call site in
// those packages goes through here instead of calling log.Printf or
// writing to os.Stderr/os.Stdout directly.
//
// The API is deliberately narrow: Error takes a fixed Category label and an
// error value, never a free-form format string. That's what keeps a
// request's path, canvas ID, query string (which carries the capability
// token), or the raw token itself from ever being interpolated into a log
// line by one of scrim's own call sites -- there's simply no format
// argument to put one in. As a second line of defense, every message is
// also run through a scrubber before being written, in case an error value
// itself happens to wrap text containing one of those (see scrub).
package logging

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Category labels the closed set of subsystems that log through this
// package. Callers use one of these constants rather than an arbitrary
// string, so log output stays a fixed, greppable vocabulary rather than
// free text a caller could shape however it likes.
type Category string

// The full set of categories scrim's daemon logs under.
const (
	CategoryHTTP   Category = "http"
	CategoryMDNS   Category = "mdns"
	CategoryAuth   Category = "auth"
	CategorySSE    Category = "sse"
	CategoryDaemon Category = "daemon"
	// CategoryConfig covers startup-time configuration concerns that aren't
	// tied to a request, e.g. internal/config's permission-hardening.
	CategoryConfig Category = "config"
	// CategoryDirectory covers the optional read-only directory feeder
	// (internal/authentik): a failed background pull degrades autocomplete but
	// never a request, and logs one constant, PII-free line here.
	CategoryDirectory Category = "directory"
)

var (
	mu  sync.Mutex
	out io.Writer = os.Stderr
)

// SetOutput redirects where subsequent log lines are written -- e.g. once
// the daemon has opened its own log file and wants everything after that
// point to land there instead of its inherited stderr. Passing nil resets
// to os.Stderr.
func SetOutput(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	if w == nil {
		w = os.Stderr
	}
	out = w
}

// Error logs err under category: a single line of "<timestamp> [<category>]
// <message>", with the message scrubbed (see scrub) before being written. A
// nil err is a no-op, so call sites can write `logging.Error(cat, err)`
// directly after an `if err != nil` without an extra branch disappearing.
//
// mu is held for the whole read-out-then-write, not just the read of out --
// Error is called concurrently from the HTTP server, the idle reaper, and
// other daemon goroutines, all sharing the same underlying io.Writer.
// Releasing the lock between reading out and writing to it would let two
// concurrent calls race on that writer, interleaving or corrupting their
// output; holding it across the whole call makes each Error call atomic
// with respect to both other Error calls and a concurrent SetOutput.
func Error(category Category, err error) {
	if err == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	// A failure to write a log line has nowhere else to go and nothing
	// actionable to do about it -- it's silently dropped, same as every
	// other best-effort write in this codebase.
	_, _ = fmt.Fprintf(out, "%s [%s] %s\n", time.Now().UTC().Format(time.RFC3339), category, scrub(err.Error()))
}

// StdLogger returns a *log.Logger that forwards every line written to it to
// Error under category, scrubbed exactly like a direct Error call. It
// exists for stdlib APIs that only accept a *log.Logger -- most notably
// http.Server.ErrorLog: left nil, net/http falls back to its own
// package-level logger (straight to os.Stderr) for things like malformed
// requests or a panic recovered inside a handler, bypassing scrim's logging
// policy entirely. Wiring http.Server.ErrorLog to StdLogger(CategoryHTTP)
// closes that gap.
func StdLogger(category Category) *log.Logger {
	return log.New(stdWriter{category: category}, "", 0)
}

type stdWriter struct{ category Category }

func (w stdWriter) Write(p []byte) (int, error) {
	Error(w.category, errors.New(strings.TrimRight(string(p), "\n")))
	return len(p), nil
}

// scrub redacts substrings that could carry a request path, canvas ID, or
// capability token from s, regardless of which category logged it or how
// the text was built. It is defense-in-depth: Error's own API (category +
// error, no format string) already keeps scrim's own log call sites from
// interpolating one of these in the first place, but an error value passed
// in could still wrap arbitrary text (e.g. from a lower-level library) that
// happens to contain one.
var (
	// reCanvasPath matches a "/c/..." path fragment through to the next
	// whitespace, which -- since a capability token travels as that same
	// path's "?t=" query parameter -- also happens to swallow the query
	// string whenever the two appear together, as they always do in a
	// request-derived error.
	reCanvasPath = regexp.MustCompile(`/c/\S*`)
	// reQuery catches a "?..." query string on its own, for the rarer case
	// where it's not attached to a "/c/" path (e.g. a bare "/?t=...").
	reQuery = regexp.MustCompile(`\?\S*`)
	// reHexRun catches a capability token even with no surrounding URL
	// context at all. A token is 32 bytes of crypto/rand, hex-encoded (64
	// hex characters -- see state.NewToken); 24 is a deliberately generous
	// lower bound so a truncated token still gets caught.
	reHexRun = regexp.MustCompile(`\b[0-9a-fA-F]{24,}\b`)
)

// redactedPath/redactedQuery/redactedToken are deliberately plain
// placeholders -- they must not themselves reproduce the substrings this
// package promises never to log ("/c/", "?", a long hex run), or scrubbing
// would defeat its own purpose.
const (
	redactedPath  = "<redacted-canvas-path>"
	redactedQuery = "<redacted-query>"
	redactedToken = "<redacted-token>"
)

func scrub(s string) string {
	s = reCanvasPath.ReplaceAllString(s, redactedPath)
	s = reQuery.ReplaceAllString(s, redactedQuery)
	s = reHexRun.ReplaceAllString(s, redactedToken)
	return s
}
