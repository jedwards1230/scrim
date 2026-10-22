package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/scrim/internal/config"
)

// The protocol revisions are asserted verbatim rather than read back from the
// SDK, so an SDK-side rename or default change cannot make these tests pass
// vacuously.
const (
	// legacyProtocol is what clients negotiate with scrim through `initialize`
	// today.
	legacyProtocol = "2025-11-25"
	// modernProtocol is the sessionless revision (SEP-2575). go-sdk serves it
	// over streamable HTTP only when the handler is stateless.
	modernProtocol = "2026-07-28"
)

// pathTestCanvasID is the canvas id the `path` tool is called with. `path` is
// a pure local-mode lookup (no daemon, no hub), so a reply carrying the
// computed directory proves the call reached the tool handler.
const pathTestCanvasID = "proto-check"

// mcpCall is one JSON-RPC message POSTed to /mcp.
type mcpCall struct {
	method string
	params map[string]any
	// notification omits the JSON-RPC id (the SDK answers 202, no body).
	notification bool
	// header carries the extra request headers (MCP-Protocol-Version,
	// Mcp-Session-Id, Mcp-Method, ...).
	header http.Header
}

// mcpReply is the decoded response to an mcpCall.
type mcpReply struct {
	status int
	header http.Header
	raw    string
	env    map[string]any // the JSON-RPC envelope; nil when the body is not one
}

// newProtocolTestServer stands up the production handler newHTTPHandler
// builds (local mode, OAuth off), so the /mcp handler under test is exactly
// the one ServeHTTP serves.
func newProtocolTestServer(t *testing.T) (*httptest.Server, config.Config) {
	t.Helper()
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	ts := httptest.NewServer(newHTTPHandler(cfg, "test", nil, nil))
	t.Cleanup(ts.Close)
	return ts, cfg
}

// postMCP sends call to ts's /mcp endpoint and decodes the reply.
func postMCP(t *testing.T, ts *httptest.Server, call mcpCall) mcpReply {
	t.Helper()

	msg := map[string]any{"jsonrpc": "2.0", "method": call.method}
	if !call.notification {
		msg["id"] = 1
	}
	if call.params != nil {
		msg["params"] = call.params
	}
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal %s: %v", call.method, err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+mcpPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, vs := range call.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", call.method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", call.method, err)
	}
	return mcpReply{
		status: resp.StatusCode,
		header: resp.Header,
		raw:    string(raw),
		env:    decodeRPCEnvelope(resp.Header.Get("Content-Type"), raw),
	}
}

// decodeRPCEnvelope decodes a streamable-HTTP response body. The handler
// answers calls with an SSE stream by default, so the envelope arrives as the
// payload of a `data:` line; an application/json body (e.g. a protocol
// rejection) is decoded directly.
func decodeRPCEnvelope(contentType string, raw []byte) map[string]any {
	decode := func(b []byte) map[string]any {
		var env map[string]any
		if err := json.Unmarshal(b, &env); err != nil {
			return nil
		}
		if _, ok := env["jsonrpc"]; !ok {
			return nil
		}
		return env
	}
	if !strings.HasPrefix(contentType, "text/event-stream") {
		return decode(raw)
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
			if env := decode([]byte(strings.TrimSpace(data))); env != nil {
				return env
			}
		}
	}
	return nil
}

// resultOf returns the reply's JSON-RPC result object, failing the test on a
// non-200 status or a JSON-RPC error (the -32022 "protocol version ... is not
// supported by this server" rejection lands here).
func (r mcpReply) resultOf(t *testing.T, what string) map[string]any {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200 (body: %s)", what, r.status, r.raw)
	}
	if r.env == nil {
		t.Fatalf("%s: no JSON-RPC envelope in body: %s", what, r.raw)
	}
	if rpcErr, ok := r.env["error"]; ok {
		t.Fatalf("%s: JSON-RPC error %v (body: %s)", what, rpcErr, r.raw)
	}
	result, ok := r.env["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: want an object result, got %#v (body: %s)", what, r.env["result"], r.raw)
	}
	return result
}

// assertPathResult checks a tools/call `path` result actually came from the
// tool handler (not an error result): it carries the canvas directory under
// cfg's canvases dir.
func assertPathResult(t *testing.T, cfg config.Config, result map[string]any) {
	t.Helper()
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("path returned a tool error: %#v", result)
	}
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("path: want structuredContent, got %#v", result)
	}
	got, _ := sc["path"].(string)
	if !strings.HasPrefix(got, cfg.CanvasesDir()) || !strings.HasSuffix(got, pathTestCanvasID) {
		t.Fatalf("path: got %q, want %s's directory under %s", got, pathTestCanvasID, cfg.CanvasesDir())
	}
}

// TestHTTPLegacyCallWithProtocolMetaIsAccepted replays the pattern a
// forwarding MCP proxy uses once it speaks 2026-07-28 upstream (ContextForge
// v1.1.0+ does this for its federated servers): negotiate 2025-11-25 with
// `initialize`, then forward the downstream client's `_meta` -- including the
// 2026-07-28-era key io.modelcontextprotocol/protocolVersion -- on tools/call.
//
// A stateful handler (StreamableHTTPOptions nil) rejects ANY call carrying
// that key with HTTP 400 / -32022 `protocol version "2025-11-25" is not
// supported by this server`. The stateless handler serves it. The session id
// is forwarded when the server issues one, so a stateful handler fails here
// for the real reason, not for a missing session.
func TestHTTPLegacyCallWithProtocolMetaIsAccepted(t *testing.T) {
	ts, cfg := newProtocolTestServer(t)

	initReply := postMCP(t, ts, mcpCall{
		method: "initialize",
		params: map[string]any{
			"protocolVersion": legacyProtocol,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "proxy-stand-in", "version": "0.0.0"},
		},
	})
	initResult := initReply.resultOf(t, "initialize@"+legacyProtocol)
	if got := initResult["protocolVersion"]; got != legacyProtocol {
		t.Fatalf("initialize negotiated protocolVersion = %#v, want %q", got, legacyProtocol)
	}

	header := http.Header{"Mcp-Protocol-Version": {legacyProtocol}}
	if sid := initReply.header.Get("Mcp-Session-Id"); sid != "" {
		header.Set("Mcp-Session-Id", sid)
	}

	initialized := postMCP(t, ts, mcpCall{method: "notifications/initialized", notification: true, header: header})
	if initialized.status != http.StatusAccepted {
		t.Fatalf("notifications/initialized: status = %d, want 202 (body: %s)", initialized.status, initialized.raw)
	}

	call := postMCP(t, ts, mcpCall{
		method: "tools/call",
		params: map[string]any{
			"name":      "path",
			"arguments": map[string]any{"id": pathTestCanvasID},
			"_meta":     map[string]any{mcp.MetaKeyProtocolVersion: legacyProtocol},
		},
		header: header,
	})
	assertPathResult(t, cfg, call.resultOf(t, "tools/call@"+legacyProtocol+" with _meta protocolVersion"))
}

// TestHTTPModernProtocolIsServed drives the sessionless 2026-07-28 revision:
// no initialize, the MCP-Protocol-Version header matched by the per-request
// `_meta` triple, plus the Mcp-Method / Mcp-Name headers that revision
// requires. A stateful handler both hides 2026-07-28 from server/discover (a
// silent downgrade) and rejects the call with -32022.
func TestHTTPModernProtocolIsServed(t *testing.T) {
	ts, cfg := newProtocolTestServer(t)

	modern := func(method string, params map[string]any) mcpCall {
		params["_meta"] = map[string]any{
			mcp.MetaKeyProtocolVersion:    modernProtocol,
			mcp.MetaKeyClientCapabilities: map[string]any{},
			mcp.MetaKeyClientInfo:         map[string]any{"name": "scrim-protocol-test", "version": "0.0.0"},
		}
		h := http.Header{
			"Mcp-Protocol-Version": {modernProtocol},
			"Mcp-Method":           {method},
		}
		if name, ok := params["name"].(string); ok {
			h.Set("Mcp-Name", name)
		}
		return mcpCall{method: method, params: params, header: h}
	}

	t.Run("server/discover advertises "+modernProtocol, func(t *testing.T) {
		result := postMCP(t, ts, modern("server/discover", map[string]any{})).resultOf(t, "server/discover")
		raw, _ := result["supportedVersions"].([]any)
		var versions []string
		for _, v := range raw {
			if s, ok := v.(string); ok {
				versions = append(versions, s)
			}
		}
		if !slices.Contains(versions, modernProtocol) {
			t.Fatalf("server/discover supportedVersions = %v, want it to include %q (a stateful handler withholds it)", versions, modernProtocol)
		}
	})

	t.Run("tools/call", func(t *testing.T) {
		call := postMCP(t, ts, modern("tools/call", map[string]any{"name": "path", "arguments": map[string]any{"id": pathTestCanvasID}}))
		assertPathResult(t, cfg, call.resultOf(t, "tools/call@"+modernProtocol))
	})
}
