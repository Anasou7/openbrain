package store

import (
	"context"
	"database/sql"
	"sort"
)

// ProjectContext is everything an agent (or a person) needs to pick up a
// project: what it is, where everyone left off, and what is in the way. It is
// read in one transaction, so it is a consistent snapshot.
type ProjectContext struct {
	Project           Project         `json:"project"`
	Profile           ProfileSummary  `json:"profile"` // size and freshness of the project profile; read it with get_project_profile
	GeneratedAt       Time            `json:"generated_at"`
	StaleAfterSeconds int64           `json:"stale_after_seconds"`
	Compact           bool            `json:"compact"`
	Truncated         bool            `json:"truncated"`       // some text was clipped (compact mode only)
	LatestHandoffs    []Handoff       `json:"latest_handoffs"` // newest from each actor
	RecentUpdates     []ProjectUpdate `json:"recent_updates"`  // newest project updates: what changed, for people
	ActorStates       []ActorState    `json:"actor_states"`
	OpenBlockers      []Blocker       `json:"open_blockers"`
	PinnedNotes       []Note          `json:"pinned_notes"`
	HumanNotes        []Note          `json:"human_notes"`  // latest notes written by people
	RecentNotes       []Note          `json:"recent_notes"` // latest notes by anyone; empty in compact mode
	RecentDecisions   []Decision      `json:"recent_decisions"`
	RecentActivity    []Activity      `json:"recent_activity"`
}

// ContextOptions shape a ProjectContext.
type ContextOptions struct {
	// Compact returns fewer items and clips long text to ClipChars, for
	// agents with a limited context window. Latest handoffs, states and the
	// project itself are never dropped.
	Compact bool
	// ActivityLimit overrides the number of recent activity entries
	// (default 10 compact, 25 full; at most 200).
	ActivityLimit int
}

// ClipChars is the per-field text limit in compact context.
const ClipChars = 1500

type contextLimits struct{ pinned, human, recent, updates, decisions, blockers, activity int }

var (
	fullLimits    = contextLimits{pinned: 50, human: 10, recent: 10, updates: 10, decisions: 10, blockers: 100, activity: 25}
	compactLimits = contextLimits{pinned: 10, human: 5, recent: 0, updates: 5, decisions: 5, blockers: 20, activity: 10}
)

func (s *Store) ProjectContext(ctx context.Context, ref string, opt ContextOptions) (ProjectContext, error) {
	lim := fullLimits
	if opt.Compact {
		lim = compactLimits
	}
	if opt.ActivityLimit > 0 {
		lim.activity = min(opt.ActivityLimit, 200)
	}
	c := ProjectContext{GeneratedAt: Now(), StaleAfterSeconds: int64(s.StaleAfter.Seconds()), Compact: opt.Compact, RecentNotes: []Note{}}
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		if c.Project, err = getProject(ctx, tx, ref); err != nil {
			return err
		}
		id := c.Project.ID
		if c.Profile, err = profileSummary(ctx, tx, id); err != nil {
			return err
		}
		if c.LatestHandoffs, err = latestHandoffs(ctx, tx, id); err != nil {
			return err
		}
		if c.RecentUpdates, err = listUpdates(ctx, tx, id, lim.updates); err != nil {
			return err
		}
		if c.ActorStates, err = s.queryStates(ctx, tx, `WHERE s.project_id = ?`, id); err != nil {
			return err
		}
		if c.OpenBlockers, err = listBlockers(ctx, tx, id, "open", lim.blockers); err != nil {
			return err
		}
		if c.PinnedNotes, err = listNotes(ctx, tx, id, NoteFilter{PinnedOnly: true, Limit: lim.pinned}); err != nil {
			return err
		}
		if c.HumanNotes, err = listNotes(ctx, tx, id, NoteFilter{HumanOnly: true, Limit: lim.human}); err != nil {
			return err
		}
		if lim.recent > 0 {
			if c.RecentNotes, err = listNotes(ctx, tx, id, NoteFilter{Limit: lim.recent}); err != nil {
				return err
			}
		}
		if c.RecentDecisions, err = listDecisions(ctx, tx, id, 0, lim.decisions); err != nil {
			return err
		}
		c.RecentActivity, err = listActivity(ctx, tx, id, 0, lim.activity)
		return err
	})
	if err == nil && opt.Compact {
		c.Truncated = c.clip()
	}
	return c, err
}

// clip shortens every long free-text field to ClipChars and reports whether
// anything was cut.
func (c *ProjectContext) clip() bool {
	cut := false
	clipTo := func(s *string, n int) {
		if r := []rune(*s); len(r) > n {
			*s = string(r[:n]) + "… [truncated; request the full context for the rest]"
			cut = true
		}
	}
	f := func(s *string) { clipTo(s, ClipChars) }
	for i := range c.RecentUpdates {
		u := &c.RecentUpdates[i]
		f(&u.Summary)
		clipTo(&u.Details, ClipChars/5) // secondary; the summary carries the meaning
	}
	f(&c.Project.Description)
	f(&c.Project.CurrentFocus)
	for i := range c.LatestHandoffs {
		h := &c.LatestHandoffs[i]
		f(&h.Summary)
		f(&h.CurrentState)
		f(&h.CurrentTask)
		f(&h.Blockers)
		f(&h.NextSteps)
	}
	for _, notes := range [][]Note{c.PinnedNotes, c.HumanNotes, c.RecentNotes} {
		for i := range notes {
			f(&notes[i].Content)
		}
	}
	for i := range c.OpenBlockers {
		f(&c.OpenBlockers[i].Description)
	}
	for i := range c.RecentDecisions {
		f(&c.RecentDecisions[i].Decision)
		f(&c.RecentDecisions[i].Reasoning)
	}
	return cut
}

// Overview is the cross-project grid: one row per project, one column per
// actor that has done anything on the listed projects (people always shown).
type Overview struct {
	Actors   []ActorRef    `json:"actors"`
	Projects []OverviewRow `json:"projects"`
}

type OverviewRow struct {
	Project      Project          `json:"project"`
	LatestUpdate *ProjectUpdate   `json:"latest_update,omitempty"`
	Actors       map[string]*Cell `json:"actors"` // by actor slug; absent if the actor never touched the project
}

// Cell is the latest known position of one actor on one project.
type Cell struct {
	State   *ActorState    `json:"state,omitempty"`
	Update  *ProjectUpdate `json:"latest_update,omitempty"`
	Handoff *Handoff       `json:"latest_handoff,omitempty"`
	Note    *Note          `json:"latest_note,omitempty"`
}

func (s *Store) Overview(ctx context.Context, includeArchived bool) (Overview, error) {
	var o Overview
	err := s.read(ctx, func(tx *sql.Tx) error {
		projects, err := listProjects(ctx, tx, includeArchived)
		if err != nil {
			return err
		}
		o.Projects = make([]OverviewRow, len(projects))
		rows := make(map[int64]*OverviewRow, len(projects))
		for i, p := range projects {
			o.Projects[i] = OverviewRow{Project: p, Actors: map[string]*Cell{}}
			rows[p.ID] = &o.Projects[i]
		}
		columns := map[string]ActorRef{}
		cell := func(projectID int64, a ActorRef) *Cell {
			row := rows[projectID]
			if row == nil {
				return &Cell{} // project not listed (archived); discard
			}
			columns[a.Slug] = a
			if row.Actors[a.Slug] == nil {
				row.Actors[a.Slug] = &Cell{}
			}
			return row.Actors[a.Slug]
		}

		// ponytail: scans the latest-per-group of whole tables; fine for
		// hundreds of projects, add paging if anyone runs thousands.
		handoffs, err := queryHandoffs(ctx, tx, `WHERE h.id IN (SELECT max(id) FROM handoffs GROUP BY project_id, actor_id)`)
		if err != nil {
			return err
		}
		for i := range handoffs {
			cell(handoffs[i].ProjectID, handoffs[i].Actor).Handoff = &handoffs[i]
		}
		updates, err := queryUpdates(ctx, tx, `WHERE u.id IN (SELECT max(id) FROM project_updates GROUP BY project_id, actor_id)`)
		if err != nil {
			return err
		}
		for i := range updates {
			u := &updates[i]
			cell(u.ProjectID, u.Actor).Update = u
			if row := rows[u.ProjectID]; row != nil && (row.LatestUpdate == nil || u.ID > row.LatestUpdate.ID) {
				row.LatestUpdate = u
			}
		}
		rowsN, err := tx.QueryContext(ctx, noteSelect+` WHERE n.id IN (SELECT max(id) FROM notes GROUP BY project_id, actor_id)`)
		notes, err := collect(rowsN, err, scanNote)
		if err != nil {
			return err
		}
		for i := range notes {
			cell(notes[i].ProjectID, notes[i].Actor).Note = &notes[i]
		}
		states, err := s.queryStates(ctx, tx, "")
		if err != nil {
			return err
		}
		for i := range states {
			cell(states[i].ProjectID, states[i].Actor).State = &states[i]
		}

		humans, err := tx.QueryContext(ctx, `SELECT slug, display_name, kind FROM actors WHERE kind = 'human'`)
		people, err := collect(humans, err, func(sc scanner) (a ActorRef, err error) {
			return a, sc.Scan(&a.Slug, &a.DisplayName, &a.Kind)
		})
		if err != nil {
			return err
		}
		for _, a := range people {
			columns[a.Slug] = a
		}
		for _, a := range columns {
			o.Actors = append(o.Actors, a)
		}
		sort.Slice(o.Actors, func(i, j int) bool {
			hi, hj := o.Actors[i].Kind == "human", o.Actors[j].Kind == "human"
			if hi != hj {
				return hi
			}
			return o.Actors[i].Slug < o.Actors[j].Slug
		})
		return nil
	})
	return o, err
}
