package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/scrim/internal/config"
)

// countingRegistrar builds a registrar whose hub call only counts invocations,
// so the dedupe policy is testable without any HTTP.
func countingRegistrar(t *testing.T, capacity int, err error) (*agentRegistrar, func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	a := &agentRegistrar{
		call: func(context.Context) error {
			mu.Lock()
			n++
			mu.Unlock()
			return err
		},
		timeout: time.Second,
		seen:    make(map[connKey]struct{}),
		cap:     capacity,
	}
	return a, func() int {
		a.wait()
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// TestAgentRegistrarDedupe pins the one-call-per-connection policy: which
// sequences of validated actors collapse to a single hub registration and which
// legitimately register again.
func TestAgentRegistrarDedupe(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	base := actor{ID: "u-1", ClientID: "claude-desktop", IssuedAt: t0}

	cases := []struct {
		name  string
		seq   []actor
		want  int
		capsz int
	}{
		{
			name: "an unseen connection registers exactly once",
			seq:  []actor{base},
			want: 1,
		},
		{
			name: "the same connection again makes no further call",
			seq:  []actor{base, base, base},
			want: 1,
		},
		{
			name: "a NEWER iat for the same subject+client registers again",
			// This is what lets re-authorizing at the IdP clear a revocation:
			// the hub only lifts one for a token issued after it.
			seq:  []actor{base, withIat(base, t0.Add(time.Hour))},
			want: 2,
		},
		{
			name: "an older iat is a different credential and also registers",
			seq:  []actor{base, withIat(base, t0.Add(-time.Hour))},
			want: 2,
		},
		{
			name: "a different OAuth client for the same subject is its own connection",
			seq:  []actor{base, {ID: "u-1", ClientID: "other-agent", IssuedAt: t0}},
			want: 2,
		},
		{
			name: "a different subject for the same client is its own connection",
			seq:  []actor{base, {ID: "u-2", ClientID: "claude-desktop", IssuedAt: t0}},
			want: 2,
		},
		{
			name: "an actor with no subject registers nothing",
			seq:  []actor{{ClientID: "claude-desktop", IssuedAt: t0}},
			want: 0,
		},
		{
			name: "a token with no iat still registers, once",
			seq:  []actor{{ID: "u-3", ClientID: "claude-desktop"}, {ID: "u-3", ClientID: "claude-desktop"}},
			want: 1,
		},
		{
			name: "eviction at the cap costs at most a redundant re-registration",
			// cap 2: registering three connections evicts the first, so seeing
			// it again registers a second time (idempotent hub-side).
			capsz: 2,
			seq: []actor{
				base,
				{ID: "u-2", ClientID: "c", IssuedAt: t0},
				{ID: "u-3", ClientID: "c", IssuedAt: t0},
				base,
			},
			want: 4,
		},
		{
			name:  "under the cap a repeat is still suppressed",
			capsz: 2,
			seq:   []actor{base, {ID: "u-2", ClientID: "c", IssuedAt: t0}, base},
			want:  2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capsz := tc.capsz
			if capsz == 0 {
				capsz = agentRegistrarCap
			}
			a, calls := countingRegistrar(t, capsz, nil)
			for _, act := range tc.seq {
				a.Register(act)
			}
			if got := calls(); got != tc.want {
				t.Errorf("registrations = %d, want %d", got, tc.want)
			}
		})
	}
}

func withIat(a actor, at time.Time) actor {
	a.IssuedAt = at
	return a
}

// TestAgentRegistrarNilIsInert pins that a registrar that was never constructed
// (local mode, stdio, OAuth off) is safe to call and does nothing.
func TestAgentRegistrarNilIsInert(t *testing.T) {
	var a *agentRegistrar
	a.Register(actor{ID: "u-1", ClientID: "c"})
	a.wait()
}

// TestAgentRegistrarSurvivesAFailingHub pins that a registration error is
// swallowed and never retried per-request: the connection is still recorded by
// the hub's gate on the first real tool call, so a broken hub degrades to the
// old timing rather than to one call per request.
func TestAgentRegistrarSurvivesAFailingHub(t *testing.T) {
	a, calls := countingRegistrar(t, agentRegistrarCap, errors.New("hub is down"))
	act := actor{ID: "u-1", ClientID: "c", IssuedAt: time.Unix(1, 0)}
	a.Register(act)
	a.Register(act)
	if got := calls(); got != 1 {
		t.Errorf("registrations = %d, want 1 (a failure must not be retried per request)", got)
	}
}

// hubRecorder is a fake hub that records every machine-API request it receives,
// so a test can assert what was called and with which attribution headers.
type hubRecorder struct {
	mu     sync.Mutex
	hits   []hubHit
	delay  time.Duration // artificial latency on /api/status (the registration call)
	status int           // response status for /api/status (0 = 200)
}

type hubHit struct {
	path     string
	auth     string
	actorID  string
	clientID string
	issuedAt string
}

func (h *hubRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.hits = append(h.hits, hubHit{
		path:     r.URL.Path,
		auth:     r.Header.Get("Authorization"),
		actorID:  r.Header.Get(hdrActorID),
		clientID: r.Header.Get(hdrActorClientID),
		issuedAt: r.Header.Get(hdrActorIssuedAt),
	})
	delay, status := h.delay, h.status
	h.mu.Unlock()

	if r.URL.Path == "/api/status" {
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != 0 {
			http.Error(w, "boom", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pid":1,"version":"test"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`[]`))
}

func (h *hubRecorder) matching(path string) []hubHit {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hubHit
	for _, hit := range h.hits {
		if hit.path == path {
			out = append(out, hit)
		}
	}
	return out
}

// oauthStack stands up the full streamable-HTTP stack in hub mode against the
// hub at hubURL, returning the test server and the validator (whose registrar
// the test inspects).
func oauthStack(t *testing.T, as *fakeAS, hubURL string) (*httptest.Server, *oauthValidator) {
	t.Helper()
	v := newTestValidator(t, as, "https://scrim-mcp.example")
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	handler := newHTTPHandler(cfg, "test", &HubTarget{BaseURL: hubURL, Token: "admin-token"}, v)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, v
}

// connect performs a real MCP handshake (initialize) over the stack presenting
// token, and returns the live session. It deliberately calls NO tool: the point
// is that connecting alone must register the agent connection.
func connect(t *testing.T, ts *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint: ts.URL + mcpPath,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{
			base:   http.DefaultTransport,
			bearer: token,
		}},
		DisableStandaloneSSE: true,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("client Connect over streamable-HTTP: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func mintFor(t *testing.T, as *fakeAS, sub, client string, iat time.Time) string {
	t.Helper()
	extra := map[string]any{"azp": client}
	if !iat.IsZero() {
		extra["iat"] = iat.Unix()
	}
	return as.mint(t, mintOpts{aud: testAudience, sub: sub, scope: scopeRead, extra: extra})
}

// TestRegisterAtConnect is the load-bearing integration test: a client that
// authorizes and connects but calls NO tool must already be registered with the
// hub -- exactly one registration, carrying the admin bearer plus the actor
// headers the hub's admitAgentConn reads.
func TestRegisterAtConnect(t *testing.T) {
	as := newFakeAS(t)
	rec := &hubRecorder{}
	hub := httptest.NewServer(rec)
	defer hub.Close()

	ts, v := oauthStack(t, as, hub.URL)
	iat := time.Now().Add(-time.Minute).Truncate(time.Second)
	connect(t, ts, mintFor(t, as, "u-1", "claude-desktop", iat))
	v.registrar.wait()

	hits := rec.matching("/api/status")
	if len(hits) != 1 {
		t.Fatalf("hub saw %d registration call(s), want exactly 1 (connect must register, and only once)", len(hits))
	}
	h := hits[0]
	if h.auth != "Bearer admin-token" {
		t.Errorf("registration Authorization = %q, want Bearer admin-token", h.auth)
	}
	if h.actorID != "u-1" {
		t.Errorf("registration %s = %q, want u-1", hdrActorID, h.actorID)
	}
	if h.clientID != "claude-desktop" {
		t.Errorf("registration %s = %q, want claude-desktop (a connection with no client id is unnameable)", hdrActorClientID, h.clientID)
	}
	if h.issuedAt != strconv.FormatInt(iat.Unix(), 10) {
		t.Errorf("registration %s = %q, want %d (without it a revocation could never be lifted)", hdrActorIssuedAt, h.issuedAt, iat.Unix())
	}
}

// TestRegisterAtConnectDedupesAcrossRequests pins that a whole session's worth
// of requests (initialize + notifications + tool calls) yields ONE registration
// -- the dedupe is per connection, not per request.
func TestRegisterAtConnectDedupesAcrossRequests(t *testing.T) {
	as := newFakeAS(t)
	rec := &hubRecorder{}
	hub := httptest.NewServer(rec)
	defer hub.Close()

	ts, v := oauthStack(t, as, hub.URL)
	token := mintFor(t, as, "u-1", "claude-desktop", time.Now())
	session := connect(t, ts, token)
	for i := 0; i < 3; i++ {
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list"}); err != nil {
			t.Fatalf("CallTool(list): %v", err)
		}
	}
	v.registrar.wait()

	if n := len(rec.matching("/api/status")); n != 1 {
		t.Errorf("hub saw %d registration call(s) across a full session, want exactly 1", n)
	}
	if n := len(rec.matching("/api/canvases")); n != 3 {
		t.Errorf("hub saw %d list call(s), want 3 (the tool calls themselves must be unaffected)", n)
	}
}

// TestRegisterAtConnectReRegistersOnNewerToken pins the re-authorization path:
// a fresh token for the SAME subject+client, issued later, registers again.
// Without that call a hub-side revocation could never be lifted from this
// process, since the dedupe would suppress the only thing that clears it.
func TestRegisterAtConnectReRegistersOnNewerToken(t *testing.T) {
	as := newFakeAS(t)
	rec := &hubRecorder{}
	hub := httptest.NewServer(rec)
	defer hub.Close()

	ts, v := oauthStack(t, as, hub.URL)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	fresh := time.Now().Truncate(time.Second)

	connect(t, ts, mintFor(t, as, "u-1", "claude-desktop", old))
	v.registrar.wait()
	connect(t, ts, mintFor(t, as, "u-1", "claude-desktop", fresh))
	v.registrar.wait()

	hits := rec.matching("/api/status")
	if len(hits) != 2 {
		t.Fatalf("hub saw %d registration call(s), want 2 (a re-authorized token must re-register)", len(hits))
	}
	if hits[1].issuedAt != strconv.FormatInt(fresh.Unix(), 10) {
		t.Errorf("second registration iat = %q, want the NEW token's %d", hits[1].issuedAt, fresh.Unix())
	}
}

// TestRegisterAtConnectSurvivesABrokenHub pins the invariant that matters most:
// a hub whose registration route errors or hangs must not fail or delay
// authentication. Each case connects and makes a tool call against a hub whose
// /api/status is broken while its other routes work.
func TestRegisterAtConnectSurvivesABrokenHub(t *testing.T) {
	cases := []struct {
		name string
		rec  *hubRecorder
	}{
		{name: "status errors 503", rec: &hubRecorder{status: http.StatusServiceUnavailable}},
		{name: "status hangs", rec: &hubRecorder{delay: 3 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			as := newFakeAS(t)
			hub := httptest.NewServer(tc.rec)
			defer hub.Close()

			ts, v := oauthStack(t, as, hub.URL)
			start := time.Now()
			session := connect(t, ts, mintFor(t, as, "u-1", "claude-desktop", time.Now()))
			if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list"}); err != nil {
				t.Fatalf("CallTool(list) must still succeed with a broken hub registration: %v", err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("connect+call took %v; registration must not block the request path", d)
			}
			// The hanging case leaves a goroutine sleeping; drain it so the
			// test does not race the httptest server's Close.
			v.registrar.wait()
		})
	}
}

// TestRegisterAtConnectSurvivesAnUnreachableHub is the same invariant against a
// hub that is not listening at all (connection refused rather than a slow or
// erroring response). Auth must still succeed; only the tool call fails, and it
// fails for its own reasons.
func TestRegisterAtConnectSurvivesAnUnreachableHub(t *testing.T) {
	as := newFakeAS(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // nothing is listening on deadURL now

	ts, v := oauthStack(t, as, deadURL)
	start := time.Now()
	// Connecting performs the MCP handshake through the OAuth gate; a dead hub
	// must not turn a valid token into a 401/503.
	connect(t, ts, mintFor(t, as, "u-1", "claude-desktop", time.Now()))
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("connect took %v against a dead hub; registration must not block it", d)
	}
	v.registrar.wait()
}

// TestRegisterAtConnectInertWithoutHubOrOAuth pins that the registrar exists
// ONLY for an OAuth-authenticated hub-mode transport. Local mode has no remote
// hub to register with, and an OAuth-off transport never runs the gate at all
// (stdio likewise never builds this handler).
func TestRegisterAtConnectInertWithoutHubOrOAuth(t *testing.T) {
	as := newFakeAS(t)
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}

	t.Run("local mode with OAuth gets no registrar", func(t *testing.T) {
		v := newTestValidator(t, as, "https://scrim-mcp.example")
		_ = newHTTPHandler(cfg, "test", nil, v)
		if v.registrar != nil {
			t.Error("local mode must not register agent connections: there is no remote hub")
		}
	})

	t.Run("hub mode without OAuth makes no registration call", func(t *testing.T) {
		rec := &hubRecorder{}
		hub := httptest.NewServer(rec)
		defer hub.Close()

		handler := newHTTPHandler(cfg, "test", &HubTarget{BaseURL: hub.URL, Token: "admin-token"}, nil)
		ts := httptest.NewServer(handler)
		defer ts.Close()

		transport := &mcp.StreamableClientTransport{Endpoint: ts.URL + mcpPath, DisableStandaloneSSE: true}
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
		session, err := client.Connect(context.Background(), transport, nil)
		if err != nil {
			t.Fatalf("client Connect: %v", err)
		}
		defer func() { _ = session.Close() }()

		if n := len(rec.matching("/api/status")); n != 0 {
			t.Errorf("hub saw %d registration call(s) with OAuth off, want 0", n)
		}
	})
}

// TestRegisterAtConnectLogsNothingIdentifying pins the logging rule: the
// registration path must never emit a subject, client id, token, or hub URL. It
// captures the standard logger (the only sink this package could accidentally
// reach) across a full connect + failing-registration cycle.
func TestRegisterAtConnectLogsNothingIdentifying(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	as := newFakeAS(t)
	rec := &hubRecorder{status: http.StatusServiceUnavailable} // exercise the error path too
	hub := httptest.NewServer(rec)
	defer hub.Close()

	ts, v := oauthStack(t, as, hub.URL)
	token := mintFor(t, as, "secret-subject", "secret-client", time.Now())
	connect(t, ts, token)
	v.registrar.wait()

	got := buf.String()
	for _, secret := range []string{"secret-subject", "secret-client", token, hub.URL, "admin-token"} {
		if strings.Contains(got, secret) {
			t.Errorf("log output leaked %q:\n%s", secret, got)
		}
	}
}
