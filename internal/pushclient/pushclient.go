// Package pushclient implements the client side of `scrim push`: packing a
// local canvas directory into an uncompressed tar archive, POSTing it to a
// hub's /api/push/<id> endpoint (see internal/server's hub mode), and
// (optionally, via Watch) re-pushing on every local change. It is imported
// only by internal/cli's push verb -- the default `scrim add`/`serve` path
// never touches this package.
package pushclient

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pushTimeout bounds a single push request -- these are local-network calls
// to a hub the caller controls, not long-running operations.
const pushTimeout = 30 * time.Second

// Pack tars dir's contents into an uncompressed archive of relative paths.
// Only regular files and directories are included; a symlink, device, or
// fifo encountered while walking dir is silently skipped rather than
// erroring, mirroring the hub's own extraction policy of never trusting
// anything but plain files/directories.
func Pack(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		switch {
		case info.Mode().IsDir():
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return fmt.Errorf("building tar header for %s: %w", rel, err)
			}
			hdr.Name = rel + "/"
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			return packFile(tw, path, rel, info)
		default:
			// Symlinks, devices, fifos: skip rather than error -- a push is
			// best-effort over whatever a canvas directory happens to
			// contain, and the hub would reject these entry types anyway.
			return nil
		}
	})
	if walkErr != nil {
		return nil, fmt.Errorf("packing %s: %w", dir, walkErr)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing tar writer: %w", err)
	}
	return buf.Bytes(), nil
}

func packFile(tw *tar.Writer, path, rel string, info os.FileInfo) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("building tar header for %s: %w", rel, err)
	}
	hdr.Name = rel
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", rel, err)
	}
	f, err := os.Open(path) //nolint:gosec // path comes from filepath.Walk under a caller-provided local canvas directory
	if err != nil {
		return fmt.Errorf("opening %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("copying %s into archive: %w", rel, err)
	}
	return nil
}

// pushResponse mirrors the hub's POST /api/push/<id> JSON body.
type pushResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Push POSTs the tar archive in data to hubURL's push endpoint for id,
// authenticated with token as a bearer credential ("Authorization: Bearer
// <token>"); title/description/icon ride along as URL query parameters (an
// empty one is simply omitted). It returns the canvas's full URL on the
// hub, or an error including the response body for a non-2xx status.
func Push(ctx context.Context, hubURL, id, token, title, description, icon string, data []byte) (string, error) {
	base := strings.TrimRight(hubURL, "/")
	u, err := url.Parse(base + "/api/push/" + url.PathEscape(id))
	if err != nil {
		return "", fmt.Errorf("building push URL: %w", err)
	}
	q := u.Query()
	if title != "" {
		q.Set("title", title)
	}
	if description != "" {
		q.Set("description", description)
	}
	if icon != "" {
		q.Set("icon", icon)
	}
	u.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("building push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("pushing to %s: %w", hubURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading push response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("push to %s failed (%d): %s", hubURL, resp.StatusCode, bytes.TrimSpace(body))
	}

	var parsed pushResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decoding push response: %w", err)
	}
	return base + parsed.URL, nil
}
