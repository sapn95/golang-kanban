// Package identity answers one question for a request: who is making it.
//
// Three modes, because the same binary runs in three situations. In none the
// board has no users and every request is anonymous. In proxy a reverse proxy
// in front of the app has already authenticated the caller and states the
// result in a header. In access the app is behind Cloudflare Access, which
// sends a signed assertion the app verifies itself.
//
// The distinction between proxy and access is not cosmetic. A header can be
// set by anything that can reach the port, so proxy mode is only sound when
// the app is unreachable except through the proxy. The Access assertion is a
// JWT signed by Cloudflare, so it is safe even if the port is exposed. Prefer
// access wherever it is available.
package identity

import (
	"context"
	"net/http"
	"strings"
)

// Mode selects how a request's user is established.
type Mode string

const (
	// ModeNone treats every request as anonymous.
	ModeNone Mode = "none"
	// ModeProxy trusts a header set by a trusted reverse proxy.
	ModeProxy Mode = "proxy"
	// ModeAccess verifies a Cloudflare Access JWT.
	ModeAccess Mode = "access"
)

// User is who a request is from. The zero User is anonymous.
type User struct {
	Email string
	// Name is a display name when the provider supplies one, else empty.
	Name string
}

// Anonymous reports whether the user is unidentified.
func (u User) Anonymous() bool { return u.Email == "" }

// Display is what to show in the interface: the name when there is one, else
// the part of the address before the @, else "anonymous".
func (u User) Display() string {
	switch {
	case u.Name != "":
		return u.Name
	case u.Email != "":
		if i := strings.IndexByte(u.Email, '@'); i > 0 {
			return u.Email[:i]
		}
		return u.Email
	default:
		return "anonymous"
	}
}

// Initial is the first character of Display, for an avatar bubble. It is a
// rune and not a byte, so a name that starts outside ASCII is not cut in half.
func (u User) Initial() string {
	for _, r := range u.Display() {
		return string(r)
	}
	return ""
}

type ctxKey struct{}

// NewContext returns ctx carrying u.
func NewContext(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// FromContext returns the user carried by ctx. The zero User when there is
// none, so callers that do not care about the difference need no second
// return value.
func FromContext(ctx context.Context) User {
	u, _ := ctx.Value(ctxKey{}).(User)
	return u
}

// Config configures Middleware.
type Config struct {
	Mode Mode
	// Header is the header read in ModeProxy.
	Header string
	// Verifier validates the assertion in ModeAccess.
	Verifier interface {
		Verify(ctx context.Context, token string) (User, error)
	}
	// OnError is called when an assertion is present but does not verify.
	// The request continues as anonymous; it is not rejected, because
	// rejecting here would turn a key rotation into an outage on a board
	// that is already behind Access.
	OnError func(err error)
}

// The header Cloudflare Access sets on every request it forwards.
const AccessAssertionHeader = "Cf-Access-Jwt-Assertion"

// Middleware puts the request's user into the context.
func Middleware(cfg Config) func(http.Handler) http.Handler {
	header := cfg.Header
	if header == "" {
		header = "X-Forwarded-Email"
	}
	onErr := cfg.OnError
	if onErr == nil {
		onErr = func(error) {}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var u User
			switch cfg.Mode {
			case ModeProxy:
				u.Email = strings.TrimSpace(r.Header.Get(header))
			case ModeAccess:
				if tok := r.Header.Get(AccessAssertionHeader); tok != "" && cfg.Verifier != nil {
					v, err := cfg.Verifier.Verify(r.Context(), tok)
					if err != nil {
						onErr(err)
					} else {
						u = v
					}
				}
			}
			if !u.Anonymous() {
				r = r.WithContext(NewContext(r.Context(), u))
			}
			next.ServeHTTP(w, r)
		})
	}
}
