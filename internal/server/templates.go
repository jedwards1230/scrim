package server

import (
	_ "embed"
	"html/template"
)

//go:embed templates/partials.html.tmpl
var partialsTemplateSrc string

// partialsTemplate holds the action-free page partials (theme CSS/JS, the
// share dialog's CSS/markup/JS) shared by every server-rendered page. It is
// never executed directly -- pages are parsed on top of a clone of it.
var partialsTemplate = template.Must(template.New("partials").Parse(partialsTemplateSrc))

// mustPageTemplate parses a page template on top of a fresh CLONE of the
// shared partials, so the page can invoke {{template "share-js" .}} and
// friends. The clone matters: html/template's contextual escaper mutates a
// template tree in place the first time it's executed, and a partial invoked
// from a <style> on one page and a <script> on another would otherwise fight
// over one escaped tree. A clone per page keeps each page's analysis its own.
//
// It panics on a malformed template, which is what we want at package init:
// these sources are embedded at build time, so a parse failure is a build
// defect, not a runtime condition.
func mustPageTemplate(name, src string) *template.Template {
	return template.Must(template.Must(partialsTemplate.Clone()).New(name).Parse(src))
}
