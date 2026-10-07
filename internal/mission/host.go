package mission

import (
	"time"
)

// hostIDPrefix marks a host identifier, in the same spirit as the operation
// and mission prefixes.
const hostIDPrefix = "h_"

// HostID identifies one machine's q installation.
//
// It is random rather than derived from the hostname. A hostname is something a
// user changes, and two laptops restored from the same backup share one; an
// identity that says which state file a mission lives in has to survive the
// first and tell the second apart.
type HostID string

// NewHostID returns a fresh host identifier.
func NewHostID() (HostID, error) {
	s, err := randomHex(idBytes)
	if err != nil {
		return "", err
	}

	return HostID(hostIDPrefix + s), nil
}

// Valid reports whether the identifier is well formed.
func (id HostID) Valid() bool { return validID(string(id), hostIDPrefix) }

// String implements fmt.Stringer.
func (id HostID) String() string { return string(id) }

// HostInfo names a q installation for people and for its peer.
type HostInfo struct {
	ID HostID `json:"id"`
	// Name is what a card shows for this host, e.g. "mini". It defaults to the
	// machine's hostname and carries no meaning to q beyond display.
	Name string `json:"name,omitempty"`
}

// Role is the part a host plays when two q installations are paired.
//
// The roles are not symmetric and are not meant to be. The primary is the
// machine the human sits at: it dials, it is preferred for running agents, and
// it wins any tie. The secondary is the machine that is always on: it only
// answers, and it runs agents when the primary is away.
type Role string

// The roles a paired host can take. The zero value means q is not paired and
// runs as it always has.
const (
	RoleStandalone Role = ""
	RolePrimary    Role = "primary"
	RoleSecondary  Role = "secondary"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleStandalone, RolePrimary, RoleSecondary:
		return true
	default:
		return false
	}
}

// Lease says which host may run a mission's agent and write what it observes.
//
// A mission's records live on both hosts, and so do its worktrees. The lease is
// the one thing that does not: exactly one host holds it, and only that host
// launches, reconciles, and meters the mission. Everything the holder writes
// about the run flows to the other host, which treats it as read-only.
type Lease struct {
	// Holder is the host that currently runs the mission. Empty means a mission
	// from before leases existed, which this host holds.
	Holder HostID `json:"holder,omitempty"`
	// Epoch increases by one each time the lease changes hands.
	//
	// It is what resolves two hosts each believing they hold a mission, which
	// happens whenever one takes over while the other is merely asleep: the
	// higher epoch wins, and the primary wins a tie. No clock is compared, so
	// two machines disagreeing about the time cannot change the answer.
	Epoch int `json:"epoch,omitempty"`
}

// HeldBy reports whether host holds the lease.
func (l Lease) HeldBy(host HostID) bool { return l.Holder == "" || l.Holder == host }

// Take returns the lease as held by host, one epoch on.
func (l Lease) Take(host HostID) Lease { return Lease{Holder: host, Epoch: l.Epoch + 1} }

// Peer is the other q installation this one is paired with.
type Peer struct {
	HostInfo

	// LastSyncAt is when this host last completed an exchange with its peer, by
	// this host's own clock.
	//
	// Both roles record it, and each reads it differently. The secondary reads it
	// as "when the primary was last seen", which is what decides a takeover. The
	// primary reads it as "how long since anyone confirmed my leases", which is
	// what stops a laptop waking from sleep from starting work its peer already
	// took.
	LastSyncAt time.Time `json:"lastSyncAt,omitzero"`
	// LastError is why the most recent exchange failed, empty after a success.
	LastError string `json:"lastError,omitempty"`
}

// Seen reports whether the peer completed an exchange within the given window.
func (p *Peer) Seen(now time.Time, within time.Duration) bool {
	return p != nil && !p.LastSyncAt.IsZero() && now.Sub(p.LastSyncAt) <= within
}

// Tombstone records that an entity was deleted.
//
// Deletes physically remove the record, which is right for one machine and
// wrong for two: a peer that still has the mission would hand it straight back
// on the next exchange. The tombstone is what lets a delete be told apart from
// "never heard of it".
type Tombstone struct {
	// Kind is "operation" or "mission".
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// At is when the delete happened, used only to expire the tombstone.
	At time.Time `json:"at"`
}

// Tombstone kinds.
const (
	TombstoneOperation = "operation"
	TombstoneMission   = "mission"
)

// tombstoneTTL is how long a delete is remembered.
//
// It only has to outlast the longest a peer can go without an exchange and
// still be expected back. A laptop that was away for longer than this and
// returns holding a mission the other side deleted will hand it back, which is
// the conservative failure: a card reappears rather than work disappearing.
const tombstoneTTL = 30 * 24 * time.Hour

// Tombstoned reports whether the snapshot remembers deleting the entity.
func (s Snapshot) Tombstoned(kind, id string) bool {
	for _, t := range s.Tombstones {
		if t.Kind == kind && t.ID == id {
			return true
		}
	}

	return false
}

// bury records a delete, replacing any earlier tombstone for the same entity.
func (s *Snapshot) bury(kind, id string, now time.Time) {
	if s.Tombstoned(kind, id) {
		return
	}

	s.Tombstones = append(s.Tombstones, Tombstone{Kind: kind, ID: id, At: now})
}

// expireTombstones drops deletes old enough that no peer can still disagree.
func (s *Snapshot) expireTombstones(now time.Time) {
	kept := s.Tombstones[:0:0]

	for _, t := range s.Tombstones {
		if now.Sub(t.At) < tombstoneTTL {
			kept = append(kept, t)
		}
	}

	s.Tombstones = kept
}

// Holds reports whether this snapshot's host holds the mission's lease.
func (s Snapshot) Holds(ms Mission) bool { return ms.Lease.HeldBy(s.Self.ID) }

// PeerID returns the paired host's id, or empty when unpaired.
func (s Snapshot) PeerID() HostID {
	if s.Peer == nil {
		return ""
	}

	return s.Peer.ID
}

// HostName returns the display name of a host this snapshot knows about, or the
// bare id for one it does not.
func (s Snapshot) HostName(id HostID) string {
	switch {
	case id == "" || id == s.Self.ID:
		return hostLabel(s.Self)
	case s.Peer != nil && id == s.Peer.ID:
		return hostLabel(s.Peer.HostInfo)
	default:
		return string(id)
	}
}

// hostLabel prefers a host's name and falls back to its id.
func hostLabel(h HostInfo) string {
	if h.Name != "" {
		return h.Name
	}

	return string(h.ID)
}
