package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"openbrain/internal/mcpserver"
	"openbrain/internal/store"
)

var ctx = context.Background()

type env struct {
	url    string
	st     *store.Store
	tokens map[string]string // actor slug -> secret
}

func setup(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, tokens: map[string]string{}}
	for _, slug := range []string{"claude", "codex"} {
		if _, err := st.CreateActor(ctx, store.ActorInput{Slug: slug}); err != nil {
			t.Fatal(err)
		}
		_, secret, err := st.CreateToken(ctx, slug, "test")
		if err != nil {
			t.Fatal(err)
		}
		e.tokens[slug] = secret
	}
	srv := httptest.NewServer(mcpserver.New(st, "test", slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

// bearer adds the Authorization header and records protocol versions used.
type bearer struct {
	token    string
	mu       sync.Mutex
	versions map[string]bool
}

func (b *bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	b.mu.Lock()
	if b.versions == nil {
		b.versions = map[string]bool{}
	}
	b.versions[r.Header.Get("MCP-Protocol-Version")] = true
	b.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, *bearer, error) {
	t.Helper()
	b := &bearer{token: token}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: b}}, nil)
	if err == nil {
		t.Cleanup(func() { cs.Close() })
	}
	return cs, b, err
}

func session(t *testing.T, e *env, actor string) *mcp.ClientSession {
	t.Helper()
	cs, _, err := connect(t, e.url, e.tokens[actor])
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// call invokes a tool and decodes its structured output. A tool-level error
// fails the test unless wantErr is set.
func call[T any](t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) T {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned an error: %s", name, text(res))
	}
	var out T
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: decoding structured output: %v", name, err)
	}
	return out
}

// callErr expects the call to fail, as a protocol error or a tool error, and
// returns the message.
func callErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("%s(%v) should have failed, got %s", name, args, text(res))
	}
	return text(res)
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

var allTools = []string{
	"add_note", "find_project", "get_overview", "get_project_context", "get_project_history", "get_project_profile", "get_project_updates",
	"list_projects", "publish_project_update", "raise_blocker",
	"record_decision", "record_handoff", "register_project", "resolve_blocker", "set_agent_state", "sync_project_profile", "update_project",
	"update_project_knowledge", "upsert_project_feature",
}

func TestDiscovery(t *testing.T) {
	e := setup(t)
	cs, b, err := connect(t, e.url, e.tokens["claude"])
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, allTools) {
		t.Fatalf("tools:\n got %v\nwant %v", names, allTools)
	}
	if !b.versions["2026-07-28"] {
		t.Errorf("SDK client should negotiate the stateless 2026-07-28 protocol; saw %v", b.versions)
	}

	readOnly := map[string]bool{"list_projects": true, "find_project": true, "get_project_context": true, "get_overview": true,
		"get_project_profile": true, "get_project_updates": true, "get_project_history": true}
	additive := map[string]bool{"register_project": true, "add_note": true, "record_handoff": true, "record_decision": true, "raise_blocker": true, "publish_project_update": true}
	required := map[string][]string{
		"get_project_context": {"project"}, "register_project": {"name"}, "update_project": {"project"},
		"add_note": {"content", "project"}, "record_handoff": {"project", "summary"}, "set_agent_state": {"project", "state"},
		"record_decision": {"decision", "project"}, "raise_blocker": {"description", "project"}, "resolve_blocker": {"blocker_id"},
		"publish_project_update": {"category", "project", "summary", "title"},
		"get_project_profile":    {"project"}, "sync_project_profile": {"project"}, "update_project_knowledge": {"project", "section_key"},
		"upsert_project_feature": {"project"}, "get_project_updates": {"project"}, "get_project_history": {"kind", "project"},
	}
	for _, tool := range res.Tools {
		schema := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if schema["type"] != "object" || tool.OutputSchema == nil || len(tool.Description) < 80 {
			t.Errorf("%s: incomplete definition", tool.Name)
		}
		for p := range props {
			if p == "actor" || p == "author" || p == "actor_id" {
				t.Errorf("%s exposes an %q argument; identity must come from the token", tool.Name, p)
			}
		}
		var req []string
		rs, _ := schema["required"].([]any) // absent when nothing is required
		for _, r := range rs {
			req = append(req, r.(string))
		}
		slices.Sort(req)
		if want := required[tool.Name]; want != nil && !slices.Equal(req, want) {
			t.Errorf("%s required = %v, want %v", tool.Name, req, want)
		}
		a := tool.Annotations
		switch {
		case a == nil:
			t.Errorf("%s has no annotations", tool.Name)
		case readOnly[tool.Name] != a.ReadOnlyHint:
			t.Errorf("%s readOnlyHint = %v", tool.Name, a.ReadOnlyHint)
		case !a.ReadOnlyHint && (a.DestructiveHint == nil || *a.DestructiveHint == additive[tool.Name]):
			t.Errorf("%s destructiveHint wrong", tool.Name)
		case a.OpenWorldHint == nil || *a.OpenWorldHint:
			t.Errorf("%s should be closed-world", tool.Name)
		}
	}
	state := find(res.Tools, "set_agent_state").InputSchema.(map[string]any)["properties"].(map[string]any)["state"].(map[string]any)
	if fmt.Sprint(state["enum"]) != "[idle working blocked waiting finished]" {
		t.Errorf("state enum: %v", state["enum"])
	}
	category := find(res.Tools, "publish_project_update").InputSchema.(map[string]any)["properties"].(map[string]any)["category"].(map[string]any)
	nested := find(res.Tools, "record_handoff").InputSchema.(map[string]any)["properties"].(map[string]any)["user_update"].(map[string]any)["properties"].(map[string]any)["category"].(map[string]any)
	for _, c := range []map[string]any{category, nested} {
		if fmt.Sprint(c["enum"]) != "[new improved fixed changed important]" {
			t.Errorf("category enum: %v", c["enum"])
		}
	}

	// Definitions are deterministic across sessions.
	again, _ := session(t, e, "codex").ListTools(ctx, nil)
	a, _ := json.Marshal(res.Tools)
	c, _ := json.Marshal(again.Tools)
	if !bytes.Equal(a, c) {
		t.Error("tool definitions differ between sessions")
	}
}

func TestProjectUpdates(t *testing.T) {
	e := setup(t)
	claude, codex := session(t, e, "claude"), session(t, e, "codex")
	call[map[string]any](t, claude, "register_project", map[string]any{"name": "Demo"})

	u := call[store.ProjectUpdate](t, claude, "publish_project_update", map[string]any{
		"project": "demo", "title": "Login is now faster", "summary": "The delay noticed during task 3 is fixed.",
		"category": "fixed", "task_reference": "Task 3", "idempotency_key": "u1",
	})
	if u.Actor.Slug != "claude" || u.Title != "Login is now faster" || u.TaskReference != "Task 3" {
		t.Fatalf("published: %+v", u)
	}
	again := call[store.ProjectUpdate](t, claude, "publish_project_update", map[string]any{
		"project": "demo", "title": "Login is now faster", "summary": "The delay noticed during task 3 is fixed.",
		"category": "fixed", "idempotency_key": "u1",
	})
	if again.ID != u.ID {
		t.Fatalf("idempotent retry created update %d (first %d)", again.ID, u.ID)
	}

	base := map[string]any{"project": "demo", "title": "T", "summary": "S", "category": "new"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	callErr(t, codex, "publish_project_update", with("actor", "claude"))
	callErr(t, codex, "publish_project_update", with("category", "release"))
	if msg := callErr(t, codex, "publish_project_update", with("project", "nope")); !strings.Contains(msg, "not found") {
		t.Errorf("unknown project: %s", msg)
	}
	callErr(t, codex, "publish_project_update", map[string]any{"project": "demo", "title": "Missing summary", "category": "new"})
	if all, _ := e.st.Updates(ctx, "demo", store.UpdateFilter{}); len(all) != 1 {
		t.Fatalf("rejected calls must not create updates: %d", len(all))
	}

	// record_handoff: user_update is optional, attributed to the caller, and linked.
	plain := call[store.Handoff](t, codex, "record_handoff", map[string]any{"project": "demo", "summary": "Refactored the cache."})
	if plain.Update != nil {
		t.Fatalf("handoff without user_update created %+v", plain.Update)
	}
	h := call[store.Handoff](t, codex, "record_handoff", map[string]any{
		"project": "demo", "summary": "Fixed the nav collapse bug; tests pass.", "branch": "main", "commit_sha": "e38a210", "set_state": "finished",
		"user_update": map[string]any{"title": "Mobile navigation now works", "summary": "The menu opens and closes correctly on phones.", "category": "fixed"},
	})
	if h.Update == nil || h.Update.HandoffID != h.ID || h.Update.Actor.Slug != "codex" || h.Update.CommitSHA != "e38a210" {
		t.Fatalf("handoff update: %+v", h.Update)
	}
	callErr(t, codex, "record_handoff", map[string]any{"project": "demo", "summary": "x",
		"user_update": map[string]any{"title": "T", "summary": "S", "category": "new", "actor": "claude"}})
	callErr(t, codex, "record_handoff", map[string]any{"project": "demo", "summary": "x",
		"user_update": map[string]any{"title": "T", "summary": "S", "category": "minor"}})

	c := call[store.ProjectContext](t, claude, "get_project_context", map[string]any{"project": "demo"})
	if len(c.RecentUpdates) != 2 || c.RecentUpdates[0].Title != "Mobile navigation now works" {
		t.Fatalf("context updates: %+v", c.RecentUpdates)
	}
	type overview struct {
		RecentUpdates []struct{ Project, Actor, Title string } `json:"recent_updates"`
		Projects      []struct {
			LatestUpdate *struct{ Title string } `json:"latest_update"`
			Actors       map[string]struct {
				LastUpdate string `json:"last_update"`
			} `json:"actors"`
		} `json:"projects"`
	}
	o := call[overview](t, claude, "get_overview", nil)
	switch {
	case len(o.RecentUpdates) != 2 || o.RecentUpdates[0].Actor != "codex" || o.RecentUpdates[0].Project != "demo":
		t.Fatalf("recent updates: %+v", o.RecentUpdates)
	case o.Projects[0].LatestUpdate == nil || o.Projects[0].LatestUpdate.Title != "Mobile navigation now works":
		t.Fatalf("latest update: %+v", o.Projects[0].LatestUpdate)
	case o.Projects[0].Actors["claude"].LastUpdate != "Login is now faster":
		t.Fatalf("claude's cell: %+v", o.Projects[0].Actors["claude"])
	}
}

func find(tools []*mcp.Tool, name string) *mcp.Tool {
	for _, t := range tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

type summary struct {
	ID     int64  `json:"id"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
}

func TestWorkflowAndAttribution(t *testing.T) {
	e := setup(t)
	claude, codex := session(t, e, "claude"), session(t, e, "codex")

	reg := call[struct {
		Created bool          `json:"created"`
		Project store.Project `json:"project"`
	}](t, claude, "register_project", map[string]any{"name": "Widget", "repo_url": "git@github.com:acme/widget.git", "current_focus": "auth"})
	if !reg.Created || reg.Project.Slug != "widget" {
		t.Fatalf("register: %+v", reg)
	}
	dup := call[struct {
		Created bool          `json:"created"`
		Project store.Project `json:"project"`
	}](t, codex, "register_project", map[string]any{"name": "Widget again", "repo_url": "https://github.com/Acme/widget"})
	if dup.Created || dup.Project.ID != reg.Project.ID {
		t.Fatalf("duplicate register: %+v", dup)
	}

	type found struct {
		Status     string    `json:"status"`
		Project    *summary  `json:"project"`
		Candidates []summary `json:"candidates"`
	}
	if f := call[found](t, claude, "find_project", map[string]any{"repo_url": "https://github.com/acme/widget.git"}); f.Status != "found" || f.Project.Slug != "widget" {
		t.Fatalf("find by repo: %+v", f)
	}
	if f := call[found](t, claude, "find_project", map[string]any{"project": "widget"}); f.Status != "found" {
		t.Fatalf("find by slug: %+v", f)
	}
	if f := call[found](t, claude, "find_project", map[string]any{"repo_url": "git@github.com:acme/none.git"}); f.Status != "not_found" {
		t.Fatalf("find unknown repo: %+v", f)
	}

	p := call[store.Project](t, claude, "update_project", map[string]any{"project": "widget", "status": "paused", "current_focus": "tests"})
	if p.Status != "paused" || p.CurrentFocus != "tests" {
		t.Fatalf("update: %+v", p)
	}
	n := call[store.Note](t, claude, "add_note", map[string]any{"project": "widget", "content": "CI needs Go 1.25", "category": "ops", "pinned": true})
	if n.Actor.Slug != "claude" || n.Kind != "ops" || !n.Pinned {
		t.Fatalf("note: %+v", n)
	}
	h := call[store.Handoff](t, claude, "record_handoff", map[string]any{
		"project": "widget", "summary": "Auth API done", "next_steps": "UI", "branch": "feature/auth",
		"commit_sha": "9AD281C", "metadata": map[string]any{"tests": 42}, "set_state": "idle",
	})
	if h.Actor.Slug != "claude" || h.CommitSHA != "9ad281c" || string(h.Metadata) != `{"tests":42}` {
		t.Fatalf("handoff: %+v", h)
	}
	s := call[store.ActorState](t, codex, "set_agent_state", map[string]any{"project": "widget", "state": "working", "current_task": "login form"})
	if s.Actor.Slug != "codex" || s.State != "working" {
		t.Fatalf("state: %+v", s)
	}
	d := call[store.Decision](t, codex, "record_decision", map[string]any{"project": "widget", "decision": "Server-side sessions", "reasoning": "simpler revocation"})
	b := call[store.Blocker](t, codex, "raise_blocker", map[string]any{"project": "widget", "description": "Need design copy", "context": "Error states undefined"})
	if d.Actor.Slug != "codex" || b.Actor.Slug != "codex" || !strings.Contains(b.Description, "Context: Error states undefined") {
		t.Fatalf("decision/blocker: %+v %+v", d, b)
	}
	r := call[store.Blocker](t, claude, "resolve_blocker", map[string]any{"blocker_id": b.ID, "resolution": "copy received"})
	if !r.Resolved || r.ResolvedBy.Slug != "claude" {
		t.Fatalf("resolve: %+v", r)
	}
	if r2 := call[store.Blocker](t, codex, "resolve_blocker", map[string]any{"blocker_id": b.ID, "resolution": "other"}); r2.ResolvedBy.Slug != "claude" || r2.Resolution != "copy received" {
		t.Fatalf("resolving twice must not change the resolver: %+v", r2)
	}

	c := call[store.ProjectContext](t, codex, "get_project_context", map[string]any{"project": "widget"})
	if !c.Compact || len(c.LatestHandoffs) != 1 || len(c.ActorStates) != 2 || len(c.PinnedNotes) != 1 || len(c.OpenBlockers) != 0 || len(c.RecentDecisions) != 1 {
		t.Fatalf("context: compact=%v handoffs=%d states=%d pinned=%d blockers=%d decisions=%d",
			c.Compact, len(c.LatestHandoffs), len(c.ActorStates), len(c.PinnedNotes), len(c.OpenBlockers), len(c.RecentDecisions))
	}
	if full := call[store.ProjectContext](t, codex, "get_project_context", map[string]any{"project": "widget", "compact": false, "activity_limit": 3}); full.Compact || len(full.RecentActivity) != 3 {
		t.Fatalf("full context: compact=%v activity=%d", full.Compact, len(full.RecentActivity))
	}

	list := call[struct {
		Projects []summary `json:"projects"`
	}](t, claude, "list_projects", map[string]any{})
	if len(list.Projects) != 1 || list.Projects[0].Status != "paused" {
		t.Fatalf("list: %+v", list)
	}
	if active := call[struct {
		Projects []summary `json:"projects"`
	}](t, claude, "list_projects", map[string]any{"status": "active"}); len(active.Projects) != 0 {
		t.Fatalf("status filter: %+v", active)
	}

	o := call[struct {
		Projects []struct {
			Actors map[string]struct {
				State       string `json:"state"`
				LastHandoff string `json:"last_handoff"`
			} `json:"actors"`
		} `json:"projects"`
	}](t, codex, "get_overview", map[string]any{})
	if len(o.Projects) != 1 || o.Projects[0].Actors["claude"].LastHandoff != "Auth API done" || o.Projects[0].Actors["codex"].State != "working" {
		t.Fatalf("overview: %+v", o)
	}

	// Every write is attributed to the token's actor and marked as MCP.
	want := map[string]string{
		"project.created": "claude", "project.status_changed": "claude", "project.focus_changed": "claude",
		"note.added": "claude", "handoff.created": "claude", "decision.recorded": "codex",
		"blocker.created": "codex", "blocker.resolved": "claude",
	}
	activity, _ := e.st.Activity(ctx, "widget", 0, 100)
	for _, a := range activity {
		if a.Via != "mcp" {
			t.Errorf("%s via %q, want mcp", a.Kind, a.Via)
		}
		if w, ok := want[a.Kind]; ok && a.Actor.Slug != w {
			t.Errorf("%s by %s, want %s", a.Kind, a.Actor.Slug, w)
		}
	}

	// Retrying with an idempotency key does not duplicate.
	args := map[string]any{"project": "widget", "content": "retry me", "idempotency_key": "4f1c"}
	if a, b := call[store.Note](t, claude, "add_note", args), call[store.Note](t, claude, "add_note", args); a.ID != b.ID {
		t.Fatal("retried add_note created a duplicate")
	}
}

func TestToolErrors(t *testing.T) {
	e := setup(t)
	cs := session(t, e, "claude")
	call[map[string]any](t, cs, "register_project", map[string]any{"name": "Widget", "repo_url": "https://github.com/acme/widget"})
	if _, err := e.st.CreateProject(ctx, store.ProjectInput{Attribution: store.Attribution{Actor: "user"}, Name: "Widget docs", RepoURL: "git@github.com:acme/widget.git"}); err != nil {
		t.Fatal(err)
	}

	amb := call[struct {
		Status     string    `json:"status"`
		Candidates []summary `json:"candidates"`
	}](t, cs, "find_project", map[string]any{"repo_url": "git@github.com:acme/widget"})
	if amb.Status != "ambiguous" || len(amb.Candidates) != 2 {
		t.Fatalf("ambiguous lookup: %+v", amb)
	}

	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_project_context", map[string]any{"project": "nope"}, "not found"},
		{"add_note", map[string]any{"project": "nope", "content": "x"}, "not found"},
		{"resolve_blocker", map[string]any{"blocker_id": 999}, "not found"},
		{"find_project", map[string]any{}, "pass project"},
		{"record_handoff", map[string]any{"project": "widget"}, "summary"},
		{"record_handoff", map[string]any{"project": "widget", "summary": "x", "commit_sha": "zzz"}, "commit_sha"},
		{"set_agent_state", map[string]any{"project": "widget", "state": "sleeping"}, "state"},
		{"add_note", map[string]any{"project": "widget", "content": "   "}, "content"},
		{"update_project", map[string]any{"project": "widget"}, "at least one"},
		{"register_project", map[string]any{"name": "Leaky", "repo_url": "https://ghp_x@github.com/acme/leaky"}, "credentials"},
		{"add_note", map[string]any{"project": "widget", "content": "x", "actor": "codex"}, ""},
		{"no_such_tool", map[string]any{}, ""},
	} {
		msg := callErr(t, cs, c.tool, c.args)
		if c.want != "" && !strings.Contains(msg, c.want) {
			t.Errorf("%s(%v): error %q should mention %q", c.tool, c.args, msg, c.want)
		}
	}
	notes, _ := e.st.Notes(ctx, "widget", store.NoteFilter{})
	if len(notes) != 0 {
		t.Fatalf("rejected calls wrote %d notes", len(notes))
	}
}

func post(t *testing.T, url, token string, headers map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"legacy","version":"0"}}}`

func TestAuthentication(t *testing.T) {
	e := setup(t)
	body := fmt.Sprintf(initialize, "2025-06-18")
	for name, tok := range map[string]string{"none": "", "wrong": "pbt_wrong", "admin-style": "0123456789abcdef0123456789abcdef"} {
		res, _ := post(t, e.url, tok, nil, body)
		if res.StatusCode != 401 || !strings.Contains(res.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s token: status %d, WWW-Authenticate %q", name, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
	}
	if _, _, err := connect(t, e.url, ""); err == nil {
		t.Error("SDK client connected without a token")
	}

	tokens, _ := e.st.Tokens(ctx, false)
	for _, tk := range tokens {
		if tk.Actor.Slug == "codex" {
			e.st.RevokeToken(ctx, tk.ID)
		}
	}
	if res, _ := post(t, e.url, e.tokens["codex"], nil, body); res.StatusCode != 401 {
		t.Errorf("revoked token: %d", res.StatusCode)
	}

	res, _ := post(t, e.url, e.tokens["claude"], map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, body)
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site browser request: %d", res.StatusCode)
	}
	get, _ := http.NewRequest("GET", e.url, nil)
	get.Header.Set("Authorization", "Bearer "+e.tokens["claude"])
	if res, err := http.DefaultClient.Do(get); err != nil || res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on a stateless endpoint should be 405: %v", res.StatusCode)
	}
}

func TestLegacyClients(t *testing.T) {
	e := setup(t)
	tok := e.tokens["claude"]
	for _, version := range []string{"2025-11-25", "2025-06-18", "2025-03-26"} {
		res, body := post(t, e.url, tok, nil, fmt.Sprintf(initialize, version))
		var init struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
				ServerInfo      struct{ Name string }
				Instructions    string
			}
		}
		json.Unmarshal([]byte(body), &init)
		if res.StatusCode != 200 || init.Result.ProtocolVersion != version || init.Result.ServerInfo.Name != "openbrain" || init.Result.Instructions == "" {
			t.Fatalf("initialize %s: %d %s", version, res.StatusCode, body)
		}
		h := map[string]string{"MCP-Protocol-Version": version}
		post(t, e.url, tok, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		_, body = post(t, e.url, tok, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		if strings.Count(body, `"name":"`) < len(allTools) || !strings.Contains(body, "record_handoff") {
			t.Fatalf("tools/list on %s: %s", version, body)
		}
	}

	h := map[string]string{"MCP-Protocol-Version": "2025-06-18"}
	_, body := post(t, e.url, tok, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"register_project","arguments":{"name":"Legacy"}}}`)
	if !strings.Contains(body, `"created":true`) {
		t.Fatalf("legacy tools/call: %s", body)
	}
	if a, _ := e.st.Activity(ctx, "legacy", 0, 1); len(a) != 1 || a[0].Actor.Slug != "claude" || a[0].Via != "mcp" {
		t.Fatalf("legacy write attribution: %+v", a)
	}

	res, body := post(t, e.url, tok, h, `{"jsonrpc":"2.0","id":4,"method":`)
	if res.StatusCode < 400 && !strings.Contains(body, "-32700") {
		t.Fatalf("malformed JSON: %d %s", res.StatusCode, body)
	}
	res, body = post(t, e.url, tok, h, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"add_note","arguments":"not an object"}}`)
	if res.StatusCode < 400 && !strings.Contains(body, `"error"`) && !strings.Contains(body, `"isError":true`) {
		t.Fatalf("malformed arguments: %d %s", res.StatusCode, body)
	}
}

func TestConcurrentMCPWrites(t *testing.T) {
	e := setup(t)
	call[map[string]any](t, session(t, e, "claude"), "register_project", map[string]any{"name": "Busy"})
	var wg sync.WaitGroup
	errs := make(chan string, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cs, _, err := connect(t, e.url, e.tokens[[]string{"claude", "codex"}[i%2]])
			if err != nil {
				errs <- err.Error()
				return
			}
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "add_note", Arguments: map[string]any{"project": "busy", "content": fmt.Sprint("note ", i)}})
			if err != nil {
				errs <- err.Error()
			} else if res.IsError {
				errs <- text(res)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
	if notes, _ := e.st.Notes(ctx, "busy", store.NoteFilter{Limit: 100}); len(notes) != 40 {
		t.Fatalf("notes after concurrent MCP writes: %d", len(notes))
	}
}
