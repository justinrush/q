package domain

import (
	"strings"
	"unicode"
)

// Slug length limits. Components are capped well below any filesystem limit so
// that an operation slug, a mission slug, and a tmux prefix can be concatenated
// without truncating the parts that distinguish them.
const (
	// MaxSlugLen bounds a single operation or mission slug.
	MaxSlugLen = 40
	// MaxTmuxSessionLen bounds a generated tmux session name. tmux itself is
	// more permissive, but long names make status lines and `tmux ls` unusable.
	MaxTmuxSessionLen = 55
)

// tmuxSessionPrefix marks sessions q owns, so `tmux list-sessions` can be
// filtered to ours without consulting state.
const tmuxSessionPrefix = "q-"

// fallbackSlug is used when a name slugs down to nothing, e.g. a name written
// entirely in punctuation or a script Slug does not transliterate.
const fallbackSlug = "untitled"

// Slug reduces a human name to lowercase alphanumerics and single hyphens.
//
// The output is safe to use as a path component, a git branch component, and a
// tmux target: it contains none of the characters those three disagree about
// (dots and colons break tmux targets, spaces break argv-free shell reuse, and
// leading hyphens look like flags).
func Slug(name string) string {
	var b strings.Builder

	b.Grow(len(name))

	var pendingHyphen bool

	for _, r := range strings.ToLower(name) {
		switch {
		case unicode.IsLetter(r) && r < unicode.MaxASCII, unicode.IsDigit(r) && r < unicode.MaxASCII:
			if pendingHyphen && b.Len() > 0 {
				b.WriteByte('-')
			}

			pendingHyphen = false

			b.WriteRune(r)
		default:
			// Collapse any run of separators or non-ASCII into one hyphen,
			// emitted only once we know a real character follows.
			pendingHyphen = true
		}
	}

	return truncateSlug(b.String())
}

// truncateSlug trims a slug to MaxSlugLen, preferring to cut at a hyphen so the
// result stays readable.
func truncateSlug(s string) string {
	if s == "" {
		return fallbackSlug
	}

	if len(s) <= MaxSlugLen {
		return s
	}

	cut := s[:MaxSlugLen]
	if idx := strings.LastIndexByte(cut, '-'); idx > MaxSlugLen/2 {
		cut = cut[:idx]
	}

	return strings.Trim(cut, "-")
}

// MissionDirName is the directory name for a mission's worktrees, e.g.
// "nebula-migration--discussions-api". Operation and mission are joined with a double
// hyphen so the boundary survives slugs that contain single hyphens.
func MissionDirName(operationSlug, missionSlug string) string {
	return operationSlug + "--" + missionSlug
}

// TmuxSessionName builds the tmux session name for a mission.
//
// The mission id suffix guarantees uniqueness even when two operations contain
// similarly named missions. That matters more than it looks: tmux target matching
// is a prefix/fnmatch match unless the target is written "=name", so two
// sessions sharing a prefix are a real source of misdirected commands.
func TmuxSessionName(operationSlug, missionSlug string, id MissionID) string {
	suffix := "-" + id.Short()
	base := tmuxSessionPrefix + MissionDirName(operationSlug, missionSlug)

	if budget := MaxTmuxSessionLen - len(suffix); len(base) > budget {
		base = strings.Trim(base[:budget], "-")
	}

	return base + suffix
}

// IsTmuxSessionName reports whether a tmux session name was created by q.
func IsTmuxSessionName(name string) bool {
	return strings.HasPrefix(name, tmuxSessionPrefix)
}

// BranchName builds the git branch for a mission, e.g. "jarush/discussions-api".
// The prefix is the user's branch namespace, matching the convention already in
// use in their repos.
func BranchName(prefix, missionSlug string) string {
	prefix = strings.Trim(Slug(prefix), "-")
	if prefix == "" || prefix == fallbackSlug {
		return missionSlug
	}

	return prefix + "/" + missionSlug
}

// BranchSafe renders a branch name for use as a filesystem or tmux component,
// matching the transformation the user's own shell helpers apply (slashes, dots,
// and colons all become hyphens).
func BranchSafe(branch string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '.' || r == ':' {
			return '-'
		}

		return r
	}, branch)
}
