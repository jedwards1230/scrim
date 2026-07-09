package identity

import (
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
)

const (
	linkSecret      = "correct-horse-battery-staple"
	otherLinkSecret = "not-the-secret"
)

func linkGrant(secret string) canvas.Grant {
	return canvas.Grant{Kind: canvas.GrantLink, LinkID: "abc123", LinkSecretHash: canvas.HashLinkSecret(secret)}
}

func TestCanView(t *testing.T) {
	owner := "alice@example.com"
	admin := Claims{Admin: true}
	alice := Claims{Subject: "sub-alice", Email: owner}
	bob := Claims{Subject: "sub-bob", Email: "bob@example.com", Groups: []string{"eng", "ops"}}
	anon := Claims{}

	tests := []struct {
		name         string
		owner        string
		grants       []canvas.Grant
		claims       Claims
		presentedKey string
		want         bool
	}{
		{"admin sees any canvas", owner, nil, admin, "", true},
		{"admin sees an unowned canvas", "", nil, admin, "", true},
		{"owner sees own canvas", owner, nil, alice, "", true},
		{"non-owner without grant is denied", owner, nil, bob, "", false},
		{"anonymous without grant is denied", owner, nil, anon, "", false},

		{"empty owner never matches empty email", "", nil, anon, "", false},
		{"empty owner never matches admin-string email", "", nil, Claims{Email: "admin"}, "", false},

		{"user grant matches the target email", owner, []canvas.Grant{{Kind: canvas.GrantUser, Target: "bob@example.com"}}, bob, "", true},
		{"user grant rejects a different email", owner, []canvas.Grant{{Kind: canvas.GrantUser, Target: "carol@example.com"}}, bob, "", false},
		{"user grant never matches an anonymous empty email", owner, []canvas.Grant{{Kind: canvas.GrantUser, Target: ""}}, anon, "", false},

		{"group grant matches a member", owner, []canvas.Grant{{Kind: canvas.GrantGroup, Target: "eng"}}, bob, "", true},
		{"group grant rejects a non-member", owner, []canvas.Grant{{Kind: canvas.GrantGroup, Target: "finance"}}, bob, "", false},
		{"empty-target group grant never matches", owner, []canvas.Grant{{Kind: canvas.GrantGroup, Target: ""}}, Claims{Email: "x@y", Groups: []string{""}}, "", false},

		{"everyone grant matches any authenticated caller", owner, []canvas.Grant{{Kind: canvas.GrantEveryone}}, bob, "", true},
		{"everyone grant matches a subject-only caller", owner, []canvas.Grant{{Kind: canvas.GrantEveryone}}, Claims{Subject: "s"}, "", true},
		{"everyone grant rejects an anonymous caller", owner, []canvas.Grant{{Kind: canvas.GrantEveryone}}, anon, "", false},
		{"everyone grant is not satisfied by a link secret alone", owner, []canvas.Grant{{Kind: canvas.GrantEveryone}}, anon, linkSecret, false},

		{"link grant matches the correct secret (anonymous)", owner, []canvas.Grant{linkGrant(linkSecret)}, anon, linkSecret, true},
		{"link grant rejects a wrong secret", owner, []canvas.Grant{linkGrant(linkSecret)}, anon, otherLinkSecret, false},
		{"link grant rejects an empty presented secret", owner, []canvas.Grant{linkGrant(linkSecret)}, anon, "", false},
		{"link grant with empty stored hash rejects any secret", owner, []canvas.Grant{{Kind: canvas.GrantLink}}, anon, linkSecret, false},

		{"unknown grant kind is ignored", owner, []canvas.Grant{{Kind: "wat", Target: "bob@example.com"}}, bob, "", false},
		{"first matching grant wins among several", owner, []canvas.Grant{{Kind: canvas.GrantUser, Target: "nobody@x"}, {Kind: canvas.GrantGroup, Target: "eng"}}, bob, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanView(tc.owner, tc.grants, tc.claims, tc.presentedKey); got != tc.want {
				t.Errorf("CanView(owner=%q, grants=%+v, claims=%+v, key=%q) = %v, want %v",
					tc.owner, tc.grants, tc.claims, tc.presentedKey, got, tc.want)
			}
		})
	}
}

func TestCanWrite(t *testing.T) {
	owner := "alice@example.com"
	tests := []struct {
		name   string
		owner  string
		claims Claims
		want   bool
	}{
		{"admin can write any canvas", owner, Claims{Admin: true}, true},
		{"admin can write an unowned canvas", "", Claims{Admin: true}, true},
		{"owner can write own canvas", owner, Claims{Email: owner}, true},
		{"non-owner cannot write", owner, Claims{Email: "bob@example.com"}, false},
		{"anonymous cannot write", owner, Claims{}, false},
		{"a grantee cannot write (grants are view-only)", owner, Claims{Email: "bob@example.com", Groups: []string{"eng"}}, false},
		{"empty owner is not writable by an empty email", "", Claims{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanWrite(tc.owner, tc.claims); got != tc.want {
				t.Errorf("CanWrite(owner=%q, claims=%+v) = %v, want %v", tc.owner, tc.claims, got, tc.want)
			}
		})
	}
}

func TestAuthenticated(t *testing.T) {
	tests := []struct {
		name   string
		claims Claims
		want   bool
	}{
		{"zero claims are anonymous", Claims{}, false},
		{"admin is authenticated", Claims{Admin: true}, true},
		{"email is authenticated", Claims{Email: "x@y"}, true},
		{"subject-only is authenticated", Claims{Subject: "s"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.claims.Authenticated(); got != tc.want {
				t.Errorf("Authenticated(%+v) = %v, want %v", tc.claims, got, tc.want)
			}
		})
	}
}
