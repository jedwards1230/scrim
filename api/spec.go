// Package api embeds the hand-authored OpenAPI 3.1 specification for the scrim
// hub machine API. It lives at the repo root (not under internal/) so the
// embed directive can reach openapi.yaml in this same directory -- a go:embed
// pattern cannot escape its package directory with "..", so the server package
// under internal/ imports these bytes rather than embedding the file itself.
// The spec is the canonical machine-API reference and is served by a hub at
// GET /api/openapi.yaml (see internal/server).
package api

import _ "embed"

// OpenAPISpecYAML is the raw bytes of openapi.yaml, embedded at build time.
//
//go:embed openapi.yaml
var OpenAPISpecYAML []byte
