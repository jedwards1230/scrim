//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
)

const (
	// dirPerm is the permission enforced on c.Dir: owner-only
	// read/write/traverse. Nothing under it (the state file, the log file,
	// canvas contents) is reachable by another user on the same host once
	// this holds, regardless of those entries' own individual permissions.
	dirPerm = 0o700
	// filePerm is the permission enforced on the state file and log file:
	// owner-only read/write.
	filePerm = 0o600
)

// hardenDir creates dir if missing and (re)sets its permission to dirPerm
// either way -- MkdirAll alone only applies a permission to a directory it
// actually creates, leaving a preexisting looser-permissioned directory
// untouched.
func hardenDir(dir string) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("tightening permissions on %s: %w", dir, err)
	}
	return nil
}

// hardenFile tightens the permission of an existing file at path to
// filePerm. A missing file is not an error -- there's nothing to tighten
// yet, and whatever creates it is responsible for using filePerm from the
// start.
func hardenFile(path string) error {
	if err := os.Chmod(path, filePerm); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("tightening permissions on %s: %w", path, err)
	}
	return nil
}
