package mcpserver

import (
	"context"
	"testing"

	"github.com/jedwards1230/scrim/internal/config"
)

// TestToolAnnotationsMatchScopes is the drift guard the annotations helper
// exists to make possible: every tool registered by newServer must (a) have a
// name present in toolScopes (oauth.go's read/write source of truth) and (b)
// advertise a ReadOnlyHint that agrees with that classification
// (scopeRead => true, scopeWrite => false). It runs against BOTH local and
// hub mode so the local-only `path` tool is covered too.
func TestToolAnnotationsMatchScopes(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	cases := []struct {
		name string
		hub  *HubTarget
	}{
		{"local", nil},
		{"hub", &HubTarget{BaseURL: "http://127.0.0.1:7788", Token: "tok"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(cfg, "test", tc.hub)
			session := connectInMemory(t, srv)

			seen := map[string]bool{}
			for tool, err := range session.Tools(context.Background(), nil) {
				if err != nil {
					t.Fatalf("Tools iteration: %v", err)
				}
				seen[tool.Name] = true

				scope, ok := toolScopes[tool.Name]
				if !ok {
					t.Errorf("tool %q is registered but absent from toolScopes", tool.Name)
					continue
				}
				if tool.Annotations == nil {
					t.Errorf("tool %q has no Annotations", tool.Name)
					continue
				}
				wantReadOnly := scope == scopeRead
				if tool.Annotations.ReadOnlyHint != wantReadOnly {
					t.Errorf("tool %q: ReadOnlyHint = %v, want %v (toolScopes[%q] = %q)",
						tool.Name, tool.Annotations.ReadOnlyHint, wantReadOnly, tool.Name, scope)
				}
				if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
					t.Errorf("tool %q: OpenWorldHint = %v, want explicit false", tool.Name, tool.Annotations.OpenWorldHint)
				}
			}
			if len(seen) == 0 {
				t.Fatal("no tools registered")
			}
		})
	}
}
