package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// pushAuthHeader is the header a write request must present the hub's push
// token in, as a standard bearer credential.
const pushAuthHeader = "Authorization"

// bearerPrefix is the value prefix pushAuthHeader must carry before the
// token itself.
const bearerPrefix = "Bearer "

// withHubGate replaces withAuth in hub mode (see routes.go): it gates
// writes (any method other than GET/HEAD -- POST /api/push, POST /api/stop,
// any POST/DELETE under /api/canvases) behind the hub's push token, and
// reads (GET/HEAD -- the index, /c/..., SSE, favicon, /api/status) behind a
// CIDR allowlist check first and then, if configured, the hub's separate
// read token via checkToken (the same logic withAuth uses for the default
// daemon, just against a different expected token).
//
// The push token is the write gate on its own -- writes are deliberately
// not also CIDR-checked, since a legitimate push client is commonly outside
// the read allowlist (e.g. a developer's laptop pushing to a homelab hub it
// isn't itself permitted to browse from).
func (s *Server) withHubGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.hasValidPushToken(r) {
				http.Error(w, "unauthorized: missing or invalid push token", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		ip, err := clientIP(r)
		if err != nil || !ipAllowed(ip, s.hubCfg.allowedNets) {
			http.Error(w, "forbidden: client is not in the allowed range", http.StatusForbidden)
			return
		}

		if s.hubCfg.readToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		checkToken(w, r, next, s.hubCfg.readToken)
	})
}

// hasValidPushToken reports whether r carries the hub's push token as a
// standard "Authorization: Bearer <token>" header, compared in constant
// time.
func (s *Server) hasValidPushToken(r *http.Request) bool {
	authz := r.Header.Get(pushAuthHeader)
	if !strings.HasPrefix(authz, bearerPrefix) {
		return false
	}
	return constantTimeEqual(strings.TrimPrefix(authz, bearerPrefix), s.hubCfg.pushToken)
}

// clientIP extracts the request's client IP from r.RemoteAddr, stripping
// the port net/http always attaches for a real network connection.
//
// X-Forwarded-For is deliberately NOT honored here: it's a plain request
// header any client can set to an arbitrary value, so trusting it would let
// any caller claim to be inside the allowlist. Phase 2 (a trusted-proxy
// layer in front of the hub, e.g. Traefik) will need to add explicit
// trusted-proxy handling before that header can be trusted; Phase 1 (this
// code) has no such layer, so it stays unused.
func clientIP(r *http.Request) (net.IP, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port present (uncommon outside of tests using a synthetic
		// RemoteAddr) -- treat the whole value as the host.
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("hub: parsing client IP from remote addr %q", r.RemoteAddr)
	}
	return ip, nil
}

// ipAllowed reports whether ip falls inside any of nets.
func ipAllowed(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
