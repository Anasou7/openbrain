package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"openbrain/internal/store"
)

func (s *Server) parsePages() map[string]*template.Template {
	funcs := template.FuncMap{
		"u":         s.url,
		"asset":     s.asset,
		"icon":      icon,
		"ago":       ago,
		"excerpt":   store.Excerpt,
		"shortSHA":  func(sha string) string { return sha[:min(len(sha), 10)] },
		"cell":      cellSummary,
		"hasPrefix": strings.HasPrefix,
		"via":       viaLabel,
		"glyph":     glyph,
		"paras":     paras,
		"clock":     func(t store.Time) string { return t.Local().Format("15:04") },
		"subnav":    func(slug, active string) subnavView { return subnavView{slug, active} },
		"join":      strings.Join,
		"humanize":  func(s string) string { return strings.ReplaceAll(s, "_", " ") },
	}
	pages := map[string]*template.Template{}
	for _, name := range []string{"overview", "projects", "project", "profile", "updates", "activity", "agents", "system", "account"} {
		pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+name+".html"))
	}
	for _, name := range []string{"login", "setup", "error"} {
		pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/auth.html", "templates/"+name+".html"))
	}
	return pages
}

func (s *Server) dashboardRoutes(mux *http.ServeMux) {
	mux.Handle("GET /static/", s.static())
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setupSubmit)

	mux.Handle("GET /{$}", s.page(s.overview))
	mux.Handle("GET /projects", s.page(s.projects))
	mux.Handle("POST /projects", s.page(s.createProject))
	mux.Handle("GET /p/{ref}", s.page(s.project))
	mux.Handle("POST /p/{ref}/edit", s.page(s.editProject))
	mux.Handle("POST /p/{ref}/notes", s.page(s.addNote))
	mux.Handle("POST /p/{ref}/notes/{id}/pin", s.page(s.pinNote))
	mux.Handle("POST /p/{ref}/handoffs", s.page(s.addHandoff))
	mux.Handle("GET /p/{ref}/profile", s.page(s.projectProfile))
	mux.Handle("POST /p/{ref}/profile", s.page(s.saveSection))
	mux.Handle("POST /p/{ref}/profile/{key}", s.page(s.saveSection))
	mux.Handle("POST /p/{ref}/profile/{key}/remove", s.page(s.removeSection))
	mux.Handle("POST /p/{ref}/features", s.page(s.saveFeature))
	mux.Handle("POST /p/{ref}/features/{slug}", s.page(s.saveFeature))
	mux.Handle("GET /p/{ref}/updates", s.page(s.projectUpdates))
	mux.Handle("POST /p/{ref}/updates", s.page(s.publishUpdate))
	mux.Handle("POST /p/{ref}/decisions", s.page(s.addDecision))
	mux.Handle("POST /p/{ref}/blockers", s.page(s.addBlocker))
	mux.Handle("POST /p/{ref}/blockers/{id}", s.page(s.updateBlocker))
	mux.Handle("GET /activity", s.page(s.activity))
	mux.Handle("GET /agents", s.page(s.agents))
	mux.Handle("POST /agents", s.adminPage(s.createActor))
	mux.Handle("POST /agents/tokens", s.adminPage(s.createToken))
	mux.Handle("POST /agents/tokens/{id}/revoke", s.adminPage(s.revokeToken))
	mux.Handle("GET /account", s.page(s.account))
	mux.Handle("POST /account/name", s.page(s.accountName))
	mux.Handle("POST /account/password", s.page(s.accountPassword))
	mux.Handle("POST /account/sessions", s.page(s.accountSessions))
	mux.Handle("GET /system", s.adminPage(s.system))
	mux.Handle("POST /system/users", s.adminPage(s.createUser))
	mux.Handle("POST /system/users/{id}/disable", s.adminPage(s.disableUser))
	mux.Handle("POST /system/users/{id}/enable", s.adminPage(s.enableUser))
	mux.Handle("POST /system/users/{id}/password", s.adminPage(s.resetPassword))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, v *viewer) {
	s.render(w, r, http.StatusNotFound, "error", v, map[string]any{"Code": 404, "Message": "That page does not exist.", "Link": "/"})
}

// back redirects after a form post. Input errors travel back in the query
// string and are shown on the page; anything else is a server error.
func (s *Server) back(w http.ResponseWriter, r *http.Request, path, anchor string, err error) {
	if err != nil {
		var ve *store.ValidationError
		if !errors.As(err, &ve) && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, err)
			return
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "err=" + url.QueryEscape(sentence(err.Error()))
	}
	if anchor != "" {
		path += "#" + anchor
	}
	s.redirect(w, r, path)
}

// done redirects with a confirmation message.
func (s *Server) done(w http.ResponseWriter, r *http.Request, path, msg string) {
	s.redirect(w, r, path+"?msg="+url.QueryEscape(msg))
}

// field returns a pointer to a posted form value, or nil if it was not sent.
func field(r *http.Request, name string) *string {
	if !r.PostForm.Has(name) {
		return nil
	}
	v := r.PostForm.Get(name)
	return &v
}

func projectPath(ref string) string { return "/p/" + url.PathEscape(ref) }

// mcpURL is the address agents should use, as seen by this browser.
func (s *Server) mcpURL(r *http.Request) string {
	scheme := "http"
	if s.secure(r) {
		scheme = "https"
	}
	host := r.Host
	if s.trusted(remoteAddr(r)) && r.Header.Get("X-Forwarded-Host") != "" {
		host = r.Header.Get("X-Forwarded-Host")
	}
	return scheme + "://" + host + s.url("/mcp")
}

// --- overview ---

type metric struct {
	Label, Value, Note string
}

type agentLine struct {
	Actor   store.ActorRef
	State   string
	Stale   bool
	Text    string
	Project string
	At      store.Time
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request, v *viewer) {
	o, err := s.st.Overview(r.Context(), false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var active, blocked, today, working int
	agents := map[string]*agentLine{}
	for _, row := range o.Projects {
		p := row.Project
		if p.Status == "active" {
			active++
		}
		if p.OpenBlockers > 0 {
			blocked++
		}
		if time.Since(p.LastActivityAt.Time) < 24*time.Hour {
			today++
		}
		for slug, c := range row.Actors {
			if slug == v.User.Actor.Slug {
				continue
			}
			cv := cellSummary(c)
			line := agents[slug]
			if line != nil && !cv.At.After(line.At.Time) {
				continue
			}
			line = &agentLine{Text: cv.Text, Project: p.Name, At: cv.At}
			if c.State != nil {
				line.Actor, line.State, line.Stale = c.State.Actor, c.State.State, c.State.Stale
			} else if c.Handoff != nil {
				line.Actor = c.Handoff.Actor
			} else if c.Note != nil {
				line.Actor = c.Note.Actor
			}
			agents[slug] = line
		}
	}
	var lines []*agentLine
	for _, a := range o.Actors {
		if l := agents[a.Slug]; l != nil && a.Kind != "human" {
			lines = append(lines, l)
			if l.State == "working" && !l.Stale {
				working++
			}
		}
	}
	work, err := s.st.RecentUpdates(r.Context(), 6, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "overview", v, map[string]any{
		"Nav": "overview", "Context": true, "O": o, "Agents": lines, "Work": work, "MCPURL": s.mcpURL(r),
		"Metrics": []metric{
			{"Active", strconv.Itoa(active), "projects in progress"},
			{"Blocked", strconv.Itoa(blocked), "with open blockers"},
			{"Updated today", strconv.Itoa(today), "changed in the last 24h"},
			{"Agents working", strconv.Itoa(working), "reporting a live task"},
		},
	})
}

// --- projects ---

func (s *Server) projects(w http.ResponseWriter, r *http.Request, v *viewer) {
	status := r.URL.Query().Get("status")
	list, err := s.st.Projects(r.Context(), status == "archived" || status == "all")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	shown := []store.Project{}
	for _, p := range list {
		if status == "" || status == "all" || p.Status == status {
			shown = append(shown, p)
		}
	}
	s.render(w, r, http.StatusOK, "projects", v, map[string]any{
		"Nav": "projects", "Projects": shown, "Status": status, "Statuses": store.ProjectStatuses,
		"New": r.URL.Query().Has("new"), "Total": len(list),
	})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, v *viewer) {
	f := r.PostForm
	p, err := s.st.CreateProject(r.Context(), store.ProjectInput{
		Attribution: v.by(), Name: f.Get("name"), Slug: f.Get("slug"), Description: f.Get("description"),
		CurrentFocus: f.Get("current_focus"), RepoURL: f.Get("repo_url"), DefaultBranch: f.Get("default_branch"),
	})
	if err != nil {
		s.back(w, r, "/projects?new", "new", err)
		return
	}
	s.redirect(w, r, projectPath(p.Slug))
}

func (s *Server) project(w http.ResponseWriter, r *http.Request, v *viewer) {
	ctx := r.Context()
	c, err := s.st.ProjectContext(ctx, r.PathValue("ref"), store.ContextOptions{})
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, v)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id := strconv.FormatInt(c.Project.ID, 10)
	notes, err1 := s.st.Notes(ctx, id, store.NoteFilter{Limit: 200})
	decisions, err2 := s.st.Decisions(ctx, id, 0, 100)
	handoffs, err3 := s.st.Handoffs(ctx, id, "", 0, 30)
	activity, err4 := s.st.Activity(ctx, id, 0, 50)
	resolved, err5 := s.st.Blockers(ctx, id, "resolved", 20)
	prof, err6 := s.st.Profile(ctx, id, store.ProfileOptions{SkipFeatures: true})
	if err := errors.Join(err1, err2, err3, err4, err5, err6); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "project", v, map[string]any{
		"Nav": "projects", "C": c, "Notes": notes, "Decisions": decisions, "Handoffs": handoffs,
		"Activity": activity, "Resolved": resolved, "Statuses": store.ProjectStatuses, "Snap": productSnapshot(prof),
	})
}

func (s *Server) editProject(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref := r.PathValue("ref")
	p, err := s.st.UpdateProject(r.Context(), ref, store.ProjectPatch{
		Attribution: v.by(), Name: field(r, "name"), Slug: field(r, "slug"), Status: field(r, "status"),
		Description: field(r, "description"), CurrentFocus: field(r, "current_focus"),
		RepoURL: field(r, "repo_url"), DefaultBranch: field(r, "default_branch"),
	})
	if err != nil {
		s.back(w, r, projectPath(ref), "edit", err)
		return
	}
	s.redirect(w, r, projectPath(p.Slug))
}

func (s *Server) addNote(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref, f := r.PathValue("ref"), r.PostForm
	_, err := s.st.AddNote(r.Context(), ref, store.NoteInput{
		Attribution: v.by(), Kind: f.Get("kind"), Content: f.Get("content"), Pinned: f.Get("pinned") == "on",
	})
	s.back(w, r, projectPath(ref), "notes", err)
}

func (s *Server) pinNote(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref := r.PathValue("ref")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, v)
		return
	}
	pinned := r.PostForm.Get("pinned") == "1"
	_, err = s.st.UpdateNote(r.Context(), id, store.NotePatch{Attribution: v.by(), Pinned: &pinned})
	if p, perr := s.st.Project(r.Context(), ref); perr == nil {
		ref = p.Slug // the note partial links by ID; land back on the readable URL
	}
	s.back(w, r, projectPath(ref), fmt.Sprintf("note-%d", id), err)
}

func (s *Server) addHandoff(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref, f := r.PathValue("ref"), r.PostForm
	_, err := s.st.AddHandoff(r.Context(), ref, store.HandoffInput{
		Attribution: v.by(), Summary: f.Get("summary"), CurrentTask: f.Get("current_task"),
		NextSteps: f.Get("next_steps"), Blockers: f.Get("blockers"),
	})
	s.back(w, r, projectPath(ref), "handoffs", err)
}

// --- project updates ---

type updateDay struct {
	Date, Label string
	Updates     []store.ProjectUpdate
}

// byDay groups updates (newest first) under their local calendar date, for
// the patch-notes layout.
func byDay(us []store.ProjectUpdate) []updateDay {
	var days []updateDay
	for _, u := range us {
		t := u.CreatedAt.Local()
		if d := t.Format("2006-01-02"); len(days) == 0 || days[len(days)-1].Date != d {
			days = append(days, updateDay{Date: d, Label: t.Format("2 January 2006")})
		}
		days[len(days)-1].Updates = append(days[len(days)-1].Updates, u)
	}
	return days
}

func (s *Server) projectUpdates(w http.ResponseWriter, r *http.Request, v *viewer) {
	p, err := s.st.Project(r.Context(), r.PathValue("ref"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, v)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	const pageSize = 30
	updates, err := s.st.Updates(r.Context(), strconv.FormatInt(p.ID, 10), store.UpdateFilter{Before: before, Limit: pageSize})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var older int64
	if len(updates) == pageSize {
		older = updates[len(updates)-1].ID
	}
	s.render(w, r, http.StatusOK, "updates", v, map[string]any{
		"Nav": "projects", "P": p, "Days": byDay(updates), "Older": older, "Paged": before > 0,
		"Categories": store.UpdateCategories,
	})
}

// publishUpdate lets a person publish an update; the signed-in user is the author.
func (s *Server) publishUpdate(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref, f := r.PathValue("ref"), r.PostForm
	_, err := s.st.AddUpdate(r.Context(), ref, store.UpdateInput{Attribution: v.by(), UpdateContent: store.UpdateContent{
		Title: f.Get("title"), Summary: f.Get("summary"), Details: f.Get("details"),
		Category: f.Get("category"), TaskReference: f.Get("task_reference"),
	}})
	if err != nil {
		s.back(w, r, projectPath(ref)+"/updates", "publish", err)
		return
	}
	s.done(w, r, projectPath(ref)+"/updates", "Update published.")
}

func (s *Server) addDecision(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref, f := r.PathValue("ref"), r.PostForm
	_, err := s.st.AddDecision(r.Context(), ref, store.DecisionInput{
		Attribution: v.by(), Decision: f.Get("decision"), Reasoning: f.Get("reasoning"),
	})
	s.back(w, r, projectPath(ref), "decisions", err)
}

func (s *Server) addBlocker(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref := r.PathValue("ref")
	_, err := s.st.AddBlocker(r.Context(), ref, store.BlockerInput{Attribution: v.by(), Description: r.PostForm.Get("description")})
	s.back(w, r, projectPath(ref), "blockers", err)
}

func (s *Server) updateBlocker(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref := r.PathValue("ref")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, v)
		return
	}
	resolved := r.PostForm.Get("resolved") == "1"
	_, err = s.st.UpdateBlocker(r.Context(), id, store.BlockerPatch{Attribution: v.by(), Resolved: &resolved, Resolution: field(r, "resolution")})
	s.back(w, r, projectPath(ref), "blockers", err)
}

// --- activity ---

func (s *Server) activity(w http.ResponseWriter, r *http.Request, v *viewer) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	const pageSize = 50
	feed, err := s.st.Feed(r.Context(), before, pageSize)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var older int64
	if len(feed) == pageSize {
		older = feed[len(feed)-1].ID
	}
	s.render(w, r, http.StatusOK, "activity", v, map[string]any{"Nav": "activity", "Feed": feed, "Older": older, "Paged": before > 0})
}

// --- agents ---

type newToken struct {
	Token  store.Token
	Secret string
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request, v *viewer) {
	s.renderAgents(w, r, v, http.StatusOK, nil)
}

// renderAgents shows actors and, to administrators, token metadata. created
// is set only on the response to creating a token, the one time its secret
// is shown.
func (s *Server) renderAgents(w http.ResponseWriter, r *http.Request, v *viewer, status int, created *newToken) {
	actors, err := s.st.Actors(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byActor := map[string][]store.Token{}
	if v.User.IsAdmin() {
		tokens, err := s.st.Tokens(r.Context(), true)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, t := range tokens {
			byActor[t.Actor.Slug] = append(byActor[t.Actor.Slug], t)
		}
	}
	var agents, people []store.Actor
	for _, a := range actors {
		if a.Kind == "human" {
			people = append(people, a)
		} else {
			agents = append(agents, a)
		}
	}
	s.render(w, r, status, "agents", v, map[string]any{
		"Nav": "agents", "Agents": agents, "People": people, "All": actors, "Tokens": byActor,
		"Kinds": store.ActorKinds, "New": created, "MCPURL": s.mcpURL(r),
	})
}

func (s *Server) createActor(w http.ResponseWriter, r *http.Request, v *viewer) {
	f := r.PostForm
	_, err := s.st.CreateActor(r.Context(), store.ActorInput{Slug: f.Get("slug"), DisplayName: f.Get("display_name"), Kind: f.Get("kind")})
	s.back(w, r, "/agents", "", err)
}

// createToken renders the page directly, rather than redirecting, so the
// secret never appears in a URL or the browser history.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request, v *viewer) {
	t, secret, err := s.st.CreateToken(r.Context(), r.PostForm.Get("actor"), r.PostForm.Get("label"))
	if err != nil {
		s.back(w, r, "/agents", "", err)
		return
	}
	s.log.Info("token created", "token_id", t.ID, "actor", t.Actor.Slug, "by_user_id", v.User.ID)
	s.renderAgents(w, r, v, http.StatusOK, &newToken{t, secret})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, v *viewer) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, v)
		return
	}
	t, err := s.st.RevokeToken(r.Context(), id)
	if err == nil {
		s.log.Info("token revoked", "token_id", t.ID, "actor", t.Actor.Slug, "by_user_id", v.User.ID)
	}
	s.back(w, r, "/agents", "", err)
}

// --- account ---

func (s *Server) account(w http.ResponseWriter, r *http.Request, v *viewer) {
	n, err := s.st.ActiveSessions(r.Context(), v.User.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "account", v, map[string]any{"Nav": "account", "Sessions": n})
}

func (s *Server) accountName(w http.ResponseWriter, r *http.Request, v *viewer) {
	if err := s.st.SetDisplayName(r.Context(), v.User.ID, r.PostForm.Get("display_name")); err != nil {
		s.back(w, r, "/account", "", err)
		return
	}
	s.done(w, r, "/account", "Display name updated.")
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request, v *viewer) {
	f := r.PostForm
	if f.Get("password") != f.Get("confirm") {
		s.back(w, r, "/account", "password", &store.ValidationError{Field: "new password", Problem: "and its confirmation do not match"})
		return
	}
	if err := s.st.ChangePassword(r.Context(), v.User.ID, f.Get("current"), f.Get("password"), v.Session.ID); err != nil {
		s.back(w, r, "/account", "password", err)
		return
	}
	s.log.Info("password changed", "user_id", v.User.ID)
	s.done(w, r, "/account", "Password changed. Your other sessions were signed out.")
}

func (s *Server) accountSessions(w http.ResponseWriter, r *http.Request, v *viewer) {
	n, err := s.st.RevokeOtherSessions(r.Context(), v.User.ID, v.Session.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.done(w, r, "/account", fmt.Sprintf("Signed out %d other session(s).", n))
}

// --- system (administrators) ---

func (s *Server) system(w http.ResponseWriter, r *http.Request, v *viewer) {
	users, err := s.st.Users(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	base := s.base
	if base == "" {
		base = "/"
	}
	s.render(w, r, http.StatusOK, "system", v, map[string]any{
		"Nav": "system", "Users": users, "Roles": store.UserRoles, "Version": s.cfg.Version,
		"Schema": store.SchemaVersion(), "BasePath": base, "MCPURL": s.mcpURL(r), "AdminToken": s.cfg.AdminToken != "",
	})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, v *viewer) {
	f := r.PostForm
	if f.Get("password") != f.Get("confirm") {
		s.back(w, r, "/system", "new-user", &store.ValidationError{Field: "password", Problem: "and its confirmation do not match"})
		return
	}
	u, err := s.st.CreateUser(r.Context(), store.UserInput{
		Username: f.Get("username"), DisplayName: f.Get("display_name"), Password: f.Get("password"), Role: f.Get("role"),
	})
	if err != nil {
		s.back(w, r, "/system", "new-user", err)
		return
	}
	s.log.Info("user created", "user_id", u.ID, "role", u.Role, "by_user_id", v.User.ID)
	s.done(w, r, "/system", "Created "+u.Username+".")
}

func (s *Server) userID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (s *Server) disableUser(w http.ResponseWriter, r *http.Request, v *viewer) {
	s.setDisabled(w, r, v, true)
}

func (s *Server) enableUser(w http.ResponseWriter, r *http.Request, v *viewer) {
	s.setDisabled(w, r, v, false)
}

func (s *Server) setDisabled(w http.ResponseWriter, r *http.Request, v *viewer, disabled bool) {
	id, err := s.userID(r)
	if err != nil {
		s.notFound(w, r, v)
		return
	}
	if err := s.st.SetDisabled(r.Context(), v.User.ID, id, disabled); err != nil {
		s.back(w, r, "/system", "", err)
		return
	}
	s.log.Info("user access changed", "user_id", id, "disabled", disabled, "by_user_id", v.User.ID)
	msg := "Account enabled."
	if disabled {
		msg = "Account disabled and signed out."
	}
	s.done(w, r, "/system", msg)
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request, v *viewer) {
	id, err := s.userID(r)
	if err != nil {
		s.notFound(w, r, v)
		return
	}
	f := r.PostForm
	if f.Get("password") != f.Get("confirm") {
		s.back(w, r, "/system", "", &store.ValidationError{Field: "password", Problem: "and its confirmation do not match"})
		return
	}
	if err := s.st.SetPassword(r.Context(), id, f.Get("password")); err != nil {
		s.back(w, r, "/system", "", err)
		return
	}
	s.log.Info("password reset", "user_id", id, "by_user_id", v.User.ID)
	s.done(w, r, "/system", "Password reset. That user's sessions were signed out.")
}

// --- presentation helpers ---

// viaLabel describes the channel an activity entry arrived through.
func viaLabel(via string) string {
	switch via {
	case "dashboard":
		return ""
	case "mcp":
		return "via MCP"
	case "api":
		return "via API"
	case "admin-api":
		return "via API (admin token)"
	}
	return "via " + via
}

// glyph pairs every status with a distinct shape, so states stay legible
// without color.
func glyph(status string) string {
	switch status {
	case "active":
		return "●"
	case "working":
		return "◐" // spins while live; see .st-working

	case "paused", "waiting":
		return "‖"
	case "done", "finished":
		return "✓"
	case "archived":
		return "▪"
	case "blocked":
		return "▲"
	case "stale":
		return "△"
	}
	return "○" // idle and anything unknown
}

// paras splits text into paragraphs at blank lines. Line breaks inside a
// paragraph are kept by CSS; nothing is interpreted as markup.
func paras(s string) []string {
	var out []string
	for _, p := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// subnavView is the project section navigation; Active names the current tab.
type subnavView struct{ Slug, Active string }

func ago(t store.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t.Time)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}

type cellView struct {
	Text string
	At   store.Time
}

// cellSummary picks what best says where an actor is on a project. Its latest
// project update comes first: it states, for people, the last meaningful
// thing the actor achieved, unless the actor has since reported live work
// (working, waiting or blocked on a task), which is more current. Without an
// update: what it is doing right now if it is busy and that report is
// current, otherwise the freshest of its latest handoff and latest note.
func cellSummary(c *store.Cell) cellView {
	st := c.State
	live := st != nil && st.CurrentTask != "" && (st.State == "working" || st.State == "waiting" || st.State == "blocked")
	if u := c.Update; u != nil && !(live && st.UpdatedAt.After(u.CreatedAt.Time)) {
		return cellView{u.Title, u.CreatedAt}
	}
	var v cellView
	if c.Handoff != nil {
		v = cellView{c.Handoff.Summary, c.Handoff.CreatedAt}
	}
	if c.Note != nil && c.Note.CreatedAt.After(v.At.Time) {
		v = cellView{c.Note.Content, c.Note.CreatedAt}
	}
	if st := c.State; st != nil {
		busy := st.State == "working" || st.State == "waiting" || st.State == "blocked"
		if busy && st.CurrentTask != "" && !st.UpdatedAt.Before(v.At.Time) {
			v = cellView{st.CurrentTask, st.UpdatedAt}
		} else if v.At.IsZero() || st.UpdatedAt.After(v.At.Time) {
			v.At = st.UpdatedAt
		}
	}
	return v
}
