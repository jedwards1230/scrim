package cli

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"

	"github.com/jedwards1230/scrim/internal/mdns"
	"github.com/jedwards1230/scrim/internal/state"
)

// defaultHTTPPort is the port a browser (or any HTTP client) assumes when
// none is given in a "http://" URL — omitting it from a printed URL is
// purely cosmetic (":80" adds nothing a browser needs), not a privacy
// measure like the rest of this package.
const defaultHTTPPort = 80

// baseURLFor builds the URL for path (e.g. "/") against the daemon
// described by st, appending the capability token as a "?t=" query
// parameter when auth is enabled. Canvas URLs the daemon itself returns
// (apiclient.CanvasResponse.URL) already have this baked in server-side —
// this is only for URLs the CLI builds itself, like the bare dashboard URL
// printed by `open` (no id) and `status`.
func baseURLFor(st *state.State, path string) string {
	u := fmt.Sprintf("http://%s%s", formatHostPort(st.Host, st.Port), path)
	if st.AuthEnabled() {
		u += "?t=" + st.Token
	}
	return u
}

// formatHostPort formats host:port for a printed URL, omitting the port
// entirely when it's the default HTTP port — a printed "http://host/" reads
// cleaner than "http://host:80/", and browsers treat them identically.
// Every other port is kept explicit.
func formatHostPort(host string, port int) string {
	if port == defaultHTTPPort {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// printURLLines writes each of lines to w, one per line. Callers compute
// lines via urlLines first -- keeping the two separate lets a caller that
// also needs the lines itself (e.g. to pick the primary one for
// openBrowser) reuse the same slice instead of recomputing it.
func printURLLines(w io.Writer, lines []string) {
	for _, line := range lines {
		outln(w, line)
	}
}

// urlLines returns the line(s) to print for rawURL given the daemon's bind
// host: a single line unchanged when host is loopback-only (mDNS inactive),
// or the scrim.local variant followed by the original as a fallback when
// it's not.
func urlLines(host, rawURL string) []string {
	if mdns.IsLoopbackHost(host) {
		return []string{rawURL}
	}
	withMDNSHost, err := rewriteURLHost(rawURL, mdns.ServiceHost)
	if err != nil {
		return []string{rawURL}
	}
	return []string{withMDNSHost, rawURL}
}

// rewriteURLHost returns rawURL with its host (but not its port, path, or
// query) replaced by newHost.
func rewriteURLHost(rawURL, newHost string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if _, port, splitErr := net.SplitHostPort(u.Host); splitErr == nil {
		u.Host = net.JoinHostPort(newHost, port)
	} else {
		u.Host = newHost
	}
	return u.String(), nil
}
