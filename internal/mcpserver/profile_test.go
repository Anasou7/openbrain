package mcpserver_test

import (
	"fmt"
	"strings"
	"testing"

	"openbrain/internal/store"
)

func TestProjectProfile(t *testing.T) {
	e := setup(t)
	claude, codex := session(t, e, "claude"), session(t, e, "codex")
	call[map[string]any](t, claude, "register_project", map[string]any{"name": "Demo"})

	sync := call[store.SyncResult](t, claude, "sync_project_profile", map[string]any{
		"project": "demo", "source_branch": "main", "source_commit": "a81c9e3",
		"sections": []any{
			map[string]any{"key": "overview", "content": "Shared memory for people and AI agents."},
			map[string]any{"key": "icp", "content": "Small teams running several AI coding agents."},
			map[string]any{"key": "pricing", "status": "unknown", "content": "Not in the repository or docs."},
			map[string]any{"key": "release_process", "content": "Tagged releases."},
		},
		"features": []any{
			map[string]any{"name": "Handoffs", "summary": "Agents record where they stopped.", "use_cases": []any{"Resume work", "Switch agents"}},
			map[string]any{"name": "Project updates", "summary": "Plain-English notes of what changed.", "status": "available"},
		},
	})
	if len(sync.Sections.Created) != 4 || len(sync.Features.Created) != 2 || len(sync.Completeness.Unknown) != 1 {
		t.Fatalf("sync: %+v", sync)
	}

	// Identity comes from the token, even inside nested items.
	callErr(t, codex, "sync_project_profile", map[string]any{"project": "demo", "actor": "claude",
		"sections": []any{map[string]any{"key": "faq", "content": "x"}}})
	callErr(t, codex, "sync_project_profile", map[string]any{"project": "demo",
		"sections": []any{map[string]any{"key": "faq", "content": "x", "actor": "claude"}}})
	callErr(t, codex, "sync_project_profile", map[string]any{"project": "demo",
		"features": []any{map[string]any{"name": "X", "summary": "S", "status": "shipped"}}})
	callErr(t, codex, "update_project_knowledge", map[string]any{"project": "demo", "section_key": "faq", "content": "x", "actor": "claude"})

	full := call[store.Profile](t, codex, "get_project_profile", map[string]any{"project": "demo"})
	if len(full.Sections) != 4 || len(full.Features) != 2 || full.Sections[0].Key != "overview" || full.Sections[3].Key != "release_process" ||
		full.Sections[0].UpdatedBy.Slug != "claude" || full.Sections[0].SourceCommit != "a81c9e3" || len(full.Features[0].UseCases) != 2 {
		t.Fatalf("full profile: %+v", full)
	}
	one := call[store.Profile](t, codex, "get_project_profile", map[string]any{"project": "demo", "section": "icp"})
	if len(one.Sections) != 1 || one.Sections[0].Key != "icp" || len(one.Features) != 0 {
		t.Fatalf("section mode: %+v", one)
	}
	if msg := callErr(t, codex, "get_project_profile", map[string]any{"project": "demo", "section": "integrations"}); !strings.Contains(msg, "no verified information") {
		t.Errorf("missing section: %s", msg)
	}
	feats := call[store.Profile](t, codex, "get_project_profile", map[string]any{"project": "demo", "include_sections": false})
	if len(feats.Sections) != 0 || len(feats.Features) != 2 {
		t.Fatalf("features only: %+v", feats)
	}

	sec := call[store.ProfileSection](t, codex, "update_project_knowledge", map[string]any{
		"project": "demo", "section_key": "limitations", "content": "No mobile app.", "source_commit": "b2c3d4e",
	})
	if sec.UpdatedBy.Slug != "codex" || sec.Title != "Limitations" {
		t.Fatalf("knowledge update: %+v", sec)
	}
	type upserted struct {
		Created bool          `json:"created"`
		Feature store.Feature `json:"feature"`
	}
	created := call[upserted](t, codex, "upsert_project_feature", map[string]any{"project": "demo", "name": "Mobile app", "summary": "Use OpenBrain on a phone.", "status": "planned"})
	changed := call[upserted](t, claude, "upsert_project_feature", map[string]any{"project": "demo", "slug": "mobile-app", "status": "removed"})
	if !created.Created || changed.Created || changed.Feature.Status != "removed" || changed.Feature.CreatedBy.Slug != "codex" || changed.Feature.UpdatedBy.Slug != "claude" {
		t.Fatalf("feature upsert: %+v / %+v", created, changed)
	}
	if p := call[store.Profile](t, codex, "get_project_profile", map[string]any{"project": "demo", "include_sections": false}); len(p.Features) != 2 {
		t.Fatalf("removed feature should be hidden: %d", len(p.Features))
	}
	if p := call[store.Profile](t, codex, "get_project_profile", map[string]any{"project": "demo", "feature": "mobile-app"}); len(p.Features) != 1 || p.Features[0].Status != "removed" {
		t.Fatalf("feature mode finds removed features: %+v", p.Features)
	}

	c := call[store.ProjectContext](t, codex, "get_project_context", map[string]any{"project": "demo"})
	if c.Profile.Sections != 5 || c.Profile.Features != 2 {
		t.Fatalf("context profile summary: %+v", c.Profile)
	}
}

func TestUpdatesAndHistoryPaging(t *testing.T) {
	e := setup(t)
	claude := session(t, e, "claude")
	call[map[string]any](t, claude, "register_project", map[string]any{"name": "Demo"})
	for i, cat := range []string{"new", "fixed", "fixed"} {
		call[store.ProjectUpdate](t, claude, "publish_project_update", map[string]any{
			"project": "demo", "title": fmt.Sprintf("Change %d", i), "summary": "Why it matters.", "category": cat})
		call[store.Note](t, claude, "add_note", map[string]any{"project": "demo", "content": fmt.Sprintf("note %d", i)})
	}
	type page struct {
		Updates    []store.ProjectUpdate `json:"updates"`
		NextBefore int64                 `json:"next_before"`
	}
	p1 := call[page](t, claude, "get_project_updates", map[string]any{"project": "demo", "limit": 2})
	p2 := call[page](t, claude, "get_project_updates", map[string]any{"project": "demo", "limit": 2, "before": p1.NextBefore})
	if len(p1.Updates) != 2 || p1.Updates[0].Title != "Change 2" || p1.NextBefore == 0 || len(p2.Updates) != 1 || p2.NextBefore != 0 {
		t.Fatalf("update paging: %+v / %+v", p1, p2)
	}
	if fixed := call[page](t, claude, "get_project_updates", map[string]any{"project": "demo", "category": "fixed"}); len(fixed.Updates) != 2 {
		t.Fatalf("category filter: %d", len(fixed.Updates))
	}
	callErr(t, claude, "get_project_updates", map[string]any{"project": "demo", "category": "release"})

	type hist struct {
		Notes      []store.Note `json:"notes"`
		NextBefore int64        `json:"next_before"`
	}
	h1 := call[hist](t, claude, "get_project_history", map[string]any{"project": "demo", "kind": "notes", "limit": 2})
	h2 := call[hist](t, claude, "get_project_history", map[string]any{"project": "demo", "kind": "notes", "before": h1.NextBefore})
	if len(h1.Notes) != 2 || len(h2.Notes) != 1 || h2.Notes[0].Content != "note 0" {
		t.Fatalf("history paging: %+v / %+v", h1, h2)
	}
	callErr(t, claude, "get_project_history", map[string]any{"project": "demo", "kind": "everything"})
}
