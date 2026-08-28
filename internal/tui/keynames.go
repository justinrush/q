package tui

// Key names the views compare against.
//
// These are the strings bubbletea reports for a keypress. They are named constants
// because the same few appear in every form and dialog, and a typo in one would
// silently disable a control rather than fail to compile.
const (
	keyEnter    = "enter"
	keyEsc      = "esc"
	keySpace    = " "
	keyTab      = "tab"
	keyShiftTab = "shift+tab"
	keySave     = "ctrl+s"
	keyLaunch   = "ctrl+r"
	keyUp       = "up"
	keyDown     = "down"
	keyLeft     = "left"
	keyRight    = "right"
	keyVimUp    = "k"
	keyVimDown  = "j"
	keyVimLeft  = "h"
	keyVimRight = "l"
	keyQuit     = "q"
	keyToggleX  = "x"
)
