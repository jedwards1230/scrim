package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/daemon"
	"github.com/jedwards1230/scrim/internal/openurl"
)

// openBrowserEnvVar is the environment variable that persistently opts in to
// the auto-launch behavior --browser enables for a single invocation.
const openBrowserEnvVar = "SCRIM_OPEN_BROWSER"

// cmdOpen implements `scrim open [<id>] [--browser]`. It self-starts the
// daemon if needed and prints the URL for a canvas (or the dashboard, with
// no id) via resolveAndPrintURLs -- the same resolution logic cmdLink uses.
// Launching that URL in the platform's default browser is opt-in -- via
// --browser or a truthy SCRIM_OPEN_BROWSER -- since scrim's daemon is
// commonly self-started by an agent on the user's behalf, and a browser tab
// popping up unprompted is a surprise, not a convenience. The printed URL is
// always there, whether or not auto-open was requested; when it was and it
// fails, that's reported as a notice, not a command failure. When it wasn't
// requested at all, a one-line stderr hint points at how to opt in --
// scrim link is the print-only sibling for anyone (especially an agent) who
// never wants that hint or the possibility of a launch in the first place.
func cmdOpen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("open", stderr)
	cf := registerCommonFlags(fs)
	browserFlag := fs.Bool("browser", false, "launch the URL in your default browser (default: print only; env SCRIM_OPEN_BROWSER)")
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	pos := fs.Args()
	if len(pos) > 1 {
		return usageError(stderr, "usage: scrim open [<id>] [--browser]")
	}
	launch := shouldOpenBrowser(*browserFlag, os.Getenv(openBrowserEnvVar))

	id := ""
	if len(pos) == 1 {
		id = pos[0]
	}

	cfg := cf.toConfig()
	primary, code, ok := resolveAndPrintURLs(cfg, id, stdout, stderr)
	if !ok {
		return code
	}

	if launch {
		openBrowser(primary, stderr)
	} else {
		outf(stderr, "browser launch is opt-in -- pass --browser or set SCRIM_OPEN_BROWSER=1\n")
	}
	return 0
}

// resolveAndPrintURLs self-starts the daemon, resolves the URL(s) for id --
// or, when id is "", the dashboard -- against it, and prints them to
// stdout. It's the single implementation shared by cmdOpen and cmdLink,
// the only two verbs that print a canvas's URL; they differ only in what
// they do after this returns (cmdOpen may also launch a browser; cmdLink
// never does). On failure ok is false and exitCode is the code the caller
// should return without printing anything further -- an error has already
// been written to stderr. On success, primary is the URL a caller wanting
// to auto-launch a browser should hand to openBrowser (see primaryURL).
func resolveAndPrintURLs(cfg config.Config, id string, stdout, stderr io.Writer) (primary string, exitCode int, ok bool) {
	st, err := daemon.Ensure(cfg)
	if err != nil {
		errOut(stderr, err)
		return "", 1, false
	}

	if id == "" {
		url := baseURLFor(st, "/")
		lines := urlLines(st.Host, url)
		printURLLines(stdout, lines)
		return primaryURL(lines, url), 0, true
	}

	if err := canvas.ValidateID(id); err != nil {
		errOut(stderr, err)
		return "", 2, false
	}

	apiBaseURL := fmt.Sprintf("http://%s:%d", st.Host, st.Port)
	client := apiclient.NewWithToken(apiBaseURL, st.Token)
	canvases, err := client.ListCanvases(context.Background())
	if err != nil {
		errOut(stderr, err)
		return "", 1, false
	}
	for _, c := range canvases {
		if c.ID == id {
			lines := urlLines(st.Host, c.URL)
			printURLLines(stdout, lines)
			return primaryURL(lines, c.URL), 0, true
		}
	}
	outf(stderr, "error: canvas %q not found\n", id)
	return "", 1, false
}

// shouldOpenBrowser reports whether cmdOpen should launch the browser: only
// when explicitly opted in, via the --browser flag or a truthy
// SCRIM_OPEN_BROWSER env value. envVal is parsed the same way
// config.FromEnv parses SCRIM_NO_AUTH -- strconv.ParseBool, with an empty or
// malformed value treated as not set (false) rather than an error, since an
// env-sourced opt-in is always overridable by the explicit flag.
func shouldOpenBrowser(flagSet bool, envVal string) bool {
	if flagSet {
		return true
	}
	if envVal == "" {
		return false
	}
	b, err := strconv.ParseBool(envVal)
	return err == nil && b
}

// primaryURL returns the URL that was printed as the first line -- i.e. the
// one the browser should actually be pointed at. When mDNS is active,
// lines[0] is the scrim.local URL, which works even when the daemon is
// bound to an address like 0.0.0.0 or :: that isn't itself navigable; the
// raw fallback is only used defensively, in case lines somehow came back
// empty (urlLines never actually returns an empty slice).
func primaryURL(lines []string, fallback string) string {
	if len(lines) > 0 {
		return lines[0]
	}
	return fallback
}

// launchBrowser is openurl.Open, indirected through a package variable so
// tests can confirm cmdOpen's default (no --browser, no env) never calls
// into openurl -- which itself execs a real OS command and can't be
// meaningfully asserted on in a unit test -- without a broader mocking seam.
var launchBrowser = openurl.Open

// openBrowser launches url in the platform's default browser, printing a
// one-line notice to stderr on failure. It never affects cmdOpen's exit
// code -- the URL is already on stdout, so a failed or unsupported auto-open
// is a nice-to-have that didn't pan out, not an error.
func openBrowser(url string, stderr io.Writer) {
	if err := launchBrowser(url); err != nil {
		outf(stderr, "notice: could not open a browser automatically (%v); use the URL above\n", err)
	}
}
