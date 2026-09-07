package identity

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AccessVerifier checks the JWT Cloudflare Access puts on every forwarded
// request. It speaks only what that one token needs: RS256, the claims
// Cloudflare sets, and the JWKS endpoint of one team.
//
// Written against the standard library rather than a JWT package. The surface
// here is narrow and fixed, and a dependency that parses attacker-controlled
// tokens is the kind that has to be watched forever.
type AccessVerifier struct {
	// CertsURL is https://<team>.cloudflareaccess.com/cdn-cgi/access/certs.
	CertsURL string
	// Audience is the application's AUD tag, from the Access application.
	// An assertion for a different application must not open this one.
	Audience string
	// Issuer is https://<team>.cloudflareaccess.com.
	Issuer string

	HTTP *http.Client
	Now  func() time.Time
	// KeyTTL is how long a fetched key set is reused. Cloudflare rotates
	// every six weeks and keeps the old key valid for seven days, so this
	// only has to be short enough to pick up a rotation well inside a week.
	KeyTTL time.Duration

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// ErrNoKey is returned when the token names a key the endpoint does not serve.
var ErrNoKey = errors.New("identity: signing key not found")

func (v *AccessVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *AccessVerifier) client() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (v *AccessVerifier) ttl() time.Duration {
	if v.KeyTTL > 0 {
		return v.KeyTTL
	}
	return time.Hour
}

type jwks struct {
	Keys []struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// key returns the public key for kid, fetching the key set when the cached
// copy is stale or does not contain it. A miss forces one refetch, so a
// rotation is picked up immediately rather than after the TTL.
func (v *AccessVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	fresh := v.now().Sub(v.fetchedAt) < v.ttl()
	k, ok := v.keys[kid]
	v.mu.Unlock()
	if ok && fresh {
		return k, nil
	}

	keys, err := v.fetch(ctx)
	if err != nil {
		// A refetch during an outage should not invalidate a key that
		// verified a moment ago.
		if ok {
			return k, nil
		}
		return nil, err
	}

	v.mu.Lock()
	v.keys, v.fetchedAt = keys, v.now()
	k, ok = keys[kid]
	v.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: kid %q", ErrNoKey, kid)
	}
	return k, nil
}

func (v *AccessVerifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.CertsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity: certs endpoint returned %s", resp.Status)
	}
	var set jwks
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("identity: decoding certs: %w", err)
	}
	out := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		// The exponent is a big-endian byte string of arbitrary length;
		// pad it to four bytes so it can be read as a uint32.
		var buf [4]byte
		copy(buf[4-len(e):], e)
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(binary.BigEndian.Uint32(buf[:]))}
	}
	if len(out) == 0 {
		return nil, errors.New("identity: certs endpoint served no usable RSA keys")
	}
	return out, nil
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type accessClaims struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Iss   string `json:"iss"`
	Exp   int64  `json:"exp"`
	Nbf   int64  `json:"nbf"`
	// aud is a string in some tokens and an array in others.
	Aud audience `json:"aud"`
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a audience) has(want string) bool {
	for _, v := range a {
		if v == want {
			return true
		}
	}
	return false
}

// Verify checks the signature and the claims and returns the user the token
// names. Every failure is an error; there is no partial success.
func (v *AccessVerifier) Verify(ctx context.Context, token string) (User, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return User{}, errors.New("identity: token is not three dot-separated parts")
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return User{}, fmt.Errorf("identity: header: %w", err)
	}
	var h jwtHeader
	if err := json.Unmarshal(headerRaw, &h); err != nil {
		return User{}, fmt.Errorf("identity: header: %w", err)
	}
	// Pinning the algorithm is what stops a token that asks to be verified
	// with "none", or with HMAC keyed on the public key.
	if h.Alg != "RS256" {
		return User{}, fmt.Errorf("identity: unexpected algorithm %q", h.Alg)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return User{}, fmt.Errorf("identity: signature: %w", err)
	}
	key, err := v.key(ctx, h.Kid)
	if err != nil {
		return User{}, err
	}
	signed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, signed[:], sig); err != nil {
		return User{}, fmt.Errorf("identity: signature does not verify: %w", err)
	}

	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return User{}, fmt.Errorf("identity: claims: %w", err)
	}
	var c accessClaims
	if err := json.Unmarshal(claimsRaw, &c); err != nil {
		return User{}, fmt.Errorf("identity: claims: %w", err)
	}

	now := v.now()
	if c.Exp != 0 && now.After(time.Unix(c.Exp, 0)) {
		return User{}, errors.New("identity: token has expired")
	}
	if c.Nbf != 0 && now.Before(time.Unix(c.Nbf, 0).Add(-time.Minute)) {
		return User{}, errors.New("identity: token is not valid yet")
	}
	if v.Audience != "" && !c.Aud.has(v.Audience) {
		// Without this a token minted for any other application on the
		// same team would open this one.
		return User{}, errors.New("identity: token is for a different application")
	}
	if v.Issuer != "" && c.Iss != v.Issuer {
		return User{}, fmt.Errorf("identity: unexpected issuer %q", c.Iss)
	}
	if c.Email == "" {
		return User{}, errors.New("identity: token carries no email claim")
	}
	return User{Email: c.Email, Name: c.Name}, nil
}
