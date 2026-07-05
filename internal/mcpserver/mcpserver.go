// Package mcpserver exposes scrim's CLI surface as MCP tools over stdio
// (default) or streamable-HTTP. Every tool drives the SAME reusable primitives
// the matching CLI verb calls (config/daemon/apiclient/canvas/snapshot/
// pushclient), so behaviour — and the safety invariants below — are identical
// from both faces.
//
// Invariants this package upholds:
//   - On the stdio transport stdout is the MCP protocol channel: nothing here
//     ever writes to stdout except through the MCP SDK. Diagnostics go to
//     stderr only.
//   - No tool ever launches a browser or execs open/xdg-open. The `link` tool
//     returns URLs as DATA; it is the print-only sibling of `scrim link`, and
//     this package deliberately does not import internal/openurl.
//   - Canvas URLs, canvas content, and capability tokens are never logged.
package mcpserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/daemon"
	"github.com/jedwards1230/scrim/internal/pushclient"
	"github.com/jedwards1230/scrim/internal/snapshot"
	"github.com/jedwards1230/scrim/internal/state"
)

// server holds the resolved config every tool handler needs to target (or
// self-start) the right daemon and compute on-disk paths, plus the scrim
// version reported in the MCP implementation handshake.
type server struct {
	cfg config.Config
	ver string
}

// resolveDaemon returns an apiclient plus the daemon state for cfg. When
// selfStart is true it self-starts a daemon if none is healthy (daemon.Ensure,
// the same seam the daemon-backed CLI verbs use) and running is always true on
// a nil error; when false it only probes for an already-healthy daemon
// (daemon.TryLoadHealthy) and reports running=false without starting anything.
//
// It is a package-level variable purely so tests can override it with an
// httptest-backed client + synthetic state, exercising the daemon-backed
// handlers (add/list/link/status/rm) without spawning a real detached daemon
// process. This mirrors cli.launchBrowser's test seam idiom.
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

// NewServer builds the MCP server registering all 10 scrim tools against cfg.
// A blank ver is reported as "dev" in the implementation handshake.
func NewServer(cfg config.Config, ver string) *mcp.Server {
	if ver == "" {
		ver = "dev"
	}
	s := &server{cfg: cfg, ver: ver}
	srv := mcp.NewServer(&mcp.Implementation{Name: "scrim", Version: ver}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "add",
		Description: "Register a canvas (self-starts the scrim daemon). Returns its on-disk directory and view URL.",
	}, s.handleAdd)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list",
		Description: "List registered canvases (self-starts the scrim daemon).",
	}, s.handleList)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "link",
		Description: "Return the view URL for a canvas, or the dashboard URL when no id is given (self-starts the scrim daemon). URLs are returned as data — this never launches a browser.",
	}, s.handleLink)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "path",
		Description: "Return the on-disk directory for a canvas. Pure filesystem lookup — does not talk to or start the daemon.",
	}, s.handlePath)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "rm",
		Description: "Remove a canvas (via the daemon when one is running, otherwise directly on disk).",
	}, s.handleRm)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "snap",
		Description: "Snapshot a canvas's current contents. Pure filesystem operation — does not start the daemon.",
	}, s.handleSnap)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "snaps",
		Description: "List a canvas's snapshots, newest first. Pure filesystem read — does not start the daemon.",
	}, s.handleSnaps)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "revert",
		Description: "Restore a canvas from a snapshot (latest by default), taking a safety snapshot of the current contents first. Pure filesystem operation — does not start the daemon.",
	}, s.handleRevert)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "status",
		Description: "Report scrim daemon status. Does not self-start; returns running=false when no daemon is healthy.",
	}, s.handleStatus)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "push",
		Description: "Tar a local canvas and push it to a hub once. Reads the canvas straight off disk; never launches a browser.",
	}, s.handlePush)

	return srv
}

// Serve builds the scrim MCP server and runs it on the stdio transport,
// blocking until ctx is cancelled or stdin closes. stderr is accepted for
// symmetry with ServeHTTP and as the sanctioned diagnostics channel, but the
// stdio path stays deliberately silent: stdout carries the MCP protocol and
// emitting anything unprompted would only add noise for the MCP host.
func Serve(ctx context.Context, cfg config.Config, ver string, stderr io.Writer) error {
	_ = stderr
	srv := NewServer(cfg, ver)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// ── tool: add ───────────────────────────────────────────────────────────────

type addInput struct {
	ID          string `json:"id" jsonschema:"canvas id to create (required)"`
	Title       string `json:"title,omitempty" jsonschema:"optional canvas title"`
	Description string `json:"description,omitempty" jsonschema:"optional canvas description"`
	Icon        string `json:"icon,omitempty" jsonschema:"optional emoji icon; the daemon derives a default from the id when omitted"`
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
	client, _, _, err := resolveDaemon(s.cfg, true)
	if err != nil {
		return errorResult(err.Error()), addOutput{}, nil
	}
	info, err := client.CreateCanvas(ctx, in.ID, in.Title, in.Description, in.Icon)
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
	client, _, _, err := resolveDaemon(s.cfg, true)
	if err != nil {
		return errorResult(err.Error()), listOutput{}, nil
	}
	canvases, err := client.ListCanvases(ctx)
	if err != nil {
		return errorResult(err.Error()), listOutput{}, nil
	}
	out := listOutput{Canvases: make([]canvasSummary, 0, len(canvases))}
	for _, c := range canvases {
		out.Canvases = append(out.Canvases, canvasSummary{
			ID:         c.ID,
			Title:      c.Title,
			URL:        c.URL,
			Dir:        c.Dir,
			Icon:       c.Icon,
			Color:      c.Color,
			ModifiedAt: c.ModifiedAt,
			SSEClients: c.SSEClients,
		})
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
	// Validate before self-starting so a bad id never spins up a daemon.
	if in.ID != "" {
		if err := canvas.ValidateID(in.ID); err != nil {
			return errorResult(err.Error()), linkOutput{}, nil
		}
	}

	client, st, _, err := resolveDaemon(s.cfg, true)
	if err != nil {
		return errorResult(err.Error()), linkOutput{}, nil
	}

	if in.ID == "" {
		url := dashboardURL(st)
		return textResult(url), linkOutput{URLs: []string{url}}, nil
	}

	canvases, err := client.ListCanvases(ctx)
	if err != nil {
		return errorResult(err.Error()), linkOutput{}, nil
	}
	for _, c := range canvases {
		if c.ID == in.ID {
			// c.URL already carries the ?t=<token> query when auth is enabled
			// (the daemon bakes it in server-side).
			return textResult(c.URL), linkOutput{URLs: []string{c.URL}}, nil
		}
	}
	return errorResult(fmt.Sprintf("canvas %q not found", in.ID)), linkOutput{}, nil
}

// dashboardURL builds the token-qualified dashboard URL for the daemon
// described by st: http://host:port/ plus a ?t=<token> query when the daemon
// has auth enabled. It mirrors cli.baseURLFor for the "/" (no-id) case.
func dashboardURL(st *state.State) string {
	url := st.BaseURL() + "/"
	if st.AuthEnabled() {
		url += "?t=" + st.Token
	}
	return url
}

// ── tool: path ───────────────────────────────────────────────────────────--

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
	// rm never self-starts: delete via the daemon when one is already healthy,
	// otherwise straight off disk (mirrors cli.cmdRm).
	client, _, running, err := resolveDaemon(s.cfg, false)
	if err != nil {
		return errorResult(err.Error()), rmOutput{}, nil
	}
	if running {
		if delErr := client.DeleteCanvas(ctx, in.ID); delErr != nil {
			return errorResult(delErr.Error()), rmOutput{}, nil
		}
	} else if delErr := canvas.Delete(s.cfg.CanvasesDir(), s.cfg.MetaDir(), in.ID); delErr != nil {
		return errorResult(delErr.Error()), rmOutput{}, nil
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

func (s *server) handleSnap(_ context.Context, _ *mcp.CallToolRequest, in snapInput) (*mcp.CallToolResult, snapOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), snapOutput{}, nil
	}
	entry, err := snapshot.Create(canvas.Dir(s.cfg.CanvasesDir(), in.ID), s.cfg.VersionsDir(), in.ID, in.Label)
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

func (s *server) handleSnaps(_ context.Context, _ *mcp.CallToolRequest, in snapsInput) (*mcp.CallToolResult, snapsOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), snapsOutput{}, nil
	}
	entries, err := snapshot.List(s.cfg.VersionsDir(), in.ID)
	if err != nil {
		return errorResult(err.Error()), snapsOutput{}, nil
	}
	// snapshot.List returns oldest-first; present newest-first, matching
	// cli.cmdSnaps.
	out := snapsOutput{Snapshots: make([]snapshotSummary, 0, len(entries))}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		out.Snapshots = append(out.Snapshots, snapshotSummary{
			Name:      e.Name,
			Timestamp: e.Timestamp,
			Label:     e.Label,
		})
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

func (s *server) handleRevert(_ context.Context, _ *mcp.CallToolRequest, in revertInput) (*mcp.CallToolResult, revertOutput, error) {
	if err := canvas.ValidateID(in.ID); err != nil {
		return errorResult(err.Error()), revertOutput{}, nil
	}
	canvasDir := canvas.Dir(s.cfg.CanvasesDir(), in.ID)
	versionsDir := s.cfg.VersionsDir()

	// Replicate cli.cmdRevert exactly: resolve the target BEFORE taking the
	// safety snapshot, so a bare revert doesn't restore the canvas to its own
	// current state.
	target := in.Snapshot
	if target == "" {
		latest, ok, err := snapshot.Latest(versionsDir, in.ID)
		if err != nil {
			return errorResult(err.Error()), revertOutput{}, nil
		}
		if !ok {
			return errorResult(fmt.Sprintf("no snapshots for canvas %s", in.ID)), revertOutput{}, nil
		}
		target = latest.Name
	}

	if fi, statErr := os.Stat(canvasDir); statErr == nil && fi.IsDir() {
		if _, err := snapshot.Create(canvasDir, versionsDir, in.ID, "prerevert"); err != nil {
			return errorResult(err.Error()), revertOutput{}, nil
		}
	}

	entry, err := snapshot.Revert(canvasDir, versionsDir, in.ID, target)
	if err != nil {
		return errorResult(err.Error()), revertOutput{}, nil
	}
	return textResult(fmt.Sprintf("reverted %s to snapshot %s (pre-revert state saved as a new snapshot)", in.ID, entry.Name)),
		revertOutput{Reverted: in.ID, Snapshot: entry.Name}, nil
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
	// status is the daemon health-check; it must never self-start one.
	client, _, running, err := resolveDaemon(s.cfg, false)
	if err != nil {
		return errorResult(err.Error()), statusOutput{}, nil
	}
	if !running {
		return textResult("no daemon running"), statusOutput{Running: false}, nil
	}
	resp, err := client.Status(ctx)
	if err != nil {
		return errorResult(err.Error()), statusOutput{}, nil
	}
	out := statusOutput{
		Running:            true,
		PID:                resp.PID,
		Host:               resp.Host,
		Port:               resp.Port,
		Version:            resp.Version,
		UptimeSeconds:      resp.UptimeSeconds,
		CanvasCount:        resp.CanvasCount,
		SSEClients:         resp.SSEClients,
		IdleSeconds:        resp.IdleSeconds,
		IdleTimeoutSeconds: resp.IdleTimeoutSeconds,
	}
	return textResult(fmt.Sprintf("daemon running (pid %d, %d canvas(es))", resp.PID, resp.CanvasCount)), out, nil
}

// ── tool: push ───────────────────────────────────────────────────────────--

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
	if fi, err := os.Stat(canvasDir); err != nil || !fi.IsDir() {
		return errorResult(fmt.Sprintf("canvas %q not found at %s", in.ID, canvasDir)), pushOutput{}, nil
	}
	info, err := canvas.Get(s.cfg.CanvasesDir(), s.cfg.MetaDir(), in.ID)
	if err != nil {
		return errorResult(err.Error()), pushOutput{}, nil
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
