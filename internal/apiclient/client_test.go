package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient wires a Client (optionally token-bearing) to an httptest
// server running handler, and returns both. The server is torn down via
// t.Cleanup.
func newTestClient(t *testing.T, token string, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	if token != "" {
		return NewWithToken(ts.URL, token), ts
	}
	return New(ts.URL), ts
}

func TestClientStatusRoundTrip(t *testing.T) {
	c, _ := newTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/status" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(StatusResponse{Version: "1.2.3", CanvasCount: 4, Active: true})
	})

	got, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got.Version != "1.2.3" || got.CanvasCount != 4 || !got.Active {
		t.Errorf("Status() = %+v, want version=1.2.3 canvas_count=4 active=true", got)
	}
}

// TestClientTokenAttachment proves NewWithToken attaches the token as the "?t="
// query parameter on every request, and that a plain New() client sends none.
func TestClientTokenAttachment(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		wantToken string
	}{
		{name: "token-bearing client sends ?t=", token: "secret-tok", wantToken: "secret-tok"},
		{name: "plain client sends no token", token: "", wantToken: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotToken string
			c, _ := newTestClient(t, tt.token, func(w http.ResponseWriter, r *http.Request) {
				gotToken = r.URL.Query().Get("t")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(StatusResponse{Version: "dev"})
			})
			if _, err := c.Status(context.Background()); err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if gotToken != tt.wantToken {
				t.Errorf("server saw token %q, want %q", gotToken, tt.wantToken)
			}
		})
	}
}

// TestClientCreateCanvasSendsJSONBody checks the POST verb: a JSON body with
// the right fields, the application/json Content-Type, and the decoded
// response.
func TestClientCreateCanvasSendsJSONBody(t *testing.T) {
	var gotBody map[string]any
	var gotContentType string
	c, _ := newTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/canvases" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotContentType = r.Header.Get("Content-Type")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CanvasResponse{ID: "note", Title: "T", URL: "/c/note/"})
	})

	got, err := c.CreateCanvas(context.Background(), "note", "T", "desc", "🎨")
	if err != nil {
		t.Fatalf("CreateCanvas() error = %v", err)
	}
	if got.ID != "note" || got.URL != "/c/note/" {
		t.Errorf("CreateCanvas() = %+v, want id=note url=/c/note/", got)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody["id"] != "note" || gotBody["title"] != "T" || gotBody["description"] != "desc" || gotBody["icon"] != "🎨" {
		t.Errorf("request body = %v, want id/title/description/icon set", gotBody)
	}
}

func TestClientListCanvases(t *testing.T) {
	c, _ := newTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]CanvasResponse{{ID: "a"}, {ID: "b"}})
	})
	got, err := c.ListCanvases(context.Background())
	if err != nil {
		t.Fatalf("ListCanvases() error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("ListCanvases() = %+v, want [a b]", got)
	}
}

// TestClientDeleteCanvasEscapesID confirms DeleteCanvas path-escapes the id
// and issues a DELETE. A 2xx with an empty body must decode as success.
func TestClientDeleteCanvasEscapesID(t *testing.T) {
	var gotPath, gotMethod string
	c, _ := newTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteCanvas(context.Background(), "a b"); err != nil {
		t.Fatalf("DeleteCanvas() error = %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/canvases/a%20b" {
		t.Errorf("path = %q, want /api/canvases/a%%20b", gotPath)
	}
}

func TestClientStop(t *testing.T) {
	var gotMethod, gotPath string
	c, _ := newTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/stop" {
		t.Errorf("Stop() issued %s %s, want POST /api/stop", gotMethod, gotPath)
	}
}

// TestClientErrStatusOnNon2xx pins the non-2xx path: do() returns a typed
// *ErrStatus carrying the code and the (trimmed) response body.
func TestClientErrStatusOnNon2xx(t *testing.T) {
	c, _ := newTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "  bad request body  \n")
	})
	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("Status() error = nil, want an ErrStatus")
	}
	var se *ErrStatus
	if !errors.As(err, &se) {
		t.Fatalf("error = %v (%T), want *ErrStatus", err, err)
	}
	if se.Code != http.StatusBadRequest {
		t.Errorf("ErrStatus.Code = %d, want 400", se.Code)
	}
	if se.Body != "bad request body" {
		t.Errorf("ErrStatus.Body = %q, want the trimmed body", se.Body)
	}
	if got := se.Error(); got != "daemon responded 400: bad request body" {
		t.Errorf("ErrStatus.Error() = %q, want the formatted message", got)
	}
}

// TestIsNotFound covers the 404 helper across its true, false, and non-ErrStatus
// cases.
func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "404 ErrStatus", err: &ErrStatus{Code: http.StatusNotFound}, want: true},
		{name: "500 ErrStatus", err: &ErrStatus{Code: http.StatusInternalServerError}, want: false},
		{name: "wrapped 404", err: errors.Join(errors.New("ctx"), &ErrStatus{Code: http.StatusNotFound}), want: true},
		{name: "non-ErrStatus error", err: errors.New("boom"), want: false},
		{name: "nil error", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotFound(tt.err); got != tt.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestClientDecodeErrorOnMalformedBody covers do()'s JSON-decode failure path:
// a 2xx with a non-empty body that isn't valid JSON for the target type.
func TestClientDecodeErrorOnMalformedBody(t *testing.T) {
	c, _ := newTestClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{not valid json")
	})
	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("Status() error = nil, want a decode error")
	}
	if got := err.Error(); !strings.Contains(got, "decoding response") {
		t.Errorf("error = %q, want it to mention decoding the response", got)
	}
}
