// Command openbrain runs the OpenBrain server and its maintenance tasks.
// Configuration comes from environment variables; see usage.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"openbrain/internal/store"
	"openbrain/internal/web"
)

// version is the release this source tree prepares. Builds can override it:
// go build -ldflags "-X main.version=0.1.0-rc1" ./cmd/openbrain
var version = "0.1.0"

const usage = `openbrain: shared project state for people and AI agents

Usage:
  openbrain serve            run the HTTP server (applies pending migrations first)
  openbrain migrate          apply pending database migrations and exit
  openbrain backup [file]    write a consistent online backup
                             (default: <db dir>/backups/openbrain-<UTC time>.db)
  openbrain restore <file>   replace the database contents with a backup
  openbrain version          print the version

People (run as the service user, e.g. sudo -u openbrain ...):
  openbrain user list
  openbrain user create <username> [--name NAME] [--admin]
                             prints a generated password once
  openbrain user reset-password <username>
                             prints a new generated password once; signs them out
  openbrain user disable <username> | enable <username>

Agents:
  openbrain actor create <slug> [--name NAME] [--kind agent|human|automation]
  openbrain actor list
  openbrain token create --actor SLUG [--label LABEL]
                             prints a new actor token once, on stdout
  openbrain token list [--all]
  openbrain token revoke <id>

Environment (the pre-rename BRAIN_* names are still read, with a warning):
  OPENBRAIN_DB               SQLite database file          (default: openbrain.db)
  OPENBRAIN_ADDR             listen address                (default: 127.0.0.1:8080)
  OPENBRAIN_BASE_PATH        URL prefix, e.g. /openbrain   (default: / )
  OPENBRAIN_TRUSTED_PROXIES  proxies whose X-Forwarded-* headers are believed
                             (default: 127.0.0.1,::1)
  OPENBRAIN_TOKEN            optional admin API token, 32+ chars
  OPENBRAIN_STALE_AFTER      age at which a busy agent state is flagged stale (default: 1h)
  OPENBRAIN_LOG_FORMAT       text or json                  (default: text)
  OPENBRAIN_LOG_LEVEL        debug, info, warn or error    (default: info)
`

var log *slog.Logger

func main() {
	var level slog.Level
	level.UnmarshalText([]byte(env("LOG_LEVEL", ""))) // empty or invalid keeps info
	opts := &slog.HandlerOptions{Level: level}
	log = slog.New(slog.NewTextHandler(os.Stderr, opts))
	if env("LOG_FORMAT", "") == "json" {
		log = slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve()
	case "migrate":
		err = migrate(ctx)
	case "backup":
		err = backup(ctx, args)
	case "restore":
		err = restore(ctx, args)
	case "user":
		err = userCmd(ctx, args)
	case "actor":
		err = actorCmd(ctx, args)
	case "token":
		err = tokenCmd(ctx, args)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

var warned = map[string]bool{}

// env reads OPENBRAIN_<name>, falling back to the pre-rename BRAIN_<name> so
// existing installations keep working through the upgrade.
func env(name, def string) string {
	if v := os.Getenv("OPENBRAIN_" + name); v != "" {
		return v
	}
	if v := os.Getenv("BRAIN_" + name); v != "" {
		if !warned[name] && log != nil {
			warned[name] = true
			log.Warn("deprecated environment variable; rename it", "old", "BRAIN_"+name, "new", "OPENBRAIN_"+name)
		}
		return v
	}
	return def
}

func dbPath() string { return env("DB", "openbrain.db") }

func openMigrated(ctx context.Context) (*store.Store, error) {
	st, err := store.Open(dbPath())
	if err != nil {
		return nil, err
	}
	from, to, err := st.Migrate(ctx)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if from == to {
		log.Info("database schema up to date", "db", dbPath(), "version", to)
	} else {
		log.Info("applied migrations", "db", dbPath(), "from", from, "to", to)
	}
	return st, nil
}

func serve() error {
	adminToken := env("TOKEN", "")
	if adminToken != "" && len(adminToken) < 32 {
		return errors.New("OPENBRAIN_TOKEN, if set, must be at least 32 characters (generate one with: openssl rand -hex 32)")
	}
	staleAfter, err := time.ParseDuration(env("STALE_AFTER", "1h"))
	if err != nil || staleAfter <= 0 {
		return fmt.Errorf("OPENBRAIN_STALE_AFTER must be a positive duration such as 30m or 2h, got %q", env("STALE_AFTER", ""))
	}
	base, err := web.NormalizeBasePath(env("BASE_PATH", "/"))
	if err != nil {
		return fmt.Errorf("OPENBRAIN_BASE_PATH: %w", err)
	}
	proxies, err := web.ParseTrustedProxies(env("TRUSTED_PROXIES", "127.0.0.1,::1"))
	if err != nil {
		return fmt.Errorf("OPENBRAIN_TRUSTED_PROXIES: %w", err)
	}
	addr := env("ADDR", "127.0.0.1:8080")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	st.StaleAfter = staleAfter

	srv := &http.Server{
		Handler: web.New(st, web.Config{
			BasePath: base, AdminToken: adminToken, TrustedProxies: proxies, Version: version, Log: log,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	shown := base
	if shown == "" {
		shown = "/"
	}
	users, _ := st.UserCount(ctx)
	log.Info("openbrain listening", "addr", ln.Addr().String(), "base_path", shown, "version", version,
		"admin_token", adminToken != "", "setup_pending", users == 0)
	if users == 0 {
		log.Info("no accounts yet: open " + strings.TrimSuffix(shown, "/") + "/setup in a browser to create the administrator")
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

func migrate(ctx context.Context) error {
	st, err := openMigrated(ctx)
	if err != nil {
		return err
	}
	return st.Close()
}

func backup(ctx context.Context, args []string) error {
	src := dbPath()
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("database: %w", err) // don't create an empty one
	}
	dst := filepath.Join(filepath.Dir(src), "backups", "openbrain-"+time.Now().UTC().Format("20060102T150405Z")+".db")
	if len(args) > 0 {
		dst = args[0]
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	st, err := store.Open(src)
	if err != nil {
		return err
	}
	defer st.Close()
	start := time.Now()
	if err := st.Backup(ctx, dst); err != nil {
		return err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return err
	}
	log.Info("backup written and verified", "file", dst, "bytes", fi.Size(), "dur", time.Since(start).Round(time.Millisecond))
	return nil
}

func restore(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: openbrain restore <backup-file>")
	}
	st, err := store.Open(dbPath())
	if err != nil {
		return err
	}
	defer st.Close()
	saved, err := st.Restore(ctx, args[0])
	if err != nil {
		return err
	}
	if saved != "" {
		log.Info("previous database saved", "file", saved)
	}
	from, to, err := st.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("restored, but migrating the restored database failed: %w", err)
	}
	log.Info("restore complete", "from", args[0], "db", dbPath(), "schema_from", from, "schema_to", to)
	return nil
}

// openAdmin opens the database for an administrative command, applying any
// pending migrations without the startup chatter of serve.
func openAdmin(ctx context.Context) (*store.Store, error) {
	st, err := store.Open(dbPath())
	if err != nil {
		return nil, err
	}
	if from, to, err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	} else if from != to {
		fmt.Fprintf(os.Stderr, "applied migrations %d -> %d\n", from, to)
	}
	return st, nil
}

// leadingArg splits "create claude --name X" style arguments: the flag
// package stops at the first positional argument, so take it off first.
func leadingArg(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// generatePassword returns a random password from an unambiguous alphabet,
// about 119 bits for 20 characters.
func generatePassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789-_"
	b := make([]byte, 20)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

func userCmd(ctx context.Context, args []string) error {
	sub, args := leadingArg(args)
	const help = "usage: openbrain user list | create <username> [--name NAME] [--admin] | reset-password <username> | disable <username> | enable <username>"
	if sub == "" {
		return errors.New(help)
	}
	st, err := openAdmin(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "list":
		users, err := st.Users(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tUSERNAME\tDISPLAY NAME\tROLE\tLAST SIGN-IN\tSTATUS")
		for _, u := range users {
			last, status := "never", "active"
			if !u.LastLoginAt.IsZero() {
				last = u.LastLoginAt.Format(time.DateTime)
			}
			if !u.DisabledAt.IsZero() {
				status = "disabled"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", u.ID, u.Username, u.Actor.DisplayName, u.Role, last, status)
		}
		return tw.Flush()

	case "create":
		username, rest := leadingArg(args)
		fs := flag.NewFlagSet("user create", flag.ContinueOnError)
		name := fs.String("name", "", "display name (default: the username)")
		admin := fs.Bool("admin", false, "make the account an administrator")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if username == "" {
			return errors.New("usage: openbrain user create <username> [--name NAME] [--admin]")
		}
		role := "user"
		if *admin {
			role = "admin"
		}
		password := generatePassword()
		u, err := st.CreateUser(ctx, store.UserInput{Username: username, DisplayName: *name, Password: password, Role: role})
		if err != nil {
			return err
		}
		log.Info("user created", "user_id", u.ID, "role", u.Role, "via", "cli")
		fmt.Fprintf(os.Stderr, "Created %s %q (%s). Their password is shown once, below; it is not stored.\nThey can change it under Account after signing in.\n\n", u.Role, u.Username, u.Actor.DisplayName)
		fmt.Println(password)
		return nil

	case "reset-password", "disable", "enable":
		username, _ := leadingArg(args)
		if username == "" {
			return errors.New(help)
		}
		u, err := st.UserByName(ctx, username)
		if err != nil {
			return err
		}
		switch sub {
		case "reset-password":
			password := generatePassword()
			if err := st.SetPassword(ctx, u.ID, password); err != nil {
				return err
			}
			log.Info("password reset", "user_id", u.ID, "via", "cli")
			fmt.Fprintf(os.Stderr, "New password for %q, shown once; their sessions were signed out:\n\n", u.Username)
			fmt.Println(password)
		case "disable":
			// From the shell (acting user 0) only the last-admin rule applies.
			if err := st.SetDisabled(ctx, 0, u.ID, true); err != nil {
				return err
			}
			fmt.Printf("Disabled %q and signed out their sessions.\n", u.Username)
		case "enable":
			if err := st.SetDisabled(ctx, 0, u.ID, false); err != nil {
				return err
			}
			fmt.Printf("Enabled %q.\n", u.Username)
		}
		return nil
	}
	return errors.New(help)
}

func actorCmd(ctx context.Context, args []string) error {
	sub, args := leadingArg(args)
	switch sub {
	case "list":
		st, err := openAdmin(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		actors, err := st.Actors(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "SLUG\tNAME\tKIND\tCREATED")
		for _, a := range actors {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Slug, a.DisplayName, a.Kind, a.CreatedAt.Format(time.DateOnly))
		}
		return tw.Flush()

	case "create":
		slug, rest := leadingArg(args)
		fs := flag.NewFlagSet("actor create", flag.ContinueOnError)
		name := fs.String("name", "", "display name (default: the slug)")
		kind := fs.String("kind", "agent", "agent, human or automation")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if slug == "" {
			return errors.New("usage: openbrain actor create <slug> [--name NAME] [--kind agent|human|automation]")
		}
		st, err := openAdmin(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		a, err := st.CreateActor(ctx, store.ActorInput{Slug: slug, DisplayName: *name, Kind: *kind})
		if errors.Is(err, store.ErrConflict) {
			if a, err = st.Actor(ctx, slug); err != nil {
				return err
			}
			fmt.Printf("Actor %q already exists (%s, %s); nothing changed.\n", a.Slug, a.DisplayName, a.Kind)
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("Created actor %q (%s, %s).\nNext: openbrain token create --actor %s --label <machine-name>\n", a.Slug, a.DisplayName, a.Kind, a.Slug)
		return nil
	}
	return errors.New("usage: openbrain actor create <slug> [--name NAME] [--kind KIND] | openbrain actor list")
}

func tokenCmd(ctx context.Context, args []string) error {
	sub, args := leadingArg(args)
	switch sub {
	case "create":
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		actor := fs.String("actor", "", "slug of the actor the token writes as (required)")
		label := fs.String("label", "", "where the token is used, e.g. laptop")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *actor == "" {
			return errors.New("usage: openbrain token create --actor SLUG [--label LABEL]")
		}
		st, err := openAdmin(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		t, secret, err := st.CreateToken(ctx, *actor, *label)
		var ve *store.ValidationError
		if errors.As(err, &ve) && ve.Field == "actor" {
			return fmt.Errorf("%w; create it first: openbrain actor create %s", err, *actor)
		}
		if err != nil {
			return err
		}
		log.Info("token created", "token_id", t.ID, "actor", t.Actor.Slug, "via", "cli")
		fmt.Fprintf(os.Stderr, "Created token #%d for actor %q. It is shown once, below; it is not stored.\n"+
			"Give it to the agent through an environment variable (e.g. OPENBRAIN_MCP_TOKEN on the agent's machine), never a committed file.\n"+
			"MCP endpoint: <your OpenBrain URL>/mcp  with header  Authorization: Bearer <token>\n\n", t.ID, t.Actor.Slug)
		fmt.Println(secret)
		return nil

	case "list":
		fs := flag.NewFlagSet("token list", flag.ContinueOnError)
		all := fs.Bool("all", false, "include revoked tokens")
		if err := fs.Parse(args); err != nil {
			return err
		}
		st, err := openAdmin(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		tokens, err := st.Tokens(ctx, *all)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tACTOR\tTOKEN\tLABEL\tCREATED\tLAST USED\tSTATUS")
		for _, t := range tokens {
			used, status := "never", "active"
			if !t.LastUsedAt.IsZero() {
				used = t.LastUsedAt.Format(time.DateTime)
			}
			if !t.RevokedAt.IsZero() {
				status = "revoked " + t.RevokedAt.Format(time.DateOnly)
			}
			fmt.Fprintf(tw, "%d\t%s\t%s…\t%s\t%s\t%s\t%s\n", t.ID, t.Actor.Slug, t.Prefix, t.Label, t.CreatedAt.Format(time.DateOnly), used, status)
		}
		return tw.Flush()

	case "revoke":
		idStr, _ := leadingArg(args)
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return errors.New("usage: openbrain token revoke <id>  (IDs are shown by token list)")
		}
		st, err := openAdmin(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		t, err := st.RevokeToken(ctx, id)
		if err != nil {
			return err
		}
		log.Info("token revoked", "token_id", t.ID, "actor", t.Actor.Slug, "via", "cli")
		fmt.Printf("Token #%d (%s…, actor %s) is revoked; requests using it are rejected immediately.\n", t.ID, t.Prefix, t.Actor.Slug)
		return nil
	}
	return errors.New("usage: openbrain token create --actor SLUG [--label LABEL] | token list [--all] | token revoke <id>")
}
