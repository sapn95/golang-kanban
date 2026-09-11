package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pictures for the people on the board, fetched by this process and served from
// this origin.
//
// Pointing an <img> at github.com would tell GitHub the address of whoever is
// looking at the board and which people are on their screen, on every render,
// and img-src would have to allow a host that is not this one. So the picture is
// fetched here, once a day per person, and the page keeps asking only this
// origin. It is also the only way the board works on a network that cannot
// reach GitHub at all: the fetch fails, the card shows initials.
//
// Only the addresses in the configured map are resolved, and the only URL this
// ever builds is a login's picture on the configured host. Neither the request
// path nor a card's assignee can send it somewhere else.
const (
	avatarTTL      = 24 * time.Hour
	avatarFailTTL  = 10 * time.Minute
	avatarMaxBytes = 512 << 10
	avatarSize     = "128"
	avatarCache    = "private, max-age=86400"
)

// avatarHost is where a login's picture comes from. GitHub redirects this to
// the CDN that actually serves it, which the client follows.
const avatarHost = "https://github.com"

type avatars struct {
	logins map[string]string // address, lower-cased, to GitHub login
	allow  map[string]bool   // the logins above, for the request path
	host   string
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]*avatarEntry
}

type avatarEntry struct {
	body    []byte
	kind    string
	err     error
	fetched time.Time
}

// newAvatars prepares the proxy for a map of address to GitHub login. It
// returns nil for an empty map, which is what turns the feature off.
func newAvatars(logins map[string]string) *avatars {
	if len(logins) == 0 {
		return nil
	}
	a := &avatars{
		logins: make(map[string]string, len(logins)),
		allow:  make(map[string]bool, len(logins)),
		host:   avatarHost,
		client: &http.Client{Timeout: 5 * time.Second},
		now:    func() time.Time { return time.Now().UTC() },
		cache:  map[string]*avatarEntry{},
	}
	for addr, login := range logins {
		addr, login = strings.ToLower(strings.TrimSpace(addr)), strings.TrimSpace(login)
		if addr == "" || login == "" {
			continue
		}
		a.logins[addr] = login
		a.allow[login] = true
	}
	if len(a.logins) == 0 {
		return nil
	}
	return a
}

// avatarURL is the picture of whoever owns addr, or "" when nobody configured
// one. The template falls back to the initials bubble on "".
func (s *Server) avatarURL(addr string) string {
	if s.avatars == nil {
		return ""
	}
	login, ok := s.avatars.logins[strings.ToLower(strings.TrimSpace(addr))]
	if !ok {
		return ""
	}
	return "/avatar/" + login
}

// serveAvatar answers with a cached picture, or 404 so that the page falls back
// to initials. A picture is not worth an error page.
func (s *Server) serveAvatar(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	if !s.avatars.allow[login] {
		http.NotFound(w, r)
		return
	}
	body, kind, err := s.avatars.picture(r.Context(), login)
	if err != nil {
		s.log.Debug("avatar unavailable", "login", login, "err", err)
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", kind)
	// private, because the board itself is behind a login. A day is short
	// enough that a changed picture arrives and long enough that a board full
	// of cards is one request, not one per card.
	h.Set("Cache-Control", avatarCache)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// picture returns login's picture from the cache, or fetches it. Failures are
// cached too, for a shorter time: without that, a login GitHub does not have is
// re-requested for every card that carries it, on every render.
//
// Two requests for the same cold login can both fetch. That costs one extra
// request and no correctness, where locking across the fetch would let a slow
// upstream hold up every other picture on the page.
func (a *avatars) picture(ctx context.Context, login string) ([]byte, string, error) {
	if e := a.cached(login); e != nil {
		return e.body, e.kind, e.err
	}
	body, kind, err := a.download(ctx, login)
	a.mu.Lock()
	a.cache[login] = &avatarEntry{body: body, kind: kind, err: err, fetched: a.now()}
	a.mu.Unlock()
	return body, kind, err
}

// cached returns a picture already fetched for login, or nil when there is
// none or the one there has gone stale.
func (a *avatars) cached(login string) *avatarEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.cache[login]
	if e == nil {
		return nil
	}
	ttl := avatarTTL
	if e.err != nil {
		ttl = avatarFailTTL
	}
	if a.now().Sub(e.fetched) > ttl {
		delete(a.cache, login)
		return nil
	}
	return e
}

// download fetches one picture from GitHub and returns its bytes and content
// type. Requested by the server, so a browser on the board never talks to
// github.com; see docs/adr/0008.
func (a *avatars) download(ctx context.Context, login string) ([]byte, string, error) {
	// Detached from the request, so a viewer who navigates away mid-fetch does
	// not leave a cancelled error in the cache for everybody else.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	url := a.host + "/" + login + ".png?size=" + avatarSize
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("avatar for %q: upstream said %s", login, resp.Status)
	}
	kind := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	switch kind {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, "", fmt.Errorf("avatar for %q: upstream sent %q, which is not an image", login, kind)
	}
	// One byte over the cap is a read of the cap plus one, so a body that is too
	// large is refused rather than silently cut in half.
	body, err := io.ReadAll(io.LimitReader(resp.Body, avatarMaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > avatarMaxBytes {
		return nil, "", fmt.Errorf("avatar for %q: larger than %d bytes", login, avatarMaxBytes)
	}
	return body, kind, nil
}
