package server

import (
	"strings"
	"testing"
)

func TestLooksLikeCompleteHTMLDocument(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "full document with doctype",
			content: "<!DOCTYPE html>\n<html><head></head><body>hi</body></html>",
			want:    true,
		},
		{
			name:    "full document with only html tag, no doctype",
			content: "<html><body>hi</body></html>",
			want:    true,
		},
		{
			name:    "lowercase doctype",
			content: "<!doctype html><html><body>hi</body></html>",
			want:    true,
		},
		{
			name:    "mixed case html tag",
			content: "<HTML><body>hi</body></HTML>",
			want:    true,
		},
		{
			name:    "bare fragment with no wrapper tags",
			content: "<h1>Hello</h1>\n<p>a fragment</p>",
			want:    false,
		},
		{
			name:    "fragment mentioning html in prose, not as a tag",
			content: "<p>This document is written in html and looks nice.</p>",
			want:    false,
		},
		{
			name:    "empty content",
			content: "",
			want:    false,
		},
		{
			name:    "fragment containing an unrelated tag only",
			content: "<div>no wrapper here</div>",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := looksLikeCompleteHTMLDocument([]byte(tt.content))
			if got != tt.want {
				t.Errorf("looksLikeCompleteHTMLDocument(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestWrapInSkeleton(t *testing.T) {
	fragment := "<h1>Fragment Content</h1>"
	wrapped := string(wrapInSkeleton([]byte(fragment)))

	if !strings.Contains(wrapped, fragment) {
		t.Errorf("wrapped output missing fragment content: %s", wrapped)
	}
	if !strings.Contains(wrapped, `<meta name="viewport" content="width=device-width, initial-scale=1">`) {
		t.Errorf("wrapped output missing viewport meta tag: %s", wrapped)
	}
	if !strings.Contains(wrapped, "scrim:skeleton") {
		t.Errorf("wrapped output missing distinguishing skeleton marker: %s", wrapped)
	}
	if !strings.Contains(wrapped, "prefers-color-scheme: dark") {
		t.Errorf("wrapped output missing dark-mode theming: %s", wrapped)
	}
	if !strings.Contains(wrapped, "--scrim-bg") || !strings.Contains(wrapped, "--scrim-fg") {
		t.Errorf("wrapped output missing CSS custom property theming: %s", wrapped)
	}
	if !looksLikeCompleteHTMLDocument([]byte(wrapped)) {
		t.Errorf("wrapped output does not look like a complete HTML document: %s", wrapped)
	}
}

func TestRenderMarkdown(t *testing.T) {
	source := "# Title\n\nSome text with a [link](https://example.com).\n\n```go\nfmt.Println(\"hi\")\n```\n"

	rendered, err := renderMarkdown([]byte(source))
	if err != nil {
		t.Fatalf("renderMarkdown() error = %v", err)
	}
	html := string(rendered)

	if !strings.Contains(html, "<h1>Title</h1>") {
		t.Errorf("rendered markdown missing heading: %s", html)
	}
	if !strings.Contains(html, `<a href="https://example.com">link</a>`) {
		t.Errorf("rendered markdown missing link: %s", html)
	}
	if !strings.Contains(html, "<pre><code") {
		t.Errorf("rendered markdown missing code block: %s", html)
	}
	if !strings.Contains(html, "fmt.Println") {
		t.Errorf("rendered markdown missing code block content: %s", html)
	}
}

func TestRenderMarkdownThenWrapInSkeleton(t *testing.T) {
	source := "# Notes\n\nHello from markdown."
	rendered, err := renderMarkdown([]byte(source))
	if err != nil {
		t.Fatalf("renderMarkdown() error = %v", err)
	}
	wrapped := string(wrapInSkeleton(rendered))

	if !strings.Contains(wrapped, "<h1>Notes</h1>") {
		t.Errorf("wrapped rendered markdown missing heading: %s", wrapped)
	}
	if !strings.Contains(wrapped, "Hello from markdown.") {
		t.Errorf("wrapped rendered markdown missing body text: %s", wrapped)
	}
	if !strings.Contains(wrapped, `name="viewport"`) {
		t.Errorf("wrapped rendered markdown missing viewport meta tag: %s", wrapped)
	}
}
