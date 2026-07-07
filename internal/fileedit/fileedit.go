// Package fileedit implements the exact-string replacement semantics behind
// the edit_file MCP tool and the hub machine API's PATCH files endpoint:
// occurrence counting, single-vs-replace_all resolution, and the edited-size
// cap live in ONE place, so local mode, hub mode, and the hub handler can
// never drift apart. It is a pure leaf package (stdlib only), deliberately
// importable from both internal/mcpserver and internal/server -- neither can
// import the other (mcpserver's own tests already import internal/server, so
// the reverse import would cycle).
//
// Every error message produced here is deliberately path-free: the hub
// handler serves them verbatim in JSON error bodies and the MCP tool surfaces
// them verbatim to an agent, and this surface never discloses server-side
// paths.
package fileedit

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrOldStringEmpty reports a missing old_string -- an empty needle would
// match everywhere and means the caller wanted write_file, not an edit.
var ErrOldStringEmpty = errors.New("old_string is required")

// ErrNoChange reports old_string == new_string: the edit is a no-op the
// caller almost certainly didn't intend.
var ErrNoChange = errors.New("old_string and new_string are identical (nothing would change)")

// ErrNotFound reports old_string matching nothing in the file.
var ErrNotFound = errors.New("old_string not found in file")

// MultipleMatchesError reports old_string matching more than once without
// replace_all. Count names the occurrence count so the caller can either opt
// into replace_all or pick a more unique string.
type MultipleMatchesError struct{ Count int }

func (e *MultipleMatchesError) Error() string {
	return fmt.Sprintf("old_string occurs %d times; set replace_all or use a more unique string", e.Count)
}

// TooLargeError reports an edited result that would exceed the caller's
// per-file cap. Size is the would-be size in bytes; Max is the cap.
type TooLargeError struct{ Size, Max int }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("edited file would be %d bytes, over the %d-byte per-file limit", e.Size, e.Max)
}

// ErrNoEdits reports an empty edits slice handed to ApplyBatch -- a batch
// with nothing to do is almost certainly a caller mistake, distinct from a
// single edit whose old_string is empty (ErrOldStringEmpty).
var ErrNoEdits = errors.New("edits is empty")

// Edit is one replacement in a batch: the same three fields a single Apply
// call takes.
type Edit struct {
	OldString  string
	NewString  string
	ReplaceAll bool
}

// BatchError reports which edit in an ApplyBatch sequence failed, wrapping the
// underlying cause (ErrNotFound, *MultipleMatchesError, *TooLargeError, etc.).
// Index is the zero-based position of the failing edit in the input slice, so
// a caller can point at exactly which replacement didn't apply. It Unwraps to
// the cause, so errors.Is/As and the HTTP-status mapping see through it.
type BatchError struct {
	Index int
	Err   error
}

func (e *BatchError) Error() string {
	return fmt.Sprintf("edit %d: %s", e.Index, e.Err.Error())
}

func (e *BatchError) Unwrap() error { return e.Err }

// ApplyBatch applies edits sequentially to content -- each against the result
// of the previous one -- and returns the final bytes plus the total number of
// replacements across all edits. It is all-or-nothing and transactional: the
// FIRST edit that fails (not-found, ambiguous without replace_all, or a
// result over maxBytes) aborts the whole batch with a *BatchError naming that
// edit's index, and NO partial result is returned -- the caller writes the
// original file untouched. An empty edits slice is ErrNoEdits.
//
// Each step reuses Apply, so single-edit and batch semantics can never drift:
// a one-element batch behaves exactly like the matching Apply call (only the
// error is wrapped in a *BatchError). The size cap is enforced at every step,
// bounding memory even if an intermediate result would balloon before a later
// edit shrinks it.
func ApplyBatch(content []byte, edits []Edit, maxBytes int) ([]byte, int, error) {
	if len(edits) == 0 {
		return nil, 0, ErrNoEdits
	}
	buf := content
	total := 0
	for i, e := range edits {
		edited, n, err := Apply(buf, e.OldString, e.NewString, e.ReplaceAll, maxBytes)
		if err != nil {
			return nil, 0, &BatchError{Index: i, Err: err}
		}
		buf = edited
		total += n
	}
	return buf, total, nil
}

// Apply replaces oldStr with newStr in content and returns the edited bytes
// plus the number of replacements made. Semantics mirror an exact-string Edit
// tool: oldStr must be non-empty and differ from newStr; without replaceAll,
// oldStr must occur EXACTLY once (zero is ErrNotFound, more is
// *MultipleMatchesError); with replaceAll, every occurrence is replaced. An
// edited result larger than maxBytes is *TooLargeError -- the edit is refused
// before any caller writes a byte.
func Apply(content []byte, oldStr, newStr string, replaceAll bool, maxBytes int) ([]byte, int, error) {
	if oldStr == "" {
		return nil, 0, ErrOldStringEmpty
	}
	if oldStr == newStr {
		return nil, 0, ErrNoChange
	}
	count := bytes.Count(content, []byte(oldStr))
	if count == 0 {
		return nil, 0, ErrNotFound
	}
	if count > 1 && !replaceAll {
		return nil, 0, &MultipleMatchesError{Count: count}
	}
	edited := bytes.ReplaceAll(content, []byte(oldStr), []byte(newStr))
	if len(edited) > maxBytes {
		return nil, 0, &TooLargeError{Size: len(edited), Max: maxBytes}
	}
	return edited, count, nil
}
