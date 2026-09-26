package terminal

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCurrentCommandUsesExactSessionAndConfiguredBinary(t *testing.T) {
	t.Setenv("TMUX", "")
	for _, steal := range []bool{false, true} {
		// Shell metacharacters must remain literal in the target argument.
		cmd := CurrentCommand(t.Context(), "/custom path/tmux", "q-test; echo bad", steal)
		want := []string{"/custom path/tmux", "attach-session"}
		if steal {
			want = append(want, "-d")
		}
		want = append(want, "-t", "=q-test; echo bad")
		if !reflect.DeepEqual(cmd.Args, want) {
			t.Fatalf("args = %q, want %q", cmd.Args, want)
		}
	}
}

func TestCurrentCommandSwitchesInsideTmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-test/default,123,0")
	cmd := CurrentCommand(t.Context(), "tmux", "q-mission", false)
	want := []string{"tmux", "switch-client", "-t", "=q-mission"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %q, want %q", cmd.Args, want)
	}
}

func TestCurrentStealOnlyDetachesOtherMissionClients(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-test/default,123,0")
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	t.Setenv("ATTACH_TEST_LOG", log)
	bin := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$ATTACH_TEST_LOG\"\n" +
		"case \"$1\" in\n" +
		"  display-message) printf '/dev/pts/current\\n' ;;\n" +
		"  list-clients) printf '/dev/pts/current\\n/dev/pts/other\\n' ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := CurrentCommand(t.Context(), bin, "q-mission", true).Run(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "display-message -p #{client_tty}\n" +
		"switch-client -t =q-mission -c /dev/pts/current\n" +
		"list-clients -t =q-mission -F #{client_tty}\n" +
		"detach-client -t /dev/pts/other\n"
	if string(data) != want {
		t.Fatalf("calls = %q, want %q", data, want)
	}
}
