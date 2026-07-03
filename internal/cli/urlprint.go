package cli

import (
	"fmt"
	"io"
	"net"
	"net/url"

	"github.com/jedwards1230/scrim/internal/mdns"
	"github.com/jedwards1230/scrim/internal/state"
)

// baseURLFor builds the URL for path (e.g. "/") against the daemon
// described by st, appending the capability token as a "?t=" query
// parameter when auth is enabled. Canvas URLs the daemon itself returns
// (apiclient.CanvasResponse.URL) already have this baked in server-side —
// this is only for URLs the CLI builds itself, like the bare dashboard URL
// printed by `open` (no id) and `status`.
func baseURLFor(st *state.State, path string) string {
	u := fmt.Sprintf("http://%s:%d%s", st.Host, st.Port, path)
	if st.AuthEnabled() {
		u += "?t=" + st.Token
	}
	return u
}

// printURLLines writes the line(s) for a served URL to w: just rawURL when
// the daemon isn't advertising over mDNS (bound to loopback only), or both
// the scrim.local URL and the plain host:port URL — as a fallback, since
// mDNS can be blocked on some networks — when it is.
func printURLLines(w io.Writer, host, rawURL string) {
	for _, line := range urlLines(host, rawURL) {
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
