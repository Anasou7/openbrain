package store

import (
	"context"
	"net/url"
	"strings"
)

// RepoKey reduces a git remote to "host/path" so that equivalent spellings
// match: "git@github.com:Acme/Widget.git", "https://github.com/acme/widget"
// and "ssh://git@github.com:22/acme/widget" all become
// "github.com/acme/widget". Credentials, ports, a trailing ".git" and case
// are ignored. It returns "" for input it cannot interpret.
func RepoKey(remote string) string {
	s := strings.ToLower(strings.TrimSpace(remote))
	var host, path string
	switch i := strings.Index(s, ":"); {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		host, path = u.Hostname(), u.Path
	case i > 0 && !strings.Contains(s[:i], "/"): // scp-like: [user@]host:path
		host, path = s[:i], s[i+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
	default: // "github.com/acme/widget" or a local path
		path = s
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	if host == "" {
		return path
	}
	return host + "/" + path
}

// ProjectsByRepo returns the projects, archived ones included, whose
// repository URL refers to the same repository as remote.
func (s *Store) ProjectsByRepo(ctx context.Context, remote string) ([]Project, error) {
	key := RepoKey(remote)
	if key == "" {
		return nil, &ValidationError{"repo_url", "is not a recognisable repository URL"}
	}
	all, err := listProjects(ctx, s.r, true)
	return matchRepo(all, key), err
}

// ponytail: linear scan over all projects; store the key in an indexed
// column if anyone tracks thousands of projects.
func matchRepo(projects []Project, key string) []Project {
	out := []Project{}
	for _, p := range projects {
		if p.RepoURL != "" && RepoKey(p.RepoURL) == key {
			out = append(out, p)
		}
	}
	return out
}
