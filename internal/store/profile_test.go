package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func str(s string) *string { return &s }

func TestProfileSections(t *testing.T) {
	st := fixture(t)

	sec := must(st.PutSection(ctx, "demo", SectionInput{Attribution: by("claude"), SectionChange: SectionChange{
		Key: "Overview", Content: str("A shared project memory for people and AI agents."), SourceCommit: "A81C9E3",
	}}))
	if sec.Key != "overview" || sec.Title != "What it is" || !sec.Canonical || sec.CreatedBy.Slug != "claude" || sec.SourceCommit != "a81c9e3" {
		t.Fatalf("created section: %+v", sec)
	}
	custom := must(st.PutSection(ctx, "demo", SectionInput{Attribution: by("claude"), SectionChange: SectionChange{
		Key: "release_process", Content: str("Tagged releases every two weeks."),
	}}))
	if custom.Title != "Release process" || custom.Canonical {
		t.Fatalf("custom section: %+v", custom)
	}
	// Unknown facts are explicit, and need no content.
	must(st.PutSection(ctx, "demo", SectionInput{Attribution: by("claude"), SectionChange: SectionChange{Key: "pricing", Status: str("unknown")}}))
	_, err := st.PutSection(ctx, "demo", SectionInput{Attribution: by("claude"), SectionChange: SectionChange{Key: "faq"}})
	wantInvalid(t, err, "content")
	_, err = st.PutSection(ctx, "demo", SectionInput{Attribution: by("claude"), SectionChange: SectionChange{Key: "Bad Key!", Content: str("x")}})
	wantInvalid(t, err, "key")
	_, err = st.PutSection(ctx, "demo", SectionInput{Attribution: by("claude"), SectionChange: SectionChange{Key: "faq", Content: str("x"), Status: str("maybe")}})
	wantInvalid(t, err, "status")

	// Another actor edits; the creator is kept, the editor recorded.
	edited := must(st.PutSection(ctx, "demo", SectionInput{Attribution: by("codex"), SectionChange: SectionChange{Key: "overview", Content: str("Shared memory for agents.")}}))
	if edited.CreatedBy.Slug != "claude" || edited.UpdatedBy.Slug != "codex" || edited.SourceCommit != "" {
		t.Fatalf("edited section: %+v", edited)
	}
	// Writing the same content again changes nothing, not even the editor.
	same := must(st.PutSection(ctx, "demo", SectionInput{Attribution: by("claude"), SectionChange: SectionChange{Key: "overview", Content: str("Shared memory for agents.")}}))
	if same.UpdatedBy.Slug != "codex" || !same.UpdatedAt.Equal(edited.UpdatedAt.Time) {
		t.Fatalf("unchanged write touched the section: %+v", same)
	}

	prof := must(st.Profile(ctx, "demo", ProfileOptions{}))
	var keys []string
	for _, s := range prof.Sections {
		keys = append(keys, s.Key)
	}
	if !slices.Equal(keys, []string{"overview", "pricing", "release_process"}) {
		t.Fatalf("section order (canonical, then custom): %v", keys)
	}
	c := prof.Completeness
	if !slices.Equal(c.Known, []string{"overview"}) || !slices.Equal(c.Unknown, []string{"pricing"}) ||
		!slices.Equal(c.Custom, []string{"release_process"}) || len(c.Missing) != len(CanonicalSections)-2 {
		t.Fatalf("completeness: %+v", c)
	}
	if one := must(st.Section(ctx, "demo", "pricing")); one.Status != "unknown" {
		t.Fatalf("single section: %+v", one)
	}
	if _, err := st.Section(ctx, "demo", "faq"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing section: %v", err)
	}

	if err := st.DeleteSection(ctx, "demo", "release_process", by("user")); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSection(ctx, "demo", "release_process", by("user")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	kinds := activityKinds(t, st, "demo")
	if kinds[0] != "profile.section_removed" || slices.Index(kinds, "profile.section_updated") < 0 {
		t.Fatalf("activity: %v", kinds)
	}
}

func TestFeatures(t *testing.T) {
	st := fixture(t)
	in := FeatureInput{Attribution: by("claude"), FeatureChange: FeatureChange{
		Name: str("Team Workspaces"), Summary: str("Manage several client projects from one account."),
		TargetUsers: str("Agencies and consultants."), UseCases: &[]string{" Separate client work ", "", "Run several products"},
		Limitations: str("No workspace-level billing yet."),
	}}
	f, created, err := st.PutFeature(ctx, "demo", in)
	if err != nil || !created {
		t.Fatal(err)
	}
	if f.Slug != "team-workspaces" || f.Status != "available" || !slices.Equal(f.UseCases, []string{"Separate client work", "Run several products"}) || f.CreatedBy.Slug != "claude" {
		t.Fatalf("created feature: %+v", f)
	}
	if _, err := st.CreateFeature(ctx, "demo", in); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create: %v", err)
	}

	// Only the fields sent change.
	upd, created, err := st.PutFeature(ctx, "demo", FeatureInput{Attribution: by("codex"), FeatureChange: FeatureChange{
		Slug: "team-workspaces", Limitations: str(""),
	}})
	if err != nil || created || upd.Limitations != "" || upd.Summary != f.Summary || upd.UpdatedBy.Slug != "codex" || upd.CreatedBy.Slug != "claude" {
		t.Fatalf("partial update: %+v %v", upd, err)
	}
	dep := must(st.UpdateFeature(ctx, f.ID, FeatureInput{Attribution: by("user"), FeatureChange: FeatureChange{Status: str("deprecated")}}))
	if dep.Status != "deprecated" {
		t.Fatalf("status: %+v", dep)
	}
	if _, err := st.UpdateFeature(ctx, f.ID, FeatureInput{Attribution: by("user"), FeatureChange: FeatureChange{Slug: "renamed"}}); err == nil {
		t.Fatal("slug change accepted")
	}
	if _, _, err := st.PutFeature(ctx, "demo", FeatureInput{Attribution: by("claude"), FeatureChange: FeatureChange{Name: str("No summary")}}); err == nil {
		t.Fatal("new feature without a summary accepted")
	}
	_, _, err = st.PutFeature(ctx, "demo", FeatureInput{Attribution: by("claude"), FeatureChange: FeatureChange{Slug: "x", Name: str("X"), Summary: str("S"), Status: str("gone")}})
	wantInvalid(t, err, "status")

	must(st.UpdateFeature(ctx, f.ID, FeatureInput{Attribution: by("user"), FeatureChange: FeatureChange{Status: str("removed")}}))
	if visible := must(st.Features(ctx, "demo", false)); len(visible) != 0 {
		t.Fatalf("removed features should be hidden by default: %+v", visible)
	}
	if all := must(st.Features(ctx, "demo", true)); len(all) != 1 || all[0].Status != "removed" {
		t.Fatalf("removed features are kept, not deleted: %+v", all)
	}
	if kinds := activityKinds(t, st, "demo"); kinds[0] != "feature.updated" || slices.Index(kinds, "feature.added") < 0 {
		t.Fatalf("activity: %v", kinds)
	}
	if got := must(st.Feature(ctx, "demo", "team-workspaces")); got.ID != f.ID {
		t.Fatalf("by slug: %+v", got)
	}
}

func TestSyncProfile(t *testing.T) {
	st := fixture(t)
	first := must(st.SyncProfile(ctx, "demo", ProfileInput{Attribution: by("claude"), ProfileChange: ProfileChange{
		SourceBranch: "main", SourceCommit: "a81c9e3",
		Sections: []SectionChange{
			{Key: "overview", Content: str("What it is.")},
			{Key: "icp", Content: str("Small SaaS teams.")},
			{Key: "pricing", Status: str("unknown"), Content: str("Not in the repository or docs.")},
		},
		Features: []FeatureChange{
			{Name: str("Shared context"), Summary: str("Agents read where others left off.")},
			{Name: str("Handoffs"), Summary: str("Agents record where they stopped.")},
		},
	}}))
	if len(first.Sections.Created) != 3 || len(first.Features.Created) != 2 || len(first.Completeness.Known) != 2 {
		t.Fatalf("first sync: %+v", first)
	}
	if sec := must(st.Section(ctx, "demo", "icp")); sec.SourceBranch != "main" || sec.SourceCommit != "a81c9e3" {
		t.Fatalf("sync-level source not applied: %+v", sec)
	}

	// A partial sync merges: nothing omitted is deleted.
	second := must(st.SyncProfile(ctx, "demo", ProfileInput{Attribution: by("codex"), ProfileChange: ProfileChange{
		Sections: []SectionChange{{Key: "icp", Content: str("Small SaaS teams.")}, {Key: "faq", Content: str("Q and A.")}},
		Features: []FeatureChange{{Slug: "handoffs", Limitations: str("No attachments.")}},
	}}))
	if !slices.Equal(second.Sections.Unchanged, []string{"icp"}) || !slices.Equal(second.Sections.Created, []string{"faq"}) ||
		!slices.Equal(second.Features.Updated, []string{"handoffs"}) {
		t.Fatalf("second sync: %+v", second)
	}
	prof := must(st.Profile(ctx, "demo", ProfileOptions{}))
	if len(prof.Sections) != 4 || len(prof.Features) != 2 {
		t.Fatalf("partial sync deleted something: %d sections, %d features", len(prof.Sections), len(prof.Features))
	}

	// One bad item rolls the whole sync back.
	_, err := st.SyncProfile(ctx, "demo", ProfileInput{Attribution: by("claude"), ProfileChange: ProfileChange{
		Sections: []SectionChange{{Key: "security", Content: str("Self-hosted.")}},
		Features: []FeatureChange{{Name: str("Brand new"), Description: str("no summary")}},
	}})
	wantInvalid(t, err, "summary")
	if _, err := st.Section(ctx, "demo", "security"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed sync left a section behind: %v", err)
	}
	_, err = st.SyncProfile(ctx, "demo", ProfileInput{Attribution: by("claude"), ProfileChange: ProfileChange{
		Sections: []SectionChange{{Key: "faq", Content: str("a")}, {Key: "FAQ", Content: str("b")}},
	}})
	wantInvalid(t, err, "sections[1].key")
	_, err = st.SyncProfile(ctx, "demo", ProfileInput{Attribution: by("claude")})
	wantInvalid(t, err, "sections")

	// Repeating a sync is a no-op and adds no activity.
	before := len(activityKinds(t, st, "demo"))
	again := must(st.SyncProfile(ctx, "demo", ProfileInput{Attribution: by("codex"), ProfileChange: ProfileChange{
		Sections: []SectionChange{{Key: "faq", Content: str("Q and A.")}},
	}}))
	if len(again.Sections.Unchanged) != 1 || len(activityKinds(t, st, "demo")) != before {
		t.Fatalf("no-op sync: %+v", again)
	}
	if kinds := activityKinds(t, st, "demo"); kinds[0] != "profile.synced" {
		t.Fatalf("activity: %v", kinds)
	}
	if a := must(st.Activity(ctx, "demo", 0, 1)); a[0].Summary != "synced the project profile: added 1 section; updated 1 feature" {
		t.Fatalf("sync summary: %q", a[0].Summary)
	}

	c := must(st.ProjectContext(ctx, "demo", ContextOptions{Compact: true}))
	if c.Profile.Sections != 4 || c.Profile.Features != 2 || c.Profile.UpdatedAt.IsZero() {
		t.Fatalf("context profile summary: %+v", c.Profile)
	}
}

func TestHistoryPaging(t *testing.T) {
	st := fixture(t)
	for i := 0; i < 5; i++ {
		must(st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("claude"), Summary: "h"}))
		must(st.AddDecision(ctx, "demo", DecisionInput{Attribution: by("claude"), Decision: "d"}))
		must(st.AddNote(ctx, "demo", NoteInput{Attribution: by("claude"), Content: "n"}))
	}
	h := must(st.Handoffs(ctx, "demo", "", 0, 2))
	h2 := must(st.Handoffs(ctx, "demo", "", h[1].ID, 10))
	d := must(st.Decisions(ctx, "demo", 0, 2))
	d2 := must(st.Decisions(ctx, "demo", d[1].ID, 10))
	n := must(st.Notes(ctx, "demo", NoteFilter{Limit: 2}))
	n2 := must(st.Notes(ctx, "demo", NoteFilter{Before: n[1].ID, Limit: 10}))
	if len(h2) != 3 || h2[0].ID >= h[1].ID || len(d2) != 3 || len(n2) != 3 {
		t.Fatalf("paging: handoffs %d, decisions %d, notes %d", len(h2), len(d2), len(n2))
	}
}

func TestMigrateFromSchema4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0001_init", "0002_actor_tokens", "0003_users", "0004_project_updates"} {
		b, _ := migrationFS.ReadFile("migrations/" + name + ".sql")
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatal(err)
		}
	}
	db.Exec("PRAGMA user_version = 4")
	db.Exec(`INSERT INTO projects (slug, name, created_at, updated_at) VALUES ('kept', 'Kept', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z')`)
	db.Exec(`INSERT INTO project_updates (project_id, actor_id, title, summary, category, created_at) VALUES (1, 1, 'Kept update', 'S', 'new', '2026-01-02T00:00:00.000Z')`)
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if from, to, err := st.Migrate(ctx); err != nil || from != 4 || to != SchemaVersion() || to != 5 {
		t.Fatalf("upgrade: %d -> %d, %v", from, to, err)
	}
	if u := must(st.LatestUpdate(ctx, "kept")); u.Title != "Kept update" {
		t.Fatalf("existing data: %+v", u)
	}
	must(st.PutSection(ctx, "kept", SectionInput{Attribution: by("user"), SectionChange: SectionChange{Key: "overview", Content: str("Works after upgrade.")}}))
}
