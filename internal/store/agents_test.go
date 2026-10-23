package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTokens(t *testing.T) {
	st := fixture(t)
	tok, secret, err := st.CreateToken(ctx, "claude", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) || len(secret) != len(TokenPrefix)+43 || !strings.HasPrefix(secret, tok.Prefix) {
		t.Fatalf("secret %q / prefix %q", secret, tok.Prefix)
	}
	if tok.Actor.Slug != "claude" || tok.Label != "laptop" || !tok.LastUsedAt.IsZero() {
		t.Fatalf("token: %+v", tok)
	}
	_, other, _ := st.CreateToken(ctx, "claude", "desktop")
	if other == secret {
		t.Fatal("two tokens share a secret")
	}

	// The plaintext must not be anywhere in the database files.
	st.w.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	for _, f := range []string{st.path, st.path + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte(secret[len(tok.Prefix):])) {
			t.Fatalf("token secret found in %s", f)
		}
	}

	got, err := st.Authenticate(ctx, secret)
	if err != nil || got.Actor.Slug != "claude" || got.ID != tok.ID || got.LastUsedAt.IsZero() {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	for _, bad := range []string{"", "pbt_", secret + "x", secret[:len(secret)-1], "Bearer " + secret, strings.ToUpper(secret)} {
		if _, err := st.Authenticate(ctx, bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Authenticate(%q) = %v, want ErrInvalidToken", bad, err)
		}
	}

	list := must(st.Tokens(ctx, false))
	b, _ := json.Marshal(list)
	if len(list) != 2 || bytes.Contains(b, []byte(secret[len(tok.Prefix):])) || bytes.Contains(b, []byte("hash")) {
		t.Fatalf("token list leaks or is wrong: %s", b)
	}

	_, _, err = st.CreateToken(ctx, "ghost", "")
	wantInvalid(t, err, "actor")

	revoked := must(st.RevokeToken(ctx, tok.ID))
	if revoked.RevokedAt.IsZero() {
		t.Fatal("not revoked")
	}
	if _, err := st.Authenticate(ctx, secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token accepted: %v", err)
	}
	again := must(st.RevokeToken(ctx, tok.ID))
	if !again.RevokedAt.Equal(revoked.RevokedAt.Time) {
		t.Fatal("second revoke changed the revocation time")
	}
	if n := len(must(st.Tokens(ctx, false))); n != 1 {
		t.Fatalf("active tokens: %d", n)
	}
	if n := len(must(st.Tokens(ctx, true))); n != 2 {
		t.Fatalf("all tokens: %d", n)
	}
	if _, err := st.RevokeToken(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing: %v", err)
	}
}

func TestTokenLastUsedThrottled(t *testing.T) {
	st := fixture(t)
	tok, secret, _ := st.CreateToken(ctx, "claude", "")
	first := must(st.Authenticate(ctx, secret)).LastUsedAt
	second := must(st.Authenticate(ctx, secret)).LastUsedAt
	if !second.Equal(first.Time) {
		t.Fatal("last_used_at should be written at most once a minute")
	}
	st.w.Exec(`UPDATE actor_tokens SET last_used_at = ? WHERE id = ?`, Time{time.Now().Add(-2 * time.Minute)}, tok.ID)
	if third := must(st.Authenticate(ctx, secret)).LastUsedAt; time.Since(third.Time) > time.Minute {
		t.Fatalf("stale last_used_at not refreshed: %v", third)
	}
}

func TestRepoKey(t *testing.T) {
	for _, in := range []string{
		"git@github.com:Acme/Widget.git",
		"https://github.com/acme/widget",
		"https://github.com/acme/widget.git/",
		"ssh://git@github.com/acme/widget.git",
		"ssh://git@github.com:22/acme/widget",
		"https://user:secret@github.com/acme/widget",
		"github.com/acme/widget",
		"  HTTPS://GitHub.com/Acme/Widget  ",
	} {
		if got := RepoKey(in); got != "github.com/acme/widget" {
			t.Errorf("RepoKey(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{
		"https://gitlab.com/group/sub/proj.git": "gitlab.com/group/sub/proj",
		"git@github.com:acme/other.git":         "github.com/acme/other",
		"/srv/git/thing.git":                    "srv/git/thing",
		"":                                      "",
		"https://github.com/":                   "",
	} {
		if got := RepoKey(in); got != want {
			t.Errorf("RepoKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegisterAndFindByRepo(t *testing.T) {
	st := fixture(t)
	p, created, err := st.RegisterProject(ctx, ProjectInput{Attribution: by("claude"), Name: "Widget", RepoURL: "git@github.com:acme/widget.git"})
	if err != nil || !created {
		t.Fatalf("register: %v %v", created, err)
	}
	again, created, err := st.RegisterProject(ctx, ProjectInput{Attribution: by("codex"), Name: "Widget (codex)", RepoURL: "https://github.com/acme/widget"})
	if err != nil || created || again.ID != p.ID {
		t.Fatalf("duplicate registration: created=%v id=%d err=%v", created, again.ID, err)
	}
	if n := len(must(st.ProjectsByRepo(ctx, "https://github.com/Acme/Widget.git"))); n != 1 {
		t.Fatalf("lookup: %d", n)
	}
	if n := len(must(st.ProjectsByRepo(ctx, "https://github.com/acme/nothing"))); n != 0 {
		t.Fatalf("lookup of unknown repo: %d", n)
	}
	_, err = st.ProjectsByRepo(ctx, "  ")
	wantInvalid(t, err, "repo_url")

	// Plain CreateProject allows a second project on one repository (monorepos).
	must(st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "Widget docs", RepoURL: "https://github.com/acme/widget"}))
	if n := len(must(st.ProjectsByRepo(ctx, "git@github.com:acme/widget"))); n != 2 {
		t.Fatalf("two projects on one repo: %d", n)
	}

	for _, bad := range []string{"https://ghp_secret@github.com/acme/x.git", "https://u:p@example.com/x.git", "ssh://git:pw@host/x.git", "has space"} {
		_, err := st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "X " + bad, RepoURL: bad})
		wantInvalid(t, err, "repo_url")
	}
	_, err = st.UpdateProject(ctx, "widget", ProjectPatch{Attribution: by("user"), RepoURL: ptr("https://tok@github.com/acme/widget")})
	wantInvalid(t, err, "repo_url")
	must(st.CreateProject(ctx, ProjectInput{Attribution: by("user"), Name: "SSH ok", RepoURL: "ssh://git@github.com/acme/ok.git"}))
}

func TestIdempotency(t *testing.T) {
	st := fixture(t)
	note := func(actor, key string) Note {
		return must(st.AddNote(ctx, "demo", NoteInput{Attribution: by(actor), Content: "only once", IdempotencyKey: key}))
	}
	n1, n2 := note("claude", "k1"), note("claude", "k1")
	if n1.ID != n2.ID || len(must(st.Notes(ctx, "demo", NoteFilter{}))) != 1 {
		t.Fatalf("retried note created a duplicate: %d vs %d", n1.ID, n2.ID)
	}
	if n3 := note("codex", "k1"); n3.ID == n1.ID {
		t.Fatal("keys must be scoped to the actor")
	}
	_, err := st.AddDecision(ctx, "demo", DecisionInput{Attribution: by("claude"), Decision: "x", IdempotencyKey: "k1"})
	wantInvalid(t, err, "idempotency_key")

	h := HandoffInput{Attribution: by("claude"), Summary: "done", IdempotencyKey: "h1"}
	if a, b := must(st.AddHandoff(ctx, "demo", h)), must(st.AddHandoff(ctx, "demo", h)); a.ID != b.ID {
		t.Fatal("retried handoff created a duplicate")
	}
	d := DecisionInput{Attribution: by("claude"), Decision: "Go", IdempotencyKey: "d1"}
	if a, b := must(st.AddDecision(ctx, "demo", d)), must(st.AddDecision(ctx, "demo", d)); a.ID != b.ID {
		t.Fatal("retried decision created a duplicate")
	}
	bl := BlockerInput{Attribution: by("claude"), Description: "stuck", IdempotencyKey: "b1"}
	if a, b := must(st.AddBlocker(ctx, "demo", bl)), must(st.AddBlocker(ctx, "demo", bl)); a.ID != b.ID {
		t.Fatal("retried blocker created a duplicate")
	}
	p := ProjectInput{Attribution: by("claude"), Name: "Keyed", IdempotencyKey: "p1"}
	if a, b := must(st.CreateProject(ctx, p)), must(st.CreateProject(ctx, p)); a.ID != b.ID {
		t.Fatal("retried project creation should return the first project, not conflict")
	}
	added := 0
	for _, e := range must(st.Activity(ctx, "demo", 0, 100)) {
		if e.Kind == "note.added" {
			added++
		}
	}
	if added != 2 {
		t.Fatalf("note.added entries: %d, want 2 (claude once, codex once)", added)
	}

	_, err = st.AddNote(ctx, "demo", NoteInput{Attribution: by("claude"), Content: "x", IdempotencyKey: "has space"})
	wantInvalid(t, err, "idempotency_key")

	// Expired keys are pruned on the next keyed write.
	st.w.Exec(`UPDATE idempotency_keys SET created_at = ?`, Time{time.Now().Add(-8 * 24 * time.Hour)})
	note("claude", "fresh")
	var left int
	st.w.QueryRow(`SELECT count(*) FROM idempotency_keys`).Scan(&left)
	if left != 1 {
		t.Fatalf("keys after prune: %d, want 1", left)
	}
}

func TestCompactContext(t *testing.T) {
	st := fixture(t)
	long := strings.Repeat("é", ClipChars+500)
	for i := range 12 {
		must(st.AddNote(ctx, "demo", NoteInput{Attribution: by("user"), Content: fmt.Sprintf("pinned %d", i), Pinned: true}))
	}
	must(st.AddNote(ctx, "demo", NoteInput{Attribution: by("claude"), Content: long}))
	must(st.AddHandoff(ctx, "demo", HandoffInput{Attribution: by("claude"), Summary: long}))
	for i := range 7 {
		must(st.AddDecision(ctx, "demo", DecisionInput{Attribution: by("user"), Decision: fmt.Sprintf("decision %d", i)}))
	}

	full := must(st.ProjectContext(ctx, "demo", ContextOptions{}))
	if full.Compact || full.Truncated || len(full.PinnedNotes) != 12 || len(full.RecentNotes) != 10 ||
		len(full.RecentDecisions) != 7 || len(full.RecentActivity) != 22 || full.LatestHandoffs[0].Summary != long {
		t.Fatalf("full: pinned=%d recent=%d decisions=%d activity=%d", len(full.PinnedNotes), len(full.RecentNotes), len(full.RecentDecisions), len(full.RecentActivity))
	}
	c := must(st.ProjectContext(ctx, "demo", ContextOptions{Compact: true}))
	if !c.Compact || !c.Truncated || len(c.PinnedNotes) != 10 || len(c.HumanNotes) != 5 || c.RecentNotes == nil || len(c.RecentNotes) != 0 ||
		len(c.RecentDecisions) != 5 || len(c.RecentActivity) != 10 || len(c.LatestHandoffs) != 1 {
		t.Fatalf("compact: pinned=%d human=%d recent=%d decisions=%d activity=%d", len(c.PinnedNotes), len(c.HumanNotes), len(c.RecentNotes), len(c.RecentDecisions), len(c.RecentActivity))
	}
	if s := c.LatestHandoffs[0].Summary; utf8.RuneCountInString(s) > ClipChars+60 || !strings.Contains(s, "[truncated") {
		t.Fatalf("handoff summary not clipped: %d runes", utf8.RuneCountInString(s))
	}
	if n := len(must(st.ProjectContext(ctx, "demo", ContextOptions{Compact: true, ActivityLimit: 3})).RecentActivity); n != 3 {
		t.Fatalf("activity limit: %d", n)
	}
	fb, _ := json.Marshal(full)
	cb, _ := json.Marshal(c)
	if len(cb) >= len(fb) {
		t.Fatalf("compact (%d bytes) not smaller than full (%d bytes)", len(cb), len(fb))
	}
}

func TestMigrateFromFirstRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := migrationFS.ReadFile("migrations/0001_init.sql")
	for _, q := range []string{string(v1), "PRAGMA user_version = 1",
		`INSERT INTO projects (slug, name, created_at, updated_at) VALUES ('old', 'Old', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	from, to, err := st.Migrate(ctx)
	if err != nil || from != 1 || to != SchemaVersion() || to < 2 {
		t.Fatalf("upgrade: %d -> %d, %v", from, to, err)
	}
	if p := must(st.Project(ctx, "old")); p.Name != "Old" {
		t.Fatalf("data lost: %+v", p)
	}
	if _, _, err := st.CreateToken(ctx, "user", "after upgrade"); err != nil {
		t.Fatal(err)
	}
}
