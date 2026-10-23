package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"openbrain/internal/store"
)

// --- API credentials -------------------------------------------------------

// principal is who an API request authenticated as: the administrator
// (OPENBRAIN_TOKEN) or one actor (an actor token). Browser sessions are a
// separate credential class and never authenticate the API or MCP.
type principal struct {
	admin bool
	token store.Token // actor tokens only
}

// adminTokenOK compares in constant time. Hashing first makes the comparison
// independent of the guessed token's length.
func (s *Server) adminTokenOK(given string) bool {
	if s.cfg.AdminToken == "" {
		return false
	}
	a, b := sha256.Sum256([]byte(given)), sha256.Sum256([]byte(s.cfg.AdminToken))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// authenticate resolves the bearer credential. It returns
// store.ErrInvalidToken for a missing, wrong or revoked token.
func (s *Server) authenticate(r *http.Request) (principal, error) {
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || secret == "" {
		return principal{}, store.ErrInvalidToken
	}
	if s.adminTokenOK(secret) {
		return principal{admin: true}, nil
	}
	t, err := s.st.Authenticate(r.Context(), secret)
	return principal{token: t}, err
}

// attribution decides whom a write is recorded as. An actor token always
// writes as its own actor, so the client cannot choose; only the admin token
// may name the actor, and its writes are marked as such in the activity log.
func (p principal) attribution(claimed string) (store.Attribution, error) {
	if p.admin {
		return store.Attribution{Actor: claimed, Via: "admin-api"}, nil
	}
	if claimed != "" && claimed != p.token.Actor.Slug {
		return store.Attribution{}, forbidden{fmt.Errorf("this token belongs to actor %q and cannot write as %q", p.token.Actor.Slug, claimed)}
	}
	return store.Attribution{Actor: p.token.Actor.Slug, Via: "api"}, nil
}

func (p principal) requireAdmin(what string) error {
	if p.admin {
		return nil
	}
	return forbidden{fmt.Errorf("%s requires the admin token", what)}
}

type forbidden struct{ error }

// --- browser sessions ------------------------------------------------------

const sessionCookie = "openbrain_session"

// viewer is the signed-in person behind a dashboard request.
type viewer struct {
	User    store.User
	Session store.Session
}

// by attributes a dashboard write to the signed-in person's actor; the
// browser never chooses an author.
func (v *viewer) by() store.Attribution {
	return store.Attribution{Actor: v.User.Actor.Slug, Via: "dashboard"}
}

func (s *Server) sessionCookieFor(r *http.Request, value string, maxAge int) *http.Cookie {
	path := s.base
	if path == "" {
		path = "/"
	}
	return &http.Cookie{
		Name: sessionCookie, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secure(r),
	}
}

// secure reports whether the browser reached us over HTTPS, directly or via
// a trusted proxy.
func (s *Server) secure(r *http.Request) bool {
	return r.TLS != nil || (s.trusted(remoteAddr(r)) && r.Header.Get("X-Forwarded-Proto") == "https")
}

// insecure reports whether credentials typed into this page would cross a
// network unencrypted: plain HTTP to anything but a loopback host (such as an
// SSH tunnel).
func (s *Server) insecure(r *http.Request) bool {
	if s.secure(r) {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err != nil || !addr.IsLoopback()
}

func (s *Server) currentViewer(r *http.Request) (*viewer, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, nil
	}
	u, sess, err := s.st.SessionUser(r.Context(), c.Value)
	if errors.Is(err, store.ErrInvalidToken) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &viewer{u, sess}, nil
}

type pageFunc func(w http.ResponseWriter, r *http.Request, v *viewer)

// page guards a dashboard route: it needs a signed-in user and, for posts, a
// same-origin request with a parsed form. With no accounts yet, every page
// leads to first-run setup.
func (s *Server) page(next pageFunc) http.Handler { return s.guard(next, false) }

// adminPage is page for administrators only.
func (s *Server) adminPage(next pageFunc) http.Handler { return s.guard(next, true) }

func (s *Server) guard(next pageFunc, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if !sameOrigin(r) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form submission", http.StatusBadRequest)
				return
			}
		}
		v, err := s.currentViewer(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if v == nil {
			if n, err := s.st.UserCount(r.Context()); err == nil && n == 0 {
				s.redirect(w, r, "/setup")
				return
			}
			s.redirect(w, r, "/login")
			return
		}
		if adminOnly && !v.User.IsAdmin() {
			s.render(w, r, http.StatusForbidden, "error", v, map[string]any{"Code": 403, "Message": "This page is for administrators."})
			return
		}
		next(w, r, v)
	})
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, s.url(path), http.StatusSeeOther)
}

// sameOrigin rejects cross-site form posts (CSRF). Browsers send
// Sec-Fetch-Site; older ones send Origin. A request with neither did not come
// from a cross-site browser form (and still needs the session cookie).
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// --- setup, sign-in, sign-out ------------------------------------------------

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if n, err := s.st.UserCount(r.Context()); err == nil && n == 0 {
		s.redirect(w, r, "/setup")
		return
	}
	if v, _ := s.currentViewer(r); v != nil {
		s.redirect(w, r, "/")
		return
	}
	s.render(w, r, http.StatusOK, "login", nil, map[string]any{"Insecure": s.insecure(r)})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	username, password := strings.TrimSpace(r.PostFormValue("username")), r.PostFormValue("password")
	data := map[string]any{"Username": username, "Insecure": s.insecure(r)}
	ip := s.clientIP(r)
	if wait := s.limiter.retryAfter(username, ip); wait > 0 {
		s.log.Warn("sign-in rate limited", "ip", ip)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		data["Err"] = fmt.Sprintf("Too many sign-in attempts. Try again in %d minutes.", int(wait.Minutes())+1)
		s.render(w, r, http.StatusTooManyRequests, "login", nil, data)
		return
	}
	u, err := s.st.VerifyLogin(r.Context(), username, password)
	if errors.Is(err, store.ErrInvalidLogin) {
		s.limiter.fail(username, ip)
		// Never log the submitted username: people sometimes paste a password into it.
		s.log.Warn("sign-in failed", "ip", ip, "reason", "invalid_credentials")
		data["Err"] = "Invalid username or password."
		s.render(w, r, http.StatusUnauthorized, "login", nil, data)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.limiter.succeed(username, ip)
	s.startSession(w, r, u)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User) {
	secret, _, err := s.st.CreateSession(r.Context(), u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, s.sessionCookieFor(r, secret, int(store.SessionMax.Seconds())))
	s.log.Info("signed in", "user_id", u.ID, "ip", s.clientIP(r))
	s.redirect(w, r, "/")
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if n, err := s.st.UserCount(r.Context()); err != nil || n > 0 {
		s.redirect(w, r, "/login")
		return
	}
	s.render(w, r, http.StatusOK, "setup", nil, map[string]any{"Insecure": s.insecure(r)})
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form submission", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	in := store.UserInput{Username: f.Get("username"), DisplayName: f.Get("display_name"), Password: f.Get("password")}
	data := map[string]any{"Username": in.Username, "DisplayName": in.DisplayName, "Insecure": s.insecure(r)}
	if in.Password != f.Get("confirm") {
		data["Err"] = "The passwords do not match."
		s.render(w, r, http.StatusBadRequest, "setup", nil, data)
		return
	}
	u, err := s.st.CreateFirstUser(r.Context(), in)
	var ve *store.ValidationError
	switch {
	case errors.Is(err, store.ErrSetupDone):
		s.render(w, r, http.StatusForbidden, "error", nil, map[string]any{"Code": 403, "Message": "Setup has already been completed. Sign in instead.", "Link": "/login"})
		return
	case errors.As(err, &ve):
		data["Err"] = sentence(ve.Error())
		s.render(w, r, http.StatusBadRequest, "setup", nil, data)
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	s.log.Info("first administrator created", "user_id", u.ID)
	s.startSession(w, r, u)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.st.RevokeSession(r.Context(), c.Value); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	http.SetCookie(w, s.sessionCookieFor(r, "", -1))
	s.redirect(w, r, "/login")
}

// sentence capitalises a validation message for display.
func sentence(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}
