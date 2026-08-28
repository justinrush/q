package keys

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
)

// This is the whole reason Forbidden exists. The user's tmux config routes
// ctrl+h/j/k/l to vim-tmux-navigator, whose passthrough check matches on process name
// and does not include q, so those keys move tmux panes and never arrive here.
// Binding one would produce a control that silently does nothing.
func TestNoBindingUsesAKeyTmuxIntercepts(t *testing.T) {
	for _, binding := range All() {
		for _, k := range binding.Keys() {
			if reason, forbidden := Forbidden[k]; forbidden {
				t.Errorf("binding %q uses %q, which is unavailable: %s",
					binding.Help().Key, k, reason)
			}
		}
	}
}

// Two bindings answering the same key means one of them silently loses.
func TestNoKeyIsBoundTwiceWithinAView(t *testing.T) {
	views := map[string][][]key.Binding{
		"global":     NewGlobal().FullHelp(),
		"board":      NewBoard().FullHelp(),
		"operations": NewOperations().FullHelp(),
		"form":       NewForm().FullHelp(),
	}

	for name, groups := range views {
		seen := map[string]string{}

		for _, group := range groups {
			for _, binding := range group {
				for _, k := range binding.Keys() {
					if previous, dup := seen[k]; dup {
						t.Errorf("%s view binds %q to both %q and %q", name, k, previous, binding.Help().Key)
					}

					seen[k] = binding.Help().Key
				}
			}
		}
	}
}

// A binding with no help text is invisible in the footer and the overlay, so a user
// has no way to discover it.
func TestHelpGroupsDescribeTheirBindings(t *testing.T) {
	views := map[string][][]key.Binding{
		"global":     NewGlobal().FullHelp(),
		"board":      NewBoard().FullHelp(),
		"operations": NewOperations().FullHelp(),
		"form":       NewForm().FullHelp(),
	}

	for name, groups := range views {
		for _, group := range groups {
			for _, binding := range group {
				help := binding.Help()

				if help.Key == "" {
					t.Errorf("%s view has a binding with no key label", name)
				}

				if help.Desc == "" {
					t.Errorf("%s view binding %q has no description", name, help.Key)
				}
			}
		}
	}
}

func TestEveryBindingHasAtLeastOneKey(t *testing.T) {
	for _, binding := range All() {
		if len(binding.Keys()) == 0 {
			t.Errorf("binding %q has no keys", binding.Help().Key)
		}
	}
}

// The board is the primary view, so its most important actions must be reachable
// without opening the help overlay.
func TestBoardShortHelpCoversTheEssentialActions(t *testing.T) {
	var labels []string

	for _, binding := range NewBoard().ShortHelp() {
		labels = append(labels, binding.Help().Desc)
	}

	joined := strings.Join(labels, " ")

	for _, want := range []string{"lane", "card", "move card", "open debrief", "new mission"} {
		if !strings.Contains(joined, want) {
			t.Errorf("board short help is missing %q, got %q", want, joined)
		}
	}
}

// Quitting must work from a terminal where the escape key is doing something else.
func TestQuitIsBoundToBothQAndCtrlC(t *testing.T) {
	keysBound := NewGlobal().Quit.Keys()

	var hasQ, hasCtrlC bool

	for _, k := range keysBound {
		switch k {
		case "q":
			hasQ = true
		case "ctrl+c":
			hasCtrlC = true
		}
	}

	if !hasQ || !hasCtrlC {
		t.Errorf("quit keys = %q, want both q and ctrl+c", keysBound)
	}
}
