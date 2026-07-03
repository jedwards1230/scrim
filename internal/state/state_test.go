package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, path string)
		wantErr error
		wantNil bool
	}{
		{
			name:    "missing file",
			setup:   func(t *testing.T, path string) {},
			wantErr: ErrNotFound,
			wantNil: true,
		},
		{
			name: "corrupt json",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrNotFound,
			wantNil: true,
		},
		{
			name: "invalid pid/port",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"pid":0,"port":0}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrNotFound,
			wantNil: true,
		},
		{
			name: "valid state",
			setup: func(t *testing.T, path string) {
				st := &State{PID: 123, Host: "127.0.0.1", Port: 7777, Version: "dev", StartedAt: time.Now()}
				if err := Save(path, st); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: nil,
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.json")
			tt.setup(t, path)

			got, err := Load(path)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Load() err = %v, want wrapping %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Load() unexpected err = %v", err)
			}
			if tt.wantNil && got != nil {
				t.Fatalf("Load() = %+v, want nil", got)
			}
			if !tt.wantNil && got == nil {
				t.Fatal("Load() = nil, want non-nil")
			}
		})
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.json")

	st := &State{PID: 42, Host: "127.0.0.1", Port: 7777, Version: "dev", StartedAt: time.Now()}
	if err := Save(path, st); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "daemon.json" {
			t.Errorf("leftover file after Save(): %s", e.Name())
		}
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.PID != st.PID || got.Port != st.Port {
		t.Errorf("Load() = %+v, want %+v", got, st)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.json")

	// Removing a missing file is not an error.
	if err := Remove(path); err != nil {
		t.Fatalf("Remove() on missing file error = %v", err)
	}

	if err := Save(path, &State{PID: 1, Port: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file still exists after Remove()")
	}
}

func TestNewToken(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken() error = %v", err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken() error = %v", err)
	}
	if a == "" || b == "" {
		t.Fatal("NewToken() returned empty string")
	}
	if a == b {
		t.Fatal("NewToken() returned identical tokens on successive calls")
	}
}
