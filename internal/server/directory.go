package server

import (
	"sort"

	"github.com/jedwards1230/scrim/internal/principal"
)

// The share-dialog autocomplete (GET /api/principals) reads through the
// principalLister seam (defined in handlers_principals.go). There are three
// feeder slots behind that seam, all display-only and NONE consulted by
// enforcement:
//
//  1. Lazy registry (*principal.Registry, PR #53) -- learns principals from
//     logins, verified CF headers, and grant targets, persisted to
//     principals.json. Always present on a hub; the sole source when Authentik
//     is unconfigured.
//  2. Authentik pull (*authentik.Client, this issue #54) -- OPTIONAL, read-only
//     REST pull of users/groups behind an in-memory TTL cache, NEVER persisted.
//     Composed with the lazy registry via compositeLister when configured.
//  3. SCIM (documented slot, NOT built) -- a future SCIM feeder
//     (elimity-com/scim) could implement the same List() []principal.Principal
//     and drop into the compositeLister exactly like the Authentik driver. No
//     code exists for it yet.
//
// compositeLister fans List() out across its sources and merges the results,
// de-duplicated by email. Sources are consulted in order and the first source's
// data for an email wins on conflict, with later sources only filling a blank
// display name and unioning group memberships -- so the lazy registry's
// observed data is never lost, and Authentik only enriches it and adds
// not-yet-seen principals. The merged result is email-sorted, matching the
// single-source contract handlePrincipals relies on.
type compositeLister struct {
	sources []principalLister
}

// List merges every source's principals, de-duplicated by (non-empty) email and
// sorted by email. It preserves the earlier source's data on conflict.
func (c compositeLister) List() []principal.Principal {
	merged := make(map[string]principal.Principal)
	for _, src := range c.sources {
		if src == nil {
			continue
		}
		for _, p := range src.List() {
			if p.Email == "" {
				continue
			}
			existing, ok := merged[p.Email]
			if !ok {
				merged[p.Email] = p
				continue
			}
			if existing.DisplayName == "" {
				existing.DisplayName = p.DisplayName
			}
			existing.GroupsSeen = unionGroups(existing.GroupsSeen, p.GroupsSeen)
			merged[p.Email] = existing
		}
	}
	out := make([]principal.Principal, 0, len(merged))
	for _, p := range merged {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// unionGroups appends any group names in extra that aren't already in base,
// de-duplicated and order-preserving (base first), returning nil for none.
func unionGroups(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, groups := range [][]string{base, extra} {
		for _, g := range groups {
			if g == "" {
				continue
			}
			if _, dup := seen[g]; dup {
				continue
			}
			seen[g] = struct{}{}
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
