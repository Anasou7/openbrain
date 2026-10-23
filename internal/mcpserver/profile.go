package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"openbrain/internal/store"
)

// --- updates and history ---

var historyKinds = []string{"handoffs", "decisions", "notes", "activity"}

type updatesIn struct {
	Project  string `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Category string `json:"category,omitempty" jsonschema:"only updates in this category"`
	ByActor  string `json:"by_actor,omitempty" jsonschema:"only updates published by this actor (slug)"`
	Before   int64  `json:"before,omitempty" jsonschema:"only updates older than this ID: next_before from a previous call"`
	Limit    int    `json:"limit,omitempty" jsonschema:"how many to return (default 20, max 100)"`
}

type updatesOut struct {
	Updates    []store.ProjectUpdate `json:"updates"`
	NextBefore int64                 `json:"next_before,omitempty" jsonschema:"pass as before to get older updates; absent when there are none"`
}

// pageSize applies the default and the cap to a requested page size.
func pageSize(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, 100)
}

// nextBefore is the cursor for the next page: the last ID, if the page was full.
func nextBefore(n, size int, lastID func() int64) int64 {
	if n == 0 || n < size {
		return 0
	}
	return lastID()
}

func (t *tools) getUpdates(ctx context.Context, _ *mcp.CallToolRequest, in updatesIn) (*mcp.CallToolResult, updatesOut, error) {
	n := pageSize(in.Limit, 20)
	us, err := t.st.Updates(ctx, in.Project, store.UpdateFilter{Actor: in.ByActor, Category: in.Category, Before: in.Before, Limit: n})
	if err != nil {
		return nil, updatesOut{}, t.fail("get_project_updates", err)
	}
	return nil, updatesOut{Updates: us, NextBefore: nextBefore(len(us), n, func() int64 { return us[len(us)-1].ID })}, nil
}

type historyIn struct {
	Project string `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Kind    string `json:"kind" jsonschema:"what to page through"`
	ByActor string `json:"by_actor,omitempty" jsonschema:"only entries by this actor (slug); handoffs and notes only"`
	Before  int64  `json:"before,omitempty" jsonschema:"only entries older than this ID: next_before from a previous call"`
	Limit   int    `json:"limit,omitempty" jsonschema:"how many to return (default 20, max 100)"`
}

type historyOut struct {
	Kind       string           `json:"kind"`
	Handoffs   []store.Handoff  `json:"handoffs,omitempty"`
	Decisions  []store.Decision `json:"decisions,omitempty"`
	Notes      []store.Note     `json:"notes,omitempty"`
	Activity   []store.Activity `json:"activity,omitempty"`
	NextBefore int64            `json:"next_before,omitempty" jsonschema:"pass as before to get older entries; absent when there are none"`
}

func (t *tools) getHistory(ctx context.Context, _ *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, historyOut, error) {
	n, out := pageSize(in.Limit, 20), historyOut{Kind: in.Kind}
	var err error
	switch in.Kind {
	case "handoffs":
		out.Handoffs, err = t.st.Handoffs(ctx, in.Project, in.ByActor, in.Before, n)
		out.NextBefore = nextBefore(len(out.Handoffs), n, func() int64 { return out.Handoffs[len(out.Handoffs)-1].ID })
	case "decisions":
		out.Decisions, err = t.st.Decisions(ctx, in.Project, in.Before, n)
		out.NextBefore = nextBefore(len(out.Decisions), n, func() int64 { return out.Decisions[len(out.Decisions)-1].ID })
	case "notes":
		out.Notes, err = t.st.Notes(ctx, in.Project, store.NoteFilter{Actor: in.ByActor, Before: in.Before, Limit: n})
		out.NextBefore = nextBefore(len(out.Notes), n, func() int64 { return out.Notes[len(out.Notes)-1].ID })
	case "activity":
		out.Activity, err = t.st.Activity(ctx, in.Project, in.Before, n)
		out.NextBefore = nextBefore(len(out.Activity), n, func() int64 { return out.Activity[len(out.Activity)-1].ID })
	}
	if err != nil {
		return nil, historyOut{}, t.fail("get_project_history", err)
	}
	return nil, out, nil
}

// --- profile ---

type profileIn struct {
	Project         string `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Section         string `json:"section,omitempty" jsonschema:"return only this knowledge section, by key (e.g. integrations)"`
	Feature         string `json:"feature,omitempty" jsonschema:"return only this feature, by slug"`
	IncludeSections *bool  `json:"include_sections,omitempty" jsonschema:"default true; false lists the features without the knowledge sections"`
	IncludeFeatures *bool  `json:"include_features,omitempty" jsonschema:"default true"`
	IncludeRemoved  bool   `json:"include_removed,omitempty" jsonschema:"also return removed features (default false)"`
}

func (t *tools) getProfile(ctx context.Context, _ *mcp.CallToolRequest, in profileIn) (*mcp.CallToolResult, store.Profile, error) {
	section := strings.ToLower(strings.TrimSpace(in.Section))
	feature := strings.ToLower(strings.TrimSpace(in.Feature))
	opt := store.ProfileOptions{
		SkipSections:   (in.IncludeSections != nil && !*in.IncludeSections) || feature != "",
		SkipFeatures:   (in.IncludeFeatures != nil && !*in.IncludeFeatures) || section != "",
		IncludeRemoved: in.IncludeRemoved || feature != "",
	}
	p, err := t.st.Profile(ctx, in.Project, opt)
	if err != nil {
		return nil, store.Profile{}, t.fail("get_project_profile", err)
	}
	if section != "" {
		i := slices.IndexFunc(p.Sections, func(s store.ProfileSection) bool { return s.Key == section })
		if i < 0 {
			var have []string
			for _, s := range p.Sections {
				have = append(have, s.Key)
			}
			return nil, store.Profile{}, fmt.Errorf("this project's profile has no %q section, so OpenBrain has no verified information about it; existing sections: %s",
				section, strings.Join(have, ", "))
		}
		p.Sections = p.Sections[i : i+1]
	}
	if feature != "" {
		i := slices.IndexFunc(p.Features, func(f store.Feature) bool { return f.Slug == feature })
		if i < 0 {
			return nil, store.Profile{}, fmt.Errorf("this project's profile has no feature %q; list them with include_sections=false", feature)
		}
		p.Features = p.Features[i : i+1]
	}
	return nil, p, nil
}

type sectionIn struct {
	Key     string  `json:"key" jsonschema:"canonical key such as overview, icp or limitations, or your own lowercase key"`
	Title   *string `json:"title,omitempty" jsonschema:"heading; defaults to the canonical title"`
	Content *string `json:"content,omitempty" jsonschema:"the section's complete text in plain English; replaces any previous text"`
	Status  *string `json:"status,omitempty" jsonschema:"known (default), unknown (not established; say why in content) or not_applicable"`
}

type featureIn struct {
	Slug        string    `json:"slug,omitempty" jsonschema:"stable lowercase identifier; derived from name if omitted"`
	Name        *string   `json:"name,omitempty" jsonschema:"user-facing name; required for a new feature"`
	Summary     *string   `json:"summary,omitempty" jsonschema:"one sentence: what it lets the user do; required for a new feature"`
	Description *string   `json:"description,omitempty" jsonschema:"how it works from the user's point of view"`
	Status      *string   `json:"status,omitempty" jsonschema:"planned, experimental, available (the default for a new feature), deprecated or removed"`
	TargetUsers *string   `json:"target_users,omitempty" jsonschema:"who it is for"`
	UseCases    *[]string `json:"use_cases,omitempty" jsonschema:"concrete ways it is used, one per item"`
	Limitations *string   `json:"limitations,omitempty" jsonschema:"what it does not do (yet)"`
}

func (f featureIn) change(branch, commit string) store.FeatureChange {
	return store.FeatureChange{Slug: f.Slug, Name: f.Name, Summary: f.Summary, Description: f.Description, Status: f.Status,
		TargetUsers: f.TargetUsers, UseCases: f.UseCases, Limitations: f.Limitations, SourceBranch: branch, SourceCommit: commit}
}

type syncIn struct {
	Project      string      `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Sections     []sectionIn `json:"sections,omitempty" jsonschema:"knowledge sections to create or replace, matched by key"`
	Features     []featureIn `json:"features,omitempty" jsonschema:"features to create or update, matched by slug"`
	SourceBranch string      `json:"source_branch,omitempty" jsonschema:"git branch the profile was derived from"`
	SourceCommit string      `json:"source_commit,omitempty" jsonschema:"git commit the profile was derived from"`
}

func (t *tools) syncProfile(ctx context.Context, req *mcp.CallToolRequest, in syncIn) (*mcp.CallToolResult, store.SyncResult, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.SyncResult{}, err
	}
	change := store.ProfileChange{SourceBranch: in.SourceBranch, SourceCommit: in.SourceCommit}
	for _, s := range in.Sections {
		change.Sections = append(change.Sections, store.SectionChange{Key: s.Key, Title: s.Title, Content: s.Content, Status: s.Status})
	}
	for _, f := range in.Features {
		change.Features = append(change.Features, f.change("", ""))
	}
	r, err := t.st.SyncProfile(ctx, in.Project, store.ProfileInput{Attribution: by, ProfileChange: change})
	return nil, r, t.wrote("sync_project_profile", by, err)
}

type knowledgeIn struct {
	Project      string  `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	SectionKey   string  `json:"section_key" jsonschema:"canonical key such as pricing, limitations or integrations, or your own lowercase key"`
	Title        *string `json:"title,omitempty" jsonschema:"heading; defaults to the canonical title"`
	Content      *string `json:"content,omitempty" jsonschema:"the section's complete new text in plain English; replaces the old text"`
	Status       *string `json:"status,omitempty" jsonschema:"known (default), unknown (not established; say why in content) or not_applicable"`
	SourceBranch string  `json:"source_branch,omitempty" jsonschema:"git branch the change was derived from"`
	SourceCommit string  `json:"source_commit,omitempty" jsonschema:"git commit the change was derived from"`
}

func (t *tools) updateKnowledge(ctx context.Context, req *mcp.CallToolRequest, in knowledgeIn) (*mcp.CallToolResult, store.ProfileSection, error) {
	by, err := caller(req)
	if err != nil {
		return nil, store.ProfileSection{}, err
	}
	sec, err := t.st.PutSection(ctx, in.Project, store.SectionInput{Attribution: by, SectionChange: store.SectionChange{
		Key: in.SectionKey, Title: in.Title, Content: in.Content, Status: in.Status, SourceBranch: in.SourceBranch, SourceCommit: in.SourceCommit,
	}})
	return nil, sec, t.wrote("update_project_knowledge", by, err)
}

type upsertFeatureIn struct {
	Project      string    `json:"project" jsonschema:"Project ID or slug, as returned by find_project or list_projects."`
	Slug         string    `json:"slug,omitempty" jsonschema:"stable lowercase identifier; derived from name if omitted"`
	Name         *string   `json:"name,omitempty" jsonschema:"user-facing name; required for a new feature"`
	Summary      *string   `json:"summary,omitempty" jsonschema:"one sentence: what it lets the user do; required for a new feature"`
	Description  *string   `json:"description,omitempty" jsonschema:"how it works from the user's point of view"`
	Status       *string   `json:"status,omitempty" jsonschema:"planned, experimental, available (the default for a new feature), deprecated or removed"`
	TargetUsers  *string   `json:"target_users,omitempty" jsonschema:"who it is for"`
	UseCases     *[]string `json:"use_cases,omitempty" jsonschema:"concrete ways it is used, one per item"`
	Limitations  *string   `json:"limitations,omitempty" jsonschema:"what it does not do (yet)"`
	SourceBranch string    `json:"source_branch,omitempty" jsonschema:"git branch the change was derived from"`
	SourceCommit string    `json:"source_commit,omitempty" jsonschema:"git commit the change was derived from"`
}

type upsertFeatureOut struct {
	Created bool          `json:"created" jsonschema:"true if the feature was new"`
	Feature store.Feature `json:"feature"`
}

func (t *tools) upsertFeature(ctx context.Context, req *mcp.CallToolRequest, in upsertFeatureIn) (*mcp.CallToolResult, upsertFeatureOut, error) {
	by, err := caller(req)
	if err != nil {
		return nil, upsertFeatureOut{}, err
	}
	c := featureIn{in.Slug, in.Name, in.Summary, in.Description, in.Status, in.TargetUsers, in.UseCases, in.Limitations}.change(in.SourceBranch, in.SourceCommit)
	f, created, err := t.st.PutFeature(ctx, in.Project, store.FeatureInput{Attribution: by, FeatureChange: c})
	return nil, upsertFeatureOut{Created: created, Feature: f}, t.wrote("upsert_project_feature", by, err)
}
