package server

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/renderer/html"
)

//go:embed assets/skeleton.html
var skeletonTemplate string

// mdRenderer converts markdown source to HTML. html.WithUnsafe lets raw HTML
// embedded in a canvas's markdown pass through: scrim already serves
// agent-authored HTML documents unsanitized (see serveHTML), so stripping
// raw HTML only inside markdown would be an inconsistent, surprising
// exception rather than a meaningful security boundary.
var mdRenderer = goldmark.New(
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

// renderMarkdown renders markdown source to an HTML fragment.
func renderMarkdown(source []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := mdRenderer.Convert(source, &buf); err != nil {
		return nil, fmt.Errorf("rendering markdown: %w", err)
	}
	return buf.Bytes(), nil
}

// looksLikeCompleteHTMLDocument reports whether content already contains a
// "<!doctype" or "<html" tag (case-insensitive) at a proper tag boundary,
// meaning it's a deliberately-authored complete HTML document. Such
// documents pass through untouched (aside from reload-script injection)
// rather than being wrapped in scrim's default skeleton -- see
// wrapInSkeleton.
func looksLikeCompleteHTMLDocument(content []byte) bool {
	lower := bytes.ToLower(content)
	return hasBoundedTagPrefix(lower, []byte("<!doctype")) || hasBoundedTagPrefix(lower, []byte("<html"))
}

// hasBoundedTagPrefix reports whether lower contains prefix immediately
// followed by a tag boundary -- whitespace, '>', '/', or the end of
// content -- rather than another character. A bare substring match would
// false-positive on a custom element or longer tag/declaration name that
// merely starts with the same characters, e.g. "<html-widget>" or
// "<htmlx>" both contain "<html" but are not an "<html>" tag; the same
// reasoning applies to a hypothetical "<!doctype-ish>" for consistency.
func hasBoundedTagPrefix(lower, prefix []byte) bool {
	search := lower
	offset := 0
	for {
		i := bytes.Index(search, prefix)
		if i == -1 {
			return false
		}
		end := i + len(prefix)
		if end >= len(search) || isTagBoundaryByte(search[end]) {
			return true
		}
		offset += i + 1
		search = lower[offset:]
	}
}

// isTagBoundaryByte reports whether b can legally follow a tag/declaration
// name in HTML: whitespace, the end of an opening tag ('>'), or the start
// of a self-closing slash ('/').
func isTagBoundaryByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f', '\v', '>', '/':
		return true
	default:
		return false
	}
}

// wrapInSkeleton embeds fragment (an HTML fragment, or goldmark-rendered
// markdown) into scrim's default presentation skeleton: a minimal CSS
// reset, prefers-color-scheme light/dark theming, and a responsive viewport
// meta tag. The result is a complete HTML document ready for
// injectReloadScript.
func wrapInSkeleton(fragment []byte) []byte {
	return []byte(strings.Replace(skeletonTemplate, "__SCRIM_CONTENT__", string(fragment), 1))
}
