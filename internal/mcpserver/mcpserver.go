// Package mcpserver exposes scrim's CLI surface as MCP tools over stdio
// (default) or streamable-HTTP, in either of two modes:
//
//   - LOCAL mode (default): tools drive the local scrim daemon and on-disk
//     canvas directory via the SAME primitives the matching CLI verb calls
//     (config/daemon/apiclient/canvas/snapshot/pushclient).
//   - HUB mode (`scrim mcp --hub URL`): tools drive a remote hub's
//     bearer-authenticated machine API over HTTP (internal/server's hub mode),
//     so a remotely-hosted scrim mcp can author canvas content over the wire
//     with no shared disk.
//
// The two modes are unified behind the unexported backend interface; the tool
// handlers are transport-agnostic. The tool surface is self-describing per
// mode: read_file/write_file (inline content) and edit_file (server-side
// exact-string replacement) exist in both, `path` (a server-local directory
// lookup) is local-only.
//
// Invariants this package upholds:
//   - On the stdio transport stdout is the MCP protocol channel: nothing here
//     ever writes to stdout except through the MCP SDK. Diagnostics go to
//     stderr only.
//   - No tool ever launches a browser or execs open/xdg-open. The `link` tool
//     returns URLs as DATA; it is the print-only sibling of `scrim link`, and
//     this package deliberately does not import internal/openurl.
//   - Canvas URLs, canvas content, and capability/push tokens are never logged.
package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/daemon"
	"github.com/jedwards1230/scrim/internal/fileedit"
	"github.com/jedwards1230/scrim/internal/gzipx"
	"github.com/jedwards1230/scrim/internal/pushclient"
	"github.com/jedwards1230/scrim/internal/state"
)

// HubTarget selects hub mode for a scrim MCP server: the remote hub's
// externally-reachable base URL and its push token (the machine-API bearer
// credential). A nil *HubTarget selects local mode.
type HubTarget struct {
	BaseURL string
	Token   string
}

// server holds the backend every tool handler delegates to, the resolved
// config that path (local-only) and push (both modes, reads local disk) need, the scrim version
// reported in the MCP handshake, and whether this is a local-mode server (which
// tools are registered depends on it).
type server struct {
	backend backend
	cfg     config.Config
	ver     string
	local   bool
}

// resolveDaemon returns an apiclient plus the daemon state for cfg. When
// selfStart is true it self-starts a daemon if none is healthy (daemon.Ensure,
// the same seam the daemon-backed CLI verbs use) and running is always true on
// a nil error; when false it only probes for an already-healthy daemon
// (daemon.TryLoadHealthy) and reports running=false without starting anything.
//
// It is a package-level variable purely so tests can override it with an
// httptest-backed client + synthetic state, exercising the daemon-backed
// localBackend methods without spawning a real detached daemon process.
var resolveDaemon = func(cfg config.Config, selfStart bool) (client *apiclient.Client, st *state.State, running bool, err error) {
	if selfStart {
		st, err = daemon.Ensure(cfg)
		if err != nil {
			return nil, nil, false, err
		}
	} else {
		var ok bool
		st, ok = daemon.TryLoadHealthy(cfg)
		if !ok {
			return nil, nil, false, nil
		}
	}
	client = apiclient.NewWithToken(fmt.Sprintf("http://%s:%d", st.Host, st.Port), st.Token)
	return client, st, true, nil
}

// NewServer builds the scrim MCP server for cfg. hub selects the backend: nil
// = local mode (localBackend), non-nil = hub mode (hubBackend). A blank ver is
// reported as "dev" in the implementation handshake.
func NewServer(cfg config.Config, ver string, hub *HubTarget) *mcp.Server {
	if ver == "" {
		ver = "dev"
	}
	if hub != nil {
		return newServer(newHubBackend(hub.BaseURL, hub.Token), cfg, ver, false)
	}
	return newServer(newLocalBackend(cfg), cfg, ver, true)
}

// newServer registers the tool set against b. local decides whether the
// local-only `path` tool is registered: in hub mode a server-local path is
// meaningless to a remote client, so the tool is simply absent and the surface
// is self-describing.
func newServer(b backend, cfg config.Config, ver string, local bool) *mcp.Server {
	s := &server{backend: b, cfg: cfg, ver: ver, local: local}
	srv := mcp.NewServer(&mcp.Implementation{Name: "scrim", Version: ver}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "add",
		Description: "Register a canvas. Returns its view URL. In local mode self-starts the scrim daemon; in hub mode creates it on the remote hub.",
	}, s.handleAdd)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list",
		Description: "List registered canvases.",
	}, s.handleList)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "link",
		Description: "Return the view URL for a canvas, or the dashboard URL when no id is given. URLs are returned as data — this never launches a browser.",
	}, s.handleLink)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "rm",
		Description: "Remove a canvas.",
	}, s.handleRm)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "snap",
		Description: "Snapshot a canvas's current contents.",
	}, s.handleSnap)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "snaps",
		Description: "List a canvas's snapshots, newest first.",
	}, s.handleSnaps)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "revert",
		Description: "Restore a canvas from a snapshot (latest by default), taking a safety snapshot of the current contents first.",
	}, s.handleRevert)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "copy_canvas",
		Description: "Duplicate a canvas into a new one server-side (no file bytes round-trip through the client). Fails if the destination exists unless overwrite is set, which snapshots the destination first.",
	}, s.handleCopyCanvas)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "status",
		Description: "Report scrim daemon/hub status.",
	}, s.handleStatus)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_files",
		Description: "List every file in a canvas as canvas-relative paths with size and modification time — the way to discover what a canvas contains before reading or editing it. Returns no file content.",
	}, s.handleListFiles)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_file",
		Description: "Read one file from a canvas and return its text content inline. The file must be UTF-8 text and at most ~2 MiB. Pass encoding=\"gzip+base64\" to receive larger or binary files gzip-compressed then base64-encoded instead.",
	}, s.handleReadFile)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write_file",
		Description: "Write one file into an existing canvas from inline text content (create the canvas first with add). Content is capped at ~2 MiB. Pass encoding=\"gzip+base64\" to send content gzip-compressed then base64-encoded (the cap applies to the decoded bytes) — worthwhile for large HTML/JS.",
	}, s.handleWriteFile)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "edit_file",
		Description: "Replace an exact string in one canvas file server-side — the token-efficient alternative to write_file for changing an existing file (tokens scale with the diff, not the file). old_string must occur exactly once unless replace_all is set. Pass an edits array to apply many replacements in one transactional call (all-or-nothing: any failing edit leaves the file untouched); edits is mutually exclusive with the single old_string/new_string fields.",
	}, s.handleEditFile)

	// push is local-only whole-canvas push to an external hub, reading the
	// canvas straight off local disk — unchanged from single-mode. It stays
	// registered in both modes: it operates on the mcp process's own --dir,
	// independent of the backend.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "push",
		Description: "Pack a canvas from the MACHINE RUNNING THIS MCP SERVER's own disk (its --dir) and push it once to a hub. It sources from the MCP server's filesystem — NOT the calling agent's machine and NOT the hub — so it's only useful when scrim mcp runs where the canvas files live. For a remotely-hosted MCP server (e.g. in-cluster beside the hub) that disk is the pod's, not yours: author canvas content over the wire with write_file/edit_file instead. Never launches a browser.",
	}, s.handlePush)

	// path is a server-local directory lookup — meaningless to a remote hub
	// client, so it's registered in local mode only.
	if local {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "path",
			Description: "Return the on-disk directory for a canvas. Pure local filesystem lookup — does not talk to or start the daemon. Local mode only.",
		}, s.handlePath)
	}

	return srv
}

// Serve builds the scrim MCP server and runs it on the stdio transport,
// blocking until ctx is cancelled or stdin closes. stderr is accepted for
// symmetry with ServeHTTP and as the sanctioned diagnostics channel, but the
// stdio path stays deliberately silent: stdout carries the MCP protocol and
// emitting anything unprompted would only add noise for the MCP host.
func Serve(ctx context.Context, cfg config.Config, ver string, hub *HubTarget, stderr io.Writer) error {
	_ = stderr
	srv := NewServer(cfg, ver, hub)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// ── tool: add ───────────────────────────────────────────────────────────────

type addInput struct {
	ID          string `json:"id" jsonschema:"canvas id to create (required)"`
	Title       string `json:"title,omitempty" jsonschema:"optional canvas title"`
	Description string `json:"description,omitempty" jsonschema:"optional canvas description"`
	Icon        string `json:"icon,omitempty" jsonschema:"optional emoji icon; a default is derived from the id when omitted"`
}

type addOutput struct {
	ID  string `json:"id"`
	Dir string `json:"dir"`
	URL string `json:"url"`
}

func (s *server) handleAdd(ctx context.Context, _ *mcp.CallToolRequest, in addInput) (*mcp.CallToolResult, addOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), addOutput{}, nil
	}
	info, err := s.backend.Add(ctx, in.ID, in.Title, in.Description, in.Icon)
	if err != nil {
		return errorResult(err.Error()), addOutput{}, nil
	}
	out := addOutput{ID: info.ID, Dir: info.Dir, URL: info.URL}
	return textResult(fmt.Sprintf("created canvas %s", info.ID)), out, nil
}

// ── tool: list ────────────────────────────────────────────────────────────--

type listInput struct{}

type canvasSummary struct {
	ID         string    `json:"id"`
	Title      string    `json:"title,omitempty"`
	URL        string    `json:"url"`
	Dir        string    `json:"dir"`
	Icon       string    `json:"icon"`
	Color      string    `json:"color"`
	ModifiedAt time.Time `json:"modified_at"`
	SSEClients int       `json:"sse_clients"`
}

type listOutput struct {
	Canvases []canvasSummary `json:"canvases"`
}

func (s *server) handleList(ctx context.Context, _ *mcp.CallToolRequest, _ listInput) (*mcp.CallToolResult, listOutput, error) {
	canvases, err := s.backend.List(ctx)
	if err != nil {
		return errorResult(err.Error()), listOutput{}, nil
	}
	out := listOutput{Canvases: make([]canvasSummary, 0, len(canvases))}
	for _, c := range canvases {
		// canvasSummary is CanvasInfo's wire shape -- identical fields, JSON
		// tags added -- so a direct conversion is exact.
		out.Canvases = append(out.Canvases, canvasSummary(c))
	}
	return textResult(fmt.Sprintf("%d canvas(es)", len(out.Canvases))), out, nil
}

// ── tool: link ───────────────────────────────────────────────────────────--

type linkInput struct {
	ID string `json:"id,omitempty" jsonschema:"canvas id; omit to get the dashboard URL"`
}

type linkOutput struct {
	URLs []string `json:"urls"`
}

func (s *server) handleLink(ctx context.Context, _ *mcp.CallToolRequest, in linkInput) (*mcp.CallToolResult, linkOutput, error) {
	// Validate before anything else so a bad id never reaches the backend.
	if in.ID != "" {
		if err := canvas.ValidateID(in.ID); err != nil {
			return errorResult(err.Error()), linkOutput{}, nil
		}
	}
	urls, err := s.backend.Link(ctx, in.ID)
	if err != nil {
		return errorResult(err.Error()), linkOutput{}, nil
	}
	summary := ""
	if len(urls) > 0 {
		summary = urls[0]
	}
	return textResult(summary), linkOutput{URLs: urls}, nil
}

// dashboardURL builds the token-qualified dashboard URL for the daemon
// described by st: http://host:port/ plus a ?t=<token> query when the daemon
// has auth enabled. It mirrors cli.baseURLFor for the "/" (no-id) case. Used by
// localBackend.Link.
func dashboardURL(st *state.State) string {
	url := st.BaseURL() + "/"
	if st.AuthEnabled() {
		url += "?t=" + st.Token
	}
	return url
}

// ── tool: path (local mode only) ────────────────────────────────────────────

type pathInput struct {
	ID string `json:"id" jsonschema:"canvas id (required)"`
}

type pathOutput struct {
	Path string `json:"path"`
}

func (s *server) handlePath(_ context.Context, _ *mcp.CallToolRequest, in pathInput) (*mcp.CallToolResult, pathOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), pathOutput{}, nil
	}
	dir := canvas.Dir(s.cfg.CanvasesDir(), in.ID)
	return textResult(dir), pathOutput{Path: dir}, nil
}

// ── tool: rm ─────────────────────────────────────────────────────────────--

type rmInput struct {
	ID string `json:"id" jsonschema:"canvas id to remove (required)"`
}

type rmOutput struct {
	Removed string `json:"removed"`
}

func (s *server) handleRm(ctx context.Context, _ *mcp.CallToolRequest, in rmInput) (*mcp.CallToolResult, rmOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), rmOutput{}, nil
	}
	if err := s.backend.Remove(ctx, in.ID); err != nil {
		return errorResult(err.Error()), rmOutput{}, nil
	}
	return textResult(fmt.Sprintf("removed %s", in.ID)), rmOutput{Removed: in.ID}, nil
}

// ── tool: snap ───────────────────────────────────────────────────────────--

type snapInput struct {
	ID    string `json:"id" jsonschema:"canvas id to snapshot (required)"`
	Label string `json:"label,omitempty" jsonschema:"optional label appended to the snapshot's timestamp"`
}

type snapOutput struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

func (s *server) handleSnap(ctx context.Context, _ *mcp.CallToolRequest, in snapInput) (*mcp.CallToolResult, snapOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), snapOutput{}, nil
	}
	entry, err := s.backend.Snap(ctx, in.ID, in.Label)
	if err != nil {
		return errorResult(err.Error()), snapOutput{}, nil
	}
	return textResult(fmt.Sprintf("snapshot %s created for %s", entry.Name, in.ID)),
		snapOutput{Name: entry.Name, Dir: entry.Dir}, nil
}

// ── tool: snaps ──────────────────────────────────────────────────────────--

type snapsInput struct {
	ID string `json:"id" jsonschema:"canvas id whose snapshots to list (required)"`
}

type snapshotSummary struct {
	Name      string    `json:"name"`
	Timestamp time.Time `json:"timestamp"`
	Label     string    `json:"label,omitempty"`
}

type snapsOutput struct {
	Snapshots []snapshotSummary `json:"snapshots"`
}

func (s *server) handleSnaps(ctx context.Context, _ *mcp.CallToolRequest, in snapsInput) (*mcp.CallToolResult, snapsOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), snapsOutput{}, nil
	}
	entries, err := s.backend.Snaps(ctx, in.ID)
	if err != nil {
		return errorResult(err.Error()), snapsOutput{}, nil
	}
	out := snapsOutput{Snapshots: make([]snapshotSummary, 0, len(entries))}
	for _, e := range entries {
		out.Snapshots = append(out.Snapshots, snapshotSummary{Name: e.Name, Timestamp: e.Timestamp, Label: e.Label})
	}
	return textResult(fmt.Sprintf("%d snapshot(s) for %s", len(out.Snapshots), in.ID)), out, nil
}

// ── tool: revert ─────────────────────────────────────────────────────────--

type revertInput struct {
	ID       string `json:"id" jsonschema:"canvas id to revert (required)"`
	Snapshot string `json:"snapshot,omitempty" jsonschema:"snapshot name to restore; defaults to the latest snapshot"`
}

type revertOutput struct {
	Reverted string `json:"reverted"`
	Snapshot string `json:"snapshot"`
}

func (s *server) handleRevert(ctx context.Context, _ *mcp.CallToolRequest, in revertInput) (*mcp.CallToolResult, revertOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), revertOutput{}, nil
	}
	info, err := s.backend.Revert(ctx, in.ID, in.Snapshot)
	if err != nil {
		return errorResult(err.Error()), revertOutput{}, nil
	}
	return textResult(fmt.Sprintf("reverted %s to snapshot %s (pre-revert state saved as a new snapshot)", info.Reverted, info.Snapshot)),
		revertOutput(info), nil
}

// ── tool: copy_canvas ──────────────────────────────────────────────────────

type copyCanvasInput struct {
	From      string `json:"from" jsonschema:"source canvas id to copy (required)"`
	To        string `json:"to" jsonschema:"destination canvas id (required); must not already exist unless overwrite is set"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"replace an existing destination, snapshotting it first"`
}

type copyCanvasOutput struct {
	From string `json:"from"`
	To   string `json:"to"`
	URL  string `json:"url,omitempty"`
}

func (s *server) handleCopyCanvas(ctx context.Context, _ *mcp.CallToolRequest, in copyCanvasInput) (*mcp.CallToolResult, copyCanvasOutput, error) {
	if err := canvas.ValidateID(in.From); err != nil {
		return errorResult(err.Error()), copyCanvasOutput{}, nil
	}
	if err := canvas.ValidateID(in.To); err != nil {
		return errorResult(err.Error()), copyCanvasOutput{}, nil
	}
	info, err := s.backend.CopyCanvas(ctx, in.From, in.To, in.Overwrite)
	if err != nil {
		return errorResult(err.Error()), copyCanvasOutput{}, nil
	}
	// copyCanvasOutput is CopyInfo's wire shape -- identical fields, JSON tags
	// added -- so a direct conversion is exact.
	return textResult(fmt.Sprintf("copied %s to %s", info.From, info.To)),
		copyCanvasOutput(info), nil
}

// ── tool: status ─────────────────────────────────────────────────────────--

type statusInput struct{}

type statusOutput struct {
	Running            bool    `json:"running"`
	PID                int     `json:"pid,omitempty"`
	Host               string  `json:"host,omitempty"`
	Port               int     `json:"port,omitempty"`
	Version            string  `json:"version,omitempty"`
	UptimeSeconds      float64 `json:"uptime_seconds,omitempty"`
	CanvasCount        int     `json:"canvas_count,omitempty"`
	SSEClients         int     `json:"sse_clients,omitempty"`
	IdleSeconds        float64 `json:"idle_seconds,omitempty"`
	IdleTimeoutSeconds float64 `json:"idle_timeout_seconds,omitempty"`
}

func (s *server) handleStatus(ctx context.Context, _ *mcp.CallToolRequest, _ statusInput) (*mcp.CallToolResult, statusOutput, error) {
	info, err := s.backend.Status(ctx)
	if err != nil {
		return errorResult(err.Error()), statusOutput{}, nil
	}
	if !info.Running {
		return textResult("no daemon running"), statusOutput{Running: false}, nil
	}
	out := statusOutput{
		Running:            true,
		PID:                info.PID,
		Host:               info.Host,
		Port:               info.Port,
		Version:            info.Version,
		UptimeSeconds:      info.UptimeSeconds,
		CanvasCount:        info.CanvasCount,
		SSEClients:         info.SSEClients,
		IdleSeconds:        info.IdleSeconds,
		IdleTimeoutSeconds: info.IdleTimeoutSeconds,
	}
	return textResult(fmt.Sprintf("daemon running (pid %d, %d canvas(es))", info.PID, info.CanvasCount)), out, nil
}

// ── tool: list_files ───────────────────────────────────────────────────────

type listFilesInput struct {
	ID string `json:"id" jsonschema:"canvas id whose files to list (required)"`
}

type listFilesOutput struct {
	Files []FileEntry `json:"files"`
}

func (s *server) handleListFiles(ctx context.Context, _ *mcp.CallToolRequest, in listFilesInput) (*mcp.CallToolResult, listFilesOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), listFilesOutput{}, nil
	}
	files, err := s.backend.ListFiles(ctx, in.ID)
	if err != nil {
		return errorResult(err.Error()), listFilesOutput{}, nil
	}
	if files == nil {
		files = []FileEntry{}
	}
	return textResult(fmt.Sprintf("%d file(s) in %s", len(files), in.ID)), listFilesOutput{Files: files}, nil
}

// ── tool: read_file ────────────────────────────────────────────────────────

type readFileInput struct {
	ID       string `json:"id" jsonschema:"canvas id (required)"`
	Path     string `json:"path" jsonschema:"file path within the canvas, e.g. index.html or assets/app.js (required)"`
	Encoding string `json:"encoding,omitempty" jsonschema:"omit for inline UTF-8 text, or set to gzip+base64 to receive content gzip-compressed then base64-encoded (for large or binary files)"`
}

type readFileOutput struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Content string `json:"content"`
	// Encoding reports how Content is encoded: "" (empty) for plain UTF-8
	// text, or "gzip+base64" when the caller requested compressed content.
	Encoding string `json:"encoding,omitempty"`
}

func (s *server) handleReadFile(ctx context.Context, _ *mcp.CallToolRequest, in readFileInput) (*mcp.CallToolResult, readFileOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), readFileOutput{}, nil
	}
	if in.Path == "" {
		return errorResult("path is required"), readFileOutput{}, nil
	}
	if in.Encoding != "" && in.Encoding != encodingGzipBase64 {
		return errorResult(unsupportedEncodingMsg(in.Encoding)), readFileOutput{}, nil
	}
	data, err := s.backend.ReadFile(ctx, in.ID, in.Path)
	if err != nil {
		return errorResult(err.Error()), readFileOutput{}, nil
	}
	// gzip+base64: return the compressed representation, which is binary-safe
	// (no UTF-8 requirement) and small for large files. The summary reports
	// the decoded size; the structured content carries the encoded bytes.
	if in.Encoding == encodingGzipBase64 {
		encoded := base64.StdEncoding.EncodeToString(gzipx.Deflate(data))
		out := readFileOutput{ID: in.ID, Path: in.Path, Content: encoded, Encoding: encodingGzipBase64}
		return textResult(fmt.Sprintf("%d bytes (gzip+base64 encoded)", len(data))), out, nil
	}
	// Plain-text path: content rides inline as text; a non-UTF-8 file can't be
	// represented without corruption, so refuse it (the caller can re-request
	// with encoding=gzip+base64) rather than mangle binary bytes.
	if !utf8.Valid(data) {
		return errorResult(fmt.Sprintf("file %q in canvas %q is not UTF-8 text (read_file returns text only; request encoding=gzip+base64 for binary)", in.Path, in.ID)),
			readFileOutput{}, nil
	}
	out := readFileOutput{ID: in.ID, Path: in.Path, Content: string(data)}
	return textResult(string(data)), out, nil
}

// ── content encoding (write_file / read_file) ───────────────────────────────

// encodingGzipBase64 is the one non-default content encoding read_file and
// write_file accept: the content field is the file's bytes gzip-compressed
// then base64-encoded. It shrinks a large HTML/JS payload ~4x on the wire and
// in an agent's context window (see #42); it is orthogonal to the hub machine
// API's own Content-Encoding: gzip transport (that's the mcp-server↔hub hop).
const encodingGzipBase64 = "gzip+base64"

// decodeContent turns a write_file content field + encoding into the raw bytes
// to write. An empty encoding is plain text (the bytes are the string itself);
// gzip+base64 is base64-decoded then gunzipped with the per-file cap applied to
// the DECODED size (the gzip-bomb guard). Any other encoding is rejected.
func decodeContent(content, encoding string) ([]byte, error) {
	switch encoding {
	case "":
		return []byte(content), nil
	case encodingGzipBase64:
		compressed, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("content is not valid base64: %w", err)
		}
		raw, err := gzipx.Inflate(compressed, maxFileBytes)
		if err != nil {
			if errors.Is(err, gzipx.ErrTooLarge) {
				return nil, fmt.Errorf("decoded content exceeds the %d-byte (2 MiB) per-file limit", maxFileBytes)
			}
			return nil, fmt.Errorf("content is not valid gzip: %w", err)
		}
		return raw, nil
	default:
		return nil, errors.New(unsupportedEncodingMsg(encoding))
	}
}

// unsupportedEncodingMsg is the shared rejection message for an encoding value
// read_file/write_file don't recognize.
func unsupportedEncodingMsg(encoding string) string {
	return fmt.Sprintf("unsupported encoding %q (want %q or omit for plain text)", encoding, encodingGzipBase64)
}

// ── tool: write_file ───────────────────────────────────────────────────────

type writeFileInput struct {
	ID       string `json:"id" jsonschema:"canvas id (required); the canvas must already exist"`
	Path     string `json:"path" jsonschema:"file path within the canvas, e.g. index.html or assets/app.js (required)"`
	Content  string `json:"content" jsonschema:"file content to write; plain text by default, or base64(gzip(bytes)) when encoding is gzip+base64 (capped at ~2 MiB decoded)"`
	Encoding string `json:"encoding,omitempty" jsonschema:"content encoding; omit for plain text, or set to gzip+base64 to send content gzip-compressed then base64-encoded"`
}

type writeFileOutput struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	BytesWritten int    `json:"bytes_written"`
}

func (s *server) handleWriteFile(ctx context.Context, _ *mcp.CallToolRequest, in writeFileInput) (*mcp.CallToolResult, writeFileOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), writeFileOutput{}, nil
	}
	if in.Path == "" {
		return errorResult("path is required"), writeFileOutput{}, nil
	}
	content, err := decodeContent(in.Content, in.Encoding)
	if err != nil {
		return errorResult(err.Error()), writeFileOutput{}, nil
	}
	// The cap applies to the DECODED bytes: a gzip+base64 payload is small on
	// the wire but its inflated size is what lands on disk.
	if len(content) > maxFileBytes {
		return errorResult(fmt.Sprintf("content is %d bytes, over the %d-byte (2 MiB) per-file limit", len(content), maxFileBytes)),
			writeFileOutput{}, nil
	}
	if err := s.backend.WriteFile(ctx, in.ID, in.Path, content); err != nil {
		return errorResult(err.Error()), writeFileOutput{}, nil
	}
	out := writeFileOutput{ID: in.ID, Path: in.Path, BytesWritten: len(content)}
	return textResult(fmt.Sprintf("wrote %d bytes to %s/%s", len(content), in.ID, in.Path)), out, nil
}

// ── tool: edit_file ────────────────────────────────────────────────────────

type editFileInput struct {
	ID         string     `json:"id" jsonschema:"canvas id (required)"`
	Path       string     `json:"path" jsonschema:"file path within the canvas, e.g. index.html (required); the file must already exist"`
	OldString  string     `json:"old_string,omitempty" jsonschema:"exact text to replace; must occur exactly once unless replace_all is set. Required for a single edit; omit when using edits"`
	NewString  string     `json:"new_string,omitempty" jsonschema:"replacement text; must differ from old_string. Pairs with old_string for a single edit"`
	ReplaceAll bool       `json:"replace_all,omitempty" jsonschema:"replace every occurrence of old_string instead of requiring exactly one"`
	Edits      []editSpec `json:"edits,omitempty" jsonschema:"a transactional batch of edits applied in order (all-or-nothing); mutually exclusive with old_string/new_string/replace_all"`
}

// editSpec is one entry in edit_file's batch array -- the same three fields a
// single edit takes.
type editSpec struct {
	OldString  string `json:"old_string" jsonschema:"exact text to replace; must occur exactly once unless replace_all is set"`
	NewString  string `json:"new_string" jsonschema:"replacement text; must differ from old_string"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence of old_string instead of requiring exactly one"`
}

type editFileOutput struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
}

func (s *server) handleEditFile(ctx context.Context, _ *mcp.CallToolRequest, in editFileInput) (*mcp.CallToolResult, editFileOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), editFileOutput{}, nil
	}
	if in.Path == "" {
		return errorResult("path is required"), editFileOutput{}, nil
	}

	// Batch path: the edits array is mutually exclusive with the single-edit
	// fields (matching the hub PATCH handler), and applied transactionally.
	if len(in.Edits) > 0 {
		if in.OldString != "" || in.NewString != "" || in.ReplaceAll {
			return errorResult("edits is mutually exclusive with old_string/new_string/replace_all"), editFileOutput{}, nil
		}
		edits := make([]fileedit.Edit, len(in.Edits))
		for i, e := range in.Edits {
			// Fail the two pure input errors fast, per index, before any bytes
			// cross the wire -- the wording mirrors fileedit's own messages.
			if e.OldString == "" {
				return errorResult(fmt.Sprintf("edit %d: %s", i, fileedit.ErrOldStringEmpty.Error())), editFileOutput{}, nil
			}
			if e.OldString == e.NewString {
				return errorResult(fmt.Sprintf("edit %d: %s", i, fileedit.ErrNoChange.Error())), editFileOutput{}, nil
			}
			edits[i] = fileedit.Edit{OldString: e.OldString, NewString: e.NewString, ReplaceAll: e.ReplaceAll}
		}
		info, err := s.backend.EditFileBatch(ctx, in.ID, in.Path, edits)
		if err != nil {
			return errorResult(err.Error()), editFileOutput{}, nil
		}
		return textResult(fmt.Sprintf("applied %d edit(s), %d replacement(s) in %s/%s", len(edits), info.Replacements, in.ID, info.Path)),
			editFileOutput(info), nil
	}

	// Fail the two pure input errors fast (before any file read locally, or
	// any bytes cross the wire in hub mode) with fileedit's own messages, so
	// the wording is identical to what the backend would return.
	if in.OldString == "" {
		return errorResult(fileedit.ErrOldStringEmpty.Error()), editFileOutput{}, nil
	}
	if in.OldString == in.NewString {
		return errorResult(fileedit.ErrNoChange.Error()), editFileOutput{}, nil
	}
	info, err := s.backend.EditFile(ctx, in.ID, in.Path, in.OldString, in.NewString, in.ReplaceAll)
	if err != nil {
		return errorResult(err.Error()), editFileOutput{}, nil
	}
	// editFileOutput is EditInfo's wire shape -- identical fields, JSON tags
	// added -- so a direct conversion is exact.
	return textResult(fmt.Sprintf("made %d replacement(s) in %s/%s", info.Replacements, in.ID, info.Path)),
		editFileOutput(info), nil
}

// ── tool: push (local disk → external hub, both modes) ───────────────────────

type pushInput struct {
	ID    string `json:"id" jsonschema:"local canvas id to push (required)"`
	To    string `json:"to" jsonschema:"hub base URL, e.g. http://127.0.0.1:7788 (required)"`
	Token string `json:"token" jsonschema:"hub push token (required)"`
}

type pushOutput struct {
	URL string `json:"url"`
}

func (s *server) handlePush(ctx context.Context, _ *mcp.CallToolRequest, in pushInput) (*mcp.CallToolResult, pushOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), pushOutput{}, nil
	}
	if in.To == "" {
		return errorResult("to is required (the hub base URL)"), pushOutput{}, nil
	}
	if in.Token == "" {
		return errorResult("token is required (the hub push token)"), pushOutput{}, nil
	}

	// Read the canvas straight off disk from cfg, exactly like cli.cmdPush —
	// push uses only --dir, never a daemon host/port.
	canvasDir := canvas.Dir(s.cfg.CanvasesDir(), in.ID)
	info, err := canvas.Get(s.cfg.CanvasesDir(), s.cfg.MetaDir(), in.ID)
	if err != nil {
		return errorResult(fmt.Sprintf("canvas %q not found at %s", in.ID, canvasDir)), pushOutput{}, nil
	}

	data, err := pushclient.Pack(canvasDir)
	if err != nil {
		return errorResult(err.Error()), pushOutput{}, nil
	}
	hubURL, err := pushclient.Push(ctx, in.To, in.ID, in.Token, info.Title, info.Description, info.Icon, data)
	if err != nil {
		return errorResult(err.Error()), pushOutput{}, nil
	}
	return textResult(hubURL), pushOutput{URL: hubURL}, nil
}

// ── result helpers (mirror labctl's textResult/errorResult) ─────────────────

// textResult wraps a human-readable summary in a successful CallToolResult.
// The typed Out value the handler returns alongside it is marshalled into
// StructuredContent by the SDK.
func textResult(summary string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary}},
	}
}

// errorResult wraps an expected, user-facing failure (canvas not found,
// invalid id, a validation or daemon error) as a tool-level error: IsError
// with a text message the agent can read. Handlers return it with the zero Out
// value and a NIL Go error — a non-nil Go error is reserved for genuine
// internal faults the SDK should surface as a protocol error.
func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
