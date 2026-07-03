// Package apiclient is a thin HTTP client for scrim's daemon control API
// (/api/*), used by the CLI to talk to a running daemon.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client talks to one daemon's control API.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a Client for the daemon at baseURL (e.g. "http://127.0.0.1:7777").
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{},
	}
}

// StatusResponse mirrors GET /api/status.
type StatusResponse struct {
	PID                int       `json:"pid"`
	Host               string    `json:"host"`
	Port               int       `json:"port"`
	Version            string    `json:"version"`
	StartedAt          time.Time `json:"started_at"`
	UptimeSeconds      float64   `json:"uptime_seconds"`
	CanvasCount        int       `json:"canvas_count"`
	IdleTimeoutSeconds float64   `json:"idle_timeout_seconds"`
	IdleSeconds        float64   `json:"idle_seconds"`
	SSEClients         int       `json:"sse_clients"`
	Active             bool      `json:"active"`
}

// CanvasResponse mirrors one canvas entry from /api/canvases.
type CanvasResponse struct {
	ID         string    `json:"id"`
	Title      string    `json:"title,omitempty"`
	Dir        string    `json:"dir"`
	URL        string    `json:"url"`
	ModifiedAt time.Time `json:"modified_at"`
	SSEClients int       `json:"sse_clients"`
}

// ErrStatus is returned when the daemon responds with a non-2xx status.
type ErrStatus struct {
	Code int
	Body string
}

func (e *ErrStatus) Error() string {
	return fmt.Sprintf("daemon responded %d: %s", e.Code, e.Body)
}

// Status calls GET /api/status.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.do(ctx, http.MethodGet, "/api/status", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateCanvas calls POST /api/canvases.
func (c *Client) CreateCanvas(ctx context.Context, id, title string) (*CanvasResponse, error) {
	body := struct {
		ID    string `json:"id"`
		Title string `json:"title,omitempty"`
	}{ID: id, Title: title}
	var resp CanvasResponse
	if err := c.do(ctx, http.MethodPost, "/api/canvases", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListCanvases calls GET /api/canvases.
func (c *Client) ListCanvases(ctx context.Context) ([]CanvasResponse, error) {
	var resp []CanvasResponse
	if err := c.do(ctx, http.MethodGet, "/api/canvases", nil, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// DeleteCanvas calls DELETE /api/canvases/<id>.
func (c *Client) DeleteCanvas(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/canvases/"+url.PathEscape(id), nil, nil)
}

// Stop calls POST /api/stop.
func (c *Client) Stop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/stop", nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close error is not actionable

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response from %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ErrStatus{Code: resp.StatusCode, Body: string(bytes.TrimSpace(data))}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding response from %s %s: %w", method, path, err)
	}
	return nil
}

// IsNotFound reports whether err is an ErrStatus with a 404 status.
func IsNotFound(err error) bool {
	var se *ErrStatus
	if errors.As(err, &se) {
		return se.Code == http.StatusNotFound
	}
	return false
}
