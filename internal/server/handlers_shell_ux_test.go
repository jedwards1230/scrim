package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// TestShellVersionEntriesAreOrdinalledAndTimestamped pins the version-history
// rendering the shell ships to the browser. The list itself is fetched from
// GET /api/canvases/{id}/snapshots and built client-side, so what a Go test can
// hold still is the shipped script: the ordinal ("Version 3", newest highest,
// since the API returns newest-first), the absolute timestamp on each entry's
// title, and the fact that the raw on-disk snapshot directory name is never
// rendered to a human.
func TestShellVersionEntriesAreOrdinalledAndTimestamped(t *testing.T) {
	s, _ := newTestServer(t)
	writeCanvas(t, s, "versions", "<html><body>x</body></html>")

	body, code := shellGET(t, s, "versions", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /c/versions/ = %d, want 200", code)
	}

	for _, want := range []string{
		`var ordinal = "Version " + (entries.length - i);`, // newest-first -> highest ordinal
		`if (exact) li.title = exact;`,                     // absolute timestamp on hover
		`li.setAttribute("aria-label"`,                     // announced as one string, not an empty <li>
		`then.toLocaleString()`,                            // in the viewer's own timezone
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell version-history script missing %q", want)
		}
	}
	// The on-disk directory name (e.g. 20260907-191407.867492521-after-review)
	// is an internal identifier; a version entry must never fall back to it.
	if strings.Contains(body, "e.name") {
		t.Error("version entries must not render the raw snapshot directory name")
	}
	// ...and they must not pretend to be controls: no pointer cursor, no
	// button/link markup, since nothing serves a snapshot's content yet.
	if strings.Contains(body, ".versions li:hover") {
		t.Error("version entries must carry no hover affordance")
	}
	if !strings.Contains(body, "cursor: default;") {
		t.Error("version entries should keep the default cursor (they are text, not controls)")
	}
}

// TestShellDeleteItemFollowsWriteAccess proves Delete is offered on exactly the
// same permission as Duplicate -- write access, not merely "OIDC is on" -- and
// that it is marked destructive and confirms before firing.
func TestShellDeleteItemFollowsWriteAccess(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	if err := canvas.AddGrant(s.metaDir, "alices", canvas.Grant{Kind: canvas.GrantUser, Target: "bob@example.com"}); err != nil {
		t.Fatal(err)
	}

	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	ownerBody, code := shellGET(t, s, "alices", alice)
	if code != http.StatusOK {
		t.Fatalf("GET /c/alices/ (owner) = %d, want 200", code)
	}
	for _, want := range []string{
		`id="menu-delete"`,
		`data-delete="alices"`,
		`menu-item-danger`, // visually marked destructive
		`window.confirm("Delete `,
	} {
		if !strings.Contains(ownerBody, want) {
			t.Errorf("owner's shell missing %q", want)
		}
	}

	// A view-only grantee: the endpoint would refuse them (see
	// TestSessionOwnerMayDeleteCanvas), so the control must not be there.
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)
	bobBody, bobCode := shellGET(t, s, "alices", bob)
	if bobCode != http.StatusOK {
		t.Fatalf("GET /c/alices/ (view-only) = %d, want 200", bobCode)
	}
	if strings.Contains(bobBody, `data-delete="alices"`) {
		t.Error("a view-only viewer must not be offered Delete")
	}
}

// TestShellDuplicateReportsTheNewCanvas pins item 5: a successful Duplicate
// names the copy and links to it rather than silently redirecting -- the viewer
// asked to copy this canvas, not to leave it.
func TestShellDuplicateReportsTheNewCanvas(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	body, code := shellGET(t, s, "alices", alice)
	if code != http.StatusOK {
		t.Fatalf("GET /c/alices/ = %d, want 200", code)
	}
	for _, want := range []string{
		`function showCopyMade(target)`,
		`link.textContent = target;`, // textContent, never innerHTML
		`open.textContent = "Open it";`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell duplicate-feedback script missing %q", want)
		}
	}
	// The old behavior -- navigate straight to the copy -- must be gone.
	if strings.Contains(body, `window.location.assign("/c/" + encodeURIComponent(target) + "/")`) {
		t.Error("Duplicate must not silently navigate to the copy")
	}
}
