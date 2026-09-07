package identity_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
		{"no email claim", s.token(t, "RS256", map[string]any{"email": nil}), "no email"},
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

	for i := 0; i < 3; i++ {
		if _, err := v.Verify(context.Background(), s.token(t, "RS256", nil)); err != nil {
			t.Fatalf("Verify %d: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("fetched the key set %d times for three verifications, want 1", got)
	}

	// An unknown kid must not be answered from cache: that is what a
	// rotation looks like, and waiting out the TTL would be an outage.
	unknown := newSigner(t, "k2")
	if _, err := v.Verify(context.Background(), unknown.token(t, "RS256", nil)); err == nil {
		t.Fatal("accepted a token signed by a key the endpoint does not serve")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("hits = %d after an unknown kid, want a refetch (2)", got)
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
		{identity.User{Email: "someone@example.com"}, "someone"},
		{identity.User{Email: "someone@example.com", Name: "A Person"}, "A Person"},
		{identity.User{Email: "no-at-sign"}, "no-at-sign"},
	}
	for _, tt := range tests {
		if got := tt.user.Display(); got != tt.want {
			t.Errorf("User%+v.Display() = %q, want %q", tt.user, got, tt.want)
		}
	}
}

func TestInitial(t *testing.T) {
	tests := []struct {
		user identity.User
		want string
	}{
		{identity.User{}, "a"},
		{identity.User{Email: "someone@example.com"}, "s"},
		{identity.User{Name: "Über Mensch"}, "Ü"}, // two bytes: a byte slice would cut it in half
		{identity.User{Name: "水曜日"}, "水"},
	}
	for _, tt := range tests {
		if got := tt.user.Initial(); got != tt.want {
			t.Errorf("User%+v.Initial() = %q, want %q", tt.user, got, tt.want)
		}
	}
}
