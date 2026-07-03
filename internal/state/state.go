// Package state manages scrim's daemon state file (~/.scrim/daemon.json by
// default), the record a running daemon leaves behind so CLI invocations can
// find and talk to it.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ErrNotFound is returned by Load when no state file exists.
var ErrNotFound = errors.New("state: no daemon state file")

// State is the daemon's on-disk record of itself.
type State struct {
	PID   int    `json:"pid"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Token string `json:"token"`
	// NoAuth records whether this daemon was started with auth disabled
	// (--no-auth). Readers must check this field rather than inferring
	// "auth disabled" from Token being empty -- an empty Token is only
	// meaningful once NoAuth is known, so the two fields are always set
	// together (see NewToken/Run).
	NoAuth    bool      `json:"no_auth"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// AuthEnabled reports whether requests to this daemon must present a valid
// capability token (query param or cookie). It is the single source of
// truth other packages should use instead of re-deriving the answer from
// Token/NoAuth themselves.
func (s *State) AuthEnabled() bool { return !s.NoAuth }

// BaseURL is the HTTP base URL of the daemon that wrote this state, i.e.
// where it is actually bound and listening (Host/Port), as opposed to a
// caller-supplied config that may target a different address. Callers that
// have already loaded a State (e.g. via a health check) should build their
// API client from this rather than from a Config's BaseURL, since the two
// can legitimately differ (the daemon was started with different flags/env
// than the current invocation, or an auto-assigned port).
func (s *State) BaseURL() string {
	return "http://" + net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// Load reads and parses the state file at path. A missing file returns
// ErrNotFound. A present-but-corrupt file is treated the same as "no daemon
// running" and also returns ErrNotFound, wrapped with the parse error for
// diagnostics.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from --dir/SCRIM_DIR, a trusted local config value
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reading state file %s: %w", path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%w: corrupt state file %s: %v", ErrNotFound, path, err)
	}
	if st.PID <= 0 || st.Port <= 0 {
		return nil, fmt.Errorf("%w: invalid state file %s", ErrNotFound, path)
	}
	return &st, nil
}

// Save writes the state atomically: it writes to a temp file in the same
// directory and renames it into place, so readers never observe a partially
// written file.
func Save(path string, st *State) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // state dir is a user-owned config directory, not sensitive
		return fmt.Errorf("creating state dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".daemon-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once renamed; cleans up on any early return

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // best-effort close on error path
		return fmt.Errorf("writing temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming state file into place: %w", err)
	}
	return nil
}

// Remove deletes the state file. A missing file is not an error.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing state file %s: %w", path, err)
	}
	return nil
}

// NewToken generates a random hex-encoded capability token for the state
// file's Token field: 32 bytes (256 bits) of crypto/rand, hex-encoded. The
// daemon mints a fresh one per lifetime and the HTTP server's auth
// middleware compares presented tokens against it in constant time.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
