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
	"os"
	"path/filepath"
	"time"
)

// ErrNotFound is returned by Load when no state file exists.
var ErrNotFound = errors.New("state: no daemon state file")

// State is the daemon's on-disk record of itself.
type State struct {
	PID       int       `json:"pid"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Token     string    `json:"token"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
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

// NewToken generates a random hex token for the state file's Token field.
// Phase 2 does not verify this token against anything; it exists so Phase 3
// (auth) can start using it without reshaping the state schema.
func NewToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
