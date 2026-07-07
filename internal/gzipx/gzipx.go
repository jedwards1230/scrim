// Package gzipx is a tiny gzip helper shared by the MCP server and the hub
// machine API for the optional gzip+base64 / Content-Encoding: gzip content
// paths (see issue #42). Its whole reason to exist is Inflate's decompression
// cap: gunzipping attacker-controlled bytes without bounding the output is a
// classic gzip-bomb, and centralizing that guard here means every inflate site
// (the hub PUT handler, the hub-backend GET reader, the MCP write_file decoder)
// enforces the same per-file limit rather than each re-deriving it.
//
// It is a pure leaf package (stdlib compress/gzip only) importable from both
// internal/server and internal/mcpserver, mirroring internal/fileedit.
package gzipx

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
)

// ErrTooLarge reports that inflating would exceed the caller's byte cap -- the
// gzip-bomb guard tripped. Callers map it to a 413 (the decoded payload is over
// the per-file limit), distinct from a malformed-gzip error (a 400).
var ErrTooLarge = errors.New("gzipx: decompressed size exceeds the limit")

// Inflate gunzips src, refusing to produce more than max bytes. A stream that
// would inflate past max stops with ErrTooLarge before the extra bytes are
// buffered -- so a small malicious gzip can't expand into an unbounded
// allocation. A malformed gzip stream returns gzip's own decode error (never
// ErrTooLarge), so callers can tell "not valid gzip" from "too big".
func Inflate(src []byte, max int) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()

	// Read one byte past the cap so an exactly-max result is accepted while
	// anything larger trips ErrTooLarge.
	out, err := io.ReadAll(io.LimitReader(zr, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > max {
		return nil, ErrTooLarge
	}
	return out, nil
}

// Deflate gzips src at the default compression level and returns the
// compressed bytes. Writing to a bytes.Buffer never errors, so this can't
// fail.
func Deflate(src []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(src)
	_ = zw.Close()
	return buf.Bytes()
}
