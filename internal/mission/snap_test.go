package mission

import "testing"

// state builds a report from a current snapshot and the last agreed one.
func state(snap, applied string) RepoState {
	return RepoState{Snap: Snap{Head: snap, Commit: snap}, Applied: applied}
}

// including marks a report as having the given peer snapshot merged into it.
func including(r RepoState, peerSnap string) RepoState {
	r.Includes = peerSnap

	return r
}

func TestSettle(t *testing.T) {
	cases := []struct {
		name   string
		mine   RepoState
		theirs RepoState
		holds  bool
		want   Settlement
	}{
		{"the peer has no worktree yet", state("a", ""), RepoState{}, true, SettleNothing},
		{"this host has no worktree yet", RepoState{}, state("a", ""), false, SettleNothing},
		{"already agreed and recorded", state("a", "a"), state("a", "a"), false, SettleNothing},
		{"agree, not yet recorded here", state("a", ""), state("a", "a"), true, SettleAgreed},
		{"the peer moved on, this host did not", state("a", "a"), state("b", "a"), false, SettleTake},
		{"the holder takes a mirror's edit too", state("a", "a"), state("b", "a"), true, SettleTake},
		{"this host moved on, the peer did not", state("b", "a"), state("a", "a"), true, SettleNothing},
		{"both moved on", state("b", "a"), state("c", "a"), true, SettleDiverged},
		{"both moved on, from the mirror's side", state("c", "a"), state("b", "a"), false, SettleDiverged},
		// The holder has never agreed anything, so it reads as edited; a fresh
		// mirror built from an older snapshot simply follows it.
		{"a mirror behind a holder that kept working", state("a", "a"), state("c", ""), false, SettleTake},
		{"the holder ignores a mirror that is merely behind", state("c", ""), state("a", "a"), true, SettleNothing},
		// Neither has edited, yet they differ. Only the follower moves, or the two
		// would swap states forever.
		{"both unedited: the mirror follows", state("a", "a"), state("b", "b"), false, SettleTake},
		{"both unedited: the holder stays", state("b", "b"), state("a", "a"), true, SettleNothing},
		// One host merged the other's state into its own and says so.
		{"the peer merged this state in: take the result", state("c", "a"), including(state("m", "a"), "c"), true, SettleTake},
		{"this host merged the peer's state in: wait", including(state("m", "a"), "c"), state("c", "a"), false, SettleNothing},
		{"the peer merged an older state: still diverged", state("d", "a"), including(state("m", "a"), "c"), true, SettleDiverged},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Settle(tc.mine, tc.theirs, tc.holds); got != tc.want {
				t.Errorf("Settle = %d, want %d", got, tc.want)
			}
		})
	}
}

// Whatever the two hosts report, at most one of them may decide to take the
// other's worktree. If both did, each would overwrite itself with the other's
// previous state and they would never settle.
func TestSettleNeverHasBothSidesTake(t *testing.T) {
	ids := []string{"a", "b", "c"}
	applied := []string{"", "a", "b", "c"}

	for _, mySnap := range ids {
		for _, myApplied := range applied {
			for _, theirSnap := range ids {
				for _, theirApplied := range applied {
					// Only the primary merges, so only one side ever reports having
					// included the other's state.
					for _, included := range applied {
						mine := including(state(mySnap, myApplied), included)
						theirs := state(theirSnap, theirApplied)

						for _, iHold := range []bool{true, false} {
							here := Settle(mine, theirs, iHold)
							there := Settle(theirs, mine, !iHold)

							if here == SettleTake && there == SettleTake {
								t.Errorf("both take: mine %+v theirs %+v, holder here %v", mine, theirs, iHold)
							}

							if (here == SettleDiverged) != (there == SettleDiverged) {
								t.Errorf("only one side sees divergence: mine %+v theirs %+v", mine, theirs)
							}
						}
					}
				}
			}
		}
	}
}
