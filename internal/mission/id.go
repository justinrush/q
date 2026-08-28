package mission

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// ID prefixes, so a bare identifier in a log line is self-describing.
const (
	operationIDPrefix = "op_"
	missionIDPrefix   = "ms_"
)

// idBytes is the number of random bytes behind an ID. Twelve hex characters is
// far more than a personal tool needs and keeps IDs readable in tmux session
// names.
const idBytes = 6

// OperationID identifies an operation.
type OperationID string

// MissionID identifies a mission.
type MissionID string

// NewOperationID returns a fresh operation identifier.
func NewOperationID() (OperationID, error) {
	s, err := randomHex(idBytes)
	if err != nil {
		return "", err
	}

	return OperationID(operationIDPrefix + s), nil
}

// NewMissionID returns a fresh mission identifier.
func NewMissionID() (MissionID, error) {
	s, err := randomHex(idBytes)
	if err != nil {
		return "", err
	}

	return MissionID(missionIDPrefix + s), nil
}

// Valid reports whether the identifier is well formed.
func (id OperationID) Valid() bool { return validID(string(id), operationIDPrefix) }

// Valid reports whether the identifier is well formed.
func (id MissionID) Valid() bool { return validID(string(id), missionIDPrefix) }

// String implements fmt.Stringer.
func (id OperationID) String() string { return string(id) }

// String implements fmt.Stringer.
func (id MissionID) String() string { return string(id) }

// Short returns the identifier without its prefix, for use in tmux session
// names where every character counts.
func (id MissionID) Short() string { return strings.TrimPrefix(string(id), missionIDPrefix) }

// validID checks the prefix and that the remainder is lowercase hex of the
// expected length.
//
// Uppercase is rejected even though it decodes as hex: IDs are always generated
// lowercase, so accepting "op_AABB…" would report an identifier as valid that
// could never match a stored one.
func validID(s, prefix string) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok || len(rest) != idBytes*2 {
		return false
	}

	if rest != strings.ToLower(rest) {
		return false
	}

	_, err := hex.DecodeString(rest)

	return err == nil
}

// randomHex returns n cryptographically random bytes as lowercase hex.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// NewSessionUUID returns a random RFC 4122 version 4 UUID.
//
// This exists so q can hand claude a --session-id it chose, making the
// session resumable from the instant it launches rather than only after a hook
// reports back. It is hand-rolled to avoid a dependency for fifteen lines.
func NewSessionUUID() (string, error) {
	var buf [16]byte

	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}

	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // RFC 4122 variant

	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}
