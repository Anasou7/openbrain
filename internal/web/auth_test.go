package web

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"openbrain/internal/store"
)

var same = map[string]string{"Sec-Fetch-Site": "same-origin"}

func signIn(t *testing.T, h http.Handler, base, user, pw string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, req{method: "POST", path: base + "/login", form: url.Values{"username": {user}, "password": {pw}}})
}

func TestBasePathRouting(t *testing.T) {
	for _, base := range []string{"", "/openbrain"} {
		t.Run("base="+base, func(t *testing.T) {
			h, _ := newServerAt(t, base)
			root := base + "/"

			rec := do(t, h, req{method: "GET", path: root})
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != base+"/setup" {
				t.Fatalf("fresh install: %d %q", rec.Code, rec.Header().Get("Location"))
			}
			setup := do(t, h, req{method: "GET", path: base + "/setup"}).Body.String()
			for _, want := range []string{
				`action="` + base + `/setup"`,
				`href="` + base + `/static/style.css?v=`,
			} {
				if !strings.Contains(setup, want) {
					t.Errorf("setup page missing %s", want)
				}
			}

			c := setupAt(t, h, base)
			wantPath := base
			if wantPath == "" {
				wantPath = "/"
			}
			if c.Path != wantPath {
				t.Errorf("session cookie path = %q, want %q", c.Path, wantPath)
			}

			page := do(t, h, req{method: "GET", path: root, cookie: c})
			body := page.Body.String()
			if page.Code != 200 {
				t.Fatalf("overview: %d", page.Code)
			}
			for _, want := range []string{
				`href="` + base + `/projects"`, `href="` + base + `/agents"`, `action="` + base + `/logout"`,
				`src="` + base + `/static/app.js?v=`, `data-base="` + base + `"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("overview missing %s", want)
				}
			}
			if base != "" && (strings.Contains(body, `href="/projects"`) || strings.Contains(body, `action="/logout"`)) {
				t.Error("found a link that ignores the base path")
			}

			rec = do(t, h, req{method: "POST", path: base + "/projects", cookie: c, headers: same, form: url.Values{"name": {"Demo"}}})
			if rec.Header().Get("Location") != base+"/p/demo" {
				t.Errorf("redirect after create: %q", rec.Header().Get("Location"))
			}
			for path, code := range map[string]int{
				base + "/static/style.css":                200,
				base + "/static/fonts/geist-mono.woff2":   200,
				base + "/health":                          200,
				base + "/p/demo":                          200,
				base + "/api/whoami":                      401, // no bearer
				base + "/static/fonts/does-not-exist.ttf": 404,
			} {
				if rec := do(t, h, req{method: "GET", path: path, cookie: c}); rec.Code != code {
					t.Errorf("GET %s = %d, want %d", path, rec.Code, code)
				}
			}
			if rec := do(t, h, req{method: "GET", path: base + "/api/whoami", auth: token}); rec.Code != 200 {
				t.Errorf("API under base path: %d", rec.Code)
			}
			if rec := do(t, h, req{method: "POST", path: base + "/mcp", body: `{}`}); rec.Code != 401 {
				t.Errorf("MCP under base path without a token: %d", rec.Code)
			}
			if rec := do(t, h, req{method: "POST", path: base + "/mcp", body: `{}`, cookie: c}); rec.Code != 401 {
				t.Errorf("a browser session must not authenticate MCP: %d", rec.Code)
			}

			if base != "" {
				if rec := do(t, h, req{method: "GET", path: "/"}); rec.Code != 404 {
					t.Errorf("outside the base path: %d", rec.Code)
				}
				if rec := do(t, h, req{method: "GET", path: base}); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != base+"/" {
					t.Errorf("bare base path: %d %q", rec.Code, rec.Header().Get("Location"))
				}
				if rec := do(t, h, req{method: "GET", path: base + "x/"}); rec.Code != 404 {
					t.Errorf("look-alike prefix: %d", rec.Code)
				}
			}
		})
	}
}

func TestNormalizeBasePath(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "openbrain": "/openbrain", "/openbrain/": "/openbrain", " /a/b ": "/a/b"} {
		if got, err := NormalizeBasePath(in); err != nil || got != want {
			t.Errorf("NormalizeBasePath(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"/a/../b", "/a//b", "/a b", "/a?x", "/ü"} {
		if _, err := NormalizeBasePath(bad); err == nil {
			t.Errorf("NormalizeBasePath(%q) accepted", bad)
		}
	}
}

func TestSetupFlow(t *testing.T) {
	h, st := newServer(t)
	if rec := do(t, h, req{method: "GET", path: "/login"}); rec.Header().Get("Location") != "/setup" {
		t.Fatalf("login before setup should lead to setup: %q", rec.Header().Get("Location"))
	}
	rec := do(t, h, req{method: "POST", path: "/setup", form: url.Values{"username": {"alice"}, "password": {adminPassword}, "confirm": {"different"}}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "do not match") {
		t.Fatalf("mismatched confirmation: %d", rec.Code)
	}
	rec = do(t, h, req{method: "POST", path: "/setup", form: url.Values{"username": {"alice"}, "password": {"short"}, "confirm": {"short"}}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "at least 8") {
		t.Fatalf("short password: %d", rec.Code)
	}
	if n, _ := st.UserCount(context.Background()); n != 0 {
		t.Fatal("failed setups must not create accounts")
	}

	login(t, h)
	u, _ := st.UserByName(context.Background(), "alice")
	if u.Role != "admin" || u.Actor.Slug != "alice" || u.Actor.Kind != "human" {
		t.Fatalf("first user: %+v", u)
	}
	if rec := do(t, h, req{method: "GET", path: "/setup"}); rec.Header().Get("Location") != "/login" {
		t.Fatalf("setup page after setup: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = do(t, h, req{method: "POST", path: "/setup", form: url.Values{"username": {"mallory"}, "password": {adminPassword}, "confirm": {adminPassword}}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("second anonymous setup: %d", rec.Code)
	}
	if rec := do(t, h, req{method: "POST", path: "/setup", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site setup: %d", rec.Code)
	}
}

func TestConcurrentSetupThroughHTTP(t *testing.T) {
	h, st := newServer(t)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(t, h, req{method: "POST", path: "/setup", form: url.Values{
				"username": {fmt.Sprintf("admin%d", i)}, "password": {adminPassword}, "confirm": {adminPassword},
			}})
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)
	created := 0
	for c := range codes {
		if c == http.StatusSeeOther {
			created++
		}
	}
	users, _ := st.Users(context.Background())
	if created != 1 || len(users) != 1 || users[0].Role != "admin" {
		t.Fatalf("concurrent setup created %d (users %d)", created, len(users))
	}
}

func TestLoginFlow(t *testing.T) {
	h, st := newServer(t)
	login(t, h)
	ctx := context.Background()

	rec := signIn(t, h, "", "ALICE", adminPassword) // usernames are case-insensitive
	c := sessionFrom(t, rec)
	if rec.Header().Get("Location") != "/" {
		t.Fatalf("after sign-in: %q", rec.Header().Get("Location"))
	}

	wrong := signIn(t, h, "", "alice", "not the password")
	unknown := signIn(t, h, "", "nobody", "not the password")
	for name, r := range map[string]*httptest.ResponseRecorder{"wrong password": wrong, "unknown user": unknown} {
		if r.Code != 401 || !strings.Contains(r.Body.String(), "Invalid username or password.") || len(r.Result().Cookies()) != 0 {
			t.Errorf("%s: %d", name, r.Code)
		}
	}

	// A disabled account gets the same generic answer, and its sessions end.
	bob, err := st.CreateUser(ctx, store.UserInput{Username: "bob", Password: "bob-password-1"})
	if err != nil {
		t.Fatal(err)
	}
	bobCookie := sessionFrom(t, signIn(t, h, "", "bob", "bob-password-1"))
	alice, _ := st.UserByName(ctx, "alice")
	if err := st.SetDisabled(ctx, alice.ID, bob.ID, true); err != nil {
		t.Fatal(err)
	}
	if r := signIn(t, h, "", "bob", "bob-password-1"); r.Code != 401 || !strings.Contains(r.Body.String(), "Invalid username or password.") {
		t.Errorf("disabled user: %d", r.Code)
	}
	if r := do(t, h, req{method: "GET", path: "/", cookie: bobCookie}); r.Header().Get("Location") != "/login" {
		t.Errorf("disabled user's old session still works: %d", r.Code)
	}

	// Sessions survive a restart: a new handler on the same database accepts the cookie.
	if r := do(t, handlerFor(st, ""), req{method: "GET", path: "/", cookie: c}); r.Code != 200 {
		t.Errorf("session after restart: %d", r.Code)
	}

	// Sign-out revokes the session server-side, not just in the browser.
	out := do(t, h, req{method: "POST", path: "/logout", cookie: c})
	if out.Header().Get("Location") != "/login" {
		t.Fatalf("logout: %d", out.Code)
	}
	if r := do(t, h, req{method: "GET", path: "/", cookie: c}); r.Header().Get("Location") != "/login" {
		t.Errorf("revoked session still works: %d", r.Code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	h, _ := newServer(t)
	login(t, h)
	for range loginPerUser {
		signIn(t, h, "", "alice", "wrong-password")
	}
	r := signIn(t, h, "", "alice", adminPassword) // even the right password waits
	if r.Code != http.StatusTooManyRequests || r.Header().Get("Retry-After") == "" || !strings.Contains(r.Body.String(), "Too many") {
		t.Fatalf("after %d failures: %d", loginPerUser, r.Code)
	}
	if r := signIn(t, h, "", "someone-else", "x-password"); r.Code != 401 {
		t.Errorf("other usernames from the same client should still be checked: %d", r.Code)
	}
}

// TestAuthLogsOmitSecrets checks that sign-in logging never records what the
// client typed: a password pasted into the username field must not reach the
// journal.
func TestAuthLogsOmitSecrets(t *testing.T) {
	_, st := newServer(t)
	var buf bytes.Buffer
	proxies, _ := ParseTrustedProxies("127.0.0.1,::1")
	h := New(st, Config{AdminToken: token, TrustedProxies: proxies, Log: slog.New(slog.NewTextHandler(&buf, nil))})
	cookie := login(t, h)

	const pasted = "hunter2-pasted-into-username"
	signIn(t, h, "", pasted, "wrong-password-1")
	for range loginPerUser {
		signIn(t, h, "", "alice", "wrong-password-2")
	}
	signIn(t, h, "", "alice", adminPassword) // rate limited

	out := buf.String()
	for _, secret := range []string{pasted, "wrong-password-1", "wrong-password-2", adminPassword, cookie.Value, "alice"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"reason=invalid_credentials", `msg="sign-in rate limited"`, `msg="signed in" user_id=1`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestLimiterWindow(t *testing.T) {
	l := newLimiter()
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for range loginPerUser {
		l.fail("alice", "10.0.0.1")
	}
	if l.retryAfter("alice", "10.0.0.1") == 0 {
		t.Fatal("should be limited")
	}
	if l.retryAfter("alice", "10.0.0.2") != 0 {
		t.Fatal("another client is not limited")
	}
	now = now.Add(loginWindow)
	if l.retryAfter("alice", "10.0.0.1") != 0 {
		t.Fatal("window should have expired")
	}
	for range loginPerIP {
		l.fail(fmt.Sprint(now.UnixNano()), "10.0.0.3")
		now = now.Add(time.Millisecond)
	}
	if l.retryAfter("fresh-name", "10.0.0.3") == 0 {
		t.Fatal("per-client limit should apply across usernames")
	}
	l.fail("bob", "10.0.0.4")
	l.succeed("bob", "10.0.0.4")
	if _, ok := l.hits[userKey("bob", "10.0.0.4")]; ok {
		t.Fatal("success should clear the username count")
	}
}

func TestClientIP(t *testing.T) {
	proxies, _ := ParseTrustedProxies("127.0.0.1, ::1, 10.0.0.0/8")
	s := &Server{cfg: Config{TrustedProxies: proxies}}
	cases := []struct{ remote, xff, want string }{
		{"203.0.113.9:5000", "", "203.0.113.9"},
		{"203.0.113.9:5000", "192.0.2.44", "203.0.113.9"},              // untrusted peer: header ignored
		{"127.0.0.1:5000", "198.51.100.7", "198.51.100.7"},             // trusted proxy
		{"127.0.0.1:5000", "192.0.2.66, 198.51.100.7", "198.51.100.7"}, // spoofed left entry ignored
		{"127.0.0.1:5000", "198.51.100.7, 10.1.2.3", "198.51.100.7"},
		{"[::1]:5000", "garbage", "::1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := s.clientIP(r); got != c.want {
			t.Errorf("clientIP(%s, %q) = %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestAuthorization(t *testing.T) {
	h, st := newServer(t)
	ctx := context.Background()
	admin := login(t, h)

	if r := do(t, h, req{method: "GET", path: "/projects"}); r.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous request: %d %q", r.Code, r.Header().Get("Location"))
	}

	// The admin creates a user through the System page.
	r := do(t, h, req{method: "POST", path: "/system/users", cookie: admin, headers: same, form: url.Values{
		"username": {"bob"}, "display_name": {"Bob"}, "password": {"bob-password-1"}, "confirm": {"bob-password-1"}, "role": {"user"},
	}})
	if r.Code != http.StatusSeeOther || strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatalf("admin creates user: %d %q", r.Code, r.Header().Get("Location"))
	}
	bob := sessionFrom(t, signIn(t, h, "", "bob", "bob-password-1"))

	for _, path := range []string{"/system"} {
		if r := do(t, h, req{method: "GET", path: path, cookie: bob}); r.Code != http.StatusForbidden {
			t.Errorf("user GET %s: %d", path, r.Code)
		}
	}
	for path, form := range map[string]url.Values{
		"/system/users":           {"username": {"eve"}, "password": {"eve-password"}, "confirm": {"eve-password"}},
		"/system/users/1/disable": {},
		"/agents/tokens":          {"actor": {"bob"}},
		"/agents":                 {"slug": {"rogue"}},
	} {
		if r := do(t, h, req{method: "POST", path: path, cookie: bob, headers: same, form: form}); r.Code != http.StatusForbidden {
			t.Errorf("user POST %s: %d", path, r.Code)
		}
	}
	if n, _ := st.UserCount(ctx); n != 2 {
		t.Fatalf("a normal user created an account: %d users", n)
	}
	if page := do(t, h, req{method: "GET", path: "/", cookie: bob}).Body.String(); strings.Contains(page, `href="/system"`) {
		t.Error("System link shown to a non-admin")
	}

	// Bob's dashboard writes are his, whatever the form claims.
	api(t, h, "POST", "/api/projects", map[string]any{"actor": "alice", "name": "Shared"}, 201)
	do(t, h, req{method: "POST", path: "/p/shared/notes", cookie: bob, headers: same, form: url.Values{"content": {"from bob"}, "actor": {"alice"}}})
	notes, _ := st.Notes(ctx, "shared", store.NoteFilter{})
	if len(notes) != 1 || notes[0].Actor.Slug != "bob" || notes[0].Actor.DisplayName != "Bob" {
		t.Fatalf("note attribution: %+v", notes)
	}

	// Admin disables Bob: his session ends at once; the last admin cannot be disabled.
	users, _ := st.Users(ctx)
	var bobID, aliceID int64
	for _, u := range users {
		if u.Username == "bob" {
			bobID = u.ID
		} else {
			aliceID = u.ID
		}
	}
	do(t, h, req{method: "POST", path: fmt.Sprintf("/system/users/%d/disable", bobID), cookie: admin, headers: same})
	if r := do(t, h, req{method: "GET", path: "/", cookie: bob}); r.Header().Get("Location") != "/login" {
		t.Errorf("disabled user's session: %d", r.Code)
	}
	r = do(t, h, req{method: "POST", path: fmt.Sprintf("/system/users/%d/disable", aliceID), cookie: admin, headers: same})
	if !strings.Contains(r.Header().Get("Location"), "err=") {
		t.Errorf("admin disabling themselves should be refused: %q", r.Header().Get("Location"))
	}

	// Admin password reset signs the user out.
	do(t, h, req{method: "POST", path: fmt.Sprintf("/system/users/%d/enable", bobID), cookie: admin, headers: same})
	bob = sessionFrom(t, signIn(t, h, "", "bob", "bob-password-1"))
	r = do(t, h, req{method: "POST", path: fmt.Sprintf("/system/users/%d/password", bobID), cookie: admin, headers: same,
		form: url.Values{"password": {"bob-password-2"}, "confirm": {"bob-password-2"}}})
	if strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatalf("reset: %q", r.Header().Get("Location"))
	}
	if r := do(t, h, req{method: "GET", path: "/", cookie: bob}); r.Header().Get("Location") != "/login" {
		t.Error("reset should end the user's sessions")
	}
	if r := signIn(t, h, "", "bob", "bob-password-2"); r.Code != http.StatusSeeOther {
		t.Errorf("sign in with the reset password: %d", r.Code)
	}
}

func TestAccountPage(t *testing.T) {
	h, st := newServer(t)
	here := login(t, h)
	other := sessionFrom(t, signIn(t, h, "", "alice", adminPassword))

	if r := do(t, h, req{method: "GET", path: "/account", cookie: here}); r.Code != 200 || !strings.Contains(r.Body.String(), "2 active sessions") {
		t.Fatalf("account page: %d", r.Code)
	}
	r := do(t, h, req{method: "POST", path: "/account/password", cookie: here, headers: same,
		form: url.Values{"current": {"wrong"}, "password": {"new-password-1"}, "confirm": {"new-password-1"}}})
	if !strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatalf("wrong current password accepted: %q", r.Header().Get("Location"))
	}
	r = do(t, h, req{method: "POST", path: "/account/password", cookie: here, headers: same,
		form: url.Values{"current": {adminPassword}, "password": {"new-password-1"}, "confirm": {"new-password-1"}}})
	if !strings.Contains(r.Header().Get("Location"), "msg=") {
		t.Fatalf("password change: %q", r.Header().Get("Location"))
	}
	if r := do(t, h, req{method: "GET", path: "/", cookie: here}); r.Code != 200 {
		t.Error("changing your password should keep this session")
	}
	if r := do(t, h, req{method: "GET", path: "/", cookie: other}); r.Header().Get("Location") != "/login" {
		t.Error("changing your password should end other sessions")
	}

	third := sessionFrom(t, signIn(t, h, "", "alice", "new-password-1"))
	do(t, h, req{method: "POST", path: "/account/sessions", cookie: here, headers: same})
	if r := do(t, h, req{method: "GET", path: "/", cookie: third}); r.Header().Get("Location") != "/login" {
		t.Error("sign out other sessions")
	}

	do(t, h, req{method: "POST", path: "/account/name", cookie: here, headers: same, form: url.Values{"display_name": {"Alice A."}}})
	if u, _ := st.UserByName(context.Background(), "alice"); u.Actor.DisplayName != "Alice A." {
		t.Errorf("display name: %q", u.Actor.DisplayName)
	}
}

func TestInsecureWarning(t *testing.T) {
	h, _ := newServer(t)
	cases := map[string]bool{"127.0.0.1:8080": false, "localhost:8080": false, "[::1]:8080": false, "203.0.113.5": true, "brain.example.com": true}
	for host, want := range cases {
		r := httptest.NewRequest("GET", "/setup", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if got := strings.Contains(rec.Body.String(), "not encrypted"); got != want {
			t.Errorf("host %s: warning shown = %v, want %v", host, got, want)
		}
	}
	r := httptest.NewRequest("GET", "/setup", nil)
	r.Host = "brain.example.com"
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if strings.Contains(rec.Body.String(), "not encrypted") {
		t.Error("HTTPS via a trusted proxy should not warn")
	}
}
