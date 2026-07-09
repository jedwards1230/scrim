package oidc

import (
	"strings"
	"testing"
	"time"
)

func testSigner() signer {
	return signer{key: []byte("a-test-hmac-secret-key-32-bytes!"), domain: "session"}
}

func TestSignerRoundTrip(t *testing.T) {
	s := testSigner()
	payload := []byte(`{"hello":"world"}`)
	got, err := s.verify(s.sign(payload))
	if err != nil {
		t.Fatalf("verify(sign(x)) error = %v, want nil", err)
	}
	if string(got) != string(payload) {
		t.Errorf("round-tripped payload = %q, want %q", got, payload)
	}
}

func TestSignerRejectsTampering(t *testing.T) {
	s := testSigner()
	valid := s.sign([]byte("original-payload"))
	payloadB64, macB64, _ := strings.Cut(valid, ".")

	cases := map[string]string{
		"tampered payload":   "dGFtcGVyZWQ" + "." + macB64, // valid b64, wrong MAC
		"tampered mac":       payloadB64 + "." + "AAAA",
		"no separator":       payloadB64 + macB64,
		"empty":              "",
		"bad base64 in both": "!!!.???",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.verify(value); err == nil {
				t.Errorf("verify(%q) error = nil, want a rejection", value)
			}
		})
	}
}

func TestSignerRejectsWrongKey(t *testing.T) {
	value := testSigner().sign([]byte("payload"))
	other := signer{key: []byte("a-DIFFERENT-hmac-secret-32-bytes")}
	if _, err := other.verify(value); err == nil {
		t.Error("verify with the wrong key error = nil, want a rejection")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s := testSigner()
	now := time.Unix(1_000_000, 0)
	cookie := s.encodeSession(Session{
		Subject: "user-123",
		Email:   "user@example.com",
		Name:    "User",
		Groups:  []string{"eng", "ops"},
	}, now.Add(time.Hour))

	sess, err := s.decodeSession(cookie, now)
	if err != nil {
		t.Fatalf("decodeSession error = %v, want nil", err)
	}
	if sess.Subject != "user-123" {
		t.Errorf("subject = %q, want %q", sess.Subject, "user-123")
	}
	if sess.Email != "user@example.com" || sess.Name != "User" {
		t.Errorf("session profile = %+v, want email/name round-tripped", sess)
	}
	if len(sess.Groups) != 2 || sess.Groups[0] != "eng" || sess.Groups[1] != "ops" {
		t.Errorf("session groups = %v, want [eng ops]", sess.Groups)
	}
}

// TestSignerDomainSeparation proves a session cookie cannot verify as a flow
// cookie or vice versa, even though both signers use the same HMAC key -- the
// #38 cross-cookie-replay hardening.
func TestSignerDomainSeparation(t *testing.T) {
	key := []byte("a-test-hmac-secret-key-32-bytes!")
	sessionSigner := signer{key: key, domain: "session"}
	flowSigner := signer{key: key, domain: "flow"}

	sessionCookie := sessionSigner.sign([]byte(`{"sub":"u"}`))
	if _, err := flowSigner.verify(sessionCookie); err == nil {
		t.Error("flow signer verified a session cookie, want a domain-separation rejection")
	}

	flowCookie := flowSigner.sign([]byte(`{"state":"s"}`))
	if _, err := sessionSigner.verify(flowCookie); err == nil {
		t.Error("session signer verified a flow cookie, want a domain-separation rejection")
	}
}

func TestSessionExpiry(t *testing.T) {
	s := testSigner()
	issued := time.Unix(1_000_000, 0)
	cookie := s.encodeSession(Session{Subject: "user-123"}, issued.Add(time.Hour))

	// Exactly at expiry is already invalid (>=), as is anything after.
	for _, now := range []time.Time{issued.Add(time.Hour), issued.Add(2 * time.Hour)} {
		if _, err := s.decodeSession(cookie, now); err == nil {
			t.Errorf("decodeSession at %v error = nil, want expired rejection", now)
		}
	}
	// Just before expiry is still valid.
	if _, err := s.decodeSession(cookie, issued.Add(time.Hour-time.Second)); err != nil {
		t.Errorf("decodeSession just before expiry error = %v, want nil", err)
	}
}

func TestSessionRejectsEmptySubject(t *testing.T) {
	s := testSigner()
	now := time.Unix(1_000_000, 0)
	cookie := s.encodeSession(Session{}, now.Add(time.Hour))
	if _, err := s.decodeSession(cookie, now); err == nil {
		t.Error("decodeSession with empty subject error = nil, want rejection")
	}
}

func TestFlowRoundTripAndExpiry(t *testing.T) {
	s := testSigner()
	now := time.Unix(2_000_000, 0)
	in := flowState{State: "st", Nonce: "no", Verifier: "ve", ReturnTo: "/c/x/"}
	cookie := s.encodeFlow(in, now.Add(10*time.Minute))

	out, err := s.decodeFlow(cookie, now)
	if err != nil {
		t.Fatalf("decodeFlow error = %v, want nil", err)
	}
	if out.State != "st" || out.Nonce != "no" || out.Verifier != "ve" || out.ReturnTo != "/c/x/" {
		t.Errorf("decodeFlow = %+v, want the round-tripped fields", out)
	}
	if _, err := s.decodeFlow(cookie, now.Add(11*time.Minute)); err == nil {
		t.Error("decodeFlow past expiry error = nil, want rejection")
	}
}

func TestSanitizeReturnTo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/c/foo/", "/c/foo/"},
		{"/c/foo?x=1&y=2", "/c/foo?x=1&y=2"},
		{"relative/path", "/"},       // no leading slash
		{"//evil.example.com", "/"},  // protocol-relative
		{"/\\evil.example.com", "/"}, // backslash trick
		{"https://evil.example.com", "/"},
		{"http://evil.example.com/x", "/"},
	}
	for _, tc := range cases {
		if got := sanitizeReturnTo(tc.in); got != tc.want {
			t.Errorf("sanitizeReturnTo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWithOpenID(t *testing.T) {
	if got := withOpenID(nil); got[0] != "openid" {
		t.Errorf("withOpenID(nil) = %v, want a default set starting with openid", got)
	}
	got := withOpenID([]string{"email"})
	if len(got) != 2 || got[0] != "openid" || got[1] != "email" {
		t.Errorf("withOpenID([email]) = %v, want [openid email]", got)
	}
	unchanged := []string{"openid", "custom"}
	if got := withOpenID(unchanged); len(got) != 2 || got[1] != "custom" {
		t.Errorf("withOpenID(%v) = %v, want it unchanged", unchanged, got)
	}
}
