package store

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

const pw = "correct horse battery staple"

func TestPasswordHash(t *testing.T) {
	h := HashPassword(pw)
	if !strings.HasPrefix(h, "$argon2id$v=19$m=12288,t=3,p=1$") {
		t.Fatalf("hash format: %s", h)
	}
	if h == HashPassword(pw) {
		t.Fatal("salts must differ")
	}
	if ok, rehash, err := checkPassword(h, pw); !ok || rehash || err != nil {
		t.Fatalf("verify: %v %v %v", ok, rehash, err)
	}
	if ok, _, _ := checkPassword(h, pw+"!"); ok {
		t.Fatal("wrong password accepted")
	}

	// A hash with weaker parameters still verifies and asks to be replaced.
	salt := []byte("0123456789abcdef")
	old := fmt.Sprintf("$argon2id$v=19$m=8192,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(argon2.IDKey([]byte(pw), salt, 2, 8192, 1, 32)))
	if ok, rehash, err := checkPassword(old, pw); !ok || !rehash || err != nil {
		t.Fatalf("old parameters: %v %v %v", ok, rehash, err)
	}
	if _, _, err := checkPassword("$2b$10$notargon", pw); err == nil {
		t.Fatal("foreign hash format accepted")
	}
}

func TestFirstUserClaimsSeededActor(t *testing.T) {
	st := newStore(t)
	if n := must(st.UserCount(ctx)); n != 0 {
		t.Fatalf("fresh install has %d users", n)
	}
	u := must(st.CreateFirstUser(ctx, UserInput{Username: "  Alice ", DisplayName: "Alice", Password: pw}))
	if u.Username != "alice" || u.Role != "admin" || u.Actor.Slug != "alice" || u.Actor.DisplayName != "Alice" || u.Actor.Kind != "human" {
		t.Fatalf("first user: %+v", u)
	}
	actors := must(st.Actors(ctx))
	if len(actors) != 1 || actors[0].Slug != "alice" {
		t.Fatalf("the seeded actor should be claimed, not duplicated: %+v", actors)
	}
	if _, err := st.CreateFirstUser(ctx, UserInput{Username: "mallory", Password: pw}); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second setup: %v", err)
	}
}

func TestConcurrentFirstUser(t *testing.T) {
	st := newStore(t)
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.CreateFirstUser(ctx, UserInput{Username: fmt.Sprintf("admin%d", i), Password: pw})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrSetupDone):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	users := must(st.Users(ctx))
	if ok != 1 || len(users) != 1 || users[0].Role != "admin" {
		t.Fatalf("%d setups succeeded, %d users", ok, len(users))
	}
}

func TestCreateUserActors(t *testing.T) {
	st := fixture(t) // agents claude and codex, project demo
	must(st.CreateFirstUser(ctx, UserInput{Username: "alice", Password: pw}))

	bob := must(st.CreateUser(ctx, UserInput{Username: "bob", DisplayName: "Bob", Password: pw}))
	if bob.Role != "user" || bob.Actor.Slug != "bob" || bob.Actor.Kind != "human" {
		t.Fatalf("bob: %+v", bob)
	}
	_, err := st.CreateUser(ctx, UserInput{Username: "BOB", Password: pw})
	wantInvalid(t, err, "username")
	_, err = st.CreateUser(ctx, UserInput{Username: "claude", Password: pw})
	wantInvalid(t, err, "username") // an agent's slug
	_, err = st.CreateUser(ctx, UserInput{Username: "x y", Password: pw})
	wantInvalid(t, err, "username")
	_, err = st.CreateUser(ctx, UserInput{Username: "carol", Password: "short"})
	wantInvalid(t, err, "password")
	_, err = st.CreateUser(ctx, UserInput{Username: "carolanne", Password: "CarolAnne"})
	wantInvalid(t, err, "password") // same as the username
	_, err = st.CreateUser(ctx, UserInput{Username: "carol", Password: pw, Role: "root"})
	wantInvalid(t, err, "role")

	// An unclaimed human actor with the same slug is adopted, not duplicated.
	must(st.CreateActor(ctx, ActorInput{Slug: "dana", Kind: "human"}))
	dana := must(st.CreateUser(ctx, UserInput{Username: "dana", DisplayName: "Dana", Password: pw}))
	n := 0
	for _, a := range must(st.Actors(ctx)) {
		if a.Slug == "dana" {
			n++
		}
	}
	if n != 1 || dana.Actor.DisplayName != "Dana" {
		t.Fatalf("dana: %d actors, %+v", n, dana)
	}
}

func TestVerifyLogin(t *testing.T) {
	st := newStore(t)
	alice := must(st.CreateFirstUser(ctx, UserInput{Username: "alice", Password: pw}))
	u, err := st.VerifyLogin(ctx, "ALICE", pw)
	if err != nil || u.ID != alice.ID || u.LastLoginAt.IsZero() {
		t.Fatalf("login: %+v %v", u, err)
	}
	for _, c := range [][2]string{{"alice", "wrong password"}, {"nobody", pw}, {"", pw}, {"alice", ""}} {
		if _, err := st.VerifyLogin(ctx, c[0], c[1]); !errors.Is(err, ErrInvalidLogin) {
			t.Errorf("VerifyLogin(%q, %q) = %v", c[0], c[1], err)
		}
	}
	bob := must(st.CreateUser(ctx, UserInput{Username: "bob", Password: pw}))
	if err := st.SetDisabled(ctx, alice.ID, bob.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.VerifyLogin(ctx, "bob", pw); !errors.Is(err, ErrInvalidLogin) {
		t.Errorf("disabled user: %v", err)
	}

	// Neither the password nor a session token is stored in plaintext.
	secret, _, _ := st.CreateSession(ctx, alice.ID)
	st.w.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	for _, f := range []string{st.path, st.path + "-wal"} {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte(pw)) || bytes.Contains(b, []byte(secret)) {
			t.Fatalf("plaintext credential found in %s", f)
		}
	}
}

func TestSessions(t *testing.T) {
	st := newStore(t)
	alice := must(st.CreateFirstUser(ctx, UserInput{Username: "alice", Password: pw}))
	s1, sess1, err := st.CreateSession(ctx, alice.ID)
	if err != nil || len(s1) < 40 {
		t.Fatalf("session: %q %v", s1, err)
	}
	u, got, err := st.SessionUser(ctx, s1)
	if err != nil || u.ID != alice.ID || got.ID != sess1.ID {
		t.Fatalf("resolve: %+v %v", u, err)
	}
	for _, bad := range []string{"", "nope", s1 + "x"} {
		if _, _, err := st.SessionUser(ctx, bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("SessionUser(%q) = %v", bad, err)
		}
	}

	// Expired sessions are rejected.
	s2, sess2, _ := st.CreateSession(ctx, alice.ID)
	st.w.Exec(`UPDATE user_sessions SET expires_at = ? WHERE id = ?`, Time{time.Now().Add(-time.Minute)}, sess2.ID)
	if _, _, err := st.SessionUser(ctx, s2); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expired session: %v", err)
	}

	// Use extends an idle session, but never past the absolute limit.
	st.w.Exec(`UPDATE user_sessions SET last_seen_at = ?, created_at = ? WHERE id = ?`,
		Time{time.Now().Add(-2 * time.Hour)}, Time{time.Now().Add(-SessionMax + time.Hour)}, sess1.ID)
	_, extended, _ := st.SessionUser(ctx, s1)
	if time.Until(extended.ExpiresAt.Time) > time.Hour+time.Minute {
		t.Errorf("expiry %v exceeds the absolute session limit", extended.ExpiresAt)
	}

	// Changing your password keeps this session and ends the others.
	s3, sess3, _ := st.CreateSession(ctx, alice.ID)
	if err := st.ChangePassword(ctx, alice.ID, "wrong", "new-password-1", sess3.ID); err == nil {
		t.Fatal("wrong current password accepted")
	}
	if err := st.ChangePassword(ctx, alice.ID, pw, "new-password-1", sess3.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionUser(ctx, s3); err != nil {
		t.Errorf("current session ended: %v", err)
	}
	if _, _, err := st.SessionUser(ctx, s1); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("other session survived a password change: %v", err)
	}
	if _, err := st.VerifyLogin(ctx, "alice", "new-password-1"); err != nil {
		t.Errorf("new password: %v", err)
	}

	// An admin reset ends every session; sign-out ends one.
	if err := st.SetPassword(ctx, alice.ID, "new-password-2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionUser(ctx, s3); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("session survived an admin reset: %v", err)
	}
	s4, _, _ := st.CreateSession(ctx, alice.ID)
	st.RevokeSession(ctx, s4)
	if _, _, err := st.SessionUser(ctx, s4); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("revoked session: %v", err)
	}
	s5, sess5, _ := st.CreateSession(ctx, alice.ID)
	s6, _, _ := st.CreateSession(ctx, alice.ID)
	if n := must(st.RevokeOtherSessions(ctx, alice.ID, sess5.ID)); n != 1 {
		t.Errorf("revoked %d others, want 1", n)
	}
	if _, _, err := st.SessionUser(ctx, s5); err != nil {
		t.Error("kept session ended")
	}
	if _, _, err := st.SessionUser(ctx, s6); err == nil {
		t.Error("other session survived")
	}
}

func TestLastAdminProtection(t *testing.T) {
	st := newStore(t)
	alice := must(st.CreateFirstUser(ctx, UserInput{Username: "alice", Password: pw}))
	bob := must(st.CreateUser(ctx, UserInput{Username: "bob", Password: pw}))
	wantInvalid(t, st.SetDisabled(ctx, alice.ID, alice.ID, true), "user") // yourself
	wantInvalid(t, st.SetDisabled(ctx, bob.ID, alice.ID, true), "user")   // the last admin
	carol := must(st.CreateUser(ctx, UserInput{Username: "carol", Password: pw, Role: "admin"}))
	if err := st.SetDisabled(ctx, carol.ID, alice.ID, true); err != nil {
		t.Fatalf("disabling one of two admins: %v", err)
	}
	if err := st.SetDisabled(ctx, carol.ID, alice.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDisabled(ctx, alice.ID, 999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing user: %v", err)
	}
}

func TestMigrateFromSchema2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"migrations/0001_init.sql", "migrations/0002_actor_tokens.sql"} {
		b, _ := migrationFS.ReadFile(name)
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatal(err)
		}
	}
	db.Exec("PRAGMA user_version = 2")
	db.Exec(`INSERT INTO projects (slug, name, created_at, updated_at) VALUES ('kept', 'Kept', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z')`)
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if from, to, err := st.Migrate(ctx); err != nil || from != 2 || to != SchemaVersion() {
		t.Fatalf("upgrade: %d -> %d, %v", from, to, err)
	}
	must(st.Project(ctx, "kept"))
	u := must(st.CreateFirstUser(ctx, UserInput{Username: "owner", Password: pw}))
	if u.Actor.Slug != "owner" || len(must(st.Actors(ctx))) != 1 {
		t.Fatalf("upgraded install should map the first account to the seeded actor: %+v", u)
	}
}
