package domain

import (
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"lowercases", "Discussions API", "discussions-api"},
		{"collapses separators", "foo   ///bar", "foo-bar"},
		{"trims edges", "--foo bar--", "foo-bar"},
		{"strips punctuation", "azure-tf: v2.0 (wip)!", "azure-tf-v2-0-wip"},
		{"keeps digits", "sprint 42", "sprint-42"},
		{"single word", "weave", "weave"},
		{"already a slug", "change-management-ui", "change-management-ui"},
		{"empty falls back", "", fallbackSlug},
		{"punctuation only falls back", "!!! ???", fallbackSlug},
		{"non-ascii collapses", "café münchen", "caf-m-nchen"},
	} {
		if got := Slug(tc.in); got != tc.want {
			t.Errorf("%s: Slug(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Slug output is used as a path component, a git branch component, and a tmux
// target, so it must avoid every character those three disagree about.
func TestSlugProducesSafeCharactersOnly(t *testing.T) {
	inputs := []string{
		"a.b:c/d e",
		"  weird\t\nname  ",
		"UPPER.case:with/slashes",
		"emoji 🎉 here",
		"quotes'and\"backticks`",
		"$(rm -rf /)",
	}

	for _, in := range inputs {
		got := Slug(in)

		for _, r := range got {
			isSafe := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !isSafe {
				t.Errorf("Slug(%q) = %q contains unsafe rune %q", in, got, r)
			}
		}

		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("Slug(%q) = %q has a leading or trailing hyphen", in, got)
		}

		if strings.Contains(got, "--") {
			t.Errorf("Slug(%q) = %q contains a doubled hyphen", in, got)
		}
	}
}

func TestSlugTruncatesAtHyphenBoundary(t *testing.T) {
	long := "this is a really quite long operation name that exceeds the slug limit easily"

	got := Slug(long)
	if len(got) > MaxSlugLen {
		t.Errorf("Slug len = %d, want <= %d (%q)", len(got), MaxSlugLen, got)
	}

	if strings.HasSuffix(got, "-") {
		t.Errorf("Slug(%q) = %q ends with a hyphen", long, got)
	}
}

func TestMissionDirNameUsesDoubleHyphenBoundary(t *testing.T) {
	// A double hyphen keeps the operation/mission boundary recoverable even when the
	// individual slugs contain single hyphens.
	got := MissionDirName("nebula-migration", "discussions-api")

	want := "nebula-migration--discussions-api"
	if got != want {
		t.Errorf("MissionDirName = %q, want %q", got, want)
	}
}

func TestTmuxSessionNameIsPrefixedAndUnique(t *testing.T) {
	a := TmuxSessionName("disc", "api", MissionID("ms_aabbccddeeff"))
	b := TmuxSessionName("disc", "api", MissionID("ms_112233445566"))

	if !IsTmuxSessionName(a) {
		t.Errorf("%q should be recognized as a q session", a)
	}

	if a == b {
		t.Errorf("sessions for different missions collided: %q", a)
	}

	if !strings.HasSuffix(a, "aabbccddeeff") {
		t.Errorf("%q should end with the short mission id", a)
	}
}

// Long names must still leave the disambiguating id intact, since tmux target
// matching is a prefix match unless callers write "=name".
func TestTmuxSessionNameTruncatesButKeepsID(t *testing.T) {
	operationSlug := Slug("a very long operation name indeed for testing")
	missionSlug := Slug("an equally long mission name for testing purposes")
	id := MissionID("ms_aabbccddeeff")

	got := TmuxSessionName(operationSlug, missionSlug, id)

	if len(got) > MaxTmuxSessionLen {
		t.Errorf("len = %d, want <= %d (%q)", len(got), MaxTmuxSessionLen, got)
	}

	if !strings.HasSuffix(got, id.Short()) {
		t.Errorf("%q lost the mission id suffix", got)
	}

	if !IsTmuxSessionName(got) {
		t.Errorf("%q lost the q prefix", got)
	}
}

func TestIsTmuxSessionNameRejectsForeignSessions(t *testing.T) {
	for _, name := range []string{"jarush", "mac-3-7", "", "q", "zq-foo"} {
		if IsTmuxSessionName(name) {
			t.Errorf("IsTmuxSessionName(%q) = true, want false", name)
		}
	}
}

func TestBranchName(t *testing.T) {
	for _, tc := range []struct{ prefix, slug, want string }{
		{"jarush", "discussions-api", "jarush/discussions-api"},
		{"Jarush", "api", "jarush/api"},
		{"", "api", "api"},
		{"  ", "api", "api"},
	} {
		if got := BranchName(tc.prefix, tc.slug); got != tc.want {
			t.Errorf("BranchName(%q, %q) = %q, want %q", tc.prefix, tc.slug, got, tc.want)
		}
	}
}

// This mirrors the transformation the user's own shell helpers apply, so the two
// systems name things the same way.
func TestBranchSafe(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"jarush/discussions-api", "jarush-discussions-api"},
		{"release/v1.2:rc", "release-v1-2-rc"},
		{"plain", "plain"},
	} {
		if got := BranchSafe(tc.in); got != tc.want {
			t.Errorf("BranchSafe(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
