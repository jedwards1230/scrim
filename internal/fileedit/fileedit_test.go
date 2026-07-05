package fileedit

import (
	"errors"
	"strings"
	"testing"
)

func TestApply(t *testing.T) {
	const maxBytes = 64 // small cap so the over-cap case stays readable

	tests := []struct {
		name       string
		content    string
		oldStr     string
		newStr     string
		replaceAll bool
		want       string
		wantCount  int
		wantErr    error // sentinel matched via errors.Is; nil = success
		wantMulti  int   // >0: expect *MultipleMatchesError with this count
		wantLarge  bool  // expect *TooLargeError
	}{
		{
			name:    "unique hit replaced",
			content: "<h1>hello</h1>", oldStr: "hello", newStr: "goodbye",
			want: "<h1>goodbye</h1>", wantCount: 1,
		},
		{
			name:    "zero hits",
			content: "<h1>hello</h1>", oldStr: "absent", newStr: "x",
			wantErr: ErrNotFound,
		},
		{
			name:    "multiple hits without replace_all",
			content: "a b a b a", oldStr: "a", newStr: "z",
			wantMulti: 3,
		},
		{
			name:    "replace_all replaces every occurrence",
			content: "a b a b a", oldStr: "a", newStr: "z", replaceAll: true,
			want: "z b z b z", wantCount: 3,
		},
		{
			name:    "replace_all with a single occurrence",
			content: "only once", oldStr: "once", newStr: "twice", replaceAll: true,
			want: "only twice", wantCount: 1,
		},
		{
			name:    "old equals new rejected",
			content: "same", oldStr: "same", newStr: "same",
			wantErr: ErrNoChange,
		},
		{
			name:    "empty old rejected",
			content: "anything", oldStr: "", newStr: "x",
			wantErr: ErrOldStringEmpty,
		},
		{
			name:    "result over cap rejected",
			content: "pad", oldStr: "pad", newStr: strings.Repeat("x", maxBytes+1),
			wantLarge: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, n, err := Apply([]byte(tc.content), tc.oldStr, tc.newStr, tc.replaceAll, maxBytes)

			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Apply error = %v, want %v", err, tc.wantErr)
				}
			case tc.wantMulti > 0:
				var multi *MultipleMatchesError
				if !errors.As(err, &multi) {
					t.Fatalf("Apply error = %v, want *MultipleMatchesError", err)
				}
				if multi.Count != tc.wantMulti {
					t.Errorf("MultipleMatchesError.Count = %d, want %d", multi.Count, tc.wantMulti)
				}
			case tc.wantLarge:
				var large *TooLargeError
				if !errors.As(err, &large) {
					t.Fatalf("Apply error = %v, want *TooLargeError", err)
				}
				if large.Max != maxBytes {
					t.Errorf("TooLargeError.Max = %d, want %d", large.Max, maxBytes)
				}
			default:
				if err != nil {
					t.Fatalf("Apply: %v", err)
				}
				if string(got) != tc.want {
					t.Errorf("edited = %q, want %q", got, tc.want)
				}
				if n != tc.wantCount {
					t.Errorf("replacements = %d, want %d", n, tc.wantCount)
				}
			}
		})
	}
}
