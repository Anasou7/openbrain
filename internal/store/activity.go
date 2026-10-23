package store

import (
	"context"
	"database/sql"
	"math"
)

// Activity is one entry in a project's timeline. Entries are written in the
// same transaction as the change they describe and never modified.
type Activity struct {
	ID        int64    `json:"id"`
	ProjectID int64    `json:"project_id"`
	Actor     ActorRef `json:"actor"`
	Kind      string   `json:"kind"`
	SubjectID int64    `json:"subject_id,omitempty"`
	Summary   string   `json:"summary"`
	Via       string   `json:"via"`
	CreatedAt Time     `json:"created_at"`
}

type event struct {
	project, actor, subject int64
	kind, summary, via      string
	at                      Time
}

func logActivity(ctx context.Context, tx *sql.Tx, e event) error {
	if e.via == "" {
		e.via = "unknown"
	}
	var subject any
	if e.subject != 0 {
		subject = e.subject
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO activity (project_id, actor_id, kind, subject_id, summary, via, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, e.project, e.actor, e.kind, subject, e.summary, e.via, e.at)
	return err
}

const activitySelect = `SELECT e.id, e.project_id, a.slug, a.display_name, a.kind, e.kind,
	coalesce(e.subject_id, 0), e.summary, e.via, e.created_at
FROM activity e JOIN actors a ON a.id = e.actor_id`

func scanActivity(sc scanner) (e Activity, err error) {
	err = sc.Scan(&e.ID, &e.ProjectID, &e.Actor.Slug, &e.Actor.DisplayName, &e.Actor.Kind, &e.Kind,
		&e.SubjectID, &e.Summary, &e.Via, &e.CreatedAt)
	return e, err
}

// Activity returns a project's timeline, newest first. To page backwards,
// pass the smallest ID already seen as before (0 starts from the newest).
func (s *Store) Activity(ctx context.Context, ref string, before int64, n int) ([]Activity, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return nil, err
	}
	return listActivity(ctx, s.r, p.ID, before, n)
}

// FeedItem is an activity entry together with its project, for the
// cross-project timeline.
type FeedItem struct {
	Activity
	ProjectSlug string `json:"project_slug"`
	ProjectName string `json:"project_name"`
}

// Feed returns the timeline across all projects, newest first, paged like
// Activity.
func (s *Store) Feed(ctx context.Context, before int64, n int) ([]FeedItem, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	rows, err := s.r.QueryContext(ctx, `SELECT e.id, e.project_id, a.slug, a.display_name, a.kind, e.kind,
		coalesce(e.subject_id, 0), e.summary, e.via, e.created_at, p.slug, p.name
		FROM activity e JOIN actors a ON a.id = e.actor_id JOIN projects p ON p.id = e.project_id
		WHERE e.id < ? ORDER BY e.id DESC LIMIT ?`, before, limit(n, 100))
	return collect(rows, err, func(sc scanner) (f FeedItem, err error) {
		err = sc.Scan(&f.ID, &f.ProjectID, &f.Actor.Slug, &f.Actor.DisplayName, &f.Actor.Kind, &f.Kind,
			&f.SubjectID, &f.Summary, &f.Via, &f.CreatedAt, &f.ProjectSlug, &f.ProjectName)
		return f, err
	})
}

func listActivity(ctx context.Context, q querier, projectID, before int64, n int) ([]Activity, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	rows, err := q.QueryContext(ctx, activitySelect+` WHERE e.project_id = ? AND e.id < ? ORDER BY e.id DESC LIMIT ?`,
		projectID, before, limit(n, 50))
	return collect(rows, err, scanActivity)
}
