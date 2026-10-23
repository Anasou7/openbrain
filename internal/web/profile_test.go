package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"openbrain/internal/store"
)

func TestProfileAPI(t *testing.T) {
	h, st := newServer(t)
	api(t, h, "POST", "/api/actors", map[string]any{"slug": "claude"}, 201)
	api(t, h, "POST", "/api/projects", map[string]any{"actor": "user", "name": "Demo"}, 201)
	_, secret, err := st.CreateToken(context.Background(), "claude", "test")
	if err != nil {
		t.Fatal(err)
	}

	empty := api(t, h, "GET", "/api/projects/demo/profile", nil, 200)
	if c := empty["completeness"].(map[string]any); len(c["missing"].([]any)) != len(store.CanonicalSections) {
		t.Fatalf("empty profile: %v", c)
	}
	body := map[string]any{
		"source_commit": "a81c9e3",
		"sections": []any{
			map[string]any{"key": "overview", "content": "Shared memory for agents."},
			map[string]any{"key": "pricing", "status": "unknown"},
		},
		"features": []any{map[string]any{"name": "Handoffs", "summary": "Agents record where they stopped.", "use_cases": []any{"Resume work"}}},
	}
	if rec := do(t, h, req{method: "PATCH", path: "/api/projects/demo/profile", body: body, cookie: &http.Cookie{Name: sessionCookie, Value: "x"}}); rec.Code != 401 {
		t.Fatalf("session cookie must not write the API: %d", rec.Code)
	}
	if got := api(t, h, "PATCH", "/api/projects/demo/profile", body, 400); errCode(got) != "invalid" {
		t.Fatalf("admin token without actor: %v", got)
	}
	impersonate := map[string]any{"actor": "user", "sections": []any{map[string]any{"key": "faq", "content": "x"}}}
	apiAs(t, h, secret, "PATCH", "/api/projects/demo/profile", impersonate, 403)
	r := apiAs(t, h, secret, "PATCH", "/api/projects/demo/profile", body, 200)
	if len(r["sections"].(map[string]any)["created"].([]any)) != 2 || len(r["features"].(map[string]any)["created"].([]any)) != 1 {
		t.Fatalf("sync result: %v", r)
	}

	sec := api(t, h, "GET", "/api/projects/demo/profile/overview", nil, 200)
	if sec["updated_by"].(map[string]any)["slug"] != "claude" || sec["source_commit"] != "a81c9e3" {
		t.Fatalf("section: %v", sec)
	}
	api(t, h, "GET", "/api/projects/demo/profile/faq", nil, 404)
	put := apiAs(t, h, secret, "PUT", "/api/projects/demo/profile/faq", map[string]any{"content": "Q: Is it hosted? A: No, self-hosted."}, 200)
	if put["title"] != "FAQ" {
		t.Fatalf("put section: %v", put)
	}
	apiAs(t, h, secret, "PUT", "/api/projects/demo/profile/faq", map[string]any{"key": "other", "content": "x"}, 400)
	apiAs(t, h, secret, "DELETE", "/api/projects/demo/profile/faq", nil, 200)
	api(t, h, "GET", "/api/projects/demo/profile/faq", nil, 404)
	api(t, h, "DELETE", "/api/projects/demo/profile/pricing?actor=user", nil, 200)

	features := api(t, h, "GET", "/api/projects/demo/features", nil, 200)["features"].([]any)
	if len(features) != 1 {
		t.Fatalf("features: %v", features)
	}
	id := int(features[0].(map[string]any)["id"].(float64))
	apiAs(t, h, secret, "POST", "/api/projects/demo/features", map[string]any{"name": "Handoffs", "summary": "dup"}, 409)
	apiAs(t, h, secret, "POST", "/api/projects/demo/features", map[string]any{"name": "Updates", "summary": "What changed."}, 201)
	dep := apiAs(t, h, secret, "PATCH", fmt.Sprintf("/api/features/%d", id), map[string]any{"status": "removed"}, 200)
	if dep["status"] != "removed" {
		t.Fatalf("patch feature: %v", dep)
	}
	if n := len(api(t, h, "GET", "/api/projects/demo/features", nil, 200)["features"].([]any)); n != 1 {
		t.Fatalf("removed feature listed: %d", n)
	}
	if n := len(api(t, h, "GET", "/api/projects/demo/features?include_removed=true", nil, 200)["features"].([]any)); n != 2 {
		t.Fatalf("include_removed: %d", n)
	}
	api(t, h, "GET", "/api/projects/demo/features/handoffs", nil, 200)
}

func TestProfileDashboard(t *testing.T) {
	h, st := newServer(t)
	ctx := context.Background()
	cookie := login(t, h)
	if _, err := st.CreateProject(ctx, store.ProjectInput{Attribution: store.Attribution{Actor: "alice", Via: "test"}, Name: "Demo"}); err != nil {
		t.Fatal(err)
	}
	post := func(path string, form url.Values) string {
		t.Helper()
		rec := do(t, h, req{method: "POST", path: path, form: form, cookie: cookie, headers: same})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("POST %s: %d", path, rec.Code)
		}
		return rec.Header().Get("Location")
	}
	page := func(path string) string {
		t.Helper()
		rec := do(t, h, req{method: "GET", path: path, cookie: cookie})
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	if rec := do(t, h, req{method: "GET", path: "/p/demo/profile"}); rec.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous profile view: %d", rec.Code)
	}
	if body := page("/p/demo/profile"); !strings.Contains(body, "0 of 24 sections known") || !strings.Contains(body, `aria-current="page" href="/p/demo/profile"`) {
		t.Fatal("empty profile page")
	}
	if body := page("/p/demo"); !strings.Contains(body, "No profile yet") {
		t.Fatal("project page without a profile")
	}

	loc := post("/p/demo/profile", url.Values{"key": {"overview"}, "status": {"known"}, "actor": {"claude"},
		"content": {"Shared memory <script>alert(1)</script> for agents.\n\nSecond paragraph."}})
	if !strings.HasSuffix(loc, "#s-overview") {
		t.Fatalf("redirect: %q", loc)
	}
	sec, err := st.Section(ctx, "demo", "overview")
	if err != nil || sec.UpdatedBy.Slug != "alice" {
		t.Fatalf("human attribution: %+v %v", sec, err)
	}
	post("/p/demo/profile/icp", url.Values{"title": {""}, "status": {"known"}, "content": {"Small teams with several AI agents."}})
	post("/p/demo/profile", url.Values{"key": {"pricing"}, "status": {"unknown"}, "content": {""}})
	if loc := post("/p/demo/profile", url.Values{"key": {"faq"}, "status": {"known"}, "content": {""}}); !strings.Contains(loc, "err=") {
		t.Fatalf("known section without content should fail: %q", loc)
	}
	post("/p/demo/profile", url.Values{"key": {"release_process"}, "content": {"Tagged releases."}})
	if loc := post("/p/demo/profile/release_process/remove", url.Values{}); !strings.Contains(loc, "err=") {
		t.Fatalf("removal without confirmation: %q", loc)
	}
	post("/p/demo/profile/release_process/remove", url.Values{"confirm": {"yes"}})
	if _, err := st.Section(ctx, "demo", "release_process"); err == nil {
		t.Fatal("section not removed")
	}

	loc = post("/p/demo/features", url.Values{"name": {"Team <b>Workspaces</b>"}, "summary": {"Manage several clients."}, "status": {"available"},
		"target_users": {"Agencies"}, "use_cases": {"Separate clients\r\nRun several products\r\n"}, "limitations": {"No billing per workspace."}})
	if !strings.Contains(loc, "#f-team-b-workspaces-b") {
		t.Fatalf("feature redirect: %q", loc)
	}
	post("/p/demo/features/team-b-workspaces-b", url.Values{"name": {"Team Workspaces"}, "summary": {"Manage several clients."}, "status": {"deprecated"},
		"description": {""}, "target_users": {"Agencies"}, "use_cases": {"Separate clients"}, "limitations": {""}})
	f, err := st.Feature(ctx, "demo", "team-b-workspaces-b")
	if err != nil || f.Status != "deprecated" || f.CreatedBy.Slug != "alice" || len(f.UseCases) != 1 {
		t.Fatalf("feature edits: %+v %v", f, err)
	}
	if rec := do(t, h, req{method: "POST", path: "/p/demo/profile", form: url.Values{"key": {"x"}, "content": {"y"}}, cookie: cookie,
		headers: map[string]string{"Sec-Fetch-Site": "cross-site"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site edit: %d", rec.Code)
	}

	body := page("/p/demo/profile")
	for _, want := range []string{"2 of 24 sections known", "<p>Shared memory &lt;script&gt;alert(1)&lt;/script&gt; for agents.</p>", "<p>Second paragraph.</p>",
		"Ideal customer profile", "Not established yet", "Team Workspaces", "deprecated", "Best for", "<li>Separate clients</li>",
		"Updated by", `<a href="#s-overview">`, `<option value="faq">FAQ</option>`} {
		if !strings.Contains(body, want) {
			t.Errorf("profile page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatal("profile content was not escaped")
	}
	if body := page("/p/demo"); !strings.Contains(body, "Product snapshot") || !strings.Contains(body, "Small teams with several AI agents.") ||
		!strings.Contains(body, "Shared memory &lt;script&gt;") || strings.Contains(body, "Second paragraph") {
		t.Fatal("project page snapshot")
	}
}
