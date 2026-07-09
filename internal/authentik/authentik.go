// Package authentik is the OPTIONAL, read-only Authentik directory feeder for
// the hub's share-dialog autocomplete (GET /api/principals). It pulls users and
// groups from an Authentik instance over its REST API and holds the result in an
// in-memory TTL cache, exposing a List() []principal.Principal that satisfies
// the server's principalLister seam alongside the lazily-populated registry.
//
// Three invariants govern this package. They are load-bearing for the identity
// design -- do NOT weaken them:
//
//  1. READ-ONLY. The API token is a read-only Authentik token and this client
//     only ever issues GETs (core/users, core/groups). It never mutates the
//     directory. (TestClientIssuesOnlyGETs asserts this.)
//  2. NEVER PERSISTED. Pulled directory data lives ONLY in this process's
//     memory (the cache fields on Client) for at most one TTL. It is never
//     written to disk -- not to principals.json, not anywhere. The lazy
//     principal registry's own on-disk persistence is a separate concern and is
//     unaffected. (TestClientNeverPersists asserts this.)
//  3. NEVER ENFORCED. This data feeds ONLY display/autocomplete. Access
//     enforcement (identity.CanView/CanWrite, the hub gate) reads verified
//     claims, never this cache, so sharing stays correct with Authentik
//     unreachable. A fetch failure degrades to the last-known (or empty) view
//     and never fails a request. (TestClientDegradesOnError asserts this.)
//
// A future SCIM feeder (elimity-com/scim) could implement the same
// principal-listing seam; it is intentionally NOT built here.
package authentik

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jedwards1230/scrim/internal/principal"
)

// DefaultTTL is how long pulled directory entries are cached in memory before
// the next List refetches them. ~5 minutes keeps autocomplete fresh without
// hammering Authentik on every keystroke.
const DefaultTTL = 5 * time.Minute

const (
	// requestTimeout bounds a whole directory refresh (all pages of users +
	// groups). Every outbound call runs under a context carrying this deadline
	// so a wedged Authentik can never hang an autocomplete request or the hub.
	requestTimeout = 10 * time.Second
	// pageSize is the Authentik REST page_size requested per page.
	pageSize = 100
	// maxPages is a safety valve against a broken pagination "next" loop -- a
	// well-behaved Authentik terminates far sooner (next == 0).
	maxPages = 1000
	// maxResponseBytes caps a single page body so a hostile or broken endpoint
	// can't exhaust memory; it is generous for a page of pageSize entries.
	maxResponseBytes = 8 << 20 // 8 MiB
)

// errRefreshFailed is the constant, PII-free error surfaced to the logging hook
// when a fetch fails. Network errors from net/http can embed the request URL, so
// they are deliberately NOT wrapped into the logged value -- the base URL is
// operator config (not a secret) but the logging surface promises constant,
// greppable, path-free lines, and a bare token never appears in a URL anyway
// (it travels in the Authorization header). Unexpected-status errors carry only
// the numeric code, which is likewise safe to log.
var errRefreshFailed = errors.New("authentik: directory refresh failed")

// Config configures a Client. BaseURL and Token are required (the hub only
// builds a Client when both are set); the rest are optional, mostly for wiring
// and tests.
type Config struct {
	// BaseURL is the Authentik instance root, e.g. https://auth.example.com.
	// A trailing slash is tolerated. New returns an error if it is empty or
	// unparseable -- a malformed URL is a startup error, like a bad CIDR.
	BaseURL string
	// Token is the read-only Authentik API token, sent as a Bearer credential.
	Token string
	// TTL is the in-memory cache lifetime; <= 0 uses DefaultTTL.
	TTL time.Duration
	// HTTPClient, when non-nil, is used for outbound calls (tests inject an
	// httptest client). Nil uses a default client whose Timeout backstops the
	// per-refresh context deadline.
	HTTPClient *http.Client
	// Now, when non-nil, supplies the clock (tests drive cache expiry through
	// it). Nil uses time.Now.
	Now func() time.Time
	// Log, when non-nil, receives constant, PII-free errors on a failed refresh
	// (the server wires it to internal/logging under CategoryDirectory). Nil is
	// a silent no-op.
	Log func(error)
}

// Client is a read-only Authentik directory feeder with an in-memory TTL cache.
// It is safe for concurrent use. Construct it with New.
type Client struct {
	base           string
	token          string
	httpc          *http.Client
	ttl            time.Duration
	errorTTL       time.Duration
	requestTimeout time.Duration
	now            func() time.Time
	logf           func(error)

	// fetchMu single-flights a refresh: concurrent List calls that miss the
	// cache collapse onto one outbound fetch instead of stampeding Authentik.
	fetchMu sync.Mutex

	// mu guards the cache fields below. It is held only for the fast in-memory
	// read/replace, never across a network call.
	mu           sync.Mutex
	cached       []principal.Principal
	loaded       bool
	refreshAfter time.Time
}

// New validates cfg and returns a ready Client. It errors on an empty or
// unparseable BaseURL, or an empty Token -- caught at hub startup so a
// misconfigured feeder fails loudly rather than silently degrading forever.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("authentik: base URL is required")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("authentik: parsing base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("authentik: base URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("authentik: base URL is missing a host")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("authentik: API token is required")
	}

	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	// errorTTL backs off after a failed refresh so a hard-down Authentik isn't
	// re-hit on every autocomplete keystroke, while still recovering promptly
	// once it returns. Capped at ttl so a sub-minute ttl never lengthens it.
	errorTTL := 30 * time.Second
	if errorTTL > ttl {
		errorTTL = ttl
	}
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: requestTimeout}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(error) {}
	}

	return &Client{
		base:           strings.TrimRight(u.String(), "/"),
		token:          cfg.Token,
		httpc:          httpc,
		ttl:            ttl,
		errorTTL:       errorTTL,
		requestTimeout: requestTimeout,
		now:            now,
		logf:           logf,
	}, nil
}

// List returns the cached directory (users keyed by email, groups keyed by
// name), refreshing from Authentik when the cache is empty or expired. It never
// returns an error: a refresh failure is logged (constant, PII-free) and the
// last-known view -- possibly empty on a cold failure -- is served instead, so
// autocomplete degrades to whatever the lazy registry already has. The returned
// slice is not mutated after being cached, so callers may read it directly.
func (c *Client) List() []principal.Principal {
	c.mu.Lock()
	if c.loaded && c.now().Before(c.refreshAfter) {
		cached := c.cached
		c.mu.Unlock()
		return cached
	}
	c.mu.Unlock()

	// Single-flight the refresh. Whoever loses the race re-checks the cache
	// under fetchMu below and returns the freshly-populated value without a
	// second fetch.
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()

	c.mu.Lock()
	if c.loaded && c.now().Before(c.refreshAfter) {
		cached := c.cached
		c.mu.Unlock()
		return cached
	}
	stale := c.cached
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.requestTimeout)
	defer cancel()

	fresh, err := c.refresh(ctx)
	if err != nil {
		c.logf(errRefreshFailed)
		c.mu.Lock()
		// Serve stale (possibly empty) and back off for errorTTL before the next
		// attempt -- degrade cleanly, never fail the request, never persist.
		c.cached = stale
		c.loaded = true
		c.refreshAfter = c.now().Add(c.errorTTL)
		c.mu.Unlock()
		return stale
	}

	c.mu.Lock()
	c.cached = fresh
	c.loaded = true
	c.refreshAfter = c.now().Add(c.ttl)
	c.mu.Unlock()
	return fresh
}

// refresh pulls all users and groups and returns them as one email/name-sorted
// slice of principals. Any page error aborts the whole refresh (so a partial
// directory never overwrites a good cached one).
func (c *Client) refresh(ctx context.Context) ([]principal.Principal, error) {
	users, err := c.listUsers(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := c.listGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]principal.Principal, 0, len(users)+len(groups))
	out = append(out, users...)
	out = append(out, groups...)
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// pagination is the subset of Authentik's paged-list envelope this client
// needs: next is the next page number, 0 when the current page is the last.
type pagination struct {
	Next int `json:"next"`
}

type usersPage struct {
	Pagination pagination   `json:"pagination"`
	Results    []userResult `json:"results"`
}

type userResult struct {
	Email     string     `json:"email"`
	Name      string     `json:"name"`
	Username  string     `json:"username"`
	IsActive  bool       `json:"is_active"`
	GroupsObj []groupRef `json:"groups_obj"`
}

type groupRef struct {
	Name string `json:"name"`
}

type groupsPage struct {
	Pagination pagination    `json:"pagination"`
	Results    []groupResult `json:"results"`
}

type groupResult struct {
	Name string `json:"name"`
}

// listUsers pages through core/users, mapping each active user with an email to
// a display-only principal (its groups_obj names become GroupsSeen). Users
// without an email are skipped -- they can't be keyed or granted access by
// email, which is the only handle the share dialog has.
func (c *Client) listUsers(ctx context.Context) ([]principal.Principal, error) {
	var out []principal.Principal
	for page := 1; page <= maxPages; page++ {
		var body usersPage
		if err := c.getJSON(ctx, "/api/v3/core/users/", page, &body); err != nil {
			return nil, err
		}
		for _, u := range body.Results {
			if u.Email == "" || !u.IsActive {
				continue
			}
			out = append(out, principal.Principal{
				Email:       u.Email,
				DisplayName: displayName(u),
				GroupsSeen:  groupNames(u.GroupsObj),
				Source:      "authentik",
			})
		}
		if body.Pagination.Next == 0 {
			break
		}
	}
	return out, nil
}

// listGroups pages through core/groups. A group has no email, so its grant
// target (and thus the value the share dialog submits for a group grant) is its
// name: the name is placed in Email as well as DisplayName so both the
// autocomplete option value and prefix matching work with no schema change.
func (c *Client) listGroups(ctx context.Context) ([]principal.Principal, error) {
	var out []principal.Principal
	for page := 1; page <= maxPages; page++ {
		var body groupsPage
		if err := c.getJSON(ctx, "/api/v3/core/groups/", page, &body); err != nil {
			return nil, err
		}
		for _, g := range body.Results {
			if g.Name == "" {
				continue
			}
			out = append(out, principal.Principal{
				Email:       g.Name,
				DisplayName: g.Name,
				GroupsSeen:  []string{g.Name},
				Source:      "authentik-group",
			})
		}
		if body.Pagination.Next == 0 {
			break
		}
	}
	return out, nil
}

// getJSON performs one read-only GET against path with the Bearer token and
// decodes the (length-capped) JSON body into dst. It honors ctx and returns the
// constant errRefreshFailed on any transport/status/decode failure so nothing
// path- or token-bearing reaches the caller (and thus the log).
func (c *Client) getJSON(ctx context.Context, path string, page int, dst any) error {
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(pageSize))
	reqURL := c.base + path + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return errRefreshFailed
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return errRefreshFailed
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The status code is safe to log; keep it separate from the constant
		// surfaced to the hook (see errRefreshFailed) but useful for a caller.
		return fmt.Errorf("authentik: unexpected status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(dst); err != nil {
		return errRefreshFailed
	}
	return nil
}

// displayName prefers the user's full name, falling back to the username so a
// suggestion is never label-less.
func displayName(u userResult) string {
	if strings.TrimSpace(u.Name) != "" {
		return u.Name
	}
	return u.Username
}

// groupNames extracts the non-empty group names from a user's expanded groups,
// returning nil for none (matching principal.Principal's omitempty encoding).
func groupNames(groups []groupRef) []string {
	if len(groups) == 0 {
		return nil
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if g.Name != "" {
			out = append(out, g.Name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
