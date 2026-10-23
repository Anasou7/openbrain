// Package mcpserver exposes OpenBrain to AI agents over the Model Context
// Protocol (Streamable HTTP). It is a thin adapter: every tool calls one
// operation in package store, and the caller's actor token, never a tool
// argument, decides whom a write is attributed to.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"openbrain/internal/store"
)

// instructions is sent to clients on discovery; models read it to learn the
// intended workflow.
const instructions = `OpenBrain is the shared memory a person and their AI agents use to coordinate work on projects. It holds three kinds of knowledge:
- the project profile: what the product is and does, for people who never see the code (get_project_profile)
- current state: where the work stands (get_project_context)
- change history: what meaningfully changed (get_project_updates)

When asked to fully add a project to OpenBrain or to build its profile, inspect the repository and its docs deeply, then call sync_project_profile. Record only what the repository supports; mark the rest unknown.

Workflow:
1. At the start of a session, identify the project with find_project (pass the repository's git remote URL, or a project slug), then call get_project_context and continue from the latest handoffs, decisions and blockers. Read it even if you worked on the project before: others may have changed it since. Then set_agent_state to working with a short task.
2. While working, record only durable information: decisions (record_decision), blockers (raise_blocker, resolve_blocker), notes (add_note) and state changes (set_agent_state).
3. When you finish or pause meaningful work, call record_handoff with what changed, where things stand, next steps, and the git branch and commit, and leave your state accurate (finished, idle, blocked or waiting). If something meaningful changed for the project owner, include user_update: a plain-English title and summary of the outcome, not the task status. For a milestone during a long session, use publish_project_update.

If your work changes a durable product fact (a feature, limitation, integration, supported platform, onboarding or pricing), update the profile too, with update_project_knowledge or upsert_project_feature; not for ordinary bug fixes.

In short: read once near the start, write durable context when useful, always hand off after meaningful work, and publish a human-facing update only when there is something worth telling the project owner.

For questions across projects ("what is blocked?", "where did each agent leave off?", "what changed recently?") use get_overview; prefer project updates over raw activity when saying what changed.

Your identity comes from your credential; you never name yourself. Record results and summaries, never private reasoning, chain-of-thought or transcripts.`

// New returns the handler for /mcp. Every request must carry an actor token
// (openbrain token create); the admin token is not accepted here because
// MCP writes always need an actor identity.
func New(st *store.Store, version string, log *slog.Logger) http.Handler {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "openbrain", Title: "OpenBrain", Version: version},
		&mcp.ServerOptions{Instructions: instructions, Logger: log},
	)
	(&tools{st: st, log: log}).register(server)

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		// Stateless serves the stateless 2026-07-28 protocol and still
		// answers the legacy initialize handshake for older clients. Nothing
		// is kept between requests, so memory does not grow with clients.
		Stateless:    true,
		JSONResponse: true,
		// The SDK's DNS-rebinding guard rejects proxied requests (reverse
		// proxy, Tailscale Serve), which arrive on loopback with a public
		// Host header. A rebinding attack cannot supply the bearer token
		// required below, so the guard would only break legitimate setups.
		DisableLocalhostProtection:   true,
		MaxRequestBodyBytes:          1 << 20,
		PropagateRequestCancellation: true,
	})

	verify := func(ctx context.Context, secret string, _ *http.Request) (*auth.TokenInfo, error) {
		t, err := st.Authenticate(ctx, secret)
		if errors.Is(err, store.ErrInvalidToken) {
			return nil, fmt.Errorf("%w: send an actor token (openbrain token create)", auth.ErrInvalidToken)
		}
		if err != nil {
			return nil, err
		}
		return &auth.TokenInfo{UserID: t.Actor.Slug, Extra: map[string]any{"token_id": t.ID}}, nil
	}
	authed := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(h)

	// The spec requires Origin validation. CrossOriginProtection rejects
	// cross-site browser requests and lets non-browser clients through.
	return http.NewCrossOriginProtection().Handler(challenge(authed))
}

// challenge adds the WWW-Authenticate header to 401 responses, which the SDK
// only sets when OAuth metadata is configured.
func challenge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&challengeWriter{w}, r)
	})
}

type challengeWriter struct{ http.ResponseWriter }

func (w *challengeWriter) WriteHeader(code int) {
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="openbrain"`)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *challengeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *challengeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
