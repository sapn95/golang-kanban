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
	"io"
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

	mu   sync.Mutex
	keys map[string]*rsa.PublicKey
	// fetchedAt is when keys last came back from the endpoint. It answers two
	// questions: whether the set is still fresh, and whether an unknown key id
	// is worth another fetch. The second is what keeps an invented id from
	// making an outbound request, and it does so for every invented id at once
	// rather than one at a time.
	fetchedAt time.Time
}

// maxKID is the longest key id this will look at. Cloudflare's are a few dozen
// characters; the field is read out of a JWT header before the signature is
// checked, so its size is the caller's choice until something says otherwise.
const maxKID = 128

// missTTL is how long a fetch holds off another one for an unknown key id. A
// real rotation is minutes apart, not milliseconds, so this costs a legitimate
// new key at most this long and it costs a caller inventing ids everything.
const missTTL = 30 * time.Second

// ErrNoKey is returned when the token names a key the endpoint does not serve.
var ErrNoKey = errors.New("identity: signing key not found")

// now is the clock the token's expiry is checked against, injectable so a test
// can age a token without waiting.
func (v *AccessVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// client fetches the signing keys, defaulting to one with a timeout so a slow
// key set cannot hold a request open.
func (v *AccessVerifier) client() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// ttl is how long a fetched key set is kept. Long enough that a busy board is
// not fetching keys, short enough that a rotation is picked up without a
// restart.
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
	// Before anything is looked up or remembered. A key id this long is not one
	// Cloudflare issued, and refusing it here is what keeps the size of what is
	// kept out of the caller's hands.
	if len(kid) > maxKID {
		return nil, fmt.Errorf("%w: key id is %d bytes", ErrNoKey, len(kid))
	}
	v.mu.Lock()
	fresh := v.now().Sub(v.fetchedAt) < v.ttl()
	k, ok := v.keys[kid]
	v.mu.Unlock()
	if ok && fresh {
		return k, nil
	}

	// A set fetched a moment ago does not have this id either, so there is
	// nothing to go and get. One timestamp covers every unknown id at once:
	// the id is read out of the token's header before the signature is
	// checked, so a caller who varies it on every request would otherwise make
	// the process ask the team's endpoint for keys as fast as the requests
	// arrive, with no valid token and before AUTH_REQUIRED refuses anything.
	v.mu.Lock()
	recent := v.now().Sub(v.fetchedAt) < missTTL
	v.mu.Unlock()
	if recent {
		if ok {
			return k, nil
		}
		return nil, fmt.Errorf("%w: kid %q", ErrNoKey, kid)
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

// fetch reads the team's public keys. It caches nothing; key above does that
// with what this returns. Cloudflare publishes two keys at a time so a rotation
// overlaps, which is why the result is a map and not a key.
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
	// A key set is a few kilobytes. Anything answering this URL with more is
	// not one, and should not be read into memory to find that out.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
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
	// CommonName is the service token's client id, and the only thing in the
	// assertion that says which machine is calling. Cloudflare documents the
	// two payloads side by side: a person's carries email, nbf, identity_nonce
	// and a real sub, a token's carries none of those, blanks sub and adds
	// this. Absence of an email is what tells them apart, which is also how
	// Cloudflare's own Pages plugin does it.
	CommonName string `json:"common_name"`
	Iss        string `json:"iss"`
	Exp        int64  `json:"exp"`
	Nbf        int64  `json:"nbf"`
	// aud is a string in some tokens and an array in others.
	Aud audience `json:"aud"`
}

type audience []string

// UnmarshalJSON accepts an audience as either a string or a list of them. The
// JWT spec allows both and Cloudflare sends the list.
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

// has reports whether the audience names this application. Without the check a
// token minted for any other application on the same team would be accepted.
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
	if c.Exp == 0 {
		return User{}, errors.New("identity: token carries no expiry")
	}
	if now.After(time.Unix(c.Exp, 0)) {
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
		// Cloudflare signs the same kind of assertion for a service token as
		// for a person, with the token's name in common_name and no address
		// anywhere in it. Reading that as "no identity" made every call with a
		// service token anonymous, which was invisible until AUTH_REQUIRED
		// started refusing anonymous calls and turned it into a 403.
		if c.CommonName == "" {
			return User{}, errors.New("identity: token carries neither an email nor a common name")
		}
		return User{Service: c.CommonName}, nil
	}
	return User{Email: c.Email, Name: c.Name}, nil
}
