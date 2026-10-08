package mission

import (
	"errors"
	"time"
)

// ErrClaimRaced reports that a witness's record changed between being read and
// being written. The write did not happen, and the caller reads again.
var ErrClaimRaced = errors.New("the witness's record changed underneath this write")

// Claim is the one record a witness keeps for a pair: which host is answerable
// for the missions either could run.
//
// The two hosts of a pair normally settle that between themselves, in the
// exchange. A claim is for when they cannot speak. Somewhere both can still
// reach holds it, and each consults it before acting on the other's silence, so
// that silence caused by a network neither shares is not mistaken for absence.
type Claim struct {
	// Holder is the host the claim names, empty when nobody has claimed yet.
	Holder HostID `json:"holder,omitempty"`
	// RenewedAt is when the holder last wrote the claim.
	RenewedAt time.Time `json:"renewedAt,omitzero"`
	// TTL is how long after RenewedAt the claim stands. Zero means it stands
	// until someone entitled to replaces it.
	TTL time.Duration `json:"ttl,omitempty"`
	// Version is the witness's own token for this revision of the record. A
	// write carries the version it read and is refused if the record has moved
	// on. Empty means there was no record.
	Version string `json:"-"`
}

// Expired reports whether the claim has lapsed and may be taken by another
// host. An unheld claim is expired; one with no TTL never is.
func (c Claim) Expired(now time.Time) bool {
	if c.Holder == "" {
		return true
	}

	return c.TTL > 0 && now.After(c.RenewedAt.Add(c.TTL))
}
