package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxShort = 1000     // names, tasks, branches, URLs
	maxText  = 64 << 10 // note bodies and handoff fields
	maxMeta  = 16 << 10 // metadata JSON
)

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// validator records the first problem found; later checks are no-ops.
type validator struct{ err error }

func (v *validator) fail(field, problem string) {
	if v.err == nil {
		v.err = &ValidationError{field, problem}
	}
}

// text trims *s in place and checks its length.
func (v *validator) text(field string, s *string, max int, required bool) {
	*s = strings.TrimSpace(*s)
	switch {
	case required && *s == "":
		v.fail(field, "is required")
	case len(*s) > max:
		v.fail(field, fmt.Sprintf("must be at most %d bytes", max))
	case !utf8.ValidString(*s):
		v.fail(field, "must be valid UTF-8")
	}
}

// line is text that must fit on one line, such as a title.
func (v *validator) line(field string, s *string, max int, required bool) {
	v.text(field, s, max, required)
	if strings.ContainsFunc(*s, unicode.IsControl) {
		v.fail(field, "must be a single line")
	}
}

// slug requires a lowercase handle. All-digit slugs are refused so that a
// path segment like /projects/42 is unambiguously an ID.
func (v *validator) slug(field, s string) {
	if !slugRE.MatchString(s) || strings.Trim(s, "0123456789") == "" {
		v.fail(field, "must be 1-64 lowercase letters, digits, '-' or '_', start with a letter or digit, and not be all digits")
	}
}

func (v *validator) oneOf(field, s string, allowed []string) {
	if !slices.Contains(allowed, s) {
		v.fail(field, "must be one of: "+strings.Join(allowed, ", "))
	}
}

// token checks single-line values without spaces, such as branches and URLs.
func (v *validator) token(field string, s *string) {
	v.text(field, s, maxShort, false)
	if strings.ContainsFunc(*s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		v.fail(field, "must not contain whitespace or control characters")
	}
}

// repoURL checks a git remote and refuses embedded credentials, which would
// otherwise be stored and shown to every client.
func (v *validator) repoURL(field string, s *string) {
	v.token(field, s)
	if *s == "" || v.err != nil {
		return
	}
	if strings.Contains(*s, "://") {
		u, err := url.Parse(*s)
		if err != nil {
			v.fail(field, "is not a valid URL")
			return
		}
		_, hasPassword := u.User.Password()
		web := u.Scheme == "http" || u.Scheme == "https"
		if hasPassword || (web && u.User != nil) {
			v.fail(field, "must not contain credentials; use the plain remote URL")
			return
		}
	}
	if RepoKey(*s) == "" {
		v.fail(field, "is not a recognisable repository URL")
	}
}

func (v *validator) idempotencyKey(s *string) {
	v.token("idempotency_key", s)
	if len(*s) > 128 {
		v.fail("idempotency_key", "must be at most 128 bytes")
	}
}

func (v *validator) commit(field string, s *string) {
	*s = strings.ToLower(strings.TrimSpace(*s))
	if *s != "" && (len(*s) < 4 || len(*s) > 64 || strings.Trim(*s, "0123456789abcdef") != "") {
		v.fail(field, "must be a hexadecimal commit hash (4-64 characters)")
	}
}

// metadata returns raw as compact JSON, requiring an object. Empty means {}.
func (v *validator) metadata(field string, raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	if len(raw) > maxMeta {
		v.fail(field, fmt.Sprintf("must be at most %d bytes", maxMeta))
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		v.fail(field, "must be a JSON object")
		return ""
	}
	var b bytes.Buffer
	json.Compact(&b, raw) // cannot fail: raw just parsed
	return b.String()
}

// Slugify derives a slug from a display name: "My App!" becomes "my-app".
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	s = strings.TrimRight(s, "-")
	if strings.Trim(s, "0123456789") == "" {
		s = strings.TrimRight("project-"+s, "-")
	}
	return s
}

// Excerpt returns the first line of s, cut to at most n runes.
func Excerpt(s string, n int) string {
	s, _, cut := strings.Cut(strings.TrimSpace(s), "\n")
	if utf8.RuneCountInString(s) > n {
		s, cut = string([]rune(s)[:n]), true
	}
	if cut {
		return strings.TrimSpace(s) + "…"
	}
	return s
}
