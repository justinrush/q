package styles

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// quadraticTruncate is how Truncate used to cut, kept as the reference its
// one-pass replacement must agree with.
func quadraticTruncate(s string, width int) string {
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}

	return string(runes) + "…"
}

func TestTruncateCutsWhereItAlwaysHas(t *testing.T) {
	inputs := []string{
		"",
		"short",
		"exactly ten",
		"a much longer line than any card is wide",
		"wide 中文字符 characters 漢字 mixed in",
		"emoji 🚀🔥 and a family 👨‍👩‍👧 in a line",
		"accents: café, naïve, résumé, déjà vu",
		strings.Repeat("the agent said something long ", 40),
	}

	for _, s := range inputs {
		for width := -1; width <= 45; width++ {
			got := Truncate(s, width)

			want := ""
			switch {
			case width <= 0:
			case lipgloss.Width(s) <= width:
				want = s
			case width == 1:
				want = "…"
			default:
				want = quadraticTruncate(s, width)
			}

			if got != want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", s, width, got, want)
			}

			if width > 0 && lipgloss.Width(got) > width {
				t.Errorf("Truncate(%q, %d) = %q is %d wide", s, width, got, lipgloss.Width(got))
			}
		}
	}
}
