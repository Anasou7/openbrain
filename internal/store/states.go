package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StateValues are the allowed values of ActorState.State.
var StateValues = []string{"idle", "working", "blocked", "waiting", "finished"}

// ActorState is an actor's latest working state on one project.
type ActorState struct {
	ProjectID   int64    `json:"project_id"`
	Actor       ActorRef `json:"actor"`
	State       string   `json:"state"`
	CurrentTask string   `json:"current_task"`
	StartedAt   Time     `json:"started_at,omitzero"`  // last transition into "working"
	FinishedAt  Time     `json:"finished_at,omitzero"` // last transition into "finished"
	UpdatedAt   Time     `json:"updated_at"`           // last report, even if nothing changed

	// Stale is true when the actor claims to be busy (working, waiting or
	// blocked) but has not reported for longer than the server's threshold.
	Stale bool `json:"stale"`
}

type StateInput struct {
	Attribution
	State       string  `json:"state"`
	CurrentTask *string `json:"current_task"` // nil keeps the current task
}

// SetState records an actor's state on a project. Calling it again with the
// same values is a heartbeat: it refreshes UpdatedAt without adding activity.
func (s *Store) SetState(ctx context.Context, ref string, in StateInput) (ActorState, error) {
	var v validator
	v.oneOf("state", in.State, StateValues)
	if in.CurrentTask != nil {
		v.text("current_task", in.CurrentTask, maxShort, false)
	}
	if v.err != nil {
		return ActorState{}, v.err
	}
	var projectID int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		actor, err := authorID(ctx, tx, in.Actor)
		if err != nil {
			return err
		}
		p, err := getProject(ctx, tx, ref)
		if err != nil {
			return err
		}
		projectID = p.ID
		return setState(ctx, tx, p.ID, actor, in.State, in.CurrentTask, in.Via, Now())
	})
	if err != nil {
		return ActorState{}, err
	}
	states, err := s.queryStates(ctx, s.r, `WHERE s.project_id = ? AND a.slug = ?`, projectID, in.Actor)
	if err == nil && len(states) == 0 {
		err = fmt.Errorf("state for %q %w", in.Actor, ErrNotFound)
	}
	if err != nil {
		return ActorState{}, err
	}
	return states[0], nil
}

func setState(ctx context.Context, tx *sql.Tx, projectID, actorID int64, state string, task *string, via string, now Time) error {
	var curState, curTask string
	var started, finished Time
	err := tx.QueryRowContext(ctx, `SELECT state, current_task, started_at, finished_at FROM actor_states WHERE project_id = ? AND actor_id = ?`,
		projectID, actorID).Scan(&curState, &curTask, &started, &finished)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	newTask := curTask
	if task != nil {
		newTask = *task
	}
	if !exists || state != curState {
		switch state {
		case "working":
			started = now
		case "finished":
			finished = now
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO actor_states (project_id, actor_id, state, current_task, started_at, finished_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id, actor_id) DO UPDATE SET state = excluded.state, current_task = excluded.current_task,
			started_at = excluded.started_at, finished_at = excluded.finished_at, updated_at = excluded.updated_at`,
		projectID, actorID, state, newTask, started, finished, now)
	if err != nil {
		return err
	}
	if exists && state == curState && newTask == curTask {
		return nil // heartbeat
	}
	summary := "is " + state
	if newTask != "" {
		summary += ": " + Excerpt(newTask, 120)
	}
	return logActivity(ctx, tx, event{project: projectID, actor: actorID, via: via, at: now, kind: "state.changed", summary: summary})
}

// States lists every actor's state on a project, most recently updated first.
func (s *Store) States(ctx context.Context, ref string) ([]ActorState, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return nil, err
	}
	return s.queryStates(ctx, s.r, `WHERE s.project_id = ?`, p.ID)
}

const stateSelect = `SELECT s.project_id, a.slug, a.display_name, a.kind, s.state, s.current_task,
	s.started_at, s.finished_at, s.updated_at
FROM actor_states s JOIN actors a ON a.id = s.actor_id`

func (s *Store) queryStates(ctx context.Context, q querier, where string, args ...any) ([]ActorState, error) {
	rows, err := q.QueryContext(ctx, stateSelect+" "+where+" ORDER BY s.updated_at DESC", args...)
	now := time.Now()
	return collect(rows, err, func(sc scanner) (st ActorState, err error) {
		err = sc.Scan(&st.ProjectID, &st.Actor.Slug, &st.Actor.DisplayName, &st.Actor.Kind, &st.State, &st.CurrentTask,
			&st.StartedAt, &st.FinishedAt, &st.UpdatedAt)
		busy := st.State == "working" || st.State == "waiting" || st.State == "blocked"
		st.Stale = busy && now.Sub(st.UpdatedAt.Time) > s.StaleAfter
		return st, err
	})
}
