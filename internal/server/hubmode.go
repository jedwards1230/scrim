package server

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jedwards1230/scrim/internal/config"
)

// HubOptions configures a Server constructed via NewHub -- the hub-specific
// write/read gate that replaces the default daemon's capability-token
// middleware (see withHubGate in hubgate.go). It has no default-daemon
// equivalent; server.New (the default daemon constructor) never sees it.
type HubOptions struct {
	// PushToken is the bearer token required on every write request
	// (POST /api/push, POST /api/stop, any non-GET/HEAD /api/canvases
	// call). Required -- NewHub fails closed if it's empty.
	PushToken string
	// ReadToken, if non-empty, is additionally required (after the CIDR
	// check passes) on every read request.
	ReadToken string
	// AllowCIDRs is the read allowlist, as a slice of CIDR strings (e.g.
	// "127.0.0.0/8"). Every entry must parse; a malformed one is a hard
	// startup error.
	AllowCIDRs []string
}

// hubConfig is HubOptions after validation: PushToken/ReadToken carried
// through unchanged, AllowCIDRs parsed into *net.IPNet for withHubGate's
// per-request checks.
type hubConfig struct {
	pushToken   string
	readToken   string
	allowedNets []*net.IPNet
}

// errMissingPushToken is returned by NewHub when opts.PushToken is empty --
// the hub fails closed rather than ever running a write-accepting server
// with no push gate.
var errMissingPushToken = errors.New("hub: push token is required (--push-token or SCRIM_PUSH_TOKEN)")

// NewHub returns a Server in hub mode: the same serving engine as New, plus
// the push route and the CIDR/token gate in place of the default daemon's
// capability-token middleware (see routes.go, hubgate.go). It fails closed
// if opts.PushToken is empty, or if any entry in opts.AllowCIDRs fails to
// parse as a CIDR.
func NewHub(cfg config.Config, opts HubOptions) (*Server, error) {
	if opts.PushToken == "" {
		return nil, errMissingPushToken
	}
	nets, err := parseCIDRs(opts.AllowCIDRs)
	if err != nil {
		return nil, err
	}

	s := New(cfg)
	s.hubCfg = &hubConfig{
		pushToken:   opts.PushToken,
		readToken:   opts.ReadToken,
		allowedNets: nets,
	}
	return s, nil
}

// parseCIDRs parses every entry in cidrs, returning an error on the first
// malformed one -- a hub is never started with a partially-applied
// allowlist.
func parseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		c := strings.TrimSpace(raw)
		if c == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("hub: parsing allowed CIDR %q: %w", c, err)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}
