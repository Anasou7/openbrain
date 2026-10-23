package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
)

// Handoff records where an actor left a project at the end of a working
// session. Handoffs are immutable; history is kept.
type Handoff struct {
	ID        int64    `json:"id"`
	ProjectID int64    `json:"project_id"`
	Actor     ActorRef `json:"actor"`

	// The actor's own account of the session. Treat as interpretation.
	Summary      string `json:"summary"`
	CurrentState string `json:"current_state"`
	CurrentTask  string `json:"current_task"`
	Blockers     string `json:"blockers"`
	NextSteps    string `json:"next_steps"`

	// Checkable facts about where the work stands.
	Branch    string          `json:"branch"`
	CommitSHA string          `json:"commit_sha"`
	Metadata  json.RawMessage `json:"metadata"`

	CreatedAt Time `json:"created_at"`

	// The project update published with this handoff. Only set in the
	// response to recording the handoff.
	Update *ProjectUpdate `json:"user_update,omitempty"`
}

type HandoffInput struct {
	Attribution
	Summary      string          `json:"summary"`
	CurrentState string          `json:"current_state"`
	CurrentTask  string          `json:"current_task"`
	Blockers     string          `json:"blockers"`
	NextSteps    string          `json:"next_steps"`
	Branch       string          `json:"branch"`
	CommitSHA    string          `json:"commit_sha"`
	Metadata     json.RawMessage `json:"metadata"`

	// State, if set, also updates the actor's state on the project in the
	// same transaction, e.g. "idle" or "finished" at the end of a session.
	State string `json:"state"`

	// Update, if set, also publishes a project update for the project owner,
	// attributed to the same actor and linked to the handoff, in the same
	// transaction.
	Update *UpdateContent `json:"user_update,omitempty"`

	IdempotencyKey string `json:"idempotency_key,omitempty"` // see ProjectInput
}

const handoffSelect = `SELECT h.id, h.project_id, a.slug, a.display_name, a.kind,
	h.summary, h.current_state, h.current_task, h.blockers, h.next_steps,
	h.branch, h.commit_sha, h.metadata, h.created_at
FROM handoffs h JOIN actors a ON a.id = h.actor_id`

func scanHandoff(sc scanner) (h Handoff, err error) {
	var meta string
	err = sc.Scan(&h.ID, &h.ProjectID, &h.Actor.Slug, &h.Actor.DisplayName, &h.Actor.Kind,
		&h.Summary, &h.CurrentState, &h.CurrentTask, &h.Blockers, &h.NextSteps,
		&h.Branch, &h.CommitSHA, &meta, &h.CreatedAt)
	h.Metadata = json.RawMessage(meta)
	return h, err
}

func queryHandoffs(ctx context.Context, q querier, where string, args ...any) ([]Handoff, error) {
	rows, err := q.QueryContext(ctx, handoffSelect+" "+where, args...)
	return collect(rows, err, scanHandoff)
}

func (s *Store) AddHandoff(ctx context.Context, ref string, in HandoffInput) (Handoff, error) {
	var v validator
	v.text("summary", &in.Summary, maxText, true)
	v.text("current_state", &in.CurrentState, maxText, false)
	v.text("current_task", &in.CurrentTask, maxShort, false)
	v.text("blockers", &in.Blockers, maxText, false)
	v.text("next_steps", &in.NextSteps, maxText, false)
	v.token("branch", &in.Branch)
	v.commit("commit_sha", &in.CommitSHA)
	meta := v.metadata("metadata", in.Metadata)
	if in.State != "" {
		v.oneOf("state", in.State, StateValues)
	}
	if in.Update != nil {
		v.update("user_update.", in.Update)
	}
	v.idempotencyKey(&in.IdempotencyKey)
	if v.err != nil {
		return Handoff{}, v.err
	}

	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		actor, err := authorID(ctx, tx, in.Actor)
		if err != nil {
			return err
		}
		p, err := getProject(ctx, tx, ref)
		if err != nil {
			return err
		}
		op := opFor("record_handoff", p.ID)
		if id, err = replayed(ctx, tx, actor, in.IdempotencyKey, op); err != nil || id != 0 {
			return err
		}
		now := Now()
		res, err := tx.ExecContext(ctx, `INSERT INTO handoffs
			(project_id, actor_id, summary, current_state, current_task, blockers, next_steps, branch, commit_sha, metadata, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ID, actor, in.Summary, in.CurrentState, in.CurrentTask, in.Blockers, in.NextSteps, in.Branch, in.CommitSHA, meta, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := remember(ctx, tx, actor, in.IdempotencyKey, op, id, now); err != nil {
			return err
		}
		if err := logActivity(ctx, tx, event{project: p.ID, actor: actor, subject: id, via: in.Via, at: now,
			kind: "handoff.created", summary: "handed off: " + Excerpt(in.Summary, 120)}); err != nil {
			return err
		}
		if in.Update != nil {
			if _, err := insertUpdate(ctx, tx, p.ID, actor, id, *in.Update, in.Via, now); err != nil {
				return err
			}
		}
		if in.State == "" {
			return nil
		}
		var task *string
		if in.CurrentTask != "" {
			task = &in.CurrentTask
		}
		return setState(ctx, tx, p.ID, actor, in.State, task, in.Via, now)
	})
	if err != nil {
		return Handoff{}, err
	}
	hs, err := queryHandoffs(ctx, s.r, `WHERE h.id = ?`, id)
	if err == nil && len(hs) == 0 {
		err = fmt.Errorf("handoff %d %w", id, ErrNotFound)
	}
	if err != nil {
		return Handoff{}, err
	}
	// Also covers a replayed call: the update created the first time is returned.
	us, err := queryUpdates(ctx, s.r, `WHERE u.handoff_id = ?`, id)
	if err != nil {
		return Handoff{}, err
	}
	if len(us) > 0 {
		hs[0].Update = &us[0]
	}
	return hs[0], nil
}

// Handoffs returns a project's handoff history, newest first, optionally for
// one actor. To page backwards, pass the smallest ID already seen as before.
func (s *Store) Handoffs(ctx context.Context, ref, actor string, before int64, n int) ([]Handoff, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return nil, err
	}
	if before <= 0 {
		before = math.MaxInt64
	}
	if actor == "" {
		return queryHandoffs(ctx, s.r, `WHERE h.project_id = ? AND h.id < ? ORDER BY h.id DESC LIMIT ?`, p.ID, before, limit(n, 50))
	}
	return queryHandoffs(ctx, s.r, `WHERE h.project_id = ? AND a.slug = ? AND h.id < ? ORDER BY h.id DESC LIMIT ?`, p.ID, actor, before, limit(n, 50))
}

// LatestHandoffs returns the most recent handoff from each actor on a project.
func (s *Store) LatestHandoffs(ctx context.Context, ref string) ([]Handoff, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return nil, err
	}
	return latestHandoffs(ctx, s.r, p.ID)
}

func latestHandoffs(ctx context.Context, q querier, projectID int64) ([]Handoff, error) {
	// IDs are monotonic, so max(id) per actor is that actor's newest handoff;
	// the (project_id, actor_id, id) index answers the subquery directly.
	return queryHandoffs(ctx, q, `WHERE h.id IN (SELECT max(id) FROM handoffs WHERE project_id = ? GROUP BY actor_id) ORDER BY h.id DESC`, projectID)
}
