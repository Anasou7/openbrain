package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"openbrain/internal/store"
)

const token = "test-token-0123456789abcdef0123456789abcdef"

func newServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	return newServerAt(t, "")
}

// newServerAt serves under a base path, with the admin API token set and
// loopback trusted as a proxy (the recommended deployment).
func newServerAt(t *testing.T, base string) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "openbrain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return handlerFor(st, base), st
}

func handlerFor(st *store.Store, base string) http.Handler {
	proxies, _ := ParseTrustedProxies("127.0.0.1,::1")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(st, Config{BasePath: base, AdminToken: token, TrustedProxies: proxies, Log: log})
}

type req struct {
	method, path string
	body         any    // JSON-encoded unless a string
	auth         string // bearer token; "" sends none
	form         url.Values
	cookie       *http.Cookie
	headers      map[string]string
}

func do(t *testing.T, h http.Handler, r req) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	switch b := r.body.(type) {
	case nil:
	case string:
		body = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		body = bytes.NewReader(j)
	}
	if r.form != nil {
		body = strings.NewReader(r.form.Encode())
	}
	hr := httptest.NewRequest(r.method, r.path, body)
	if r.form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if r.auth != "" {
		hr.Header.Set("Authorization", "Bearer "+r.auth)
	}
	if r.cookie != nil {
		hr.AddCookie(r.cookie)
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)
	return rec
}

// api performs a JSON call with the admin token and checks the status.
func api(t *testing.T, h http.Handler, method, path string, body any, want int) map[string]any {
	t.Helper()
	return apiAs(t, h, token, method, path, body, want)
}

// apiAs performs a JSON call with the given bearer token and checks the status.
func apiAs(t *testing.T, h http.Handler, bearer, method, path string, body any, want int) map[string]any {
	t.Helper()
	rec := do(t, h, req{method: method, path: path, body: body, auth: bearer})
	if rec.Code != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, want, rec.Body)
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestHealthAndHeaders(t *testing.T) {
	h, _ := newServer(t)
	rec := do(t, h, req{method: "GET", path: "/health"})
	if rec.Code != 200 {
		t.Fatalf("health: %d", rec.Code)
	}
	if rec := do(t, h, req{method: "GET", path: "/ready"}); rec.Code != 200 {
		t.Fatalf("ready: %d", rec.Code)
	}
	for _, k := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if rec.Header().Get(k) == "" {
			t.Errorf("missing header %s", k)
		}
	}
}

func TestAPIAuth(t *testing.T) {
	h, _ := newServer(t)
	cases := []struct {
		name string
		r    req
		want int
	}{
		{"no token", req{method: "GET", path: "/api/projects"}, 401},
		{"wrong token", req{method: "GET", path: "/api/projects", auth: "wrong"}, 401},
		{"token prefix", req{method: "GET", path: "/api/projects", auth: token[:20]}, 401},
		{"token in query", req{method: "GET", path: "/api/projects?token=" + token}, 401},
		{"session cookie is not API auth", req{method: "GET", path: "/api/projects", cookie: &http.Cookie{Name: sessionCookie, Value: "x"}}, 401},
		{"valid token", req{method: "GET", path: "/api/projects", auth: token}, 200},
	}
	for _, c := range cases {
		if rec := do(t, h, c.r); rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		} else if c.want == 401 && rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: missing WWW-Authenticate", c.name)
		}
	}
}

func TestAPIFlow(t *testing.T) {
	h, st := newServer(t)

	api(t, h, "POST", "/api/actors", map[string]any{"slug": "claude", "display_name": "Claude", "kind": "agent"}, 201)
	if m := api(t, h, "POST", "/api/actors", map[string]any{"slug": "claude"}, 409); errCode(m) != "conflict" {
		t.Fatalf("duplicate actor: %v", m)
	}
	if m := api(t, h, "GET", "/api/actors", nil, 200); len(m["actors"].([]any)) != 2 {
		t.Fatalf("actors: %v", m)
	}

	p := api(t, h, "POST", "/api/projects", map[string]any{"actor": "claude", "name": "Demo App"}, 201)
	if p["slug"] != "demo-app" {
		t.Fatalf("project: %v", p)
	}
	api(t, h, "GET", "/api/projects/demo-app", nil, 200)
	api(t, h, "GET", fmt.Sprintf("/api/projects/%v", p["id"]), nil, 200)
	if m := api(t, h, "GET", "/api/projects/nope", nil, 404); errCode(m) != "not_found" {
		t.Fatalf("missing project: %v", m)
	}

	note := api(t, h, "POST", "/api/projects/demo-app/notes", map[string]any{"actor": "claude", "content": "found a bug", "pinned": true}, 201)
	if m := api(t, h, "POST", "/api/projects/demo-app/notes", map[string]any{"actor": "ghost", "content": "x"}, 400); errCode(m) != "invalid" {
		t.Fatalf("unknown actor: %v", m)
	}
	if m := api(t, h, "POST", "/api/projects/demo-app/notes", map[string]any{"actor": "claude", "content": "x", "colour": "red"}, 400); errCode(m) != "bad_request" {
		t.Fatalf("unknown field: %v", m)
	}
	api(t, h, "POST", "/api/projects/demo-app/notes", `{"actor":"claude","content":"x"} {"again":1}`, 400)
	api(t, h, "POST", "/api/projects/demo-app/notes", `not json`, 400)
	api(t, h, "POST", "/api/projects/nope/notes", map[string]any{"actor": "claude", "content": "x"}, 404)
	api(t, h, "PATCH", fmt.Sprintf("/api/notes/%v", note["id"]), map[string]any{"actor": "user", "pinned": false}, 200)
	api(t, h, "PATCH", "/api/notes/abc", map[string]any{"actor": "user"}, 400)
	if m := api(t, h, "GET", "/api/projects/demo-app/notes?actor=claude", nil, 200); len(m["notes"].([]any)) != 1 {
		t.Fatalf("notes filter: %v", m)
	}
	api(t, h, "GET", "/api/projects/demo-app/notes?limit=-1", nil, 400)

	api(t, h, "POST", "/api/projects/demo-app/handoffs", map[string]any{
		"actor": "claude", "summary": "API done", "branch": "feature/api", "commit_sha": "9ad281c",
		"metadata": map[string]any{"tests": 42}, "state": "idle",
	}, 201)
	api(t, h, "POST", "/api/projects/demo-app/handoffs", map[string]any{"actor": "claude", "summary": "later"}, 201)
	if m := api(t, h, "GET", "/api/projects/demo-app/handoffs/latest", nil, 200); len(m["handoffs"].([]any)) != 1 {
		t.Fatalf("latest: %v", m)
	}
	if m := api(t, h, "GET", "/api/projects/demo-app/handoffs?actor=claude", nil, 200); len(m["handoffs"].([]any)) != 2 {
		t.Fatalf("history: %v", m)
	}

	s := api(t, h, "PUT", "/api/projects/demo-app/states/claude", map[string]any{"state": "working", "current_task": "tests"}, 200)
	if s["state"] != "working" || s["stale"] != false {
		t.Fatalf("state: %v", s)
	}
	api(t, h, "PUT", "/api/projects/demo-app/states/claude", map[string]any{"actor": "codex", "state": "idle"}, 400)
	api(t, h, "PUT", "/api/projects/demo-app/states/ghost", map[string]any{"state": "idle"}, 400)

	b := api(t, h, "POST", "/api/projects/demo-app/blockers", map[string]any{"actor": "claude", "description": "need a key"}, 201)
	b = api(t, h, "PATCH", fmt.Sprintf("/api/blockers/%v", b["id"]), map[string]any{"actor": "user", "resolved": true, "resolution": "done"}, 200)
	if b["resolved"] != true {
		t.Fatalf("blocker: %v", b)
	}
	api(t, h, "GET", "/api/projects/demo-app/blockers?state=bogus", nil, 400)
	api(t, h, "POST", "/api/projects/demo-app/decisions", map[string]any{"actor": "user", "decision": "Use SQLite", "reasoning": "one file"}, 201)
	api(t, h, "PATCH", "/api/projects/demo-app", map[string]any{"actor": "user", "status": "paused", "current_focus": "tests"}, 200)

	c := api(t, h, "GET", "/api/projects/demo-app/context", nil, 200)
	for _, k := range []string{"project", "latest_handoffs", "actor_states", "open_blockers", "pinned_notes", "human_notes", "recent_notes", "recent_decisions", "recent_activity"} {
		if _, ok := c[k]; !ok {
			t.Errorf("context missing %q", k)
		}
	}
	if c["project"].(map[string]any)["status"] != "paused" || len(c["recent_decisions"].([]any)) != 1 {
		t.Fatalf("context: %v", c)
	}

	act := api(t, h, "GET", "/api/projects/demo-app/activity?limit=3", nil, 200)["activity"].([]any)
	if len(act) != 3 {
		t.Fatalf("activity page: %d", len(act))
	}
	last := act[2].(map[string]any)
	older := api(t, h, "GET", fmt.Sprintf("/api/projects/demo-app/activity?before=%v", last["id"]), nil, 200)["activity"].([]any)
	if len(older) == 0 || older[0].(map[string]any)["id"].(float64) >= last["id"].(float64) {
		t.Fatalf("activity paging: %v", older)
	}
	if last["via"] != "admin-api" {
		t.Fatalf("via: %v", last)
	}

	if o := api(t, h, "GET", "/api/overview", nil, 200); len(o["projects"].([]any)) != 1 {
		t.Fatalf("overview: %v", o)
	}
	if m := api(t, h, "GET", "/api/nothing-here", nil, 404); errCode(m) != "not_found" {
		t.Fatalf("unknown endpoint: %v", m)
	}

	big := map[string]any{"actor": "claude", "content": strings.Repeat("x", 2<<20)}
	if m := api(t, h, "POST", "/api/projects/demo-app/notes", big, 413); errCode(m) != "too_large" {
		t.Fatalf("oversized body: %v", m)
	}
	if n, _ := st.Notes(context.Background(), "demo-app", store.NoteFilter{}); len(n) != 1 {
		t.Fatalf("rejected writes must not persist: %d notes", len(n))
	}
}

const adminPassword = "correct horse battery staple"

// sessionFrom returns the session cookie a response set.
func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" && c.HttpOnly && c.SameSite == http.SameSiteLaxMode {
			return c
		}
	}
	t.Fatalf("no session cookie set (status %d, location %q)", rec.Code, rec.Header().Get("Location"))
	return nil
}

// login completes first-run setup as administrator "alice" and returns her
// session cookie.
func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	return setupAt(t, h, "")
}

func setupAt(t *testing.T, h http.Handler, base string) *http.Cookie {
	t.Helper()
	rec := do(t, h, req{method: "POST", path: base + "/setup", form: url.Values{
		"username": {"alice"}, "display_name": {"Alice"}, "password": {adminPassword}, "confirm": {adminPassword},
	}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	return sessionFrom(t, rec)
}

func TestDashboard(t *testing.T) {
	h, st := newServer(t)
	ctx := context.Background()

	if rec := do(t, h, req{method: "GET", path: "/"}); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("fresh install should lead to setup: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	forged := &http.Cookie{Name: sessionCookie, Value: "forged-session-token"}
	if rec := do(t, h, req{method: "GET", path: "/", cookie: forged}); rec.Code != http.StatusSeeOther {
		t.Fatalf("forged cookie accepted: %d", rec.Code)
	}

	c := login(t, h)
	if rec := do(t, h, req{method: "GET", path: "/login"}); rec.Code != 200 {
		t.Fatalf("login page: %d", rec.Code)
	}
	same := map[string]string{"Sec-Fetch-Site": "same-origin"}
	if rec := do(t, h, req{method: "GET", path: "/", cookie: c}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "No projects yet") {
		t.Fatalf("empty home: %d", rec.Code)
	}

	rec := do(t, h, req{method: "POST", path: "/projects", cookie: c, headers: same, form: url.Values{"name": {"Web <App>"}}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/p/web-app" {
		t.Fatalf("create project: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = do(t, h, req{method: "POST", path: "/p/web-app/notes", cookie: c, headers: same, form: url.Values{"content": {"<script>alert(1)</script>"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add note: %d", rec.Code)
	}
	rec = do(t, h, req{method: "POST", path: "/p/web-app/notes", cookie: c, headers: same, form: url.Values{"content": {"  "}}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("empty note should bounce with an error: %s", loc)
	}
	rec = do(t, h, req{method: "POST", path: "/p/web-app/notes", cookie: c, headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, form: url.Values{"content": {"csrf"}}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", rec.Code)
	}
	rec = do(t, h, req{method: "POST", path: "/p/web-app/notes", cookie: c, headers: map[string]string{"Origin": "https://evil.example"}, form: url.Values{"content": {"csrf"}}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin post: %d", rec.Code)
	}
	if rec := do(t, h, req{method: "POST", path: "/p/web-app/notes", form: url.Values{"content": {"anon"}}}); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("post without session: %d", rec.Code)
	}

	notes, _ := st.Notes(ctx, "web-app", store.NoteFilter{})
	if len(notes) != 1 || notes[0].Actor.Slug != "alice" {
		t.Fatalf("dashboard note should be attributed to the signed-in user: %+v", notes)
	}
	act, _ := st.Activity(ctx, "web-app", 0, 1)
	if act[0].Via != "dashboard" {
		t.Fatalf("via: %+v", act[0])
	}

	for path, form := range map[string]url.Values{
		"/p/web-app/handoffs":                             {"summary": {"left it half done"}, "next_steps": {"finish"}},
		"/p/web-app/decisions":                            {"decision": {"Go"}, "reasoning": {"small"}},
		"/p/web-app/blockers":                             {"description": {"waiting on DNS"}},
		"/p/web-app/edit":                                 {"status": {"paused"}, "current_focus": {"launch"}},
		fmt.Sprintf("/p/%d/notes/%d/pin", 1, notes[0].ID): {"pinned": {"1"}},
	} {
		if rec := do(t, h, req{method: "POST", path: path, cookie: c, headers: same, form: form}); rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
			t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	blockers, _ := st.Blockers(ctx, "web-app", "open", 0)
	rec = do(t, h, req{method: "POST", path: fmt.Sprintf("/p/web-app/blockers/%d", blockers[0].ID), cookie: c, headers: same, form: url.Values{"resolved": {"1"}, "resolution": {"fixed"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("resolve blocker: %d", rec.Code)
	}

	page := do(t, h, req{method: "GET", path: "/p/web-app", cookie: c})
	body := page.Body.String()
	if page.Code != 200 {
		t.Fatalf("project page: %d", page.Code)
	}
	for _, want := range []string{"Web &lt;App&gt;", "&lt;script&gt;alert(1)&lt;/script&gt;", "left it half done", "waiting on DNS", "Go", "launch", "Pinned"} {
		if !strings.Contains(body, want) {
			t.Errorf("project page missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatal("note content not escaped")
	}
	home := do(t, h, req{method: "GET", path: "/", cookie: c}).Body.String()
	if !strings.Contains(home, "left it half done") || !strings.Contains(home, "Alice <span class=\"you\">(you)</span>") {
		t.Fatal("overview should show the user's latest handoff under their own named column")
	}
	if rec := do(t, h, req{method: "GET", path: "/p/missing", cookie: c}); rec.Code != 404 {
		t.Fatalf("missing project page: %d", rec.Code)
	}
	if rec := do(t, h, req{method: "GET", path: "/static/style.css"}); rec.Code != 200 {
		t.Fatalf("stylesheet: %d", rec.Code)
	}

	rec = do(t, h, req{method: "POST", path: "/logout", cookie: c})
	if cs := rec.Result().Cookies(); rec.Code != http.StatusSeeOther || len(cs) != 1 || cs[0].MaxAge >= 0 {
		t.Fatalf("logout should expire the cookie: %d %+v", rec.Code, cs)
	}
}
