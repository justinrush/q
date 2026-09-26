package main

// currentTerminalBin is resolved by the foreground client, never the daemon.
// An explicit tools.tmux override applies to attachment as well as provisioning.
func currentTerminalBin(s settings) string {
	if s.Terminal.Mode != terminalCurrent {
		return ""
	}
	if bin := s.Tools[string(toolTmux)]; bin != "" {
		return bin
	}
	return "tmux"
}
