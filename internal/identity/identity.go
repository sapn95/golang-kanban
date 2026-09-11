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
// JWT signed by Cloudflare, so a caller cannot forge one even on an exposed
// port. Prefer access wherever it is available.
//
// Neither mode refuses a caller on its own. Both establish who somebody is when
// they say, and a request arriving with no header and no assertion is served
// anonymously with every write the board has. Config.Required is what closes
// that, and a port reachable any other way needs it.
package identity

import (
	"context"
	"net/http"
	"strings"
	"unicode"
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
	// Service names a machine credential when the caller is one rather than
	// somebody. A Cloudflare Access service token proves possession of a
	// secret and establishes no person: the assertion arrives with the token's
	// name in it and no address anywhere.
	Service string
}

// Anonymous reports whether the request carries no identity at all.
//
// A service token is not anonymous. Something proved which caller it was; it
// simply was not a person, which is a different question and has its own
// answer below.
func (u User) Anonymous() bool { return u.Email == "" && u.Service == "" }

// Person reports whether there is somebody behind the request.
//
// Anything that writes a name onto a card asks this rather than Anonymous: a
// machine has no address to put there, and "@me" has nobody to mean.
func (u User) Person() bool { return u.Email != "" }

// Display is what to show in the interface: the name the provider gave, else
// a name read out of the address, else the machine's name, else "anonymous".
func (u User) Display() string {
	switch {
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return nameFromAddress(u.Email)
	case u.Service != "":
		return u.Service
	default:
		return "anonymous"
	}
}

// nameFromAddress reads a person's name out of an address when the address is
// shaped like one, and hands back the local part untouched when it is not.
//
// The reason to bother: Cloudflare Access only carries a name claim when the
// identity provider supplies one, and a one-time-PIN login supplies nothing.
// Without this the board is a wall of "sebastian.winterberger2", which is
// worse than a name and worse than an address.
//
// Guessing is bounded on purpose. A local part that is not several alphabetic
// words is left exactly as it was, so a login id like "u236858" stays itself
// instead of becoming "U236858".
func nameFromAddress(addr string) string {
	local := addr
	if i := strings.IndexByte(local, '@'); i > 0 {
		local = local[:i]
		// A +tag is routing, not part of anyone's name. Only inside an
		// address, though: an identity that is not an address at all keeps
		// every character it came with, plus sign included.
		if i := strings.IndexByte(local, '+'); i > 0 {
			local = local[:i]
		}
	}

	words := strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' })
	if len(words) < 2 {
		return local
	}
	for i, w := range words {
		// A trailing number is a disambiguator the directory added, not part
		// of the name: sebastian.winterberger2 is one person, once.
		w = strings.TrimRight(w, "0123456789")
		if !isName(w) {
			return local
		}
		words[i] = title(w)
	}
	return strings.Join(words, " ")
}

// isName reports whether w could be a word of a name: at least two letters,
// and nothing in it that is not a letter or an internal hyphen.
func isName(w string) bool {
	letters := 0
	for i, r := range w {
		switch {
		case unicode.IsLetter(r):
			letters++
		case r == '-' && i > 0 && i < len(w)-1:
		default:
			return false
		}
	}
	return letters >= 2
}

// title uppercases the first letter of every hyphen-separated part and leaves
// the rest of each alone, so "anne-marie" becomes "Anne-Marie" and a surname
// that already carries its own capital, like "McLeod", keeps it.
func title(w string) string {
	out := make([]rune, 0, len(w))
	upNext := true
	for _, r := range w {
		if upNext && unicode.IsLetter(r) {
			r = unicode.ToUpper(r)
			upNext = false
		} else if r == '-' {
			upNext = true
		}
		out = append(out, r)
	}
	return string(out)
}

// Initials is up to two letters for an avatar bubble: the first letter of the
// first and last words of Display. Runes and not bytes, so a name that starts
// outside ASCII is not cut in half.
func (u User) Initials() string {
	words := strings.Fields(u.Display())
	if len(words) == 0 {
		return ""
	}
	first := firstRune(words[0])
	if len(words) == 1 {
		return first
	}
	return first + firstRune(words[len(words)-1])
}

// firstRune is the first letter of s, upper-cased, or "" when s holds none.
// Letters only: a bubble reading "1A" for "123 Alice" says less than "A".
func firstRune(s string) string {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return string(unicode.ToUpper(r))
		}
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
	// The request continues as anonymous rather than being refused here,
	// because refusing on a verification error would turn a key rotation into
	// an outage. Whether an anonymous request is then served at all is
	// Required's decision, two fields down.
	OnError func(err error)
	// Required refuses a request that arrives with no identity instead of
	// serving it anonymously.
	//
	// Identifying a caller and letting an unidentified one through are two
	// different things, and only this closes the second. A proxy or an Access
	// tunnel in front of the board sets a header or an assertion, and anything
	// that reaches the port another way sets neither: on a LAN NodePort, or a
	// port published beside the tunnel, that is a caller with every write the
	// board has. Off by default, because a board with no second way in is not
	// made safer by it and a deployment that turns it on has to know that a key
	// rotation now refuses the page rather than drawing it signed out.
	//
	// The paths ProbePath names stay open whatever this says.
	Required bool
}

// ProbePath reports whether a path is served without an identity even when one
// is required: a kubelet has no assertion to present, and a liveness probe that
// gets a 403 restarts a healthy container in a loop. Neither path answers
// anything about a board, which is what makes them safe to leave out.
//
// A function rather than an exported set, so that what a deployment lets
// through unauthenticated cannot be widened from another package.
func ProbePath(p string) bool { return p == "/healthz" || p == "/readyz" }

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
			if u.Anonymous() {
				if cfg.Required && !ProbePath(r.URL.Path) {
					// 403 rather than 401: there is no scheme a browser could
					// satisfy by asking again, and whatever should have signed
					// this request in was not in front of it.
					http.Error(w, "This board is only reachable through the sign-in in front of it.", http.StatusForbidden)
					return
				}
			} else {
				r = r.WithContext(NewContext(r.Context(), u))
			}
			next.ServeHTTP(w, r)
		})
	}
}
