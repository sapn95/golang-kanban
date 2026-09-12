package identity_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kanban/internal/identity"
)

const (
	testAud    = "aud-for-this-app"
	testIssuer = "https://team.cloudflareaccess.com"
)

type signer struct {
	key *rsa.PrivateKey
	kid string
}

func newSigner(t *testing.T, kid string) *signer {
	t.Helper()
	// 1024 is fast and this key never leaves the test.
	k, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return &signer{key: k, kid: kid}
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

// token builds a signed JWT. claims is merged over sensible defaults so each
// test states only what it is varying.
func (s *signer) token(t *testing.T, alg string, claims map[string]any) string {
	t.Helper()
	c := map[string]any{
		"email": "someone@example.com",
		"aud":   testAud,
		"iss":   testIssuer,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"nbf":   time.Now().Add(-time.Minute).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	head := b64(map[string]string{"alg": alg, "kid": s.kid})
	body := b64(c)
	sum := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (s *signer) jwk() map[string]string {
	return map[string]string{
		"kid": s.kid, "kty": "RSA", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(s.key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	}
}

// certServer serves a JWKS containing the given signers and counts requests,
// so tests can assert on caching without timing.
func certServer(t *testing.T, hits *atomic.Int64, signers ...*signer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		keys := make([]map[string]string, 0, len(signers))
		for _, s := range signers {
			keys = append(keys, s.jwk())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func verifierFor(srv *httptest.Server) *identity.AccessVerifier {
	return &identity.AccessVerifier{
		CertsURL: srv.URL, Audience: testAud, Issuer: testIssuer,
		HTTP: srv.Client(), KeyTTL: time.Hour,
	}
}

func TestVerifyAcceptsAGoodToken(t *testing.T) {
	s := newSigner(t, "k1")
	v := verifierFor(certServer(t, nil, s))

	u, err := v.Verify(context.Background(), s.token(t, "RS256", map[string]any{"name": "A Person"}))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if u.Email != "someone@example.com" {
		t.Errorf("Email = %q, want someone@example.com", u.Email)
	}
	if u.Name != "A Person" {
		t.Errorf("Name = %q, want A Person", u.Name)
	}
	if u.Anonymous() {
		t.Error("a verified user reports as anonymous")
	}
}

// Each of these is a way in if it is not checked, so they are asserted
// individually rather than as one "rejects bad tokens" case.
func TestVerifyRejects(t *testing.T) {
	s := newSigner(t, "k1")
	other := newSigner(t, "k1") // same kid, different key
	srv := certServer(t, nil, s)
	v := verifierFor(srv)

	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"expired", s.token(t, "RS256", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}), "expired"},
		{"not yet valid", s.token(t, "RS256", map[string]any{"nbf": time.Now().Add(time.Hour).Unix()}), "not valid yet"},
		{"audience of another app", s.token(t, "RS256", map[string]any{"aud": "someone-elses-app"}), "different application"},
		{"wrong issuer", s.token(t, "RS256", map[string]any{"iss": "https://evil.example"}), "unexpected issuer"},
		{"neither an email nor a common name", s.token(t, "RS256", map[string]any{"email": nil}),
			"neither an email nor a common name"},
		{"algorithm none", s.token(t, "none", nil), "unexpected algorithm"},
		{"HMAC substituted", s.token(t, "HS256", nil), "unexpected algorithm"},
		{"signed by another key", other.token(t, "RS256", nil), "does not verify"},
		{"not a jwt", "nonsense", "three dot-separated parts"},
		{"garbage header", "!!!." + strings.Repeat("a", 8) + ".sig", "header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := v.Verify(context.Background(), tt.token)
			if err == nil {
				t.Fatalf("accepted %s, returned %+v", tt.name, u)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
			if !u.Anonymous() {
				t.Errorf("a rejected token still produced a user: %+v", u)
			}
		})
	}
}

func TestKeysAreCachedButAMissRefetches(t *testing.T) {
	var hits atomic.Int64
	s := newSigner(t, "k1")
	v := verifierFor(certServer(t, &hits, s))

	// A clock the test moves, because how long ago the last fetch was is what
	// decides whether an unknown key id is worth another one.
	at := time.Now()
	v.Now = func() time.Time { return at }

	for i := range 3 {
		if _, err := v.Verify(context.Background(), s.token(t, "RS256", nil)); err != nil {
			t.Fatalf("Verify %d: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("fetched the key set %d times for three verifications, want 1", got)
	}

	// Twenty invented key ids, one after another, with no time passing. This is
	// the shape of the attack: the id is read out of the token's header before
	// the signature is checked, so without a hold-off the process asks
	// Cloudflare for keys as fast as the requests arrive, with no valid token
	// and before AUTH_REQUIRED refuses anything.
	for i := range 20 {
		invented := newSigner(t, fmt.Sprintf("invented-%d", i))
		if _, err := v.Verify(context.Background(), invented.token(t, "RS256", nil)); err == nil {
			t.Fatal("accepted a token signed by a key the endpoint does not serve")
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d after twenty invented key ids, want no further fetches", got)
	}

	// A real rotation is minutes apart, not milliseconds. Once the hold-off has
	// passed, an unknown id fetches again, so a new key is picked up within
	// thirty seconds of appearing rather than after the whole key TTL.
	at = at.Add(time.Minute)
	rotated := newSigner(t, "k2")
	if _, err := v.Verify(context.Background(), rotated.token(t, "RS256", nil)); err == nil {
		t.Fatal("accepted a token signed by a key the endpoint does not serve")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("hits = %d a minute later, want the rotation to have been fetched (2)", got)
	}

	// And the key that does work still works, from the set just fetched.
	if _, err := v.Verify(context.Background(), s.token(t, "RS256", nil)); err != nil {
		t.Errorf("the working key stopped working: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("hits = %d; a known key fetched again", got)
	}
}

// A key id longer than any Cloudflare issues is refused before anything looks
// it up, because the field is the caller's to size until something says no.
func TestAnOversizedKeyIDIsRefusedOutright(t *testing.T) {
	var hits atomic.Int64
	s := newSigner(t, "k1")
	v := verifierFor(certServer(t, &hits, s))

	huge := newSigner(t, strings.Repeat("x", 2000))
	if _, err := v.Verify(context.Background(), huge.token(t, "RS256", nil)); err == nil {
		t.Fatal("a 2000-byte key id was looked up")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("hits = %d; an oversized key id reached the endpoint", got)
	}
}

func TestVerifySurvivesTheCertsEndpointGoingDown(t *testing.T) {
	var hits atomic.Int64
	s := newSigner(t, "k1")
	srv := certServer(t, &hits, s)
	v := verifierFor(srv)
	v.KeyTTL = time.Nanosecond // force a refetch on every call

	if _, err := v.Verify(context.Background(), s.token(t, "RS256", nil)); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	srv.Close()

	// The key that worked a moment ago keeps working; an outage at
	// Cloudflare should not log everyone out of a board behind it.
	if _, err := v.Verify(context.Background(), s.token(t, "RS256", nil)); err != nil {
		t.Errorf("Verify after the endpoint went away: %v", err)
	}
}

func TestMiddleware(t *testing.T) {
	s := newSigner(t, "k1")
	v := verifierFor(certServer(t, nil, s))
	good := s.token(t, "RS256", nil)

	tests := []struct {
		name    string
		cfg     identity.Config
		headers map[string]string
		want    string
	}{
		{"none ignores a header", identity.Config{Mode: identity.ModeNone},
			map[string]string{"X-Forwarded-Email": "spoofed@example.com"}, ""},
		{"none ignores an assertion", identity.Config{Mode: identity.ModeNone, Verifier: v},
			map[string]string{identity.AccessAssertionHeader: good}, ""},
		{"proxy reads the default header", identity.Config{Mode: identity.ModeProxy},
			map[string]string{"X-Forwarded-Email": "someone@example.com"}, "someone@example.com"},
		{"proxy reads a configured header", identity.Config{Mode: identity.ModeProxy, Header: "X-User"},
			map[string]string{"X-User": "someone@example.com"}, "someone@example.com"},
		{"proxy ignores other headers", identity.Config{Mode: identity.ModeProxy, Header: "X-User"},
			map[string]string{"X-Forwarded-Email": "spoofed@example.com"}, ""},
		{"access verifies", identity.Config{Mode: identity.ModeAccess, Verifier: v},
			map[string]string{identity.AccessAssertionHeader: good}, "someone@example.com"},
		{"access ignores a plain header", identity.Config{Mode: identity.ModeAccess, Verifier: v},
			map[string]string{"X-Forwarded-Email": "spoofed@example.com"}, ""},
		{"access rejects a bad assertion", identity.Config{Mode: identity.ModeAccess, Verifier: v},
			map[string]string{identity.AccessAssertionHeader: "nonsense"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			h := identity.Middleware(tt.cfg)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = identity.FromContext(r.Context()).Email
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, val := range tt.headers {
				req.Header.Set(k, val)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != tt.want {
				t.Errorf("email = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMiddlewareReportsVerificationFailures(t *testing.T) {
	s := newSigner(t, "k1")
	v := verifierFor(certServer(t, nil, s))
	var seen error
	h := identity.Middleware(identity.Config{
		Mode: identity.ModeAccess, Verifier: v,
		OnError: func(err error) { seen = err },
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(identity.AccessAssertionHeader, "nonsense")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen == nil {
		t.Error("a failed verification was not reported to OnError")
	}
	// Anonymous, but still served: a key rotation should not be an outage.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the request to continue as anonymous", rec.Code)
	}
}

func TestDisplay(t *testing.T) {
	tests := []struct {
		user identity.User
		want string
	}{
		{identity.User{}, "anonymous"},
		{identity.User{Email: "a.b@example.com", Name: "A Person"}, "A Person"},

		// The point of the exercise: an address in the usual corporate shape
		// reads as the person's name.
		{identity.User{Email: "nicolas.haas@example.com"}, "Nicolas Haas"},
		{identity.User{Email: "sebastian.winterberger2@example.com"}, "Sebastian Winterberger"},
		{identity.User{Email: "ada_lovelace@example.com"}, "Ada Lovelace"},
		{identity.User{Email: "jean.claude.dupont@example.com"}, "Jean Claude Dupont"},
		{identity.User{Email: "anne-marie.dupont@example.com"}, "Anne-Marie Dupont"},
		{identity.User{Email: "fiona.McLeod@example.com"}, "Fiona McLeod"},
		{identity.User{Email: "nicolas.haas+board@example.com"}, "Nicolas Haas"},
		{identity.User{Email: "émile.zola@example.com"}, "Émile Zola"},

		// And where it must not guess. A local part that is not several
		// alphabetic words is handed back exactly as it arrived.
		{identity.User{Email: "someone@example.com"}, "someone"},
		{identity.User{Email: "u236858@example.com"}, "u236858"},
		{identity.User{Email: "team.42@example.com"}, "team.42"},
		{identity.User{Email: "a.winterberger@example.com"}, "a.winterberger"},
		{identity.User{Email: "no-at-sign"}, "no-at-sign"},
		{identity.User{Email: "noreply@example.com"}, "noreply"},
		// A +tag is only routing inside an address. An identity that is not an
		// address keeps its plus sign, or "build+bot" would read as "build".
		{identity.User{Email: "build+bot"}, "build+bot"},
	}
	for _, tt := range tests {
		if got := tt.user.Display(); got != tt.want {
			t.Errorf("User%+v.Display() = %q, want %q", tt.user, got, tt.want)
		}
	}
}

func TestInitials(t *testing.T) {
	tests := []struct {
		user identity.User
		want string
	}{
		{identity.User{}, "A"},
		{identity.User{Email: "someone@example.com"}, "S"},
		// Two words in, two letters out — the point of an avatar bubble.
		{identity.User{Email: "sebastian.winterberger2@example.com"}, "SW"},
		{identity.User{Name: "Jean Claude Van Damme"}, "JD"}, // first and last, not the middle
		{identity.User{Name: "Über Mensch"}, "ÜM"},           // two bytes: a byte slice would cut it in half
		{identity.User{Name: "水曜日"}, "水"},
		{identity.User{Name: "   "}, ""},
		// Letters only. A bubble reading "1A" says less than one reading "A".
		{identity.User{Name: "123 Alice"}, "A"},
		{identity.User{Name: "42"}, ""},
	}
	for _, tt := range tests {
		if got := tt.user.Initials(); got != tt.want {
			t.Errorf("User%+v.Initials() = %q, want %q", tt.user, got, tt.want)
		}
	}
}

func TestAnInventedKidDoesNotMakeUsFetchRepeatedly(t *testing.T) {
	var hits atomic.Int64
	s := newSigner(t, "k1")
	v := verifierFor(certServer(t, &hits, s))

	// One invented kid, over and over. The varying-kid case is in
	// TestKeysAreCachedButAMissRefetches, and it is the one that matters: this
	// was closed for a repeated id long before it was closed for a caller who
	// changes it every time.
	bogus := newSigner(t, "invented")
	for i := 0; i < 10; i++ {
		if _, err := v.Verify(context.Background(), bogus.token(t, "RS256", nil)); err == nil {
			t.Fatal("a token signed by an unknown key was accepted")
		}
	}
	if got := hits.Load(); got > 2 {
		t.Errorf("fetched the key set %d times for ten invented kids, want at most 2", got)
	}

	// A real key still verifies while the miss is remembered.
	if _, err := v.Verify(context.Background(), s.token(t, "RS256", nil)); err != nil {
		t.Errorf("a valid token was refused while a miss was cached: %v", err)
	}
}

func TestATokenWithoutAnExpiryIsRefused(t *testing.T) {
	s := newSigner(t, "k1")
	v := verifierFor(certServer(t, nil, s))

	// Checking exp only when present means a token minted without one never
	// expires.
	if _, err := v.Verify(context.Background(), s.token(t, "RS256", map[string]any{"exp": nil})); err == nil {
		t.Fatal("a token with no exp claim was accepted")
	} else if !strings.Contains(err.Error(), "expiry") {
		t.Errorf("error = %v, want it to mention the missing expiry", err)
	}
}

// A mode identifies a caller. Required is what refuses one who is not
// identified at all, which is the request that reaches a LAN port beside the
// tunnel rather than through it.
func TestMiddlewareRefusesAnonymousWhenRequired(t *testing.T) {
	s := newSigner(t, "k1")
	v := verifierFor(certServer(t, nil, s))
	good := s.token(t, "RS256", nil)

	cfg := identity.Config{Mode: identity.ModeAccess, Verifier: v, Required: true}
	served := false
	h := identity.Middleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))

	call := func(t *testing.T, path, assertion string) *httptest.ResponseRecorder {
		t.Helper()
		served = false
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if assertion != "" {
			req.Header.Set(identity.AccessAssertionHeader, assertion)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("no assertion at all is refused", func(t *testing.T) {
		rec := call(t, "/b/demo", "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
		if served {
			t.Error("the handler ran for a request with no identity")
		}
	})

	t.Run("an assertion that does not verify is refused too", func(t *testing.T) {
		if rec := call(t, "/b/demo", "nonsense"); rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("a signed request is served", func(t *testing.T) {
		if rec := call(t, "/b/demo", good); rec.Code != http.StatusOK || !served {
			t.Errorf("status = %d, served = %v, want 200 and the handler to run", rec.Code, served)
		}
	})

	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run("the probe "+path+" stays open", func(t *testing.T) {
			if !identity.ProbePath(path) {
				t.Fatalf("ProbePath(%q) = false", path)
			}
			if rec := call(t, path, ""); rec.Code != http.StatusOK || !served {
				t.Errorf("status = %d, served = %v, want the probe answered", rec.Code, served)
			}
		})
	}

	t.Run("nothing else is a probe path", func(t *testing.T) {
		for _, path := range []string{"/", "/version", "/b/demo", "/healthz/", "/api/v1/boards"} {
			if identity.ProbePath(path) {
				t.Errorf("ProbePath(%q) = true; only the two probes are open", path)
			}
		}
	})

	t.Run("without Required an anonymous request is still served", func(t *testing.T) {
		open := identity.Middleware(identity.Config{Mode: identity.ModeAccess, Verifier: v})(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		rec := httptest.NewRecorder()
		open.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/b/demo", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want the default to serve anonymously", rec.Code)
		}
	})
}

// A Cloudflare service token is signed the same way a person's assertion is,
// and carries the token's name in common_name with no address anywhere. Reading
// that as no identity made every call with one anonymous, which was invisible
// until Required started refusing anonymous calls.
func TestAServiceTokenIsACallerWithNoAddress(t *testing.T) {
	s := newSigner(t, "k1")
	v := verifierFor(certServer(t, nil, s))
	// The payload Cloudflare documents for a service token: no email claim at
	// all rather than an empty one, no nbf and no identity_nonce, sub blank,
	// and the token's client id in common_name.
	machine := s.token(t, "RS256", map[string]any{
		"email": nil, "nbf": nil,
		"common_name": "88fe4a3c.access", "sub": "", "type": "app",
		"iat": time.Now().Add(-time.Minute).Unix(),
	})

	u, err := v.Verify(context.Background(), machine)
	if err != nil {
		t.Fatalf("Verify on a service token: %v", err)
	}
	if u.Service != "88fe4a3c.access" {
		t.Errorf("Service = %q, want the token's common name", u.Service)
	}
	if u.Email != "" {
		t.Errorf("Email = %q, want none: a machine has no address", u.Email)
	}
	if u.Anonymous() {
		t.Error("a service token reads as anonymous, so a board that refuses anonymous refuses it")
	}
	if u.Person() {
		t.Error("a service token reads as a person, so @me would write an empty address onto a card")
	}
	if got := u.Display(); got != "88fe4a3c.access" {
		t.Errorf("Display() = %q, want the token's name", got)
	}

	// And it gets through a board that requires an identity.
	served := false
	h := identity.Middleware(identity.Config{Mode: identity.ModeAccess, Verifier: v, Required: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served = true
			if got := identity.FromContext(r.Context()).Service; got != "88fe4a3c.access" {
				t.Errorf("the handler sees Service = %q", got)
			}
			w.WriteHeader(http.StatusOK)
		}))
	req := httptest.NewRequest(http.MethodGet, "/b/demo", nil)
	req.Header.Set(identity.AccessAssertionHeader, machine)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !served {
		t.Errorf("status = %d, served = %v, want the service token through", rec.Code, served)
	}
}
