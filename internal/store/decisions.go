package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

type Decision struct {
	ID        int64    `json:"id"`
	ProjectID int64    `json:"project_id"`
	Actor     ActorRef `json:"actor"`
	Decision  string   `json:"decision"`
	Reasoning string   `json:"reasoning"`
	CreatedAt Time     `json:"created_at"`
}

type DecisionInput struct {
	Attribution
	Decision  string `json:"decision"`
	Reasoning string `json:"reasoning"`

	IdempotencyKey string `json:"idempotency_key,omitempty"` // see ProjectInput
}

const decisionSelect = `SELECT d.id, d.project_id, a.slug, a.display_name, a.kind, d.decision, d.reasoning, d.created_at
FROM decisions d JOIN actors a ON a.id = d.actor_id`

func scanDecision(sc scanner) (d Decision, err error) {
	err = sc.Scan(&d.ID, &d.ProjectID, &d.Actor.Slug, &d.Actor.DisplayName, &d.Actor.Kind, &d.Decision, &d.Reasoning, &d.CreatedAt)
	return d, err
}

func (s *Store) AddDecision(ctx context.Context, ref string, in DecisionInput) (Decision, error) {
	var v validator
	v.text("decision", &in.Decision, maxText, true)
	v.text("reasoning", &in.Reasoning, maxText, false)
	v.idempotencyKey(&in.IdempotencyKey)
	if v.err != nil {
		return Decision{}, v.err
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
		op := opFor("record_decision", p.ID)
		if id, err = replayed(ctx, tx, actor, in.IdempotencyKey, op); err != nil || id != 0 {
			return err
		}
		now := Now()
		res, err := tx.ExecContext(ctx, `INSERT INTO decisions (project_id, actor_id, decision, reasoning, created_at) VALUES (?, ?, ?, ?, ?)`,
			p.ID, actor, in.Decision, in.Reasoning, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := remember(ctx, tx, actor, in.IdempotencyKey, op, id, now); err != nil {
			return err
		}
		return logActivity(ctx, tx, event{project: p.ID, actor: actor, subject: id, via: in.Via, at: now,
			kind: "decision.recorded", summary: "decided: " + Excerpt(in.Decision, 120)})
	})
	if err != nil {
		return Decision{}, err
	}
	d, err := scanDecision(s.r.QueryRowContext(ctx, decisionSelect+` WHERE d.id = ?`, id))
	if err != nil {
		return Decision{}, fmt.Errorf("reading decision %d: %w", id, err)
	}
	return d, nil
}

// Decisions lists a project's decisions, newest first. To page backwards,
// pass the smallest ID already seen as before.
func (s *Store) Decisions(ctx context.Context, ref string, before int64, n int) ([]Decision, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return nil, err
	}
	return listDecisions(ctx, s.r, p.ID, before, n)
}

func listDecisions(ctx context.Context, q querier, projectID, before int64, n int) ([]Decision, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	rows, err := q.QueryContext(ctx, decisionSelect+` WHERE d.project_id = ? AND d.id < ? ORDER BY d.id DESC LIMIT ?`, projectID, before, limit(n, 50))
	return collect(rows, err, scanDecision)
}
