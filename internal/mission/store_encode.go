package mission

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// missionCodec encodes the state files, reusing each mission's encoding from
// the last time it was written for as long as the mission is unchanged.
//
// The store encodes the whole board on every mutation, to see whether it
// changed and to write it if so, while a mutation nearly always changes one
// mission. Encoding was most of a mutation's cost on a large board. Comparing a
// mission with the copy it was encoded from is much cheaper than encoding it
// again, because copies share their large maps (see [Usage] and [CodexUsage])
// and reflect.DeepEqual stops at a shared map.
//
// The result is byte for byte what json.Marshal produces. A codec is used only
// under the store's lock.
type missionCodec struct {
	cache map[MissionID]cachedMission
	// gen counts encodings of the whole state, so missions that have gone are
	// dropped from the cache.
	gen int
	// files is the last encoding of each file, so a file none of whose
	// missions changed (the closed file, nearly always) is not even copied.
	files map[string]cachedFile
}

// cachedFile is one file's last encoding.
type cachedFile struct {
	ids  []MissionID
	list []byte
	base []byte
	out  []byte
}

type cachedMission struct {
	mission Mission
	data    []byte
	gen     int
}

// encode renders a snapshot as its two files.
func (c *missionCodec) encode(snap Snapshot) (encoded, error) {
	hot, closed := split(snap)

	return c.encodeParts(hot, closed)
}

func (c *missionCodec) encodeParts(hot Snapshot, closed closedState) (encoded, error) {
	c.gen++

	hotData, err := c.marshalState(hot)
	if err != nil {
		return encoded{}, err
	}

	closedData, err := c.marshalClosed(closed)
	if err != nil {
		return encoded{}, fmt.Errorf("encoding closed missions: %w", err)
	}

	for id, entry := range c.cache {
		if entry.gen < c.gen {
			delete(c.cache, id)
		}
	}

	return encoded{hot: hotData, closed: closedData}, nil
}

// marshalState encodes the main state file. It is compact: indentation added a
// third to every write for a file people rarely read.
func (c *missionCodec) marshalState(snap Snapshot) ([]byte, error) {
	missions := snap.Missions
	snap.Missions = nil

	data, err := c.splice("state", snap, missions)
	if err != nil {
		return nil, fmt.Errorf("encoding state: %w", err)
	}

	return data, nil
}

func (c *missionCodec) marshalClosed(closed closedState) ([]byte, error) {
	missions := closed.Missions
	closed.Missions = nil

	return c.splice("closed", closed, missions)
}

// missionsKey is how the encoding of a value whose missions were left out
// says so. A string value cannot produce it: encoding escapes its quotes.
var missionsKey = []byte(`"missions":null`)

// splice encodes base, whose missions field is nil, with missions in its
// place, followed by a newline. file names the file, whose last encoding is
// reused when neither base nor any of its missions changed.
func (c *missionCodec) splice(file string, base any, missions []Mission) ([]byte, error) {
	data, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}

	if missions == nil {
		return append(data, '\n'), nil
	}

	at := bytes.Index(data, missionsKey)
	if at < 0 || bytes.Count(data, missionsKey) != 1 {
		return nil, fmt.Errorf("found %d missions fields", bytes.Count(data, missionsKey))
	}

	if c.files == nil {
		c.files = map[string]cachedFile{}
	}

	last := c.files[file]

	list, reused, err := c.list(last, missions)
	if err != nil {
		return nil, err
	}

	if reused && bytes.Equal(data, last.base) {
		return last.out, nil
	}

	value := at + len(`"missions":`)
	out := make([]byte, 0, len(data)+len(list)+1)
	out = append(out, data[:value]...)
	out = append(out, list...)
	out = append(out, data[value+len("null"):]...)
	out = append(out, '\n')

	ids := make([]MissionID, len(missions))
	for i, m := range missions {
		ids[i] = m.ID
	}

	c.files[file] = cachedFile{ids: ids, list: list, base: data, out: out}

	return out, nil
}

// list encodes missions as a JSON array, reporting whether it is last's.
func (c *missionCodec) list(last cachedFile, missions []Mission) ([]byte, bool, error) {
	if c.cache == nil {
		c.cache = map[MissionID]cachedMission{}
	}

	encodings := make([][]byte, len(missions))
	size := 2
	same := last.list != nil && len(missions) == len(last.ids)

	for i, m := range missions {
		entry, ok := c.cache[m.ID]
		if !ok || !reflect.DeepEqual(entry.mission, m) {
			data, err := json.Marshal(m)
			if err != nil {
				return nil, false, err
			}

			entry = cachedMission{mission: m, data: data}
			same = false
		}

		entry.gen = c.gen
		c.cache[m.ID] = entry
		encodings[i] = entry.data
		size += len(entry.data) + 1
		same = same && last.ids[i] == m.ID
	}

	if same {
		return last.list, true, nil
	}

	out := make([]byte, 0, size)
	out = append(out, '[')

	for i, data := range encodings {
		if i > 0 {
			out = append(out, ',')
		}

		out = append(out, data...)
	}

	return append(out, ']'), false, nil
}
