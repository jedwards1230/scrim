package oidc

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// errBadCookie is the single opaque error every cookie-decoding failure maps
// to: a wrong signature, a malformed structure, an expired payload, and a
// tampered field are deliberately indistinguishable to the caller so no
// decode path can be used as an oracle. The gate treats any of them
// identically -- unauthenticated.
var errBadCookie = errors.New("oidc: invalid or expired signed cookie")

// signer signs and verifies short opaque cookie payloads with HMAC-SHA256.
// It carries the server-side secret plus a fixed domain tag; the signed values
// themselves (session claims, or the login flow's state/nonce/PKCE verifier)
// are integrity-protected but not encrypted -- they hold no data that is secret
// from the very browser they are handed to, only data an attacker must not be
// able to forge or tamper with.
//
// domain separates the two cookie kinds cryptographically: the session signer
// and the flow signer are keyed from the SAME secret but carry distinct domains
// ("session" vs "flow"), and sign mixes the domain into the MAC. A cookie of
// one kind therefore cannot verify as the other even though both are HMAC'd
// with the same key -- closing the cross-cookie replay the #38 review flagged.
type signer struct {
	key    []byte
	domain string
}

// sign returns base64url(payload) + "." + base64url(HMAC(domain || 0 || payload)).
// The payload is carried alongside its MAC rather than only its MAC so the
// verifier is fully stateless -- it recomputes the MAC over the presented
// payload and constant-time-compares, holding no per-session state itself. The
// NUL-separated domain tag makes the MAC context-bound (see the signer doc).
func (s signer) sign(payload []byte) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(s.domain))
	mac.Write([]byte{0})
	mac.Write(payload)
	sum := mac.Sum(nil)
	return b64(payload) + "." + b64(sum)
}

// verify recomputes the MAC (over this signer's domain + the presented payload)
// and constant-time compares it against the presented MAC, returning the
// payload bytes only if they match. A structurally malformed value (missing
// separator, non-base64 half) is rejected exactly like a bad signature, as is a
// value signed under a different domain.
func (s signer) verify(value string) ([]byte, error) {
	payloadB64, macB64, ok := strings.Cut(value, ".")
	if !ok {
		return nil, errBadCookie
	}
	payload, err := unb64(payloadB64)
	if err != nil {
		return nil, errBadCookie
	}
	presented, err := unb64(macB64)
	if err != nil {
		return nil, errBadCookie
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(s.domain))
	mac.Write([]byte{0})
	mac.Write(payload)
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(presented, expected) != 1 {
		return nil, errBadCookie
	}
	return payload, nil
}

// Session is the authenticated identity a valid session cookie attests to. It
// keys on the IdP subject (the stable identity key, per the auto-registration
// model) and additionally carries the email/name/groups claims the hub's
// ownership + grant enforcement needs -- captured at login so enforcement is a
// pure claims read, never an IdP round-trip. A valid, unexpired,
// correctly-signed session cookie IS the authorization for read access.
type Session struct {
	Subject string
	Email   string
	Name    string
	Groups  []string
	Expiry  int64 // unix seconds
}

// session is the on-the-wire (JSON) form of a Session inside the signed cookie.
type session struct {
	Subject string   `json:"sub"`
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	Expiry  int64    `json:"exp"` // unix seconds
}

// encodeSession signs sess valid until expiry, carrying every claim field.
func (s signer) encodeSession(sess Session, expiry time.Time) string {
	payload, _ := json.Marshal(session{
		Subject: sess.Subject,
		Email:   sess.Email,
		Name:    sess.Name,
		Groups:  sess.Groups,
		Expiry:  expiry.Unix(),
	})
	return s.sign(payload)
}

// decodeSession verifies value's signature and expiry, returning the Session it
// attests to. now is passed in so tests can drive expiry deterministically. A
// wrong signature, wrong domain, malformed payload, empty subject, or expired
// session all map to the single opaque errBadCookie.
func (s signer) decodeSession(value string, now time.Time) (Session, error) {
	payload, err := s.verify(value)
	if err != nil {
		return Session{}, err
	}
	var sess session
	if err := json.Unmarshal(payload, &sess); err != nil {
		return Session{}, errBadCookie
	}
	if sess.Subject == "" || now.Unix() >= sess.Expiry {
		return Session{}, errBadCookie
	}
	// session and Session hold the same fields (session is just the JSON-tagged
	// wire form), so a direct conversion carries every claim across.
	return Session(sess), nil
}

// flowState is the per-login-attempt state carried, signed, in a short-lived
// cookie set at /auth/login and read back at /auth/callback. It binds the
// callback to the browser that started the flow: State defeats login CSRF,
// Nonce defeats ID-token replay/injection, Verifier is the PKCE secret the
// token exchange must present, and ReturnTo is the local path to send the
// browser to afterwards.
type flowState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	ReturnTo string `json:"return_to"`
	Expiry   int64  `json:"exp"` // unix seconds
}

// encodeFlow signs a flowState valid until expiry.
func (s signer) encodeFlow(fs flowState, expiry time.Time) string {
	fs.Expiry = expiry.Unix()
	payload, _ := json.Marshal(fs)
	return s.sign(payload)
}

// decodeFlow verifies value's signature and expiry, returning the flowState.
func (s signer) decodeFlow(value string, now time.Time) (flowState, error) {
	payload, err := s.verify(value)
	if err != nil {
		return flowState{}, err
	}
	var fs flowState
	if err := json.Unmarshal(payload, &fs); err != nil {
		return flowState{}, errBadCookie
	}
	if now.Unix() >= fs.Expiry {
		return flowState{}, errBadCookie
	}
	return fs, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
