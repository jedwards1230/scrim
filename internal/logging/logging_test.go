package logging

import (
	"bytes"
	"errors"
	"os"
	"strings"
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
