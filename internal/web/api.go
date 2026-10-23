package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"openbrain/internal/store"
)

// apiFunc returns the response body; the wrapper picks the status (201 for
// POST, 200 otherwise) or maps the error.
type apiFunc func(r *http.Request, p principal) (any, error)

type badRequest struct{ error }

func (s *Server) api(mux *http.ServeMux, pattern string, h apiFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if errors.Is(err, store.ErrInvalidToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="openbrain"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing, invalid or revoked bearer token")
			return
		}
		if err != nil {
			s.apiError(w, r, err)
			return
		}
		body, err := h(r, p)
		if err != nil {
			s.apiError(w, r, err)
			return
		}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		writeJSON(w, status, body)
	})
}

func (s *Server) apiError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *store.ValidationError
	var br badRequest
	var fb forbidden
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, "invalid", ve.Error())
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body exceeds %d bytes", tooBig.Limit))
	case errors.As(err, &br):
		writeError(w, http.StatusBadRequest, "bad_request", br.Error())
	case errors.As(err, &fb):
		writeError(w, http.StatusForbidden, "forbidden", fb.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}

// decode reads a single JSON object, rejecting unknown fields so that typos
// in agent requests fail loudly instead of being silently dropped.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return tooBig
		}
		return badRequest{fmt.Errorf("invalid JSON body: %w", err)}
	}
	if dec.More() {
		return badRequest{errors.New("body must contain a single JSON object")}
	}
	return nil
}

func queryInt(r *http.Request, name string) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, badRequest{fmt.Errorf("query parameter %q must be a non-negative integer", name)}
	}
	return n, nil
}

// paging reads the limit and before query parameters of a list endpoint.
func paging(r *http.Request) (n int, before int64, err error) {
	if n, err = queryInt(r, "limit"); err != nil {
		return 0, 0, err
	}
	b, err := queryInt(r, "before")
	return n, int64(b), err
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, badRequest{errors.New("id must be an integer")}
	}
	return id, nil
}

func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	// Who am I? Lets a client check its credential.
	s.api(mux, "GET /api/whoami", func(r *http.Request, p principal) (any, error) {
		if p.admin {
			return map[string]any{"kind": "admin"}, nil
		}
		return map[string]any{"kind": "actor", "actor": p.token.Actor, "token": p.token}, nil
	})

	// Cross-project timeline.
	s.api(mux, "GET /api/activity", func(r *http.Request, p principal) (any, error) {
		n, err := queryInt(r, "limit")
		if err != nil {
			return nil, err
		}
		before, err := queryInt(r, "before")
		if err != nil {
			return nil, err
		}
		feed, err := s.st.Feed(r.Context(), int64(before), n)
		return map[string]any{"activity": feed}, err
	})

	// Actors
	s.api(mux, "GET /api/actors", func(r *http.Request, p principal) (any, error) {
		actors, err := s.st.Actors(r.Context())
		return map[string]any{"actors": actors}, err
	})
	s.api(mux, "POST /api/actors", func(r *http.Request, p principal) (any, error) {
		if err := p.requireAdmin("registering actors"); err != nil {
			return nil, err
		}
		var in store.ActorInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return s.st.CreateActor(r.Context(), in)
	})

	// Cross-project overview: the dashboard grid as data.
	s.api(mux, "GET /api/overview", func(r *http.Request, p principal) (any, error) {
		return s.st.Overview(r.Context(), r.URL.Query().Get("include_archived") == "true")
	})

	// Projects
	s.api(mux, "GET /api/projects", func(r *http.Request, p principal) (any, error) {
		if repo := r.URL.Query().Get("repo_url"); repo != "" {
			projects, err := s.st.ProjectsByRepo(r.Context(), repo)
			return map[string]any{"projects": projects}, err
		}
		projects, err := s.st.Projects(r.Context(), r.URL.Query().Get("include_archived") == "true")
		return map[string]any{"projects": projects}, err
	})
	s.api(mux, "POST /api/projects", func(r *http.Request, p principal) (any, error) {
		var in store.ProjectInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.CreateProject(r.Context(), in)
	})
	s.api(mux, "GET /api/projects/{ref}", func(r *http.Request, p principal) (any, error) {
		return s.st.Project(r.Context(), r.PathValue("ref"))
	})
	s.api(mux, "PATCH /api/projects/{ref}", func(r *http.Request, p principal) (any, error) {
		var in store.ProjectPatch
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.UpdateProject(r.Context(), r.PathValue("ref"), in)
	})
	s.api(mux, "GET /api/projects/{ref}/context", func(r *http.Request, p principal) (any, error) {
		n, err := queryInt(r, "activity_limit")
		if err != nil {
			return nil, err
		}
		return s.st.ProjectContext(r.Context(), r.PathValue("ref"), store.ContextOptions{
			Compact: r.URL.Query().Get("compact") == "true", ActivityLimit: n,
		})
	})

	// Notes
	s.api(mux, "GET /api/projects/{ref}/notes", func(r *http.Request, p principal) (any, error) {
		q := r.URL.Query()
		n, before, err := paging(r)
		if err != nil {
			return nil, err
		}
		notes, err := s.st.Notes(r.Context(), r.PathValue("ref"), store.NoteFilter{
			Actor: q.Get("actor"), HumanOnly: q.Get("human") == "true", PinnedOnly: q.Get("pinned") == "true", Before: before, Limit: n,
		})
		return map[string]any{"notes": notes}, err
	})
	s.api(mux, "POST /api/projects/{ref}/notes", func(r *http.Request, p principal) (any, error) {
		var in store.NoteInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.AddNote(r.Context(), r.PathValue("ref"), in)
	})
	s.api(mux, "PATCH /api/notes/{id}", func(r *http.Request, p principal) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		var in store.NotePatch
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.UpdateNote(r.Context(), id, in)
	})

	// Handoffs
	s.api(mux, "GET /api/projects/{ref}/handoffs", func(r *http.Request, p principal) (any, error) {
		n, before, err := paging(r)
		if err != nil {
			return nil, err
		}
		handoffs, err := s.st.Handoffs(r.Context(), r.PathValue("ref"), r.URL.Query().Get("actor"), before, n)
		return map[string]any{"handoffs": handoffs}, err
	})
	s.api(mux, "GET /api/projects/{ref}/handoffs/latest", func(r *http.Request, p principal) (any, error) {
		handoffs, err := s.st.LatestHandoffs(r.Context(), r.PathValue("ref"))
		return map[string]any{"handoffs": handoffs}, err
	})
	s.api(mux, "POST /api/projects/{ref}/handoffs", func(r *http.Request, p principal) (any, error) {
		var in store.HandoffInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.AddHandoff(r.Context(), r.PathValue("ref"), in)
	})

	// Project profile: what the project is and does, for people who never see the code.
	s.api(mux, "GET /api/projects/{ref}/profile", func(r *http.Request, p principal) (any, error) {
		q := r.URL.Query()
		return s.st.Profile(r.Context(), r.PathValue("ref"), store.ProfileOptions{
			SkipSections: q.Get("include_sections") == "false", SkipFeatures: q.Get("include_features") == "false",
			IncludeRemoved: q.Get("include_removed") == "true",
		})
	})
	s.api(mux, "PATCH /api/projects/{ref}/profile", func(r *http.Request, p principal) (any, error) {
		var in store.ProfileInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.SyncProfile(r.Context(), r.PathValue("ref"), in)
	})
	s.api(mux, "GET /api/projects/{ref}/profile/{section}", func(r *http.Request, p principal) (any, error) {
		return s.st.Section(r.Context(), r.PathValue("ref"), r.PathValue("section"))
	})
	s.api(mux, "PUT /api/projects/{ref}/profile/{section}", func(r *http.Request, p principal) (any, error) {
		var in store.SectionInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if in.Key != "" && in.Key != r.PathValue("section") {
			return nil, badRequest{errors.New("key in body does not match the URL")}
		}
		in.Key = r.PathValue("section")
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.PutSection(r.Context(), r.PathValue("ref"), in)
	})
	s.api(mux, "DELETE /api/projects/{ref}/profile/{section}", func(r *http.Request, p principal) (any, error) {
		by, err := p.attribution(r.URL.Query().Get("actor"))
		if err != nil {
			return nil, err
		}
		if err := s.st.DeleteSection(r.Context(), r.PathValue("ref"), r.PathValue("section"), by); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": r.PathValue("section")}, nil
	})

	// Features
	s.api(mux, "GET /api/projects/{ref}/features", func(r *http.Request, p principal) (any, error) {
		features, err := s.st.Features(r.Context(), r.PathValue("ref"), r.URL.Query().Get("include_removed") == "true")
		return map[string]any{"features": features}, err
	})
	s.api(mux, "GET /api/projects/{ref}/features/{slug}", func(r *http.Request, p principal) (any, error) {
		return s.st.Feature(r.Context(), r.PathValue("ref"), r.PathValue("slug"))
	})
	s.api(mux, "POST /api/projects/{ref}/features", func(r *http.Request, p principal) (any, error) {
		var in store.FeatureInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.CreateFeature(r.Context(), r.PathValue("ref"), in)
	})
	s.api(mux, "PATCH /api/features/{id}", func(r *http.Request, p principal) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		var in store.FeatureInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.UpdateFeature(r.Context(), id, in)
	})

	// Project updates: what changed, written for people.
	s.api(mux, "GET /api/projects/{ref}/updates", func(r *http.Request, p principal) (any, error) {
		n, before, err := paging(r)
		if err != nil {
			return nil, err
		}
		q := r.URL.Query()
		updates, err := s.st.Updates(r.Context(), r.PathValue("ref"), store.UpdateFilter{Actor: q.Get("actor"), Category: q.Get("category"), Before: before, Limit: n})
		return map[string]any{"updates": updates}, err
	})
	s.api(mux, "GET /api/projects/{ref}/updates/latest", func(r *http.Request, p principal) (any, error) {
		return s.st.LatestUpdate(r.Context(), r.PathValue("ref"))
	})
	s.api(mux, "POST /api/projects/{ref}/updates", func(r *http.Request, p principal) (any, error) {
		var in store.UpdateInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.AddUpdate(r.Context(), r.PathValue("ref"), in)
	})

	// Decisions
	s.api(mux, "GET /api/projects/{ref}/decisions", func(r *http.Request, p principal) (any, error) {
		n, before, err := paging(r)
		if err != nil {
			return nil, err
		}
		decisions, err := s.st.Decisions(r.Context(), r.PathValue("ref"), before, n)
		return map[string]any{"decisions": decisions}, err
	})
	s.api(mux, "POST /api/projects/{ref}/decisions", func(r *http.Request, p principal) (any, error) {
		var in store.DecisionInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.AddDecision(r.Context(), r.PathValue("ref"), in)
	})

	// Blockers
	s.api(mux, "GET /api/projects/{ref}/blockers", func(r *http.Request, p principal) (any, error) {
		n, err := queryInt(r, "limit")
		if err != nil {
			return nil, err
		}
		blockers, err := s.st.Blockers(r.Context(), r.PathValue("ref"), r.URL.Query().Get("state"), n)
		return map[string]any{"blockers": blockers}, err
	})
	s.api(mux, "POST /api/projects/{ref}/blockers", func(r *http.Request, p principal) (any, error) {
		var in store.BlockerInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		var err error
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.AddBlocker(r.Context(), r.PathValue("ref"), in)
	})
	s.api(mux, "PATCH /api/blockers/{id}", func(r *http.Request, p principal) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		var in store.BlockerPatch
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if in.Attribution, err = p.attribution(in.Actor); err != nil {
			return nil, err
		}
		return s.st.UpdateBlocker(r.Context(), id, in)
	})

	// Actor states. PUT is an upsert; repeating it is a heartbeat.
	s.api(mux, "GET /api/projects/{ref}/states", func(r *http.Request, p principal) (any, error) {
		states, err := s.st.States(r.Context(), r.PathValue("ref"))
		return map[string]any{"states": states}, err
	})
	s.api(mux, "PUT /api/projects/{ref}/states/{actor}", func(r *http.Request, p principal) (any, error) {
		var in store.StateInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if in.Actor != "" && in.Actor != r.PathValue("actor") {
			return nil, badRequest{errors.New("actor in body does not match the URL")}
		}
		var err error
		if in.Attribution, err = p.attribution(r.PathValue("actor")); err != nil {
			return nil, err
		}
		return s.st.SetState(r.Context(), r.PathValue("ref"), in)
	})

	// Activity
	s.api(mux, "GET /api/projects/{ref}/activity", func(r *http.Request, p principal) (any, error) {
		n, err := queryInt(r, "limit")
		if err != nil {
			return nil, err
		}
		before, err := queryInt(r, "before")
		if err != nil {
			return nil, err
		}
		activity, err := s.st.Activity(r.Context(), r.PathValue("ref"), int64(before), n)
		return map[string]any{"activity": activity}, err
	})
}
