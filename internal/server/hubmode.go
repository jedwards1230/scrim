package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/logging"
	"github.com/jedwards1230/scrim/internal/oidc"
	"github.com/jedwards1230/scrim/internal/principal"
	"github.com/jedwards1230/scrim/internal/usertoken"
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
	// startup error. When OIDC is configured, it is not consulted for reads --
	// see OIDC below.
	AllowCIDRs []string
	// OIDC, when non-nil, turns on native OIDC login for hub READS: reads then
	// require a valid OIDC session cookie (redirect-to-login for browsers, 401
	// otherwise) instead of the CIDR/read-token check, and the /auth/* login
	// routes are registered. Nil (the default) leaves the hub exactly as it
	// was -- CIDR-allowlisted reads. NewHub performs OIDC discovery here and
	// FAILS CLOSED if it errors, so a hub either enforces OIDC fully or does
	// not advertise it at all. Writes (the push token) are unaffected either
	// way.
	OIDC *oidc.Config
}

// hubConfig is HubOptions after validation: PushToken/ReadToken carried
// through unchanged, AllowCIDRs parsed into *net.IPNet for withHubGate's
// per-request checks.
type hubConfig struct {
	pushToken   string
	readToken   string
	allowedNets []*net.IPNet
	// pushLocks serializes concurrent pushes to the same canvas id (see
	// handlePush's swap sequence); different ids never contend.
	pushLocks keyedMutex
}

// keyedMutex hands out a distinct mutex per string key, so callers can
// serialize work on the same key while letting different keys run in
// parallel. Entries are created on demand and never reclaimed -- the key
// space here is canvas ids, which is small and bounded, so the leak is
// negligible and dropping an entry would reintroduce the very race the lock
// exists to prevent.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock acquires (creating if needed) the mutex for key and returns its
// unlock func for the caller to defer.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*sync.Mutex)
	}
	mu, ok := k.m[key]
	if !ok {
		mu = &sync.Mutex{}
		k.m[key] = mu
	}
	k.mu.Unlock()

	mu.Lock()
	return mu.Unlock
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
	// The principal registry is a lazily-populated, display-only feeder (never
	// read by enforcement). It lives under the hub's meta dir alongside the
	// canvas sidecars.
	s.principals = principal.New(s.metaDir)
	// The user-token store backs the direct (machine) plane's per-principal
	// credentials: a bearer that isn't the admin push token is resolved here.
	s.tokens = usertoken.New(s.metaDir)

	// OIDC discovery happens here so NewHub fails closed: a hub with OIDC
	// configured but an unreachable/misconfigured issuer refuses to start
	// rather than silently falling back to the CIDR gate. The discovery call
	// is bounded internally (see oidc.New); context.Background is fine as its
	// parent -- this is one-shot startup work with no request lifetime to tie
	// it to.
	if opts.OIDC != nil {
		oc := *opts.OIDC
		// Wire coarse auth-failure logging to the daemon's scrubbed logging
		// surface here (rather than in the CLI) so the server package owns its
		// own logging. The reasons oidc passes are static, PII-free strings;
		// CategoryAuth keeps them greppable.
		if oc.LogAuthFailure == nil {
			oc.LogAuthFailure = func(reason string) {
				logging.Error(logging.CategoryAuth, errors.New(reason))
			}
		}
		// Feed the principal registry on every successful login. Best-effort: a
		// registry write failure is logged (scrubbed) but never fails the login
		// -- the registry is display-only and enforcement never reads it.
		if oc.OnLogin == nil {
			registry := s.principals
			oc.OnLogin = func(email, name string, groups []string) {
				if err := registry.Observe(email, name, groups, principal.SourceLogin); err != nil {
					logging.Error(logging.CategoryAuth, fmt.Errorf("principal registry: %w", err))
				}
			}
		}
		auth, err := oidc.New(context.Background(), oc)
		if err != nil {
			return nil, err
		}
		s.oidcAuth = auth
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
