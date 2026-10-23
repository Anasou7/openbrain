# Contributing

Thanks for helping. OpenBrain is meant to stay small: one binary, one
SQLite file, a handful of dependencies, and a domain anyone can hold in their
head. Changes that keep it that way are the easiest to accept.

## Development

```sh
go test ./...          # all tests; they use temporary databases
go vet ./...
gofmt -l .             # must print nothing

OPENBRAIN_DB=dev.db go run ./cmd/openbrain serve   # then open http://127.0.0.1:8080/setup
OPENBRAIN_DB=dev.db OPENBRAIN_BASE_PATH=/openbrain go run ./cmd/openbrain serve   # test a subpath
```

## Layout

- `cmd/openbrain`: CLI and configuration.
- `internal/store`: all SQL, validation and domain operations. If it touches
  the database, it belongs here.
- `internal/web`: the HTTP API, sign-in, dashboard handlers, templates and
  static files. No SQL. Every URL it emits goes through `s.url` (the `u`
  template function) so the base path is always honoured.
- `internal/mcpserver`: MCP tools. Each tool is a thin call into
  `internal/store`. No SQL, and no rules that the REST API wouldn't also
  enforce.

Go 1.25 or newer is required, because the MCP SDK needs it.

## Ground rules

- **Migrations are append-only.** Add `internal/store/migrations/NNNN_name.sql`
  with the next number. Never edit one that has been released.
- **Every write is attributed and logged.** New write operations take an
  `Attribution` and append an `activity` row in the same transaction. Log
  meaningful changes, not heartbeats.
- **No new dependencies** without discussing it in an issue first. The standard
  library covers most needs.
- **No hardcoded agents, people, hosts or paths.** Agents are rows in `actors`.
- **Identity comes from the credential.** Never add an `actor` (or similar)
  argument to an MCP tool. On the REST API, actor tokens must keep writing
  only as themselves.
- **Tool descriptions are product.** Say when to use a tool, not just what it
  does. Never invite models to store private reasoning or transcripts.
- **Keep the visual style consistent.** Use the CSS tokens in
  `style.css`, keep status readable without colour, and don't add a
  JavaScript framework or external assets.
- **Never store or log secrets.** Passwords are Argon2id hashes, session and
  actor tokens are SHA-256 hashes, and none of them appear in logs.
- **Test behaviour, not implementation.** Store tests exercise the semantic
  operations; web tests go through the HTTP handler.

## Scope

In scope: making shared project state between people and agents more useful,
reliable and safe.

Out of scope for the core:
- LLM calls
- embeddings or vector search
- repository scanning or sync
- background workers
- agent-specific integrations (one MCP server serves every client)
