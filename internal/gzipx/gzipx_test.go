package gzipx

import (
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"
)

func TestDeflateInflateRoundTrip(t *testing.T) {
	cases := []string{"", "x", strings.Repeat("hello world ", 5000), "\x00\x01\x02binary\xff"}
	for _, in := range cases {
		compressed := Deflate([]byte(in))
		out, err := Inflate(compressed, len(in)+1)
		if err != nil {
			t.Fatalf("Inflate(%q): %v", in, err)
		}
		if string(out) != in {
			t.Errorf("round trip = %q, want %q", out, in)
		}
	}
}

func TestInflateExactlyAtCap(t *testing.T) {
	raw := bytes.Repeat([]byte("a"), 1000)
	out, err := Inflate(Deflate(raw), 1000)
	if err != nil {
		t.Fatalf("Inflate at exact cap: %v", err)
	}
	if len(out) != 1000 {
		t.Errorf("len = %d, want 1000", len(out))
	}
}

func TestInflateTooLarge(t *testing.T) {
	// A highly compressible payload: small compressed, large inflated -- the
	// gzip-bomb shape the cap defends against.
	raw := bytes.Repeat([]byte("A"), 100_000)
	compressed := Deflate(raw)
	if len(compressed) >= len(raw) {
		t.Fatalf("expected compression, got %d >= %d", len(compressed), len(raw))
	}
	_, err := Inflate(compressed, 1000)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestInflateMalformedIsNotTooLarge(t *testing.T) {
	_, err := Inflate([]byte("not gzip at all"), 1<<20)
	if err == nil {
		t.Fatal("err = nil, want a gzip decode error")
	}
	if errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want a decode error (not ErrTooLarge)", err)
	}
}

// TestInflateInteropWithStdlib proves Deflate output is standard gzip that any
// stdlib reader can consume -- the hub GET path uses gzip.NewWriter directly,
// so both sides must agree on the format.
func TestInflateInteropWithStdlib(t *testing.T) {
	raw := []byte("standard gzip please")
	zr, err := gzip.NewReader(bytes.NewReader(Deflate(raw)))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(zr); err != nil {
		t.Fatalf("read: %v", err)
	}
	if buf.String() != string(raw) {
		t.Errorf("stdlib read = %q, want %q", buf.String(), raw)
	}
}
