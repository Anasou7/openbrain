package web

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"openbrain/internal/store"
)

// profileBlock is one item of the profile page, in reading order: a knowledge
// section, or the features list.
type profileBlock struct {
	Section  *store.ProfileSection
	Features bool
}

// checkItem is one canonical section in the completeness checklist.
type checkItem struct {
	Key, Title, State string // State: known, unknown, not_applicable or missing
}

// profileBlocks orders the profile for reading: sections in canonical order
// with the features placed where the features overview belongs, then custom
// sections.
func profileBlocks(sections []store.ProfileSection) []profileBlock {
	keys := store.CanonicalKeys()
	at := slices.Index(keys, "features_overview")
	var blocks []profileBlock
	placed := false
	for i := range sections {
		idx := slices.Index(keys, sections[i].Key)
		if !placed && (idx < 0 || idx > at) {
			blocks, placed = append(blocks, profileBlock{Features: true}), true
		}
		blocks = append(blocks, profileBlock{Section: &sections[i]})
	}
	if !placed {
		blocks = append(blocks, profileBlock{Features: true})
	}
	return blocks
}

func (s *Server) projectProfile(w http.ResponseWriter, r *http.Request, v *viewer) {
	prof, err := s.st.Profile(r.Context(), r.PathValue("ref"), store.ProfileOptions{IncludeRemoved: true})
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, v)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	state := map[string]string{}
	for _, sec := range prof.Sections {
		state[sec.Key] = sec.Status
	}
	var checklist []checkItem
	var missing []store.CanonicalSection
	for _, c := range store.CanonicalSections {
		st := state[c.Key]
		if st == "" {
			st = "missing"
			missing = append(missing, c)
		}
		checklist = append(checklist, checkItem{c.Key, c.Title, st})
	}
	var active, removed []store.Feature
	for _, f := range prof.Features {
		if f.Status == "removed" {
			removed = append(removed, f)
		} else {
			active = append(active, f)
		}
	}
	s.render(w, r, http.StatusOK, "profile", v, map[string]any{
		"Nav": "projects", "P": prof.Project, "Prof": prof, "Blocks": profileBlocks(prof.Sections),
		"Features": active, "Removed": removed, "Checklist": checklist, "Missing": missing,
		"SectionStatuses": store.SectionStatuses, "FeatureStatuses": store.FeatureStatuses,
	})
}

// posted returns a posted form value, or nil if it was not sent or is blank.
func posted(r *http.Request, name string) *string {
	if v := field(r, name); v != nil && strings.TrimSpace(*v) != "" {
		return v
	}
	return nil
}

// saveSection creates or replaces a knowledge section; the signed-in person is the author.
func (s *Server) saveSection(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref, key := r.PathValue("ref"), r.PathValue("key")
	if key == "" {
		key = r.PostForm.Get("key")
	}
	_, err := s.st.PutSection(r.Context(), ref, store.SectionInput{Attribution: v.by(), SectionChange: store.SectionChange{
		Key: key, Title: posted(r, "title"), Content: field(r, "content"), Status: posted(r, "status"),
	}})
	s.back(w, r, projectPath(ref)+"/profile", "s-"+strings.ToLower(strings.TrimSpace(key)), err)
}

func (s *Server) removeSection(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref, key := r.PathValue("ref"), r.PathValue("key")
	if r.PostForm.Get("confirm") != "yes" {
		s.back(w, r, projectPath(ref)+"/profile", "s-"+key, &store.ValidationError{Field: "removal", Problem: "needs the confirmation box ticked"})
		return
	}
	err := s.st.DeleteSection(r.Context(), ref, key, v.by())
	s.back(w, r, projectPath(ref)+"/profile", "", err)
}

// saveFeature adds a feature (no slug in the URL) or updates one; the
// signed-in person is the author.
func (s *Server) saveFeature(w http.ResponseWriter, r *http.Request, v *viewer) {
	ref, slug := r.PathValue("ref"), r.PathValue("slug")
	c := store.FeatureChange{
		Slug: slug, Name: field(r, "name"), Summary: field(r, "summary"), Description: field(r, "description"),
		Status: posted(r, "status"), TargetUsers: field(r, "target_users"), Limitations: field(r, "limitations"),
	}
	if uc := field(r, "use_cases"); uc != nil {
		cases := strings.Split(strings.ReplaceAll(*uc, "\r\n", "\n"), "\n")
		c.UseCases = &cases
	}
	var err error
	if slug == "" {
		c.Slug = r.PostForm.Get("slug")
		var f store.Feature
		f, err = s.st.CreateFeature(r.Context(), ref, store.FeatureInput{Attribution: v.by(), FeatureChange: c})
		slug = f.Slug
	} else {
		_, _, err = s.st.PutFeature(r.Context(), ref, store.FeatureInput{Attribution: v.by(), FeatureChange: c})
	}
	anchor := "features"
	if err == nil {
		anchor = "f-" + slug
	}
	s.back(w, r, projectPath(ref)+"/profile", anchor, err)
}

// snapshot is the product summary at the top of a project page, from the
// profile's overview and audience sections.
type snapshot struct {
	WhatItIs, ForWhom  string
	Sections, Features int
}

func productSnapshot(prof store.Profile) snapshot {
	sn := snapshot{Sections: len(prof.Sections), Features: prof.Completeness.Features}
	first := func(key string) string {
		for _, sec := range prof.Sections {
			if sec.Key == key && sec.Status == "known" {
				if ps := paras(sec.Content); len(ps) > 0 {
					return ps[0]
				}
			}
		}
		return ""
	}
	sn.WhatItIs = first("overview")
	if sn.ForWhom = first("icp"); sn.ForWhom == "" {
		sn.ForWhom = first("target_audience")
	}
	return sn
}
