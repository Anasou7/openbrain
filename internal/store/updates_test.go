package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func content(title string) UpdateContent {
	return UpdateContent{Title: title, Summary: "What changed, and why it matters.", Category: "fixed"}
}

func TestProjectUpdates(t *testing.T) {
	st := fixture(t)
	if _, err := st.LatestUpdate(ctx, "demo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no updates yet: %v", err)
	}

	u := must(st.AddUpdate(ctx, "demo", UpdateInput{Attribution: by("claude"), UpdateContent: UpdateContent{
		Title: "  Login is now faster ", Summary: "The delay noticed during task 3 is fixed.\n\nSigning in should feel immediate.",
		Details: "The repeated session lookup was removed.", Category: " Fixed", TaskReference: "Task 3",
	}}))
	switch {
	case u.Actor.Slug != "claude" || u.ProjectSlug != "demo" || u.ProjectName != "Demo":
		t.Fatalf("attribution: %+v", u)
	case u.Title != "Login is now faster" || u.Category != "fixed" || u.TaskReference != "Task 3" || u.HandoffID != 0:
		t.Fatalf("fields: %+v", u)
	}
	if kinds := activityKinds(t, st, "demo"); kinds[0] != "update.published" {
		t.Fatalf("activity: %v", kinds)
	}

	for _, c := range []struct {
		field string
		in    UpdateContent
	}{
		{"title", UpdateContent{Summary: "s", Category: "new"}},
		{"title", UpdateContent{Title: "Two\nlines", Summary: "s", Category: "new"}},
		{"title", UpdateContent{Title: strings.Repeat("x", maxUpdateTitle+1), Summary: "s", Category: "new"}},
		{"summary", UpdateContent{Title: "t", Category: "new"}},
		{"summary", UpdateContent{Title: "t", Summary: strings.Repeat("x", maxUpdateSummary+1), Category: "new"}},
		{"category", UpdateContent{Title: "t", Summary: "s"}},
		{"category", UpdateContent{Title: "t", Summary: "s", Category: "release"}},
		{"task_reference", UpdateContent{Title: "t", Summary: "s", Category: "new", TaskReference: "a\tb"}},
	} {
		_, err := st.AddUpdate(ctx, "demo", UpdateInput{Attribution: by("claude"), UpdateContent: c.in})
		wantInvalid(t, err, c.field)
	}
	if _, err := st.AddUpdate(ctx, "nope", UpdateInput{Attribution: by("claude"), UpdateContent: content("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if _, err := st.AddUpdate(ctx, "demo", UpdateInput{Attribution: by("ghost"), UpdateContent: content("x")}); err == nil {
		t.Fatal("unknown actor accepted")
	}

	// Idempotent retries return the original update.
	key := UpdateInput{Attribution: by("codex"), UpdateContent: content("Mobile navigation works"), IdempotencyKey: "k1"}
	first := must(st.AddUpdate(ctx, "demo", key))
	if again := must(st.AddUpdate(ctx, "demo", key)); again.ID != first.ID {
		t.Fatalf("replay created a new update: %d vs %d", again.ID, first.ID)
	}
	must(st.AddUpdate(ctx, "demo", UpdateInput{Attribution: by("user"), UpdateContent: content("Pricing is final")}))

	// Newest first; filters and paging.
	all := must(st.Updates(ctx, "demo", UpdateFilter{}))
	if len(all) != 3 || all[0].Title != "Pricing is final" || all[2].Title != "Login is now faster" {
		t.Fatalf("listing: %+v", all)
	}
	if mine := must(st.Updates(ctx, "demo", UpdateFilter{Actor: "codex"})); len(mine) != 1 || mine[0].ID != first.ID {
		t.Fatalf("actor filter: %+v", mine)
	}
	if older := must(st.Updates(ctx, "demo", UpdateFilter{Before: all[1].ID})); len(older) != 1 || older[0].ID != u.ID {
		t.Fatalf("paging: %+v", older)
	}
	if latest := must(st.LatestUpdate(ctx, "demo")); latest.Title != "Pricing is final" {
		t.Fatalf("latest: %+v", latest)
	}
	if agents := must(st.RecentUpdates(ctx, 10, true)); len(agents) != 2 || agents[0].Actor.Slug != "codex" {
		t.Fatalf("recent agent updates: %+v", agents)
	}
	if everyone := must(st.RecentUpdates(ctx, 10, false)); len(everyone) != 3 {
		t.Fatalf("recent updates: %d", len(everyone))
	}
	must(st.UpdateProject(ctx, "demo", ProjectPatch{Attribution: by("user"), Status: ptr("archived")}))
	if archived := must(st.RecentUpdates(ctx, 10, false)); len(archived) != 0 {
		t.Fatalf("archived projects should drop out of recent updates: %d", len(archived))
	}
}

func TestHandoffWithUpdate(t *testing.T) {
	st := fixture(t)

	plain := must(st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("codex"), Summary: "Refactored the cache."}))
	if plain.Update != nil || len(must(st.Updates(ctx, "demo", UpdateFilter{}))) != 0 {
		t.Fatal("a handoff without user_update must not create an update")
	}

	in := HandoffInput{
		Attribution: by("claude"), Summary: "Implemented session caching. All auth tests pass.",
		NextSteps: "Update the login UI.", Branch: "main", CommitSHA: "A83BD21", State: "finished", IdempotencyKey: "h1",
		Update: &UpdateContent{Title: "Login is now faster", Summary: "Signing in should now feel immediate.", Category: "improved", TaskReference: "Task 3"},
	}
	h := must(st.AddHandoff(ctx, "demo", in))
	u := h.Update
	switch {
	case u == nil:
		t.Fatal("no update returned")
	case u.HandoffID != h.ID || u.Actor.Slug != "claude" || u.Branch != "main" || u.CommitSHA != "a83bd21":
		t.Fatalf("linked update: %+v", u)
	}
	if again := must(st.AddHandoff(ctx, "demo", in)); again.ID != h.ID || again.Update == nil || again.Update.ID != u.ID {
		t.Fatalf("replay: %+v", again)
	}
	if n := len(must(st.Updates(ctx, "demo", UpdateFilter{}))); n != 1 {
		t.Fatalf("updates after replay: %d", n)
	}
	if kinds := activityKinds(t, st, "demo"); kinds[0] != "state.changed" || kinds[1] != "update.published" || kinds[2] != "handoff.created" {
		t.Fatalf("activity: %v", kinds)
	}

	// An invalid update rejects the whole handoff, naming the nested field.
	bad := HandoffInput{Attribution: by("claude"), Summary: "More work.", Update: &UpdateContent{Title: "No summary", Category: "new"}}
	_, err := st.AddHandoff(ctx, "demo", bad)
	wantInvalid(t, err, "user_update.summary")
	if n := len(must(st.Handoffs(ctx, "demo", "", 0, 0))); n != 2 {
		t.Fatalf("handoffs after a rejected call: %d", n)
	}
}

// TestHandoffUpdateAtomic forces the update insert to fail inside the
// transaction and checks that the handoff is rolled back with it.
func TestHandoffUpdateAtomic(t *testing.T) {
	st := fixture(t)
	if _, err := st.w.Exec(`CREATE TRIGGER fail_updates BEFORE INSERT ON project_updates BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("claude"), Summary: "Work.", State: "finished",
		Update: &UpdateContent{Title: "Done", Summary: "Something changed.", Category: "new"}})
	if err == nil {
		t.Fatal("expected the forced failure")
	}
	if n := len(must(st.Handoffs(ctx, "demo", "", 0, 0))); n != 0 {
		t.Fatalf("handoff survived a failed update: %d", n)
	}
	if states := must(st.States(ctx, "demo")); len(states) != 0 {
		t.Fatalf("state survived a failed update: %+v", states)
	}
	for _, k := range activityKinds(t, st, "demo") {
		if k == "handoff.created" {
			t.Fatal("activity survived a failed update")
		}
	}
}

func TestUpdatesInContextAndOverview(t *testing.T) {
	st := fixture(t)
	for i, title := range []string{"One", "Two", "Three", "Four", "Five", "Six", "Seven"} {
		actor := []string{"claude", "codex"}[i%2]
		c := content(title)
		c.Summary, c.Details = strings.Repeat("s", ClipChars+10), strings.Repeat("d", 2000)
		must(st.AddUpdate(ctx, "demo", UpdateInput{Attribution: by(actor), UpdateContent: c}))
	}

	full := must(st.ProjectContext(ctx, "demo", ContextOptions{}))
	if len(full.RecentUpdates) != 7 || full.RecentUpdates[0].Title != "Seven" || len(full.RecentUpdates[0].Summary) != ClipChars+10 {
		t.Fatalf("full context updates: %d", len(full.RecentUpdates))
	}
	compact := must(st.ProjectContext(ctx, "demo", ContextOptions{Compact: true}))
	if len(compact.RecentUpdates) != compactLimits.updates || !compact.Truncated || len([]rune(compact.RecentUpdates[0].Details)) > ClipChars/5+80 {
		t.Fatalf("compact context updates: %d truncated=%v", len(compact.RecentUpdates), compact.Truncated)
	}

	o := must(st.Overview(ctx, false))
	row := o.Projects[0]
	switch {
	case row.LatestUpdate == nil || row.LatestUpdate.Title != "Seven":
		t.Fatalf("latest update per project: %+v", row.LatestUpdate)
	case row.Actors["claude"].Update == nil || row.Actors["claude"].Update.Title != "Seven":
		t.Fatalf("claude's latest: %+v", row.Actors["claude"].Update)
	case row.Actors["codex"].Update == nil || row.Actors["codex"].Update.Title != "Six":
		t.Fatalf("codex's latest: %+v", row.Actors["codex"].Update)
	}
}

func TestMigrateFromSchema3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"migrations/0001_init.sql", "migrations/0002_actor_tokens.sql", "migrations/0003_users.sql"} {
		b, _ := migrationFS.ReadFile(name)
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatal(err)
		}
	}
	db.Exec("PRAGMA user_version = 3")
	db.Exec(`INSERT INTO projects (slug, name, created_at, updated_at) VALUES ('kept', 'Kept', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z')`)
	db.Exec(`INSERT INTO handoffs (project_id, actor_id, summary, created_at) VALUES (1, 1, 'Earlier work', '2026-01-02T00:00:00.000Z')`)
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if from, to, err := st.Migrate(ctx); err != nil || from != 3 || to != SchemaVersion() {
		t.Fatalf("upgrade: %d -> %d, %v", from, to, err)
	}
	if hs := must(st.Handoffs(ctx, "kept", "", 0, 0)); len(hs) != 1 || hs[0].Summary != "Earlier work" {
		t.Fatalf("existing data: %+v", hs)
	}
	u := must(st.AddUpdate(ctx, "kept", UpdateInput{Attribution: by("user"), UpdateContent: content("Works after upgrade")}))
	if u.ProjectSlug != "kept" {
		t.Fatalf("update after upgrade: %+v", u)
	}
}
