package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The project profile is durable knowledge of what a project is and does,
// written for people and assistants that never see the code: knowledge
// sections (what it is, who it is for, limitations, FAQ, ...) and user-facing
// features. Handoffs say where work stands; updates say what changed.

// SectionStatuses say whether a section holds a known fact. unknown and
// not_applicable make gaps explicit instead of guessed.
var SectionStatuses = []string{"known", "unknown", "not_applicable"}

// FeatureStatuses describe a feature's life cycle. Features are never deleted
// when they go away; they become deprecated, then removed.
var FeatureStatuses = []string{"planned", "experimental", "available", "deprecated", "removed"}

// CanonicalSection is a suggested profile section with its default title.
type CanonicalSection struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// CanonicalSections are the suggested sections, in display order. Any other
// key is a custom section; adding one needs no migration.
var CanonicalSections = []CanonicalSection{
	{"overview", "What it is"},
	{"target_audience", "Who it's for"},
	{"icp", "Ideal customer profile"},
	{"problem", "Problems it solves"},
	{"features_overview", "Features overview"},
	{"use_cases", "Use cases"},
	{"user_workflows", "User workflows"},
	{"positioning", "Positioning"},
	{"differentiators", "Differentiators"},
	{"terminology", "Terminology"},
	{"competitors", "Competitors"},
	{"integrations", "Integrations"},
	{"limitations", "Limitations"},
	{"known_issues", "Known issues"},
	{"faq", "FAQ"},
	{"customer_support_notes", "Customer support"},
	{"business_model", "Business model"},
	{"pricing", "Pricing"},
	{"marketing_notes", "Marketing notes"},
	{"seo_notes", "SEO notes"},
	{"architecture", "Architecture"},
	{"deployment", "Deployment"},
	{"security", "Security"},
	{"future_direction", "Future direction"},
}

// CanonicalKeys lists the canonical section keys in display order.
func CanonicalKeys() []string {
	keys := make([]string, len(CanonicalSections))
	for i, c := range CanonicalSections {
		keys[i] = c.Key
	}
	return keys
}

func canonicalIndex(key string) int {
	return slices.IndexFunc(CanonicalSections, func(c CanonicalSection) bool { return c.Key == key })
}

// defaultSectionTitle is the canonical title, or the key made readable:
// release_process becomes "Release process".
func defaultSectionTitle(key string) string {
	if i := canonicalIndex(key); i >= 0 {
		return CanonicalSections[i].Title
	}
	t := strings.NewReplacer("_", " ", "-", " ").Replace(key)
	return strings.ToUpper(t[:1]) + t[1:]
}

type ProfileSection struct {
	Key          string   `json:"key"`
	Title        string   `json:"title"`
	Content      string   `json:"content"`
	Status       string   `json:"status"`
	Canonical    bool     `json:"canonical"`
	SourceBranch string   `json:"source_branch,omitempty"`
	SourceCommit string   `json:"source_commit,omitempty"`
	CreatedBy    ActorRef `json:"created_by"`
	UpdatedBy    ActorRef `json:"updated_by"`
	CreatedAt    Time     `json:"created_at"`
	UpdatedAt    Time     `json:"updated_at"`
}

type Feature struct {
	ID           int64    `json:"id"`
	ProjectID    int64    `json:"project_id"`
	Slug         string   `json:"slug"`
	Name         string   `json:"name"`
	Summary      string   `json:"summary"`
	Description  string   `json:"description,omitempty"`
	Status       string   `json:"status"`
	TargetUsers  string   `json:"target_users,omitempty"`
	UseCases     []string `json:"use_cases"`
	Limitations  string   `json:"limitations,omitempty"`
	SourceBranch string   `json:"source_branch,omitempty"`
	SourceCommit string   `json:"source_commit,omitempty"`
	CreatedBy    ActorRef `json:"created_by"`
	UpdatedBy    ActorRef `json:"updated_by"`
	CreatedAt    Time     `json:"created_at"`
	UpdatedAt    Time     `json:"updated_at"`
}

// SectionChange creates or changes one section. Nil fields keep their value
// (or take a default for a new section).
type SectionChange struct {
	Key          string  `json:"key"`
	Title        *string `json:"title,omitempty"`
	Content      *string `json:"content,omitempty"`
	Status       *string `json:"status,omitempty"`
	SourceBranch string  `json:"source_branch,omitempty"`
	SourceCommit string  `json:"source_commit,omitempty"`
}

type SectionInput struct {
	Attribution
	SectionChange
}

// FeatureChange creates or changes one feature, identified by slug (derived
// from the name if empty). Nil fields keep their value.
type FeatureChange struct {
	Slug         string    `json:"slug,omitempty"`
	Name         *string   `json:"name,omitempty"`
	Summary      *string   `json:"summary,omitempty"`
	Description  *string   `json:"description,omitempty"`
	Status       *string   `json:"status,omitempty"`
	TargetUsers  *string   `json:"target_users,omitempty"`
	UseCases     *[]string `json:"use_cases,omitempty"`
	Limitations  *string   `json:"limitations,omitempty"`
	SourceBranch string    `json:"source_branch,omitempty"`
	SourceCommit string    `json:"source_commit,omitempty"`
}

type FeatureInput struct {
	Attribution
	FeatureChange
}

// ProfileChange is a bulk, merging change: sections are matched by key and
// features by slug; anything not mentioned is left as it is.
type ProfileChange struct {
	Sections     []SectionChange `json:"sections"`
	Features     []FeatureChange `json:"features"`
	SourceBranch string          `json:"source_branch,omitempty"` // for items that do not set their own
	SourceCommit string          `json:"source_commit,omitempty"`
}

type ProfileInput struct {
	Attribution
	ProfileChange
}

// SyncItems lists, by key or slug, what a sync did.
type SyncItems struct {
	Created   []string `json:"created"`
	Updated   []string `json:"updated"`
	Unchanged []string `json:"unchanged"`
}

type SyncResult struct {
	Sections     SyncItems    `json:"sections"`
	Features     SyncItems    `json:"features"`
	Completeness Completeness `json:"completeness"`
}

// Completeness is a structural view of which canonical sections exist. It
// says nothing about quality.
type Completeness struct {
	Known         []string `json:"known"`
	Unknown       []string `json:"unknown"`
	NotApplicable []string `json:"not_applicable"`
	Missing       []string `json:"missing"`
	Custom        []string `json:"custom"`
	Features      int      `json:"features"` // not counting removed ones
}

type Profile struct {
	Project      Project          `json:"project"`
	Sections     []ProfileSection `json:"sections"`
	Features     []Feature        `json:"features"`
	Completeness Completeness     `json:"completeness"`
	UpdatedAt    Time             `json:"updated_at,omitzero"` // the newest section or feature change
}

// ProfileOptions trim a profile read. The zero value returns everything
// except removed features.
type ProfileOptions struct {
	SkipSections   bool
	SkipFeatures   bool
	IncludeRemoved bool
}

// ProfileSummary tells a context reader that a profile exists and how fresh
// it is, without its content.
type ProfileSummary struct {
	Sections  int  `json:"sections"`
	Features  int  `json:"features"`
	UpdatedAt Time `json:"updated_at,omitzero"`
}

const (
	maxSectionTitle = 200
	maxFeatureName  = 200
	maxTargetUsers  = 2000
	maxUseCase      = 500
	maxUseCases     = 50
)

func (v *validator) section(prefix string, c *SectionChange) {
	c.Key = strings.ToLower(strings.TrimSpace(c.Key))
	if !slugRE.MatchString(c.Key) {
		v.fail(prefix+"key", "must be 1-64 lowercase letters, digits, '_' or '-', such as overview or release_process")
	}
	if c.Title != nil {
		v.line(prefix+"title", c.Title, maxSectionTitle, true)
	}
	if c.Content != nil {
		v.text(prefix+"content", c.Content, maxText, false)
	}
	if c.Status != nil {
		*c.Status = strings.ToLower(strings.TrimSpace(*c.Status))
		v.oneOf(prefix+"status", *c.Status, SectionStatuses)
	}
	v.token(prefix+"source_branch", &c.SourceBranch)
	v.commit(prefix+"source_commit", &c.SourceCommit)
}

func (v *validator) feature(prefix string, c *FeatureChange) {
	c.Slug = strings.ToLower(strings.TrimSpace(c.Slug))
	if c.Slug == "" && c.Name != nil {
		c.Slug = Slugify(*c.Name)
	}
	if c.Slug == "" {
		v.fail(prefix+"slug", "or name is required")
	} else {
		v.slug(prefix+"slug", c.Slug)
	}
	if c.Name != nil {
		v.line(prefix+"name", c.Name, maxFeatureName, true)
	}
	if c.Summary != nil {
		v.text(prefix+"summary", c.Summary, maxShort, true)
	}
	if c.Description != nil {
		v.text(prefix+"description", c.Description, maxText, false)
	}
	if c.Status != nil {
		*c.Status = strings.ToLower(strings.TrimSpace(*c.Status))
		v.oneOf(prefix+"status", *c.Status, FeatureStatuses)
	}
	if c.TargetUsers != nil {
		v.text(prefix+"target_users", c.TargetUsers, maxTargetUsers, false)
	}
	if c.UseCases != nil {
		cases := []string{}
		for _, u := range *c.UseCases {
			v.line(prefix+"use_cases", &u, maxUseCase, false)
			if u != "" {
				cases = append(cases, u)
			}
		}
		if len(cases) > maxUseCases {
			v.fail(prefix+"use_cases", fmt.Sprintf("must have at most %d items", maxUseCases))
		}
		*c.UseCases = cases
	}
	if c.Limitations != nil {
		v.text(prefix+"limitations", c.Limitations, maxText, false)
	}
	v.token(prefix+"source_branch", &c.SourceBranch)
	v.commit(prefix+"source_commit", &c.SourceCommit)
}

// --- writes ---

// putSection creates or changes one section inside a write transaction and
// reports "created", "updated" or "unchanged". An unchanged section is not
// touched, so updated_at stays the time its content last changed.
func putSection(ctx context.Context, tx *sql.Tx, projectID, actor int64, c SectionChange, now Time) (string, int64, error) {
	var id int64
	var title, content, status string
	err := tx.QueryRowContext(ctx, `SELECT id, title, content, status FROM profile_sections WHERE project_id = ? AND key = ?`,
		projectID, c.Key).Scan(&id, &title, &content, &status)
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		return "", 0, err
	}
	old := [3]string{title, content, status}
	if created {
		title, status = defaultSectionTitle(c.Key), "known"
	}
	if c.Title != nil {
		title = *c.Title
	}
	if c.Content != nil {
		content = *c.Content
	}
	if c.Status != nil {
		status = *c.Status
	}
	if status == "known" && content == "" {
		return "", 0, &ValidationError{"content", fmt.Sprintf("is required for section %q unless its status is unknown or not_applicable", c.Key)}
	}
	// A change records the source it was made from (none, for an edit by hand).
	branch, commit := c.SourceBranch, c.SourceCommit
	if created {
		res, err := tx.ExecContext(ctx, `INSERT INTO profile_sections
			(project_id, key, title, content, status, source_branch, source_commit, created_by, updated_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, projectID, c.Key, title, content, status, branch, commit, actor, actor, now, now)
		if err != nil {
			return "", 0, err
		}
		id, err = res.LastInsertId()
		return "created", id, err
	}
	if old == [3]string{title, content, status} {
		return "unchanged", id, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE profile_sections SET title = ?, content = ?, status = ?, source_branch = ?, source_commit = ?,
		updated_by = ?, updated_at = ? WHERE id = ?`, title, content, status, branch, commit, actor, now, id)
	return "updated", id, err
}

type featureMode int

const (
	upsertFeature featureMode = iota
	createFeature
	updateFeature
)

// putFeature creates or changes one feature inside a write transaction and
// reports "created", "updated" or "unchanged", with the feature's ID.
func putFeature(ctx context.Context, tx *sql.Tx, projectID, actor int64, c FeatureChange, mode featureMode, now Time) (string, int64, error) {
	var id int64
	var f [7]string // name, summary, description, status, target_users, use_cases, limitations
	err := tx.QueryRowContext(ctx, `SELECT id, name, summary, description, status, target_users, use_cases, limitations
		FROM features WHERE project_id = ? AND slug = ?`, projectID, c.Slug).
		Scan(&id, &f[0], &f[1], &f[2], &f[3], &f[4], &f[5], &f[6])
	created := errors.Is(err, sql.ErrNoRows)
	switch {
	case err != nil && !created:
		return "", 0, err
	case created && mode == updateFeature:
		return "", 0, fmt.Errorf("feature %q %w", c.Slug, ErrNotFound)
	case !created && mode == createFeature:
		return "", 0, fmt.Errorf("feature %q %w", c.Slug, ErrConflict)
	}
	old := f
	if created {
		f[3] = "available"
	}
	for i, p := range []*string{c.Name, c.Summary, c.Description, c.Status, c.TargetUsers, nil, c.Limitations} {
		if p != nil {
			f[i] = *p
		}
	}
	if c.UseCases != nil {
		f[5] = strings.Join(*c.UseCases, "\n")
	}
	if f[0] == "" {
		return "", 0, &ValidationError{"name", fmt.Sprintf("is required for new feature %q", c.Slug)}
	}
	if f[1] == "" {
		return "", 0, &ValidationError{"summary", fmt.Sprintf("is required for new feature %q", c.Slug)}
	}
	branch, commit := c.SourceBranch, c.SourceCommit
	if created {
		res, err := tx.ExecContext(ctx, `INSERT INTO features
			(project_id, slug, name, summary, description, status, target_users, use_cases, limitations, source_branch, source_commit,
			 created_by, updated_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			projectID, c.Slug, f[0], f[1], f[2], f[3], f[4], f[5], f[6], branch, commit, actor, actor, now, now)
		if err != nil {
			return "", 0, err
		}
		id, err = res.LastInsertId()
		return "created", id, err
	}
	if old == f {
		return "unchanged", id, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE features SET name = ?, summary = ?, description = ?, status = ?, target_users = ?, use_cases = ?,
		limitations = ?, source_branch = ?, source_commit = ?, updated_by = ?, updated_at = ? WHERE id = ?`,
		f[0], f[1], f[2], f[3], f[4], f[5], f[6], branch, commit, actor, now, id)
	return "updated", id, err
}

// PutSection creates or replaces one knowledge section.
func (s *Store) PutSection(ctx context.Context, ref string, in SectionInput) (ProfileSection, error) {
	var v validator
	v.section("", &in.SectionChange)
	if v.err != nil {
		return ProfileSection{}, v.err
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		actor, p, err := authorAndProject(ctx, tx, in.Actor, ref)
		if err != nil {
			return err
		}
		now := Now()
		result, id, err := putSection(ctx, tx, p.ID, actor, in.SectionChange, now)
		if err != nil || result == "unchanged" {
			return err
		}
		verb := "updated the profile: "
		if result == "created" {
			verb = "added to the profile: "
		}
		var title string
		if err := tx.QueryRowContext(ctx, `SELECT title FROM profile_sections WHERE id = ?`, id).Scan(&title); err != nil {
			return err
		}
		return logActivity(ctx, tx, event{project: p.ID, actor: actor, subject: id, via: in.Via, at: now,
			kind: "profile.section_updated", summary: verb + title})
	})
	if err != nil {
		return ProfileSection{}, err
	}
	return s.Section(ctx, ref, in.Key)
}

// DeleteSection removes one knowledge section. It is the only way a section
// goes away: syncing never deletes.
func (s *Store) DeleteSection(ctx context.Context, ref, key string, by Attribution) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		actor, p, err := authorAndProject(ctx, tx, by.Actor, ref)
		if err != nil {
			return err
		}
		var id int64
		var title string
		err = tx.QueryRowContext(ctx, `SELECT id, title FROM profile_sections WHERE project_id = ? AND key = ?`, p.ID, key).Scan(&id, &title)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("profile section %q %w", key, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM profile_sections WHERE id = ?`, id); err != nil {
			return err
		}
		return logActivity(ctx, tx, event{project: p.ID, actor: actor, via: by.Via, at: Now(),
			kind: "profile.section_removed", summary: "removed from the profile: " + title})
	})
}

// PutFeature creates a feature or changes an existing one with the same slug.
func (s *Store) PutFeature(ctx context.Context, ref string, in FeatureInput) (Feature, bool, error) {
	f, result, err := s.writeFeature(ctx, ref, in, upsertFeature)
	return f, result == "created", err
}

// CreateFeature adds a feature, failing with ErrConflict if the slug exists.
func (s *Store) CreateFeature(ctx context.Context, ref string, in FeatureInput) (Feature, error) {
	f, _, err := s.writeFeature(ctx, ref, in, createFeature)
	return f, err
}

// UpdateFeature changes an existing feature, found by ID. Its slug is fixed.
func (s *Store) UpdateFeature(ctx context.Context, id int64, in FeatureInput) (Feature, error) {
	cur, err := s.FeatureByID(ctx, id)
	if err != nil {
		return Feature{}, err
	}
	if in.Slug != "" && in.Slug != cur.Slug {
		return Feature{}, &ValidationError{"slug", "cannot be changed"}
	}
	in.Slug = cur.Slug
	f, _, err := s.writeFeature(ctx, fmt.Sprint(cur.ProjectID), in, updateFeature)
	return f, err
}

func (s *Store) writeFeature(ctx context.Context, ref string, in FeatureInput, mode featureMode) (Feature, string, error) {
	var v validator
	v.feature("", &in.FeatureChange)
	if v.err != nil {
		return Feature{}, "", v.err
	}
	var id int64
	var result string
	err := s.write(ctx, func(tx *sql.Tx) error {
		actor, p, err := authorAndProject(ctx, tx, in.Actor, ref)
		if err != nil {
			return err
		}
		now := Now()
		var before string
		// Empty for a new feature; used to mention a status change in the activity log.
		_ = tx.QueryRowContext(ctx, `SELECT status FROM features WHERE project_id = ? AND slug = ?`, p.ID, in.Slug).Scan(&before)
		if result, id, err = putFeature(ctx, tx, p.ID, actor, in.FeatureChange, mode, now); err != nil || result == "unchanged" {
			return err
		}
		var name, status string
		if err := tx.QueryRowContext(ctx, `SELECT name, status FROM features WHERE id = ?`, id).Scan(&name, &status); err != nil {
			return err
		}
		kind, summary := "feature.updated", "updated feature: "+name
		if result == "created" {
			kind, summary = "feature.added", "added feature: "+name
		} else if status != before {
			summary += " (now " + status + ")"
		}
		return logActivity(ctx, tx, event{project: p.ID, actor: actor, subject: id, via: in.Via, at: now, kind: kind, summary: summary})
	})
	if err != nil {
		return Feature{}, "", err
	}
	f, err := s.FeatureByID(ctx, id)
	return f, result, err
}

// SyncProfile merges many sections and features in one transaction: either
// all of it applies or none does. Nothing omitted is deleted.
func (s *Store) SyncProfile(ctx context.Context, ref string, in ProfileInput) (SyncResult, error) {
	var v validator
	v.token("source_branch", &in.SourceBranch)
	v.commit("source_commit", &in.SourceCommit)
	if len(in.Sections)+len(in.Features) == 0 {
		v.fail("sections", "or features must contain at least one item")
	}
	if len(in.Sections) > 200 || len(in.Features) > 500 {
		v.fail("sections", "are limited to 200 sections and 500 features per call")
	}
	seen := map[string]bool{}
	for i := range in.Sections {
		c := &in.Sections[i]
		v.section(fmt.Sprintf("sections[%d].", i), c)
		if seen["s:"+c.Key] {
			v.fail(fmt.Sprintf("sections[%d].key", i), fmt.Sprintf("%q appears more than once", c.Key))
		}
		seen["s:"+c.Key] = true
		if c.SourceBranch == "" && c.SourceCommit == "" {
			c.SourceBranch, c.SourceCommit = in.SourceBranch, in.SourceCommit
		}
	}
	for i := range in.Features {
		c := &in.Features[i]
		v.feature(fmt.Sprintf("features[%d].", i), c)
		if seen["f:"+c.Slug] {
			v.fail(fmt.Sprintf("features[%d].slug", i), fmt.Sprintf("%q appears more than once", c.Slug))
		}
		seen["f:"+c.Slug] = true
		if c.SourceBranch == "" && c.SourceCommit == "" {
			c.SourceBranch, c.SourceCommit = in.SourceBranch, in.SourceCommit
		}
	}
	if v.err != nil {
		return SyncResult{}, v.err
	}

	r := SyncResult{Sections: SyncItems{[]string{}, []string{}, []string{}}, Features: SyncItems{[]string{}, []string{}, []string{}}}
	add := func(items *SyncItems, result, name string) {
		switch result {
		case "created":
			items.Created = append(items.Created, name)
		case "updated":
			items.Updated = append(items.Updated, name)
		default:
			items.Unchanged = append(items.Unchanged, name)
		}
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		actor, p, err := authorAndProject(ctx, tx, in.Actor, ref)
		if err != nil {
			return err
		}
		now := Now()
		for _, c := range in.Sections {
			result, _, err := putSection(ctx, tx, p.ID, actor, c, now)
			if err != nil {
				return err
			}
			add(&r.Sections, result, c.Key)
		}
		for _, c := range in.Features {
			result, _, err := putFeature(ctx, tx, p.ID, actor, c, upsertFeature, now)
			if err != nil {
				return err
			}
			add(&r.Features, result, c.Slug)
		}
		summary := syncSummary(r)
		if summary == "" {
			return nil // nothing changed
		}
		return logActivity(ctx, tx, event{project: p.ID, actor: actor, via: in.Via, at: now, kind: "profile.synced",
			summary: "synced the project profile: " + summary})
	})
	if err != nil {
		return SyncResult{}, err
	}
	prof, err := s.Profile(ctx, ref, ProfileOptions{SkipSections: true, SkipFeatures: true})
	r.Completeness = prof.Completeness
	return r, err
}

// syncSummary says what a sync changed, e.g. "added 3 sections and 1
// feature; updated 2 features", or "" if it changed nothing.
func syncSummary(r SyncResult) string {
	phrase := func(verb string, sections, features int) string {
		var items []string
		for _, c := range []struct {
			n    int
			word string
		}{{sections, "section"}, {features, "feature"}} {
			switch {
			case c.n == 1:
				items = append(items, "1 "+c.word)
			case c.n > 1:
				items = append(items, fmt.Sprintf("%d %ss", c.n, c.word))
			}
		}
		if len(items) == 0 {
			return ""
		}
		return verb + " " + strings.Join(items, " and ")
	}
	var parts []string
	for _, p := range []string{
		phrase("added", len(r.Sections.Created), len(r.Features.Created)),
		phrase("updated", len(r.Sections.Updated), len(r.Features.Updated)),
	} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "; ")
}

func authorAndProject(ctx context.Context, tx *sql.Tx, actorSlug, ref string) (int64, Project, error) {
	actor, err := authorID(ctx, tx, actorSlug)
	if err != nil {
		return 0, Project{}, err
	}
	p, err := getProject(ctx, tx, ref)
	return actor, p, err
}

// --- reads ---

const sectionSelect = `SELECT s.key, s.title, s.content, s.status, s.source_branch, s.source_commit,
	c.slug, c.display_name, c.kind, u.slug, u.display_name, u.kind, s.created_at, s.updated_at
FROM profile_sections s JOIN actors c ON c.id = s.created_by JOIN actors u ON u.id = s.updated_by`

func scanSection(sc scanner) (p ProfileSection, err error) {
	err = sc.Scan(&p.Key, &p.Title, &p.Content, &p.Status, &p.SourceBranch, &p.SourceCommit,
		&p.CreatedBy.Slug, &p.CreatedBy.DisplayName, &p.CreatedBy.Kind, &p.UpdatedBy.Slug, &p.UpdatedBy.DisplayName, &p.UpdatedBy.Kind,
		&p.CreatedAt, &p.UpdatedAt)
	p.Canonical = canonicalIndex(p.Key) >= 0
	return p, err
}

const featureSelect = `SELECT f.id, f.project_id, f.slug, f.name, f.summary, f.description, f.status, f.target_users, f.use_cases,
	f.limitations, f.source_branch, f.source_commit,
	c.slug, c.display_name, c.kind, u.slug, u.display_name, u.kind, f.created_at, f.updated_at
FROM features f JOIN actors c ON c.id = f.created_by JOIN actors u ON u.id = f.updated_by`

func scanFeature(sc scanner) (f Feature, err error) {
	var cases string
	err = sc.Scan(&f.ID, &f.ProjectID, &f.Slug, &f.Name, &f.Summary, &f.Description, &f.Status, &f.TargetUsers, &cases,
		&f.Limitations, &f.SourceBranch, &f.SourceCommit,
		&f.CreatedBy.Slug, &f.CreatedBy.DisplayName, &f.CreatedBy.Kind, &f.UpdatedBy.Slug, &f.UpdatedBy.DisplayName, &f.UpdatedBy.Kind,
		&f.CreatedAt, &f.UpdatedAt)
	f.UseCases = []string{}
	if cases != "" {
		f.UseCases = strings.Split(cases, "\n")
	}
	return f, err
}

// Section returns one knowledge section.
func (s *Store) Section(ctx context.Context, ref, key string) (ProfileSection, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return ProfileSection{}, err
	}
	sec, err := scanSection(s.r.QueryRowContext(ctx, sectionSelect+` WHERE s.project_id = ? AND s.key = ?`, p.ID, strings.ToLower(key)))
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("profile section %q %w", key, ErrNotFound)
	}
	return sec, err
}

// Feature returns one feature by slug.
func (s *Store) Feature(ctx context.Context, ref, slug string) (Feature, error) {
	p, err := getProject(ctx, s.r, ref)
	if err != nil {
		return Feature{}, err
	}
	f, err := scanFeature(s.r.QueryRowContext(ctx, featureSelect+` WHERE f.project_id = ? AND f.slug = ?`, p.ID, strings.ToLower(slug)))
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("feature %q %w", slug, ErrNotFound)
	}
	return f, err
}

func (s *Store) FeatureByID(ctx context.Context, id int64) (Feature, error) {
	f, err := scanFeature(s.r.QueryRowContext(ctx, featureSelect+` WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("feature %d %w", id, ErrNotFound)
	}
	return f, err
}

// Features lists a project's features in the order they were added.
func (s *Store) Features(ctx context.Context, ref string, includeRemoved bool) ([]Feature, error) {
	prof, err := s.Profile(ctx, ref, ProfileOptions{SkipSections: true, IncludeRemoved: includeRemoved})
	return prof.Features, err
}

// Profile reads a project's complete profile in one snapshot: sections in
// canonical order, then custom ones in the order they were added, and
// features. Nothing is clipped.
func (s *Store) Profile(ctx context.Context, ref string, opt ProfileOptions) (Profile, error) {
	var prof Profile
	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error
		if prof.Project, err = getProject(ctx, tx, ref); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, sectionSelect+` WHERE s.project_id = ? ORDER BY s.id`, prof.Project.ID)
		sections, err := collect(rows, err, scanSection)
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, featureSelect+` WHERE f.project_id = ? ORDER BY f.id`, prof.Project.ID)
		features, err := collect(rows, err, scanFeature)
		if err != nil {
			return err
		}

		slices.SortStableFunc(sections, func(a, b ProfileSection) int {
			ia, ib := canonicalIndex(a.Key), canonicalIndex(b.Key)
			if ia < 0 {
				ia = len(CanonicalSections)
			}
			if ib < 0 {
				ib = len(CanonicalSections)
			}
			return ia - ib
		})
		c := Completeness{Known: []string{}, Unknown: []string{}, NotApplicable: []string{}, Missing: []string{}, Custom: []string{}}
		have := map[string]string{}
		for _, sec := range sections {
			have[sec.Key] = sec.Status
			if !sec.Canonical {
				c.Custom = append(c.Custom, sec.Key)
			}
			if sec.UpdatedAt.After(prof.UpdatedAt.Time) {
				prof.UpdatedAt = sec.UpdatedAt
			}
		}
		for _, cs := range CanonicalSections {
			switch have[cs.Key] {
			case "known":
				c.Known = append(c.Known, cs.Key)
			case "unknown":
				c.Unknown = append(c.Unknown, cs.Key)
			case "not_applicable":
				c.NotApplicable = append(c.NotApplicable, cs.Key)
			default:
				c.Missing = append(c.Missing, cs.Key)
			}
		}
		prof.Sections, prof.Features = []ProfileSection{}, []Feature{}
		for _, f := range features {
			if f.Status != "removed" {
				c.Features++
			}
			if f.UpdatedAt.After(prof.UpdatedAt.Time) {
				prof.UpdatedAt = f.UpdatedAt
			}
			if !opt.SkipFeatures && (opt.IncludeRemoved || f.Status != "removed") {
				prof.Features = append(prof.Features, f)
			}
		}
		if !opt.SkipSections {
			prof.Sections = sections
		}
		prof.Completeness = c
		return nil
	})
	return prof, err
}

func profileSummary(ctx context.Context, q querier, projectID int64) (ProfileSummary, error) {
	var ps ProfileSummary
	err := q.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM profile_sections WHERE project_id = ?1),
		(SELECT count(*) FROM features WHERE project_id = ?1 AND status <> 'removed'),
		(SELECT max(updated_at) FROM (SELECT updated_at FROM profile_sections WHERE project_id = ?1
			UNION ALL SELECT updated_at FROM features WHERE project_id = ?1))`, projectID).Scan(&ps.Sections, &ps.Features, &ps.UpdatedAt)
	return ps, err
}
