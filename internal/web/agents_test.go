package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"openbrain/internal/store"
)

func TestActorTokensOnAPI(t *testing.T) {
	h, st := newServer(t)
	ctx := context.Background()
	for _, slug := range []string{"claude", "codex"} {
		if _, err := st.CreateActor(ctx, store.ActorInput{Slug: slug}); err != nil {
			t.Fatal(err)
		}
	}
	_, claude, _ := st.CreateToken(ctx, "claude", "test")

	who := apiAs(t, h, claude, "GET", "/api/whoami", nil, 200)
	if who["kind"] != "actor" || who["actor"].(map[string]any)["slug"] != "claude" {
		t.Fatalf("whoami: %v", who)
	}
	if fmt.Sprint(who) == "" || strings.Contains(fmt.Sprint(who), claude[10:]) {
		t.Fatal("whoami must not echo the secret")
	}
	if api(t, h, "GET", "/api/whoami", nil, 200)["kind"] != "admin" {
		t.Fatal("admin whoami")
	}

	api(t, h, "POST", "/api/projects", map[string]any{"actor": "user", "name": "Demo"}, 201)

	// An actor token writes as its own actor without naming it...
	n := apiAs(t, h, claude, "POST", "/api/projects/demo/notes", map[string]any{"content": "from claude"}, 201)
	if n["actor"].(map[string]any)["slug"] != "claude" {
		t.Fatalf("note author: %v", n)
	}
	apiAs(t, h, claude, "POST", "/api/projects/demo/notes", map[string]any{"actor": "claude", "content": "explicit self"}, 201)
	apiAs(t, h, claude, "PUT", "/api/projects/demo/states/claude", map[string]any{"state": "working"}, 200)

	// ...and cannot write as anyone else.
	for _, c := range []struct {
		method, path string
		body         map[string]any
	}{
		{"POST", "/api/projects/demo/notes", map[string]any{"actor": "codex", "content": "x"}},
		{"POST", "/api/projects/demo/notes", map[string]any{"actor": "user", "content": "x"}},
		{"POST", "/api/projects/demo/handoffs", map[string]any{"actor": "codex", "summary": "x"}},
		{"PUT", "/api/projects/demo/states/codex", map[string]any{"state": "idle"}},
		{"PATCH", "/api/projects/demo", map[string]any{"actor": "user", "status": "paused"}},
		{"POST", "/api/actors", map[string]any{"slug": "evil"}},
	} {
		if m := apiAs(t, h, claude, c.method, c.path, c.body, 403); errCode(m) != "forbidden" {
			t.Errorf("%s %s: %v", c.method, c.path, m)
		}
	}

	// The store's author-only rule still applies: claude cannot reword user's note.
	mine := api(t, h, "POST", "/api/projects/demo/notes", map[string]any{"actor": "user", "content": "mine"}, 201)
	apiAs(t, h, claude, "PATCH", fmt.Sprintf("/api/notes/%v", mine["id"]), map[string]any{"content": "changed"}, 400)

	vias := map[string]string{}
	for _, e := range api(t, h, "GET", "/api/projects/demo/activity", nil, 200)["activity"].([]any) {
		e := e.(map[string]any)
		vias[e["actor"].(map[string]any)["slug"].(string)+" "+e["kind"].(string)] = e["via"].(string)
	}
	if vias["claude note.added"] != "api" || vias["user note.added"] != "admin-api" || vias["user project.created"] != "admin-api" {
		t.Fatalf("provenance: %v", vias)
	}

	// Repository lookup and compact context over REST.
	api(t, h, "PATCH", "/api/projects/demo", map[string]any{"actor": "user", "repo_url": "git@github.com:acme/demo.git"}, 200)
	if ps := apiAs(t, h, claude, "GET", "/api/projects?repo_url="+url.QueryEscape("https://github.com/Acme/demo"), nil, 200)["projects"].([]any); len(ps) != 1 {
		t.Fatalf("repo lookup: %v", ps)
	}
	if c := apiAs(t, h, claude, "GET", "/api/projects/demo/context?compact=true&activity_limit=2", nil, 200); c["compact"] != true || len(c["recent_activity"].([]any)) != 2 {
		t.Fatalf("compact context: compact=%v activity=%d", c["compact"], len(c["recent_activity"].([]any)))
	}

	tokens, _ := st.Tokens(ctx, false)
	st.RevokeToken(ctx, tokens[0].ID)
	if rec := do(t, h, req{method: "GET", path: "/api/projects", auth: claude}); rec.Code != 401 {
		t.Fatalf("revoked token: %d", rec.Code)
	}
}

var secretRE = regexp.MustCompile(`obt_[A-Za-z0-9_-]{43}`)

func TestDashboardAgents(t *testing.T) {
	h, st := newServer(t)
	ctx := context.Background()
	c := login(t, h)
	same := map[string]string{"Sec-Fetch-Site": "same-origin"}

	if rec := do(t, h, req{method: "GET", path: "/agents"}); rec.Code != http.StatusSeeOther {
		t.Fatalf("agents page without session: %d", rec.Code)
	}
	rec := do(t, h, req{method: "POST", path: "/agents", cookie: c, headers: same, form: url.Values{"slug": {"hermes"}, "display_name": {"Hermes"}, "kind": {"agent"}}})
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("create actor: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	rec = do(t, h, req{method: "POST", path: "/agents/tokens", cookie: c, headers: same, form: url.Values{"actor": {"hermes"}, "label": {"box"}}})
	secret := secretRE.FindString(rec.Body.String())
	if rec.Code != 200 || secret == "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create token: %d, secret found=%v, cache=%q", rec.Code, secret != "", rec.Header().Get("Cache-Control"))
	}
	if tok, err := st.Authenticate(ctx, secret); err != nil || tok.Actor.Slug != "hermes" {
		t.Fatalf("dashboard-issued token: %+v %v", tok, err)
	}

	page := do(t, h, req{method: "GET", path: "/agents", cookie: c}).Body.String()
	if strings.Contains(page, secret) || !strings.Contains(page, secret[:12]) || !strings.Contains(page, "used") {
		t.Fatal("agents page must show the token prefix and use, never the secret")
	}

	tokens, _ := st.Tokens(ctx, false)
	rec = do(t, h, req{method: "POST", path: fmt.Sprintf("/agents/tokens/%d/revoke", tokens[0].ID), cookie: c, headers: same})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if _, err := st.Authenticate(ctx, secret); !errors.Is(err, store.ErrInvalidToken) {
		t.Fatalf("revoked via dashboard but still valid: %v", err)
	}
	if page := do(t, h, req{method: "GET", path: "/agents", cookie: c}).Body.String(); !strings.Contains(page, "revoked") {
		t.Fatal("revoked token not shown as revoked")
	}

	rec = do(t, h, req{method: "POST", path: "/agents/tokens", cookie: c, headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, form: url.Values{"actor": {"hermes"}}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site token creation: %d", rec.Code)
	}
	rec = do(t, h, req{method: "POST", path: "/agents/tokens", cookie: c, headers: same, form: url.Values{"actor": {"ghost"}}})
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("token for unknown actor should bounce with an error: %d", rec.Code)
	}
}
