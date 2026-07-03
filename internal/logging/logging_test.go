package logging

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// withCapturedOutput redirects the package's output to a buffer for the
// duration of the test, restoring os.Stderr on cleanup.
func withCapturedOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	SetOutput(&buf)
	t.Cleanup(func() { SetOutput(os.Stderr) })
	return &buf
}

// TestErrorNeverLeaksSensitiveContent is the load-bearing scrubbing test:
// every shape of error text that could plausibly carry a capability token,
// a "/c/<id>" path, or a canvas ID must come out the other side of Error
// with none of those substrings intact, regardless of category.
func TestErrorNeverLeaksSensitiveContent(t *testing.T) {
	const (
		fakeToken    = "deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcd"
		fakeCanvasID = "top-secret-canvas"
	)

	tests := []struct {
		name string
		err  error
	}{
		{
			name: "full request-shaped path with token",
			err:  errors.New("GET /c/" + fakeCanvasID + "/index.html?t=" + fakeToken + " failed: connection reset"),
		},
		{
			name: "bare token with no path",
			err:  errors.New("rejected token " + fakeToken),
		},
		{
			name: "query string with no leading canvas path",
			err:  errors.New("request /?t=" + fakeToken + " rejected"),
		},
		{
			name: "canvas id embedded in a canvas path with trailing text",
			err:  errors.New("serving /c/" + fakeCanvasID + "/assets/app.js: broken pipe"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := withCapturedOutput(t)
			Error(CategoryHTTP, tt.err)
			out := buf.String()
			for _, sensitive := range []string{fakeToken, fakeCanvasID, "/c/", "?t="} {
				if strings.Contains(out, sensitive) {
					t.Errorf("Error() output leaked %q; full output: %s", sensitive, out)
				}
			}
		})
	}
}

// TestStdLoggerScrubs confirms the *log.Logger adapter used for
// http.Server.ErrorLog applies the exact same scrubbing as a direct Error
// call, since it's a separate write path (Write, not Error) that could
// otherwise bypass it.
func TestStdLoggerScrubs(t *testing.T) {
	const fakeToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	buf := withCapturedOutput(t)

	l := StdLogger(CategoryHTTP)
	l.Printf("http: panic serving 127.0.0.1:54321 while handling /c/secret-id?t=%s: boom", fakeToken)

	out := buf.String()
	for _, sensitive := range []string{fakeToken, "secret-id", "/c/"} {
		if strings.Contains(out, sensitive) {
			t.Errorf("StdLogger output leaked %q; full output: %s", sensitive, out)
		}
	}
	if !strings.Contains(out, string(CategoryHTTP)) {
		t.Errorf("StdLogger output missing category %q; full output: %s", CategoryHTTP, out)
	}
}

// TestErrorIncludesCategoryAndMessage confirms Error's output is still
// useful for debugging -- the category label and the (scrubbed) message
// text must both survive -- not just "safe by being empty".
func TestErrorIncludesCategoryAndMessage(t *testing.T) {
	buf := withCapturedOutput(t)
	Error(CategoryMDNS, errors.New("multicast bind failed"))

	out := buf.String()
	if !strings.Contains(out, string(CategoryMDNS)) {
		t.Errorf("output missing category %q; full output: %s", CategoryMDNS, out)
	}
	if !strings.Contains(out, "multicast bind failed") {
		t.Errorf("output missing message; full output: %s", out)
	}
}

// TestErrorNilIsNoOp confirms a nil error never produces a log line, so
// call sites can write `logging.Error(cat, err)` right after an `if err !=
// nil` check without a redundant guard.
func TestErrorNilIsNoOp(t *testing.T) {
	buf := withCapturedOutput(t)
	Error(CategoryHTTP, nil)
	if buf.Len() != 0 {
		t.Errorf("Error(category, nil) wrote output, want none: %q", buf.String())
	}
}

// TestErrorConcurrentWritesDoNotInterleave is the regression test for the
// read-out-then-write-outside-the-lock race: Error must hold mu across the
// whole write, not just the read of out, or two goroutines racing this call
// (as the HTTP server, idle reaper, and other daemon goroutines do in
// practice) can interleave their Fprintf calls on the shared writer and
// corrupt output. Run with `go test -race` to also catch the underlying
// data race directly.
func TestErrorConcurrentWritesDoNotInterleave(t *testing.T) {
	buf := withCapturedOutput(t)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			Error(CategoryDaemon, fmt.Errorf("concurrent-message-%03d", i))
		}(i)
	}
	wg.Wait()

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("got %d log lines, want %d (interleaved/corrupted write splits or merges lines); output:\n%s", len(lines), n, out)
	}

	seen := make(map[int]bool, n)
	for _, line := range lines {
		if !strings.Contains(line, string(CategoryDaemon)) {
			t.Errorf("line missing category, likely corrupted by an interleaved write: %q", line)
			continue
		}
		msgStart := strings.Index(line, "concurrent-message-")
		if msgStart < 0 {
			t.Errorf("line missing expected message prefix, likely corrupted by an interleaved write: %q", line)
			continue
		}
		var idx int
		if _, err := fmt.Sscanf(line[msgStart:], "concurrent-message-%03d", &idx); err != nil {
			t.Errorf("line did not parse as an intact message %q: %v", line, err)
			continue
		}
		if seen[idx] {
			t.Errorf("message %03d appeared more than once, output was duplicated/split: %q", idx, line)
		}
		seen[idx] = true
	}
	if len(seen) != n {
		t.Errorf("saw %d distinct intact messages, want %d", len(seen), n)
	}
}

// TestScrub is a table-driven test of the scrubbing function itself,
// independent of Error/StdLogger's formatting around it.
func TestScrub(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		mustNotHave   []string
		mustStillHave string
	}{
		{
			name:          "canvas path with query token",
			in:            "GET /c/report/index.html?t=abcdef0123456789abcdef0123456789 500",
			mustNotHave:   []string{"/c/report", "?t=abcdef"},
			mustStillHave: "GET",
		},
		{
			name:          "bare hex token",
			in:            "invalid token abcdef0123456789abcdef0123456789abcdef01",
			mustNotHave:   []string{"abcdef0123456789abcdef0123456789abcdef01"},
			mustStillHave: "invalid token",
		},
		{
			name:          "plain message with nothing sensitive is unchanged",
			in:            "listener closed",
			mustNotHave:   nil,
			mustStillHave: "listener closed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scrub(tt.in)
			for _, s := range tt.mustNotHave {
				if strings.Contains(got, s) {
					t.Errorf("scrub(%q) = %q, still contains %q", tt.in, got, s)
				}
			}
			if !strings.Contains(got, tt.mustStillHave) {
				t.Errorf("scrub(%q) = %q, want it to still contain %q", tt.in, got, tt.mustStillHave)
			}
		})
	}
}
