package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"openbrain/internal/store"
)

func TestUpdatesAPI(t *testing.T) {
	h, st := newServer(t)
	ctx := context.Background()
	api(t, h, "POST", "/api/actors", map[string]any{"slug": "claude"}, 201)
	api(t, h, "POST", "/api/projects", map[string]any{"actor": "user", "name": "Demo"}, 201)
	_, secret, err := st.CreateToken(ctx, "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"title": "Login is now faster", "summary": "Signing in should feel immediate.", "category": "fixed"}

	if got := apiAs(t, h, "", "GET", "/api/projects/demo/updates/latest", nil, 401); errCode(got) != "unauthorized" {
		t.Fatalf("anonymous read: %v", got)
	}
	apiAs(t, h, "", "POST", "/api/projects/demo/updates", body, 401)
	if rec := do(t, h, req{method: "POST", path: "/api/projects/demo/updates", body: body, cookie: &http.Cookie{Name: sessionCookie, Value: "x"}}); rec.Code != 401 {
		t.Fatalf("session cookie must not authenticate the API: %d", rec.Code)
	}
	if got := api(t, h, "GET", "/api/projects/demo/updates/latest", nil, 404); errCode(got) != "not_found" {
		t.Fatalf("latest with no updates: %v", got)
	}

	// Actor tokens write as their actor, and only as their actor.
	u := apiAs(t, h, secret, "POST", "/api/projects/demo/updates", body, 201)
	if u["actor"].(map[string]any)["slug"] != "claude" || u["category"] != "fixed" {
		t.Fatalf("actor-token update: %v", u)
	}
	other := map[string]any{"actor": "user", "title": "T", "summary": "S", "category": "new"}
	apiAs(t, h, secret, "POST", "/api/projects/demo/updates", other, 403)
	// The admin token must name its actor explicitly.
	if got := api(t, h, "POST", "/api/projects/demo/updates", body, 400); errCode(got) != "invalid" {
		t.Fatalf("admin token without actor: %v", got)
	}
	api(t, h, "POST", "/api/projects/demo/updates", other, 201)
	bad := map[string]any{"title": "T", "summary": "S", "category": "release"}
	apiAs(t, h, secret, "POST", "/api/projects/demo/updates", bad, 400)

	list := api(t, h, "GET", "/api/projects/demo/updates", nil, 200)["updates"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["title"] != "T" {
		t.Fatalf("list: %v", list)
	}
	if mine := api(t, h, "GET", "/api/projects/demo/updates?actor=claude", nil, 200)["updates"].([]any); len(mine) != 1 {
		t.Fatalf("actor filter: %v", mine)
	}
	if latest := api(t, h, "GET", "/api/projects/demo/updates/latest", nil, 200); latest["title"] != "T" {
		t.Fatalf("latest: %v", latest)
	}

	// A handoff can carry the update; it cannot name another author for it.
	hand := apiAs(t, h, secret, "POST", "/api/projects/demo/handoffs", map[string]any{
		"summary": "Fixed the slow path.", "user_update": body,
	}, 201)
	if uu, _ := hand["user_update"].(map[string]any); uu == nil || uu["handoff_id"] != hand["id"] {
		t.Fatalf("handoff user_update: %v", hand)
	}
	sneaky := map[string]any{"summary": "x", "user_update": map[string]any{"actor": "user", "title": "T", "summary": "S", "category": "new"}}
	if got := apiAs(t, h, secret, "POST", "/api/projects/demo/handoffs", sneaky, 400); errCode(got) != "bad_request" {
		t.Fatalf("actor inside user_update: %v", got)
	}
}

func TestCellPrefersUpdate(t *testing.T) {
	at := func(min int) store.Time { return store.Time{Time: time.Date(2026, 1, 1, 12, min, 0, 0, time.UTC)} }
	update := &store.ProjectUpdate{Title: "Login is now faster", CreatedAt: at(10)}
	handoff := &store.Handoff{Summary: "Implemented session caching.", CreatedAt: at(20)}
	state := func(s, task string, min int) *store.ActorState {
		return &store.ActorState{State: s, CurrentTask: task, UpdatedAt: at(min)}
	}
	for name, c := range map[string]struct {
		cell store.Cell
		want string
	}{
		"update over a newer handoff":   {store.Cell{Update: update, Handoff: handoff, State: state("finished", "", 20)}, "Login is now faster"},
		"update over older live work":   {store.Cell{Update: update, State: state("working", "Profiling", 5)}, "Login is now faster"},
		"newer live work over update":   {store.Cell{Update: update, State: state("blocked", "Off-host backups", 30)}, "Off-host backups"},
		"no update: handoff summary":    {store.Cell{Handoff: handoff, State: state("idle", "", 20)}, "Implemented session caching."},
		"no update: current busy task":  {store.Cell{Handoff: handoff, State: state("working", "Login UI", 25)}, "Login UI"},
		"newer idle state keeps update": {store.Cell{Update: update, State: state("idle", "Old task", 30)}, "Login is now faster"},
	} {
		if got := cellSummary(&c.cell).Text; got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestUpdatesDashboard(t *testing.T) {
	h, st := newServer(t)
	ctx := context.Background()
	cookie := login(t, h)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := st.CreateProject(ctx, store.ProjectInput{Attribution: store.Attribution{Actor: "alice", Via: "test"}, Name: "Demo"})
	must(err)
	_, err = st.CreateActor(ctx, store.ActorInput{Slug: "claude", DisplayName: "Claude"})
	must(err)

	page := func(path string) string {
		t.Helper()
		rec := do(t, h, req{method: "GET", path: path, cookie: cookie})
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	if body := page("/p/demo/updates"); !strings.Contains(body, "No updates yet") || !strings.Contains(body, `action="/p/demo/updates"`) {
		t.Fatal("empty updates page")
	}
	if body := page("/p/demo"); !strings.Contains(body, "Latest update") || !strings.Contains(body, "No updates yet") {
		t.Fatal("project page without updates")
	}

	// A person publishes; the signed-in user is the author, whatever the form says.
	form := url.Values{"title": {`<script>alert(1)</script> is gone`}, "summary": {"First paragraph.\n\nSecond <b>paragraph</b>."},
		"category": {"fixed"}, "task_reference": {"Task 3"}, "actor": {"claude"}}
	rec := do(t, h, req{method: "POST", path: "/p/demo/updates", form: form, cookie: cookie, headers: same})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/p/demo/updates?msg=") {
		t.Fatalf("publish: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	u, err := st.LatestUpdate(ctx, "demo")
	must(err)
	if u.Actor.Slug != "alice" || u.TaskReference != "Task 3" {
		t.Fatalf("human attribution: %+v", u)
	}
	if rec := do(t, h, req{method: "POST", path: "/p/demo/updates", form: form, cookie: cookie, headers: map[string]string{"Sec-Fetch-Site": "cross-site"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site publish: %d", rec.Code)
	}
	badForm := url.Values{"title": {"T"}, "summary": {"S"}, "category": {"release"}}
	if rec := do(t, h, req{method: "POST", path: "/p/demo/updates", form: badForm, cookie: cookie, headers: same}); !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("invalid category should come back as an error: %q", rec.Header().Get("Location"))
	}

	body := page("/p/demo/updates")
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt; is gone", "<p>First paragraph.</p>", "<p>Second &lt;b&gt;paragraph&lt;/b&gt;.</p>",
		`class="day-head"`, "Technical details", "Task 3", `aria-current="page" href="/p/demo/updates"`} {
		if !strings.Contains(body, want) {
			t.Errorf("updates page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<b>paragraph") {
		t.Fatal("update text was not escaped")
	}
	if body := page("/p/demo"); !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt; is gone") || !strings.Contains(body, "View all updates") {
		t.Fatal("project page should show the latest update")
	}

	// An agent's update appears under Latest agent work and in its overview cell; a person's does not count as agent work.
	_, err = st.AddUpdate(ctx, "demo", store.UpdateInput{Attribution: store.Attribution{Actor: "claude", Via: "mcp"},
		UpdateContent: store.UpdateContent{Title: "Mobile navigation now works", Summary: "The menu opens on phones.", Category: "fixed"}})
	must(err)
	overview := page("/")
	work := overview[strings.Index(overview, "Latest agent work"):]
	if !strings.Contains(work, "Mobile navigation now works") || strings.Contains(work, "is gone") {
		t.Fatal("Latest agent work should list agent updates only")
	}
	if matrix := overview[:strings.Index(overview, "Latest agent work")]; !strings.Contains(matrix, `<div class="cell-text">Mobile navigation now works</div>`) {
		t.Fatal("the agent's overview cell should show its latest update")
	}
}
