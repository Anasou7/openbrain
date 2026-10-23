package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

// fixture creates agents "claude" and "codex" and project "demo".
func fixture(t *testing.T) *Store {
	t.Helper()
	st := newStore(t)
	for _, slug := range []string{"claude", "codex"} {
		if _, err := st.CreateActor(ctx, ActorInput{Slug: slug}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "Demo"}); err != nil {
		t.Fatal(err)
	}
	return st
}

func by(actor string) Attribution { return Attribution{Actor: actor, Via: "test"} }
func ptr[T any](v T) *T           { return &v }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func wantInvalid(t *testing.T, err error, field string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != field {
		t.Fatalf("want validation error on %q, got %v", field, err)
	}
}

func activityKinds(t *testing.T, st *Store, ref string) []string {
	t.Helper()
	var kinds []string
	for _, e := range must(st.Activity(ctx, ref, 0, 0)) {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func TestMigrate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	from, to, err := st.Migrate(ctx)
	if err != nil || from != 0 || to != SchemaVersion() || to < 1 {
		t.Fatalf("first migrate: from=%d to=%d err=%v", from, to, err)
	}
	if from, to, err = st.Migrate(ctx); err != nil || from != to {
		t.Fatalf("second migrate should be a no-op: from=%d to=%d err=%v", from, to, err)
	}
	if err := st.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	var mode string
	var fk int
	st.w.QueryRow("PRAGMA journal_mode").Scan(&mode)
	st.w.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	if mode != "wal" || fk != 1 {
		t.Fatalf("journal_mode=%s foreign_keys=%d", mode, fk)
	}
	if _, err := st.r.Exec("DELETE FROM actors"); err == nil {
		t.Fatal("reader pool must be read-only")
	}
	actors := must(st.Actors(ctx))
	if len(actors) != 1 || actors[0].Slug != "user" || actors[0].Kind != "human" {
		t.Fatalf("seeded actors: %+v", actors)
	}

	st.w.Exec("PRAGMA user_version = 999")
	if _, _, err := st.Migrate(ctx); err == nil {
		t.Fatal("must refuse a database newer than the binary")
	}
}

func TestActors(t *testing.T) {
	st := newStore(t)
	a := must(st.CreateActor(ctx, ActorInput{Slug: "hermes", Metadata: []byte(`{"host": "box-1"}`)}))
	if a.DisplayName != "hermes" || a.Kind != "agent" || string(a.Metadata) != `{"host":"box-1"}` {
		t.Fatalf("defaults/metadata: %+v", a)
	}
	if _, err := st.CreateActor(ctx, ActorInput{Slug: "hermes"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	_, err := st.CreateActor(ctx, ActorInput{Slug: "Bad Slug"})
	wantInvalid(t, err, "slug")
	_, err = st.CreateActor(ctx, ActorInput{Slug: "x", Kind: "robot"})
	wantInvalid(t, err, "kind")
	_, err = st.CreateActor(ctx, ActorInput{Slug: "y", Metadata: []byte(`[1]`)})
	wantInvalid(t, err, "metadata")
}

func TestProjects(t *testing.T) {
	st := newStore(t)
	p := must(st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "Hermes Dashboard!"}))
	if p.Slug != "hermes-dashboard" || p.Status != "active" || p.LastActivityAt.IsZero() {
		t.Fatalf("created: %+v", p)
	}
	if _, err := st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "Hermes dashboard"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate slug: %v", err)
	}
	_, err := st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "  "})
	wantInvalid(t, err, "name")
	_, err = st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "x", Slug: "123"})
	wantInvalid(t, err, "slug")
	_, err = st.CreateProject(ctx, ProjectInput{Attribution: by("ghost"), Name: "x"})
	wantInvalid(t, err, "actor")
	_, err = st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "x", Status: "nope"})
	wantInvalid(t, err, "status")

	byID := must(st.Project(ctx, "1"))
	if byID.Slug != p.Slug {
		t.Fatalf("lookup by id: %+v", byID)
	}
	if _, err := st.Project(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	p = must(st.UpdateProject(ctx, p.Slug, ProjectPatch{Attribution: by("user"), Status: ptr("archived"), CurrentFocus: ptr("ship v1")}))
	if p.ArchivedAt.IsZero() || p.CurrentFocus != "ship v1" {
		t.Fatalf("archive: %+v", p)
	}
	if n := len(must(st.Projects(ctx, false))); n != 0 {
		t.Fatalf("archived project listed: %d", n)
	}
	if n := len(must(st.Projects(ctx, true))); n != 1 {
		t.Fatalf("include archived: %d", n)
	}
	p = must(st.UpdateProject(ctx, p.Slug, ProjectPatch{Attribution: by("user"), Status: ptr("active"), Slug: ptr("hd")}))
	if !p.ArchivedAt.IsZero() || p.Slug != "hd" {
		t.Fatalf("unarchive/rename: %+v", p)
	}
	must(st.UpdateProject(ctx, "hd", ProjectPatch{Attribution: by("user"), Name: ptr(p.Name)})) // no-op

	got := strings.Join(activityKinds(t, st, "hd"), ",")
	want := "project.updated,project.status_changed,project.focus_changed,project.status_changed,project.created"
	if got != want {
		t.Fatalf("activity:\n got %s\nwant %s", got, want)
	}
}

func TestNotes(t *testing.T) {
	st := fixture(t)
	mine := must(st.AddNote(ctx, "demo", NoteInput{Attribution: by("user"), Content: "  remember the auth tests  "}))
	if mine.Content != "remember the auth tests" || mine.Kind != "note" || mine.Actor.Kind != "human" {
		t.Fatalf("note: %+v", mine)
	}
	must(st.AddNote(ctx, "demo", NoteInput{Attribution: by("claude"), Kind: "warning", Content: "flaky CI", Pinned: true}))

	if n := len(must(st.Notes(ctx, "demo", NoteFilter{HumanOnly: true}))); n != 1 {
		t.Fatalf("human notes: %d", n)
	}
	if n := len(must(st.Notes(ctx, "demo", NoteFilter{PinnedOnly: true}))); n != 1 {
		t.Fatalf("pinned notes: %d", n)
	}
	if n := len(must(st.Notes(ctx, "demo", NoteFilter{Actor: "claude"}))); n != 1 {
		t.Fatalf("claude notes: %d", n)
	}

	_, err := st.UpdateNote(ctx, mine.ID, NotePatch{Attribution: by("claude"), Content: ptr("rewritten")})
	wantInvalid(t, err, "actor")
	pinned := must(st.UpdateNote(ctx, mine.ID, NotePatch{Attribution: by("claude"), Pinned: ptr(true)}))
	if !pinned.Pinned || pinned.Actor.Slug != "user" {
		t.Fatalf("pin by other actor keeps authorship: %+v", pinned)
	}
	edited := must(st.UpdateNote(ctx, mine.ID, NotePatch{Attribution: by("user"), Content: ptr("auth tests done")}))
	if edited.Content != "auth tests done" {
		t.Fatalf("edit: %+v", edited)
	}
	_, err = st.AddNote(ctx, "demo", NoteInput{Attribution: by("user"), Content: ""})
	wantInvalid(t, err, "content")
	if _, err := st.UpdateNote(ctx, 999, NotePatch{Attribution: by("user"), Pinned: ptr(true)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing note: %v", err)
	}

	kinds := strings.Join(activityKinds(t, st, "demo"), ",")
	if kinds != "note.edited,note.pinned,note.added,note.added,project.created" {
		t.Fatalf("activity: %s", kinds)
	}
}

func TestHandoffs(t *testing.T) {
	st := fixture(t)
	must(st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "Other"}))
	must(st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("claude"), Summary: "API scaffolded"}))
	h2 := must(st.AddHandoff(ctx, "demo", HandoffInput{
		Attribution: by("claude"), Summary: "API done", NextSteps: "write UI",
		Branch: "feature/api", CommitSHA: "9AD281C", Metadata: []byte(`{"tests": "passing"}`), State: "finished",
	}))
	h3 := must(st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("codex"), Summary: "UI in progress"}))
	must(st.AddHandoff(ctx, "other", HandoffInput{Attribution: by("claude"), Summary: "elsewhere"}))

	if h2.CommitSHA != "9ad281c" || string(h2.Metadata) != `{"tests":"passing"}` || h2.Actor.Slug != "claude" {
		t.Fatalf("facts: %+v", h2)
	}
	latest := must(st.LatestHandoffs(ctx, "demo"))
	if len(latest) != 2 || latest[0].ID != h3.ID || latest[1].ID != h2.ID {
		t.Fatalf("latest per actor: %+v", latest)
	}
	if hist := must(st.Handoffs(ctx, "demo", "claude", 0, 0)); len(hist) != 2 || hist[0].ID != h2.ID {
		t.Fatalf("history must be preserved, newest first: %+v", hist)
	}
	states := must(st.States(ctx, "demo"))
	if len(states) != 1 || states[0].State != "finished" || states[0].FinishedAt.IsZero() {
		t.Fatalf("handoff state: %+v", states)
	}

	_, err := st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("claude"), Summary: "x", CommitSHA: "not-a-sha"})
	wantInvalid(t, err, "commit_sha")
	_, err = st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("claude"), Summary: "x", Branch: "has space"})
	wantInvalid(t, err, "branch")
	_, err = st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("claude"), Summary: "x", State: "sleeping"})
	wantInvalid(t, err, "state")

	// The latest-per-actor subquery must be answered from the index.
	rows, err := st.r.Query(`EXPLAIN QUERY PLAN SELECT max(id) FROM handoffs WHERE project_id = 1 GROUP BY actor_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		rows.Scan(&id, &parent, &unused, &detail)
		plan.WriteString(detail + "; ")
	}
	if !strings.Contains(plan.String(), "handoffs_by_project_actor") || strings.Contains(plan.String(), "TEMP B-TREE") {
		t.Fatalf("query plan: %s", plan.String())
	}
}

func TestStates(t *testing.T) {
	st := fixture(t)
	s1 := must(st.SetState(ctx, "demo", StateInput{Attribution: by("claude"), State: "working", CurrentTask: ptr("auth")}))
	if s1.StartedAt.IsZero() || s1.Stale || s1.CurrentTask != "auth" {
		t.Fatalf("working: %+v", s1)
	}
	time.Sleep(2 * time.Millisecond)
	s2 := must(st.SetState(ctx, "demo", StateInput{Attribution: by("claude"), State: "working"})) // heartbeat
	if !s2.UpdatedAt.After(s1.UpdatedAt.Time) || !s2.StartedAt.Equal(s1.StartedAt.Time) || s2.CurrentTask != "auth" {
		t.Fatalf("heartbeat: %+v", s2)
	}
	if kinds := activityKinds(t, st, "demo"); len(kinds) != 2 {
		t.Fatalf("heartbeat must not add activity: %v", kinds)
	}

	st.w.Exec(`UPDATE actor_states SET updated_at = ?`, Time{time.Now().Add(-2 * time.Hour)})
	if s := must(st.States(ctx, "demo")); !s[0].Stale {
		t.Fatalf("2h-old working state should be stale: %+v", s[0])
	}
	must(st.SetState(ctx, "demo", StateInput{Attribution: by("claude"), State: "idle"}))
	st.w.Exec(`UPDATE actor_states SET updated_at = ?`, Time{time.Now().Add(-2 * time.Hour)})
	if s := must(st.States(ctx, "demo")); s[0].Stale {
		t.Fatalf("idle is never stale: %+v", s[0])
	}
	_, err := st.SetState(ctx, "demo", StateInput{Attribution: by("claude"), State: "busy"})
	wantInvalid(t, err, "state")
}

func TestBlockersAndDecisions(t *testing.T) {
	st := fixture(t)
	b := must(st.AddBlocker(ctx, "demo", BlockerInput{Attribution: by("claude"), Description: "need API key"}))
	if b.Resolved || must(st.Project(ctx, "demo")).OpenBlockers != 1 {
		t.Fatalf("open blocker: %+v", b)
	}
	b = must(st.UpdateBlocker(ctx, b.ID, BlockerPatch{Attribution: by("user"), Resolved: ptr(true), Resolution: ptr("added to vault")}))
	if !b.Resolved || b.ResolvedBy.Slug != "user" || b.Resolution != "added to vault" || b.ResolvedAt.IsZero() {
		t.Fatalf("resolved: %+v", b)
	}
	b = must(st.UpdateBlocker(ctx, b.ID, BlockerPatch{Attribution: by("codex"), Resolution: ptr("added to vault (prod too)")}))
	if b.ResolvedBy.Slug != "user" {
		t.Fatalf("editing the resolution must keep the resolver: %+v", b)
	}
	if n := len(must(st.Blockers(ctx, "demo", "open", 0))); n != 0 {
		t.Fatalf("open after resolve: %d", n)
	}
	if n := len(must(st.Blockers(ctx, "demo", "resolved", 0))); n != 1 {
		t.Fatalf("resolved list: %d", n)
	}
	b = must(st.UpdateBlocker(ctx, b.ID, BlockerPatch{Attribution: by("claude"), Resolved: ptr(false)}))
	if b.Resolved || b.ResolvedBy != nil || b.Resolution != "" {
		t.Fatalf("reopened: %+v", b)
	}
	if _, err := st.Blockers(ctx, "demo", "sideways", 0); err == nil {
		t.Fatal("bad state filter accepted")
	}

	d := must(st.AddDecision(ctx, "demo", DecisionInput{Attribution: by("user"), Decision: "Use SQLite", Reasoning: "one file"}))
	if d.Actor.Slug != "user" || len(must(st.Decisions(ctx, "demo", 0, 0))) != 1 {
		t.Fatalf("decision: %+v", d)
	}
	kinds := strings.Join(activityKinds(t, st, "demo"), ",")
	if kinds != "decision.recorded,blocker.reopened,blocker.resolved,blocker.created,project.created" {
		t.Fatalf("activity: %s", kinds)
	}
}

func TestContextAndOverview(t *testing.T) {
	st := fixture(t)
	must(st.AddNote(ctx, "demo", NoteInput{Attribution: by("user"), Content: "my note"}))
	must(st.AddNote(ctx, "demo", NoteInput{Attribution: by("claude"), Content: "agent note", Pinned: true}))
	must(st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("claude"), Summary: "old"}))
	must(st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("claude"), Summary: "API done"}))
	must(st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("codex"), Summary: "UI in progress"}))
	must(st.SetState(ctx, "demo", StateInput{Attribution: by("codex"), State: "working", CurrentTask: ptr("forms")}))
	must(st.AddBlocker(ctx, "demo", BlockerInput{Attribution: by("codex"), Description: "design missing"}))
	must(st.AddDecision(ctx, "demo", DecisionInput{Attribution: by("user"), Decision: "Ship Friday"}))

	c := must(st.ProjectContext(ctx, "demo", ContextOptions{}))
	switch {
	case c.Project.Slug != "demo" || c.Project.OpenBlockers != 1:
		t.Fatalf("project: %+v", c.Project)
	case len(c.LatestHandoffs) != 2 || c.LatestHandoffs[1].Summary != "API done":
		t.Fatalf("latest handoffs: %+v", c.LatestHandoffs)
	case len(c.HumanNotes) != 1 || c.HumanNotes[0].Content != "my note":
		t.Fatalf("human notes: %+v", c.HumanNotes)
	case len(c.PinnedNotes) != 1 || len(c.RecentNotes) != 2:
		t.Fatalf("notes: pinned=%d recent=%d", len(c.PinnedNotes), len(c.RecentNotes))
	case len(c.ActorStates) != 1 || len(c.OpenBlockers) != 1 || len(c.RecentDecisions) != 1:
		t.Fatalf("states/blockers/decisions: %d/%d/%d", len(c.ActorStates), len(c.OpenBlockers), len(c.RecentDecisions))
	case len(c.RecentActivity) != 9 || c.RecentActivity[0].Kind != "decision.recorded":
		t.Fatalf("activity: %d %+v", len(c.RecentActivity), c.RecentActivity[0])
	case c.StaleAfterSeconds != 3600:
		t.Fatalf("stale_after_seconds: %d", c.StaleAfterSeconds)
	}
	if _, err := st.ProjectContext(ctx, "nope", ContextOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project: %v", err)
	}

	// An archived project's actors must not add columns to the default overview.
	must(st.CreateActor(ctx, ActorInput{Slug: "hermes"}))
	must(st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "Old", Status: "archived"}))
	must(st.AddHandoff(ctx, "old", HandoffInput{Attribution: by("hermes"), Summary: "retired"}))

	o := must(st.Overview(ctx, false))
	var cols []string
	for _, a := range o.Actors {
		cols = append(cols, a.Slug)
	}
	if strings.Join(cols, ",") != "user,claude,codex" || len(o.Projects) != 1 {
		t.Fatalf("overview columns %v, rows %d", cols, len(o.Projects))
	}
	row := o.Projects[0].Actors
	if row["claude"].Handoff.Summary != "API done" || row["codex"].State.CurrentTask != "forms" || row["user"].Note.Content != "my note" {
		t.Fatalf("overview cells: %+v", row)
	}
	if o := must(st.Overview(ctx, true)); len(o.Actors) != 4 || len(o.Projects) != 2 {
		t.Fatalf("overview with archived: %d actors, %d projects", len(o.Actors), len(o.Projects))
	}
}

func TestConcurrentWrites(t *testing.T) {
	st := fixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			actor := []string{"claude", "codex"}[i%2]
			_, err := st.AddNote(ctx, "demo", NoteInput{Attribution: by(actor), Content: "parallel"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := len(must(st.Notes(ctx, "demo", NoteFilter{Limit: 100}))); n != 40 {
		t.Fatalf("notes after concurrent writes: %d", n)
	}
}

func TestBackupRestore(t *testing.T) {
	st := fixture(t)
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup.db")
	if err := st.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := st.Backup(ctx, backup); err == nil {
		t.Fatal("backup must not overwrite an existing file")
	}
	if fi, _ := os.Stat(backup); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions: %v", fi.Mode().Perm())
	}
	if v, err := Verify(ctx, backup); err != nil || v != SchemaVersion() {
		t.Fatalf("verify: %d %v", v, err)
	}

	must(st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "After backup"}))
	saved := must(st.Restore(ctx, backup))
	if projects := must(st.Projects(ctx, true)); len(projects) != 1 || projects[0].Slug != "demo" {
		t.Fatalf("after restore: %+v", projects)
	}
	if _, _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// The pre-restore copy holds what was replaced.
	old, err := Open(saved)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if n := len(must(old.Projects(ctx, true))); n != 2 {
		t.Fatalf("pre-restore copy has %d projects, want 2", n)
	}

	junk := filepath.Join(dir, "junk.db")
	os.WriteFile(junk, []byte("not a database"), 0o600)
	if _, err := st.Restore(ctx, junk); err == nil {
		t.Fatal("restore accepted a non-database file")
	}
	if _, err := st.Restore(ctx, filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("restore accepted a missing file")
	}
}

func TestSlugifyAndExcerpt(t *testing.T) {
	for in, want := range map[string]string{"My App!": "my-app", "  --x--  ": "x", "2024": "project-2024", "": "project", "Ünïcode": "n-code"} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Excerpt("first line\nsecond", 50); got != "first line…" {
		t.Errorf("Excerpt multiline = %q", got)
	}
	if got := Excerpt("abcdef", 3); got != "abc…" {
		t.Errorf("Excerpt cut = %q", got)
	}
}
