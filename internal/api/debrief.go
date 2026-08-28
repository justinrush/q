package api

// The debrief vocabulary, which crosses the wire: the board asks for one, and
// the daemon answers with what it opened.

// Mode selects how the debrief session is presented.
type Mode string

// The presentation modes.
const (
	// ModeAttach opens a new terminal attached to the session.
	ModeAttach Mode = "attach"
	// ModeSteal detaches any other client first, which is wanted when a stale client
	// on another machine or terminal is holding the session.
	ModeSteal Mode = "steal"
	// ModeRaise brings an already-attached window forward instead of opening another.
	ModeRaise Mode = "raise"
	// ModePrepare arranges the panes but does not attach, which is what the board
	// wants when it is only refreshing the layout.
	ModePrepare Mode = "prepare"
)

// Result describes what opening a debrief did.
type Result struct {
	Session string `json:"session"`
	// Touched lists the repos with changes worth a look.
	Touched []Touched `json:"touched"`
	// PanesAdded counts editor panes created by this call.
	PanesAdded int `json:"panesAdded"`
	// AlreadyAttached lists the ttys of clients already viewing the session.
	AlreadyAttached []string `json:"alreadyAttached,omitempty"`
	// Attached reports that this call brought a client to the session.
	Attached bool `json:"attached"`
	// AttachCommand is the command to run by hand when q was configured not to
	// open windows itself. It is empty whenever q did open one.
	AttachCommand string `json:"attachCommand,omitempty"`
	// NeedsRelaunch reports that the session is gone and the agent must be
	// restarted before there is anything to attach to.
	NeedsRelaunch bool `json:"needsRelaunch"`
}

// Touched is one repo with debriefable changes.
type Touched struct {
	Repo         string `json:"repo"`
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
	Dirty        bool   `json:"dirty"`
	Ahead        int    `json:"ahead"`
	ShortStat    string `json:"shortStat,omitempty"`
}
