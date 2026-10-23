package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"openbrain/internal/store"
)

type tools struct {
	st  *store.Store
	log *slog.Logger
}

// typeSchemas tells schema inference how store types marshal, so tool
// outputs can reuse the same types as the REST API.
var typeSchemas = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[store.Time]():      {Type: "string", Format: "date-time"},
	reflect.TypeFor[json.RawMessage](): {Type: "object"},
}

// add registers a tool with inferred input and output schemas, constraining
// the named input fields (dotted for nested ones, e.g. "user_update.category";
// array items are stepped into, so "sections.status") to fixed values.
func add[In, Out any](s *mcp.Server, t *mcp.Tool, enums map[string][]string, h mcp.ToolHandlerFor[In, Out]) {
	in, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s input schema: %v", t.Name, err))
	}
	for path, values := range enums {
		field := in
		for _, name := range strings.Split(path, ".") {
			if field.Items != nil {
				field = field.Items
			}
			field = field.Properties[name]
		}
		for _, v := range values {
			field.Enum = append(field.Enum, v)
		}
	}
	out, err := jsonschema.For[Out](&jsonschema.ForOptions{TypeSchemas: typeSchemas})
	if err != nil {
		panic(fmt.Sprintf("tool %s output schema: %v", t.Name, err))
	}
	t.InputSchema, t.OutputSchema = in, out
	mcp.AddTool(s, t, h)
}

func ptr[T any](v T) *T { return &v }

var (
	closedWorld = ptr(false)
	readOnly    = func(title string) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: closedWorld}
	}
	// additive tools only create records.
	additive = func(title string) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(false), OpenWorldHint: closedWorld}
	}
	// updating tools change existing values; repeating a call changes nothing more.
	updating = func(title string) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: closedWorld}
	}
)

func (t *tools) register(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "list_projects",
		Annotations: readOnly("List projects"),
		Description: "List the projects in OpenBrain with their status, current focus and repository. " +
			"Use it to browse what exists, or when find_project cannot identify the project you are in. " +
			"Archived projects are hidden unless include_archived is true.",
	}, map[string][]string{"status": store.ProjectStatuses}, t.listProjects)

	add(s, &mcp.Tool{
		Name:        "find_project",
		Annotations: readOnly("Find project"),
		Description: "Identify which OpenBrain project you are working on. At the start of a session in a git repository, " +
			"pass the repository's remote URL (output of `git remote get-url origin`); SSH and HTTPS forms of the same remote match. " +
			"You can pass a project ID or slug instead. Returns status found, not_found or ambiguous. " +
			"On not_found, ask the user before calling register_project.",
	}, nil, t.findProject)

	add(s, &mcp.Tool{
		Name:        "get_project_context",
		Annotations: readOnly("Get project context"),
		Description: "Retrieve the shared state of a project before starting or resuming work: current focus, pinned and recent human notes, " +
			"the latest handoff from every person and agent, recent project updates, current agent states, open blockers, recent decisions and recent activity. " +
			"Call it after find_project at the start of every session, even if you worked on the project before, so you continue from where others left off and respect earlier decisions. " +
			"Compact by default; pass compact=false only when you need older notes and untruncated text.",
	}, nil, t.getContext)

	add(s, &mcp.Tool{
		Name:        "get_overview",
		Annotations: readOnly("Get overview"),
		Description: "Get a compact overview of all projects and where each person and agent left them: status, focus, open blockers, " +
			"each participant's state, latest project update, handoff or note, and the most recent project updates across all projects. " +
			"Use it for cross-project questions such as \"what am I working on?\", \"what is blocked?\", \"where did each agent leave things?\" " +
			"or \"what changed recently?\" (answer those from the project updates, which say what changed in plain English, rather than raw activity). " +
			"For one project's details use get_project_context.",
	}, nil, t.getOverview)

	add(s, &mcp.Tool{
		Name:        "register_project",
		Annotations: additive("Register project"),
		Description: "Register a new project in OpenBrain. Only do this when find_project returned not_found and the user wants the project tracked. " +
			"If a project with the same repository already exists, it is returned instead of creating a duplicate (created=false). " +
			"You are recorded as the creator automatically.",
	}, nil, t.registerProject)

	add(s, &mcp.Tool{
		Name:        "update_project",
		Annotations: updating("Update project"),
		Description: "Change a project's operational metadata: status, current focus, repository URL, default branch or description. " +
			"Use it when the focus genuinely shifts or the status changes (e.g. to paused or done); routine progress belongs in record_handoff. " +
			"Only the fields you pass are changed.",
	}, map[string][]string{"status": store.ProjectStatuses}, t.updateProject)

	add(s, &mcp.Tool{
		Name:        "add_note",
		Annotations: additive("Add note"),
		Description: "Add a durable note to a project, attributed to you. Use a note for information that will still be useful later and is not " +
			"better recorded as a handoff (where you left off), a decision (a choice to respect) or a blocker (something in the way): " +
			"for example a gotcha, an environment detail or a useful link. Do not record your reasoning, transcripts or every command you ran.",
	}, nil, t.addNote)

	add(s, &mcp.Tool{
		Name:        "record_handoff",
		Annotations: additive("Record handoff"),
		Description: "Record where you are leaving the project so humans and other agents can continue later. This is the main way to share your work. " +
			"Use it after completing a meaningful work session, when pausing work, or before another agent is expected to continue. " +
			"Do not use it after every tiny edit. Include what you did and where it stands, next steps, blockers, and the git branch and commit " +
			"(`git rev-parse --abbrev-ref HEAD`, `git rev-parse HEAD`). Pass set_state too, so you are not left shown as working. " +
			"The handoff is written for the next agent. If something meaningful changed for the project owner, also pass user_update: " +
			"a plain-English title and summary of the outcome, as in publish_project_update. Leave it out when there is nothing worth telling them. " +
			"If the work changed a durable product fact (a feature, limitation, integration, supported platform, onboarding or pricing), also update the profile " +
			"with update_project_knowledge or upsert_project_feature, so it does not contradict the product. " +
			"Earlier handoffs are kept, never overwritten. Summarise results; never include private reasoning.",
	}, map[string][]string{"set_state": store.StateValues, "user_update.category": store.UpdateCategories}, t.recordHandoff)

	add(s, &mcp.Tool{
		Name:        "publish_project_update",
		Annotations: additive("Publish project update"),
		Description: "Record a meaningful, human-facing project change: a short update for the project owner, not another developer " +
			"(record_handoff is for the next agent). Explain what changed and why it matters in plain English with minimal technical jargon; " +
			"describe the outcome, not the task status. Good: title \"Login is now faster\", summary \"The delay noticed during task 3 is fixed, " +
			"so signing in should feel immediate.\" Bad: \"Task 3 completed\" or \"Refactored the session cache\". " +
			"Use it when a meaningful outcome is reached, e.g. a milestone during a long session; at the end of a session you can pass user_update " +
			"to record_handoff instead. Do not use it for file edits, shell commands, trivial refactors, routine test runs or formatting, " +
			"and never include private reasoning. If there is nothing worth telling the owner, publish nothing. " +
			"Updates are permanent: to correct one, publish a newer update.",
	}, map[string][]string{"category": store.UpdateCategories}, t.publishUpdate)

	add(s, &mcp.Tool{
		Name:        "get_project_updates",
		Annotations: readOnly("Get project updates"),
		Description: "Retrieve a project's updates: meaningful, human-readable changes written for the project owner, newest first, with who published each and when. " +
			"Prefer this over raw activity when answering \"what changed?\" or looking for changes worth announcing. " +
			"Filter by category or by_actor; to page back further, pass next_before from the previous result as before.",
	}, map[string][]string{"category": store.UpdateCategories}, t.getUpdates)

	add(s, &mcp.Tool{
		Name:        "get_project_history",
		Annotations: readOnly("Get project history"),
		Description: "Page through a project's full history of handoffs, decisions, notes or activity, newest first, beyond what get_project_context includes. " +
			"Use it when you need older detail; to continue, pass next_before from the previous result as before. For what changed, use get_project_updates.",
	}, map[string][]string{"kind": historyKinds}, t.getHistory)

	add(s, &mcp.Tool{
		Name:        "get_project_profile",
		Annotations: readOnly("Get project profile"),
		Description: "Retrieve durable product and business knowledge about a project: what it is, who it is for, the problems it solves, " +
			"its features, use cases, positioning, integrations, limitations, terminology, FAQ and any custom sections, each with who last updated it and when. " +
			"Use it when you need to understand the project itself rather than its current development state (get_project_context) or recent changes (get_project_updates). " +
			"With no section or feature it returns the complete profile, unclipped; pass section for one knowledge section, feature for one feature, " +
			"or include_sections=false to list just the features. Answer only from what it contains: a section marked unknown, or one that is missing, " +
			"means OpenBrain has no verified information, so say that rather than guessing.",
	}, nil, t.getProfile)

	add(s, &mcp.Tool{
		Name:        "sync_project_profile",
		Annotations: updating("Sync project profile"),
		Description: "Create or refresh a project's comprehensive profile after deeply inspecting the local repository: README and docs, the user-facing app " +
			"(routes, pages, CLI, API), configuration, schema, integrations, examples, deployment files and product copy. Capture, in plain English, what someone " +
			"who never sees the code needs: what the product is and does, who it is for, the problems it solves, every meaningful user-facing feature (from the " +
			"user's point of view), use cases, workflows, integrations, positioning, terminology, limitations and known issues. Sections are upserted by key and " +
			"features by slug; anything you omit is left unchanged, nothing is deleted. Use canonical keys where they fit (" + strings.Join(store.CanonicalKeys(), ", ") +
			") or your own. Accuracy beats completeness: never invent pricing, audiences, competitors or claims the repository does not support; set such a " +
			"section's status to unknown (or not_applicable) instead. Do not store source code, file-by-file internals or dependency lists. Pass source_branch and source_commit.",
	}, map[string][]string{"sections.status": store.SectionStatuses, "features.status": store.FeatureStatuses}, t.syncProfile)

	add(s, &mcp.Tool{
		Name:        "update_project_knowledge",
		Annotations: updating("Update project knowledge"),
		Description: "Create or replace one durable knowledge section of a project's profile when a lasting product fact changes: pricing, target market, " +
			"supported platforms, onboarding, an integration, a limitation removed, terminology. Write the section's complete new content in plain English; " +
			"it replaces the old text. Not for bug fixes or implementation-only changes (those belong in handoffs and, if they matter to the owner, project updates). " +
			"Set status to unknown rather than guessing.",
	}, map[string][]string{"status": store.SectionStatuses}, t.updateKnowledge)

	add(s, &mcp.Tool{
		Name:        "upsert_project_feature",
		Annotations: updating("Upsert project feature"),
		Description: "Add or update one user-facing feature in a project's profile, identified by slug: when a feature is added, meaningfully changes, " +
			"gains or loses a limitation, or is deprecated or removed (set status; features are never deleted). Describe it from the user's point of view: " +
			"what it does, who it helps, use cases, limitations. Only the fields you pass change. Not for implementation-only changes.",
	}, map[string][]string{"status": store.FeatureStatuses}, t.upsertFeature)

	add(s, &mcp.Tool{
		Name:        "set_agent_state",
		Annotations: updating("Set agent state"),
		Description: "Set your own working state on a project (idle, working, blocked, waiting or finished) and optionally your current task, " +
			"so others can see what you are doing right now. Call it when that changes (starting work, becoming blocked or waiting, finishing), " +
			"not after every step, and never stop while still shown as working. " +
			"Calling it again with the same values refreshes it; a busy state that is not refreshed is eventually shown as stale. " +
			"Nothing refreshes it automatically.",
	}, map[string][]string{"state": store.StateValues}, t.setState)

	add(s, &mcp.Tool{
		Name:        "record_decision",
		Annotations: additive("Record decision"),
		Description: "Record a durable project decision that future agents and humans should respect, with the reasoning. " +
			"Use it for choices worth preserving across sessions (for example \"use SQLite, not PostgreSQL\" or \"production deploys need manual approval\"), " +
			"not for casual comments or intermediate thoughts.",
	}, nil, t.recordDecision)

	add(s, &mcp.Tool{
		Name:        "raise_blocker",
		Annotations: additive("Raise blocker"),
		Description: "Record something that blocks progress on a project and needs attention, such as a missing credential, a broken dependency " +
			"or a question only the user can answer. Not for ordinary to-dos. Check open_blockers in get_project_context first to avoid duplicates, " +
			"and call resolve_blocker once it is cleared.",
	}, nil, t.raiseBlocker)

	add(s, &mcp.Tool{
		Name:        "resolve_blocker",
		Annotations: updating("Resolve blocker"),
		Description: "Mark an open blocker as resolved, recording you as the resolver and optionally how it was resolved. " +
			"Blocker IDs are in open_blockers from get_project_context. Resolving an already-resolved blocker changes nothing.",
	}, nil, t.resolveBlocker)
}

// caller is the authenticated actor. The bearer middleware guarantees it is
// set; tool inputs never carry an actor.
func caller(req *mcp.CallToolRequest) (store.Attribution, error) {
	if req.Extra == nil || req.Extra.TokenInfo == nil || req.Extra.TokenInfo.UserID == "" {
		return store.Attribution{}, errors.New("unauthenticated request")
	}
	return store.Attribution{Actor: req.Extra.TokenInfo.UserID, Via: "mcp"}, nil
}

// fail turns a store error into a message the model can act on. Unexpected
// errors are logged and not described to the client.
func (t *tools) fail(tool string, err error) error {
	var ve *store.ValidationError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%w (find_project, list_projects and get_project_context return valid IDs)", err)
	case errors.As(err, &ve), errors.Is(err, store.ErrConflict):
		return err
	}
	t.log.Error("mcp tool failed", "tool", tool, "err", err)
	return errors.New("internal error; see the OpenBrain server log")
}

// wrote logs a successful write, or explains a failed one.
func (t *tools) wrote(tool string, by store.Attribution, err error) error {
	if err != nil {
		return t.fail(tool, err)
	}
	t.log.Info("mcp write", "tool", tool, "actor", by.Actor)
	return nil
}

// --- shapes ---

// projectSummary is the compact project shape used in lists and lookups.
type projectSummary struct {
	ID            int64      `json:"id"`
	Slug          string     `json:"slug"`
	Name          string     `json:"name"`
	Status        string     `json:"status"`
	CurrentFocus  string     `json:"current_focus,omitempty"`
	RepoURL       string     `json:"repo_url,omitempty"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	OpenBlockers  int        `json:"open_blockers"`
	UpdatedAt     store.Time `json:"updated_at" jsonschema:"time of the last activity on the project"`
}

func summarize(p store.Project) projectSummary {
	updated := p.LastActivityAt
	if updated.IsZero() {
		updated = p.UpdatedAt
	}
	return projectSummary{ID: p.ID, Slug: p.Slug, Name: p.Name, Status: p.Status, CurrentFocus: clip(p.CurrentFocus, 300),
		RepoURL: p.RepoURL, DefaultBranch: p.DefaultBranch, OpenBlockers: p.OpenBlockers, UpdatedAt: updated}
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// --- read tools ---

type listIn struct {
	Status          string `json:"status,omitempty" jsonschema:"only projects with this status"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"include archived projects (default false)"`
}

type listOut struct {
	Projects []projectSummary `json:"projects"`
}

func (t *tools) listProjects(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
	projects, err := t.st.Projects(ctx, in.IncludeArchived || in.Status == "archived")
	if err != nil {
		return nil, listOut{}, t.fail("list_projects", err)
	}
	out := listOut{Projects: []projectSummary{}}
	for _, p := range projects {
		if in.Status == "" || p.Status == in.Status {
			out.Projects = append(out.Projects, summarize(p))
		}
	}
	return nil, out, nil
}

type findIn struct {
	Project string `json:"project,omitempty" jsonschema:"project ID or slug, if known"`
	RepoURL string `json:"repo_url,omitempty" jsonschema:"git remote URL of the repository you are working in, e.g. from git remote get-url origin"`
}

type findOut struct {
	Status     string           `json:"status" jsonschema:"found, not_found or ambiguous"`
	Project    *projectSummary  `json:"project,omitempty" jsonschema:"the project, when status is found"`
	Candidates []projectSummary `json:"candidates,omitempty" jsonschema:"the matching projects, when status is ambiguous"`
	Message    string           `json:"message" jsonschema:"what to do next"`
}

func (t *tools) findProject(ctx context.Context, _ *mcp.CallToolRequest, in findIn) (*mcp.CallToolResult, findOut, error) {
	ref, repo := strings.TrimSpace(in.Project), strings.TrimSpace(in.RepoURL)
	if ref == "" && repo == "" {
		return nil, findOut{}, errors.New("pass project (an ID or slug) or repo_url")
	}
	if ref != "" {
		p, err := t.st.Project(ctx, ref)
		if errors.Is(err, store.ErrNotFound) {
			return nil, findOut{Status: "not_found", Message: fmt.Sprintf("No project has ID or slug %q. Use list_projects to see what exists.", ref)}, nil
		}
		if err != nil {
			return nil, findOut{}, t.fail("find_project", err)
		}
		msg := "Found. Call get_project_context next."
		if repo != "" && store.RepoKey(p.RepoURL) != store.RepoKey(repo) {
			msg = "Found by ID or slug, but its repository URL differs from repo_url; confirm with the user that this is the right project."
		}
		s := summarize(p)
		return nil, findOut{Status: "found", Project: &s, Message: msg}, nil
	}

	matches, err := t.st.ProjectsByRepo(ctx, repo)
	if err != nil {
		return nil, findOut{}, t.fail("find_project", err)
	}
	switch len(matches) {
	case 0:
		return nil, findOut{Status: "not_found", Message: "No project tracks this repository. If the user wants it tracked, call register_project with this repo_url."}, nil
	case 1:
		s := summarize(matches[0])
		return nil, findOut{Status: "found", Project: &s, Message: "Found. Call get_project_context next."}, nil
	}
	out := findOut{Status: "ambiguous", Message: "Several projects track this repository. Ask the user which one, then pass its slug as project."}
	for _, p := range matches {
		out.Candidates = append(out.Candidates, summarize(p))
	}
	return nil, out, nil
}

type contextIn struct {
	Project       string `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Compact       *bool  `json:"compact,omitempty" jsonschema:"default true: fewer notes and decisions, long text clipped. false returns more history and full text"`
	ActivityLimit int    `json:"activity_limit,omitempty" jsonschema:"number of recent activity entries (default 10 compact, 25 full, max 200)"`
}

func (t *tools) getContext(ctx context.Context, _ *mcp.CallToolRequest, in contextIn) (*mcp.CallToolResult, store.ProjectContext, error) {
	compact := in.Compact == nil || *in.Compact
	c, err := t.st.ProjectContext(ctx, in.Project, store.ContextOptions{Compact: compact, ActivityLimit: max(in.ActivityLimit, 0)})
	if err != nil {
		return nil, store.ProjectContext{}, t.fail("get_project_context", err)
	}
	return nil, c, nil
}

type overviewIn struct {
	IncludeArchived bool `json:"include_archived,omitempty" jsonschema:"include archived projects (default false)"`
}

type overviewOut struct {
	Actors        []store.ActorRef  `json:"actors" jsonschema:"everyone with a column in the overview: people, and every agent that has worked on a listed project"`
	Projects      []overviewProject `json:"projects"`
	RecentUpdates []updateBrief     `json:"recent_updates" jsonschema:"the newest project updates across all projects, newest first: what meaningfully changed recently"`
}

type overviewProject struct {
	Project      projectSummary          `json:"project"`
	LatestUpdate *updateBrief            `json:"latest_update,omitempty" jsonschema:"the project's newest update, if any"`
	Actors       map[string]overviewCell `json:"actors" jsonschema:"latest position of each actor on this project, keyed by actor slug"`
}

// updateBrief is a project update clipped for the cross-project overview.
type updateBrief struct {
	Project   string     `json:"project" jsonschema:"project slug"`
	Actor     string     `json:"actor" jsonschema:"slug of the actor who published it"`
	Title     string     `json:"title"`
	Summary   string     `json:"summary" jsonschema:"clipped; get_project_context has the full text"`
	Category  string     `json:"category"`
	CreatedAt store.Time `json:"created_at"`
}

func brief(u store.ProjectUpdate) updateBrief {
	return updateBrief{Project: u.ProjectSlug, Actor: u.Actor.Slug, Title: u.Title, Summary: clip(u.Summary, 280), Category: u.Category, CreatedAt: u.CreatedAt}
}

type overviewCell struct {
	State       string     `json:"state,omitempty"`
	Stale       bool       `json:"stale,omitempty" jsonschema:"true when a busy state has not been refreshed recently"`
	CurrentTask string     `json:"current_task,omitempty"`
	LastUpdate  string     `json:"last_update,omitempty" jsonschema:"title of the actor's latest project update: the last meaningful thing it achieved here"`
	LastHandoff string     `json:"last_handoff,omitempty" jsonschema:"summary of the actor's latest handoff (clipped)"`
	NextSteps   string     `json:"next_steps,omitempty" jsonschema:"next steps from that handoff (clipped)"`
	LastNote    string     `json:"last_note,omitempty" jsonschema:"the actor's latest note (clipped)"`
	UpdatedAt   store.Time `json:"updated_at,omitzero" jsonschema:"when the actor last reported anything on this project"`
}

func (t *tools) getOverview(ctx context.Context, _ *mcp.CallToolRequest, in overviewIn) (*mcp.CallToolResult, overviewOut, error) {
	o, err := t.st.Overview(ctx, in.IncludeArchived)
	if err != nil {
		return nil, overviewOut{}, t.fail("get_overview", err)
	}
	recent, err := t.st.RecentUpdates(ctx, 10, false)
	if err != nil {
		return nil, overviewOut{}, t.fail("get_overview", err)
	}
	out := overviewOut{Actors: o.Actors, Projects: []overviewProject{}, RecentUpdates: []updateBrief{}}
	for _, u := range recent {
		out.RecentUpdates = append(out.RecentUpdates, brief(u))
	}
	for _, row := range o.Projects {
		p := overviewProject{Project: summarize(row.Project), Actors: map[string]overviewCell{}}
		if u := row.LatestUpdate; u != nil {
			b := brief(*u)
			p.LatestUpdate = &b
		}
		for slug, c := range row.Actors {
			var v overviewCell
			latest := func(at store.Time) {
				if at.After(v.UpdatedAt.Time) {
					v.UpdatedAt = at
				}
			}
			// Clipped hard: this spans every project and actor, and agents
			// call get_project_context for detail.
			if c.State != nil {
				v.State, v.Stale, v.CurrentTask = c.State.State, c.State.Stale, clip(c.State.CurrentTask, 120)
				latest(c.State.UpdatedAt)
			}
			if c.Update != nil {
				v.LastUpdate = clip(c.Update.Title, 200)
				latest(c.Update.CreatedAt)
			}
			if c.Handoff != nil {
				v.LastHandoff, v.NextSteps = clip(c.Handoff.Summary, 200), clip(c.Handoff.NextSteps, 160)
				latest(c.Handoff.CreatedAt)
			}
			if c.Note != nil {
				v.LastNote = clip(c.Note.Content, 120)
				latest(c.Note.CreatedAt)
			}
			p.Actors[slug] = v
		}
		out.Projects = append(out.Projects, p)
	}
	return nil, out, nil
}

// --- write tools ---

type registerIn struct {
	Name           string `json:"name" jsonschema:"human-readable project name"`
	Slug           string `json:"slug,omitempty" jsonschema:"short lowercase handle; derived from the name if omitted"`
	Description    string `json:"description,omitempty"`
	RepoURL        string `json:"repo_url,omitempty" jsonschema:"git remote URL (without credentials); used to find the project later"`
	DefaultBranch  string `json:"default_branch,omitempty"`
	CurrentFocus   string `json:"current_focus,omitempty" jsonschema:"what the project is focused on right now"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional unique string you generate (e.g. a UUID). Retrying a call with the same key returns the original record instead of creating a duplicate."`
}

type registerOut struct {
	Created bool          `json:"created" jsonschema:"false if an existing project was returned instead"`
	Project store.Project `json:"project"`
	Message string        `json:"message"`
}

func (t *tools) registerProject(ctx context.Context, req *mcp.CallToolRequest, in registerIn) (*mcp.CallToolResult, registerOut, error) {
	by, err := caller(req)
	if err != nil {
		return nil, registerOut{}, err
	}
	p, created, err := t.st.RegisterProject(ctx, store.ProjectInput{
		Attribution: by, Name: in.Name, Slug: in.Slug, Description: in.Description, RepoURL: in.RepoURL,
		DefaultBranch: in.DefaultBranch, CurrentFocus: in.CurrentFocus, IdempotencyKey: in.IdempotencyKey,
	})
	if err := t.wrote("register_project", by, err); err != nil {
		return nil, registerOut{}, err
	}
	msg := "Registered. Call get_project_context to see its state."
	if !created {
		msg = "This project already exists (same repository, or a retried call), so nothing new was created."
	}
	return nil, registerOut{Created: created, Project: p, Message: msg}, nil
}

type updateIn struct {
	Project       string  `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Status        *string `json:"status,omitempty"`
	CurrentFocus  *string `json:"current_focus,omitempty" jsonschema:"new focus; an empty string clears it"`
	RepoURL       *string `json:"repo_url,omitempty" jsonschema:"git remote URL, without credentials"`
	DefaultBranch *string `json:"default_branch,omitempty"`
	Description   *string `json:"description,omitempty"`
}

func (t *tools) updateProject(ctx context.Context, req *mcp.CallToolRequest, in updateIn) (*mcp.CallToolResult, store.Project, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.Project{}, err
	}
	if in.Status == nil && in.CurrentFocus == nil && in.RepoURL == nil && in.DefaultBranch == nil && in.Description == nil {
		return nil, store.Project{}, errors.New("pass at least one of status, current_focus, repo_url, default_branch, description")
	}
	p, err := t.st.UpdateProject(ctx, in.Project, store.ProjectPatch{
		Attribution: by, Status: in.Status, CurrentFocus: in.CurrentFocus, RepoURL: in.RepoURL,
		DefaultBranch: in.DefaultBranch, Description: in.Description,
	})
	return nil, p, t.wrote("update_project", by, err)
}

type noteIn struct {
	Project        string `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Content        string `json:"content" jsonschema:"the note text"`
	Category       string `json:"category,omitempty" jsonschema:"optional lowercase category such as idea, warning, ops or link (default note)"`
	Pinned         bool   `json:"pinned,omitempty" jsonschema:"pin it so it appears at the top of the project context; use sparingly"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional unique string you generate (e.g. a UUID). Retrying a call with the same key returns the original record instead of creating a duplicate."`
}

func (t *tools) addNote(ctx context.Context, req *mcp.CallToolRequest, in noteIn) (*mcp.CallToolResult, store.Note, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.Note{}, err
	}
	n, err := t.st.AddNote(ctx, in.Project, store.NoteInput{
		Attribution: by, Kind: in.Category, Content: in.Content, Pinned: in.Pinned, IdempotencyKey: in.IdempotencyKey,
	})
	return nil, n, t.wrote("add_note", by, err)
}

type handoffIn struct {
	Project        string         `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Summary        string         `json:"summary" jsonschema:"what you did in this session and where it stands"`
	CurrentState   string         `json:"current_state,omitempty" jsonschema:"state of the work, e.g. tests passing, half migrated"`
	CurrentTask    string         `json:"current_task,omitempty" jsonschema:"the task you were on"`
	Blockers       string         `json:"blockers,omitempty" jsonschema:"anything in the way (use raise_blocker for blockers that need tracking)"`
	NextSteps      string         `json:"next_steps,omitempty" jsonschema:"what the next person or agent should do"`
	Branch         string         `json:"branch,omitempty" jsonschema:"git branch, from git rev-parse --abbrev-ref HEAD"`
	CommitSHA      string         `json:"commit_sha,omitempty" jsonschema:"git commit hash, from git rev-parse HEAD"`
	Metadata       map[string]any `json:"metadata,omitempty" jsonschema:"other checkable facts as a JSON object, e.g. test counts or a pull request URL"`
	SetState       string         `json:"set_state,omitempty" jsonschema:"also set your state on the project, typically idle or finished"`
	UserUpdate     *userUpdateIn  `json:"user_update,omitempty" jsonschema:"optional project update for the project owner, published with this handoff (see publish_project_update); include it only if something meaningful changed for them"`
	IdempotencyKey string         `json:"idempotency_key,omitempty" jsonschema:"Optional unique string you generate (e.g. a UUID). Retrying a call with the same key returns the original record instead of creating a duplicate."`
}

// userUpdateIn is the content of a project update, written for the project owner.
type userUpdateIn struct {
	Title         string `json:"title" jsonschema:"short headline stating the outcome, e.g. Login is now faster (not Task 4 complete)"`
	Summary       string `json:"summary" jsonschema:"one to three short plain-English paragraphs: what changed, why it matters, what the owner can expect now"`
	Details       string `json:"details,omitempty" jsonschema:"optional, slightly more technical context; keep it readable, no implementation dumps"`
	Category      string `json:"category" jsonschema:"new, improved, fixed, changed or important"`
	TaskReference string `json:"task_reference,omitempty" jsonschema:"optional reference such as Task 3 or Network setup; supporting metadata only"`
}

func (u userUpdateIn) content() store.UpdateContent {
	return store.UpdateContent{Title: u.Title, Summary: u.Summary, Details: u.Details, Category: u.Category, TaskReference: u.TaskReference}
}

func (t *tools) recordHandoff(ctx context.Context, req *mcp.CallToolRequest, in handoffIn) (*mcp.CallToolResult, store.Handoff, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.Handoff{}, err
	}
	var meta json.RawMessage
	if in.Metadata != nil {
		if meta, err = json.Marshal(in.Metadata); err != nil {
			return nil, store.Handoff{}, fmt.Errorf("metadata: %w", err)
		}
	}
	var update *store.UpdateContent
	if in.UserUpdate != nil {
		c := in.UserUpdate.content()
		update = &c
	}
	h, err := t.st.AddHandoff(ctx, in.Project, store.HandoffInput{
		Attribution: by, Summary: in.Summary, CurrentState: in.CurrentState, CurrentTask: in.CurrentTask,
		Blockers: in.Blockers, NextSteps: in.NextSteps, Branch: in.Branch, CommitSHA: in.CommitSHA,
		Metadata: meta, State: in.SetState, Update: update, IdempotencyKey: in.IdempotencyKey,
	})
	return nil, h, t.wrote("record_handoff", by, err)
}

type publishIn struct {
	Project        string `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Title          string `json:"title" jsonschema:"short headline stating the outcome, e.g. Login is now faster (not Task 4 complete)"`
	Summary        string `json:"summary" jsonschema:"one to three short plain-English paragraphs: what changed, why it matters, what the owner can expect now"`
	Details        string `json:"details,omitempty" jsonschema:"optional, slightly more technical context; keep it readable, no implementation dumps"`
	Category       string `json:"category" jsonschema:"new, improved, fixed, changed or important"`
	TaskReference  string `json:"task_reference,omitempty" jsonschema:"optional reference such as Task 3 or Network setup; supporting metadata only"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional unique string you generate (e.g. a UUID). Retrying a call with the same key returns the original record instead of creating a duplicate."`
}

func (t *tools) publishUpdate(ctx context.Context, req *mcp.CallToolRequest, in publishIn) (*mcp.CallToolResult, store.ProjectUpdate, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.ProjectUpdate{}, err
	}
	u, err := t.st.AddUpdate(ctx, in.Project, store.UpdateInput{
		Attribution: by, IdempotencyKey: in.IdempotencyKey,
		UpdateContent: userUpdateIn{in.Title, in.Summary, in.Details, in.Category, in.TaskReference}.content(),
	})
	return nil, u, t.wrote("publish_project_update", by, err)
}

type stateIn struct {
	Project     string  `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	State       string  `json:"state"`
	CurrentTask *string `json:"current_task,omitempty" jsonschema:"what you are doing; omit to keep the previous task, empty string to clear it"`
}

func (t *tools) setState(ctx context.Context, req *mcp.CallToolRequest, in stateIn) (*mcp.CallToolResult, store.ActorState, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.ActorState{}, err
	}
	st, err := t.st.SetState(ctx, in.Project, store.StateInput{Attribution: by, State: in.State, CurrentTask: in.CurrentTask})
	return nil, st, t.wrote("set_agent_state", by, err)
}

type decisionIn struct {
	Project        string `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Decision       string `json:"decision" jsonschema:"the choice, stated so it can be followed later"`
	Reasoning      string `json:"reasoning,omitempty" jsonschema:"why, and what was rejected"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional unique string you generate (e.g. a UUID). Retrying a call with the same key returns the original record instead of creating a duplicate."`
}

func (t *tools) recordDecision(ctx context.Context, req *mcp.CallToolRequest, in decisionIn) (*mcp.CallToolResult, store.Decision, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.Decision{}, err
	}
	d, err := t.st.AddDecision(ctx, in.Project, store.DecisionInput{
		Attribution: by, Decision: in.Decision, Reasoning: in.Reasoning, IdempotencyKey: in.IdempotencyKey,
	})
	return nil, d, t.wrote("record_decision", by, err)
}

type blockerIn struct {
	Project        string `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Description    string `json:"description" jsonschema:"what is blocked and what is needed"`
	Context        string `json:"context,omitempty" jsonschema:"optional details, e.g. error output or what was tried; stored with the description"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional unique string you generate (e.g. a UUID). Retrying a call with the same key returns the original record instead of creating a duplicate."`
}

func (t *tools) raiseBlocker(ctx context.Context, req *mcp.CallToolRequest, in blockerIn) (*mcp.CallToolResult, store.Blocker, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.Blocker{}, err
	}
	desc := in.Description
	if c := strings.TrimSpace(in.Context); c != "" && strings.TrimSpace(desc) != "" {
		desc = strings.TrimSpace(desc) + "\n\nContext: " + c
	}
	b, err := t.st.AddBlocker(ctx, in.Project, store.BlockerInput{Attribution: by, Description: desc, IdempotencyKey: in.IdempotencyKey})
	return nil, b, t.wrote("raise_blocker", by, err)
}

type resolveIn struct {
	BlockerID  int64  `json:"blocker_id" jsonschema:"ID from open_blockers in get_project_context"`
	Resolution string `json:"resolution,omitempty" jsonschema:"how it was resolved"`
}

func (t *tools) resolveBlocker(ctx context.Context, req *mcp.CallToolRequest, in resolveIn) (*mcp.CallToolResult, store.Blocker, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.Blocker{}, err
	}
	b, err := t.st.Blocker(ctx, in.BlockerID)
	if err != nil {
		return nil, store.Blocker{}, t.fail("resolve_blocker", err)
	}
	if b.Resolved {
		return nil, b, nil // idempotent: keep the original resolver and resolution
	}
	patch := store.BlockerPatch{Attribution: by, Resolved: ptr(true)}
	if in.Resolution != "" {
		patch.Resolution = &in.Resolution
	}
	b, err = t.st.UpdateBlocker(ctx, in.BlockerID, patch)
	return nil, b, t.wrote("resolve_blocker", by, err)
}
