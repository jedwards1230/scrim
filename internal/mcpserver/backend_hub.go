package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jedwards1230/scrim/internal/fileedit"
	"github.com/jedwards1230/scrim/internal/gzipx"
)

// gzipWireMinBytes is the file-size floor below which the hub backend sends a
// PUT body (and expects a GET response) uncompressed: below ~1 KiB, gzip's
// framing overhead outweighs the saving. It matches the hub read handler's own
// gzipReadMinBytes so both hops make the same call.
const gzipWireMinBytes = 1024

// maxCompressedReadBytes bounds the COMPRESSED bytes read from a hub GET
// response. The hub gzips any file over gzipReadMinBytes even when it doesn't
// shrink, so an at-cap incompressible file (a ~2 MiB image) comes back gzip-
// EXPANDED slightly past maxFileBytes; this cap allows deflate's worst-case
// expansion (well under 0.1% + a few framing bytes) so the stream isn't
// truncated. The DECODED size is still held to maxFileBytes by gzipx.Inflate.
const maxCompressedReadBytes = maxFileBytes + maxFileBytes/1000 + 64

// hubTimeout bounds a single hub machine-API call. These are control-plane and
// per-file operations against a hub the operator controls (commonly in-cluster
// behind ContextForge), not long-running streams.
const hubTimeout = 30 * time.Second

// hubBackend drives a remote scrim hub's bearer-authenticated machine API over
// HTTP -- the counterpart to the routes in internal/server's hub mode. It has
// no shared disk with the hub: every file read/write carries its bytes inline
// over the wire. baseURL is the hub's externally-reachable base (e.g.
// https://scrim-hub.example); token is the hub's push token, sent as
// "Authorization: Bearer <token>" on every request.
type hubBackend struct {
	baseURL string
	token   string
	http    *http.Client
}

func newHubBackend(baseURL, token string) *hubBackend {
	return &hubBackend{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http: &http.Client{
			Timeout: hubTimeout,
			// Refuse redirects outright. cleanRelPath keeps request paths
			// canonical so none should ever occur -- but if one did (e.g. a
			// ServeMux 301 to a cleaned path), Go's client would follow it as
			// a body-less GET, silently degrading a PUT/PATCH into a read
			// that "succeeds". Failing loudly beats that.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("hub sent a redirect; refusing to follow it")
			},
		},
	}
}

// canvasWire mirrors the hub's apiclient.CanvasResponse fields the backend uses.
type canvasWire struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Icon        string    `json:"icon"`
	Color       string    `json:"color"`
	Dir         string    `json:"dir"`
	URL         string    `json:"url"`
	ModifiedAt  time.Time `json:"modified_at"`
	SSEClients  int       `json:"sse_clients"`
}

func (c canvasWire) toInfo(baseURL string) CanvasInfo {
	return CanvasInfo{
		ID:    c.ID,
		Title: c.Title,
		// Always present the client-reachable view URL, never the hub's
		// own internal host:port (which may be a container address).
		URL:        linkURL(baseURL, c.ID),
		Dir:        c.Dir,
		Icon:       c.Icon,
		Color:      c.Color,
		ModifiedAt: c.ModifiedAt,
		SSEClients: c.SSEClients,
	}
}

// statusWire mirrors the hub's apiclient.StatusResponse.
type statusWire struct {
	PID                int     `json:"pid"`
	Host               string  `json:"host"`
	Port               int     `json:"port"`
	Version            string  `json:"version"`
	UptimeSeconds      float64 `json:"uptime_seconds"`
	CanvasCount        int     `json:"canvas_count"`
	IdleTimeoutSeconds float64 `json:"idle_timeout_seconds"`
	IdleSeconds        float64 `json:"idle_seconds"`
	SSEClients         int     `json:"sse_clients"`
}

// snapWire mirrors the hub's snapshotResponse.
type snapWire struct {
	Name      string    `json:"name"`
	Timestamp time.Time `json:"timestamp"`
	Label     string    `json:"label"`
}

// linkURL builds the client-reachable view URL for a canvas from the hub base,
// without any token in the URL. id=="" yields the hub root.
func linkURL(baseURL, id string) string {
	base := strings.TrimRight(baseURL, "/")
	if id == "" {
		return base + "/"
	}
	return base + "/c/" + id + "/"
}

func (b *hubBackend) List(ctx context.Context) ([]CanvasInfo, error) {
	var wire []canvasWire
	if err := b.doJSON(ctx, http.MethodGet, "/api/canvases", nil, &wire); err != nil {
		return nil, err
	}
	out := make([]CanvasInfo, 0, len(wire))
	for _, c := range wire {
		out = append(out, c.toInfo(b.baseURL))
	}
	return out, nil
}

func (b *hubBackend) Add(ctx context.Context, id, title, description, icon string) (CanvasInfo, error) {
	body := map[string]string{"id": id, "title": title, "description": description, "icon": icon}
	var wire canvasWire
	if err := b.doJSON(ctx, http.MethodPost, "/api/canvases", body, &wire); err != nil {
		return CanvasInfo{}, err
	}
	return wire.toInfo(b.baseURL), nil
}

func (b *hubBackend) Remove(ctx context.Context, id string) error {
	return b.doJSON(ctx, http.MethodDelete, "/api/canvases/"+url.PathEscape(id), nil, nil)
}

func (b *hubBackend) Status(ctx context.Context) (StatusInfo, error) {
	var wire statusWire
	if err := b.doJSON(ctx, http.MethodGet, "/api/status", nil, &wire); err != nil {
		return StatusInfo{}, err
	}
	return StatusInfo{
		Running:            true,
		PID:                wire.PID,
		Host:               wire.Host,
		Port:               wire.Port,
		Version:            wire.Version,
		UptimeSeconds:      wire.UptimeSeconds,
		CanvasCount:        wire.CanvasCount,
		SSEClients:         wire.SSEClients,
		IdleSeconds:        wire.IdleSeconds,
		IdleTimeoutSeconds: wire.IdleTimeoutSeconds,
	}, nil
}

func (b *hubBackend) Link(_ context.Context, id string) ([]string, error) {
	// No round-trip needed: the view URL is a pure function of the hub base
	// and the canvas id, and carries no token.
	return []string{linkURL(b.baseURL, id)}, nil
}

func (b *hubBackend) Snap(ctx context.Context, id, label string) (SnapInfo, error) {
	var resp struct {
		Name string `json:"name"`
		Dir  string `json:"dir"`
	}
	if err := b.doJSON(ctx, http.MethodPost, "/api/canvases/"+url.PathEscape(id)+"/snapshots", map[string]string{"label": label}, &resp); err != nil {
		return SnapInfo{}, err
	}
	return SnapInfo{Name: resp.Name, Dir: resp.Dir, Label: label}, nil
}

func (b *hubBackend) Snaps(ctx context.Context, id string) ([]SnapInfo, error) {
	var wire []snapWire
	if err := b.doJSON(ctx, http.MethodGet, "/api/canvases/"+url.PathEscape(id)+"/snapshots", nil, &wire); err != nil {
		return nil, err
	}
	// The hub already returns newest-first.
	out := make([]SnapInfo, 0, len(wire))
	for _, s := range wire {
		out = append(out, SnapInfo{Name: s.Name, Timestamp: s.Timestamp, Label: s.Label})
	}
	return out, nil
}

func (b *hubBackend) Revert(ctx context.Context, id, name string) (RevertInfo, error) {
	// The hub's revert route requires an explicit snapshot name; resolve the
	// latest here (newest-first list) when the caller didn't name one, so hub
	// mode supports the same "revert to latest" default as local mode.
	if name == "" {
		snaps, err := b.Snaps(ctx, id)
		if err != nil {
			return RevertInfo{}, err
		}
		if len(snaps) == 0 {
			return RevertInfo{}, fmt.Errorf("no snapshots for canvas %s", id)
		}
		name = snaps[0].Name
	}
	var resp struct {
		Reverted string `json:"reverted"`
		Snapshot string `json:"snapshot"`
	}
	path := "/api/canvases/" + url.PathEscape(id) + "/snapshots/" + url.PathEscape(name) + "/revert"
	if err := b.doJSON(ctx, http.MethodPost, path, nil, &resp); err != nil {
		return RevertInfo{}, err
	}
	return RevertInfo{Reverted: resp.Reverted, Snapshot: resp.Snapshot}, nil
}

func (b *hubBackend) ListFiles(ctx context.Context, id string) ([]FileEntry, error) {
	// The list route is /api/canvases/{id}/files with no trailing file path;
	// build it directly rather than via filesPath (which appends a segment).
	var wire []FileEntry
	if err := b.doJSON(ctx, http.MethodGet, "/api/canvases/"+url.PathEscape(id)+"/files", nil, &wire); err != nil {
		return nil, err
	}
	return wire, nil
}

func (b *hubBackend) CopyCanvas(ctx context.Context, from, to string, overwrite bool) (CopyInfo, error) {
	// The hub does the recursive copy + atomic swap server-side; only the
	// {to, overwrite} envelope crosses the wire. A 409 (target exists without
	// overwrite) surfaces via doJSON's hubStatusError.
	body := map[string]any{"to": to, "overwrite": overwrite}
	var resp struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := b.doJSON(ctx, http.MethodPost, "/api/canvases/"+url.PathEscape(from)+"/copy", body, &resp); err != nil {
		return CopyInfo{}, err
	}
	// Present the client-reachable view URL, never the hub's internal path.
	return CopyInfo{From: resp.From, To: resp.To, URL: linkURL(b.baseURL, resp.To)}, nil
}

func (b *hubBackend) ShareCanvas(ctx context.Context, id, kind, target string) (GrantResult, error) {
	// The hub is the enforcement point: it checks the caller owns the canvas and
	// (for a user token) that the target is within the token's allowance, minting
	// a link secret server-side for a link grant. A rejection (403/404/409)
	// surfaces via doJSON's hubStatusError with the hub's actionable message.
	body := map[string]string{"kind": kind, "target": target}
	var resp struct {
		Kind       string `json:"kind"`
		Target     string `json:"target"`
		LinkID     string `json:"link_id"`
		LinkSecret string `json:"link_secret"`
	}
	if err := b.doJSON(ctx, http.MethodPost, "/api/canvases/"+url.PathEscape(id)+"/grants", body, &resp); err != nil {
		return GrantResult{}, err
	}
	return GrantResult{Kind: resp.Kind, Target: resp.Target, LinkID: resp.LinkID, LinkSecret: resp.LinkSecret}, nil
}

func (b *hubBackend) ListGrants(ctx context.Context, id string) (GrantsResult, error) {
	var resp struct {
		Owner  string `json:"owner"`
		Grants []struct {
			Kind   string `json:"kind"`
			Target string `json:"target"`
			LinkID string `json:"link_id"`
		} `json:"grants"`
	}
	if err := b.doJSON(ctx, http.MethodGet, "/api/canvases/"+url.PathEscape(id)+"/grants", nil, &resp); err != nil {
		return GrantsResult{}, err
	}
	out := GrantsResult{Owner: resp.Owner, Grants: make([]GrantEntry, 0, len(resp.Grants))}
	for _, g := range resp.Grants {
		out.Grants = append(out.Grants, GrantEntry{Kind: g.Kind, Target: g.Target, LinkID: g.LinkID})
	}
	return out, nil
}

func (b *hubBackend) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	rel, err := cleanRelPath(path)
	if err != nil {
		return nil, err
	}
	req, err := b.newRequest(ctx, http.MethodGet, b.filesURL(id, rel), nil)
	if err != nil {
		return nil, err
	}
	// Offer to receive a compressed response. Setting Accept-Encoding
	// ourselves opts OUT of net/http's transparent gzip decoding, so we must
	// (and do) inflate manually below when the hub honors it.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading file from hub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Cap the response so a misbehaving hub can't stream unbounded bytes. For a
	// gzip response this bounds the COMPRESSED bytes (Inflate then bounds the
	// decoded size), so the cap must allow gzip's worst-case EXPANSION of an
	// at-cap incompressible file -- otherwise a ~2 MiB PNG stored in a canvas
	// would be truncated mid-stream and fail to inflate. maxCompressedReadBytes
	// covers that; the decoded size is still held to maxFileBytes below.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCompressedReadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading file response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// An error body is small JSON, never gzip-encoded, so surface it as-is.
		return nil, hubStatusError(resp.StatusCode, data)
	}
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		raw, err := gzipx.Inflate(data, maxFileBytes)
		if err != nil {
			if errors.Is(err, gzipx.ErrTooLarge) {
				return nil, fmt.Errorf("hub file exceeds the %d-byte (2 MiB) limit", maxFileBytes)
			}
			return nil, fmt.Errorf("decoding gzip response from hub: %w", err)
		}
		return raw, nil
	}
	if int64(len(data)) > maxFileBytes {
		return nil, fmt.Errorf("hub file exceeds the %d-byte (2 MiB) limit", maxFileBytes)
	}
	return data, nil
}

func (b *hubBackend) WriteFile(ctx context.Context, id, path string, content []byte) error {
	rel, err := cleanRelPath(path)
	if err != nil {
		return err
	}
	if len(content) > maxFileBytes {
		return fmt.Errorf("file exceeds the %d-byte (2 MiB) per-file limit", maxFileBytes)
	}
	// Compress a large body on the wire (Content-Encoding: gzip); the hub
	// inflates it back under the same per-file cap. Small bodies go verbatim --
	// gzip framing would only add bytes. The cap check above is against the
	// DECODED size, matching what the hub enforces on the inflated result.
	body := content
	contentEncoding := ""
	if len(content) >= gzipWireMinBytes {
		if gz := gzipx.Deflate(content); len(gz) < len(content) {
			body = gz
			contentEncoding = "gzip"
		}
	}
	req, err := b.newRequest(ctx, http.MethodPut, b.filesURL(id, rel), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("writing file to hub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return hubStatusError(resp.StatusCode, respBody)
	}
	return nil
}

func (b *hubBackend) EditFile(ctx context.Context, id, path, oldStr, newStr string, replaceAll bool) (EditInfo, error) {
	// The edit itself is applied hub-side (fileedit.Apply behind PATCH), so
	// only the strings cross the wire -- never the file. Conflict errors (409:
	// old_string not found / ambiguous) surface via doJSON's hubStatusError
	// with the hub's path-free message.
	body := map[string]any{"old_string": oldStr, "new_string": newStr, "replace_all": replaceAll}
	return b.patchEdit(ctx, id, path, body)
}

func (b *hubBackend) EditFileBatch(ctx context.Context, id, path string, edits []fileedit.Edit) (EditInfo, error) {
	// Send the whole batch in one PATCH; the hub applies it transactionally
	// (fileedit.ApplyBatch) and a failing edit surfaces as a 409 naming its
	// index, via hubStatusError's path-free message.
	wire := make([]map[string]any, len(edits))
	for i, e := range edits {
		wire[i] = map[string]any{"old_string": e.OldString, "new_string": e.NewString, "replace_all": e.ReplaceAll}
	}
	return b.patchEdit(ctx, id, path, map[string]any{"edits": wire})
}

// patchEdit is the shared PATCH round-trip behind EditFile (single) and
// EditFileBatch: it canonicalizes the path, PATCHes body to the file route,
// and maps the {path, replacements} response into an EditInfo. body is either
// the single-edit fields or an {"edits": [...]} envelope -- the hub decides
// which by their presence.
func (b *hubBackend) patchEdit(ctx context.Context, id, path string, body map[string]any) (EditInfo, error) {
	rel, err := cleanRelPath(path)
	if err != nil {
		return EditInfo{}, err
	}
	var resp struct {
		Path         string `json:"path"`
		Replacements int    `json:"replacements"`
	}
	if err := b.doJSON(ctx, http.MethodPatch, b.filesPath(id, rel), body, &resp); err != nil {
		return EditInfo{}, err
	}
	return EditInfo{Path: resp.Path, Replacements: resp.Replacements}, nil
}

// filesPath builds the machine-API files path for id+path, escaping the id as
// a single segment and each path segment individually so subdirectories
// survive while any odd characters are encoded. cleanRelPath has already
// rejected absolute paths and ".." segments and canonicalized the rest
// before this is reached, so the URL never draws a ServeMux redirect.
func (b *hubBackend) filesPath(id, path string) string {
	segs := strings.Split(strings.TrimLeft(path, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/api/canvases/" + url.PathEscape(id) + "/files/" + strings.Join(segs, "/")
}

// filesURL is filesPath with the hub base prepended, for the raw-body
// requests (ReadFile/WriteFile) that don't go through doJSON.
func (b *hubBackend) filesURL(id, path string) string {
	return b.baseURL + b.filesPath(id, path)
}

// newRequest builds an authenticated request with the bearer token attached.
// A nil body sends no body; a non-nil one is wrapped in a bytes.Reader.
func (b *hubBackend) newRequest(ctx context.Context, method, rawURL string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, r)
	if err != nil {
		return nil, fmt.Errorf("building hub request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	// When the call carries a verified CF-forwarded actor, attribute it to the
	// hub via X-Scrim-Actor-* on top of the admin bearer (#51). The hub trusts
	// these headers ONLY because they ride the valid admin push token above; a
	// call with no verified actor sends the admin bearer alone and is attributed
	// to admin. localBackend, having no remote hub, ignores the actor entirely.
	if a, ok := actorFromContext(ctx); ok {
		req.Header.Set(hdrActorID, a.ID)
		req.Header.Set(hdrActorEmail, a.Email)
		req.Header.Set(hdrActorGroups, strings.Join(a.Groups, ","))
	}
	return req, nil
}

// doJSON performs a JSON request/response round-trip against the hub: it
// marshals body (when non-nil) as JSON, attaches the bearer token, and
// unmarshals a 2xx response into out (when non-nil). A non-2xx status becomes
// an error including the response body.
func (b *hubBackend) doJSON(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
	}
	req, err := b.newRequest(ctx, method, b.baseURL+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling hub %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return fmt.Errorf("reading hub response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return hubStatusError(resp.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding hub response: %w", err)
	}
	return nil
}

// hubStatusError turns a non-2xx hub response into an error. It surfaces the
// hub's JSON {"error": "..."} message when present, otherwise the raw body,
// never a URL or token.
func hubStatusError(status int, body []byte) error {
	trimmed := bytes.TrimSpace(body)
	var je struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(trimmed, &je) == nil && je.Error != "" {
		return fmt.Errorf("hub responded %d: %s", status, capExcerpt(je.Error))
	}
	return fmt.Errorf("hub responded %d: %s", status, capExcerpt(string(trimmed)))
}

// capExcerpt bounds an error-body excerpt so a large (up to ~2 MiB) hub
// response can never balloon an error string handed back to an MCP client.
func capExcerpt(s string) string {
	const max = 4096
	if len(s) > max {
		return s[:max] + "... (truncated)"
	}
	return s
}
