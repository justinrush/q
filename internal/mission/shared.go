package mission

import (
	"reflect"
	"strings"
	"time"
)

// What each persisted field means to a paired host.
//
// Two q installations share missions, and most of what a mission records is
// true on only one of them: a worktree path, a tmux pane, the directory an
// agent's transcript sits in. Copying those across is not merely useless, it is
// actively wrong, because the receiving host would act on them.
//
// So every field of [Operation], [Mission], [Repo], and [RepoWork] declares, in
// a `q` struct tag, which of four things it is. The tag is the single place
// that decision is made, and everything that moves state between hosts —
// projecting it for the wire, comparing it, merging it — is written in terms of
// the classes rather than of field names. A field added without a tag fails a
// test, which is the point: the alternative is finding out it leaked a path
// when a mission on the other machine tries to use it.
type fieldClass string

const (
	// classKey is identity. It is the same everywhere and never merged.
	classKey fieldClass = "key"
	// classSpec is what a human wrote: the brief, the repositories, the model.
	// Either host may change it, and the higher revision wins.
	classSpec fieldClass = "spec"
	// classRun is what the running agent produced: the lane, the last message,
	// the cost. Only the lease holder writes it.
	classRun fieldClass = "run"
	// classLocal is true on one host only and is never sent.
	classLocal fieldClass = "local"
	// classMeta is the bookkeeping that decides the other three: the lease and
	// the revision counters. The merge handles each of them by name.
	classMeta fieldClass = "meta"
)

// classTag is the struct tag a field's class is declared in.
const classTag = "q"

// timeType and its pointer are compared by instant rather than by
// representation. A time that has been through JSON has lost its monotonic
// reading and may have gained a different location, so reflect.DeepEqual calls
// two readings of the same moment different.
var (
	timeType    = reflect.TypeFor[time.Time]()
	timePtrType = reflect.TypeFor[*time.Time]()
)

// classOf returns the class a struct field declares, or empty for none.
func classOf(f reflect.StructField) fieldClass {
	return fieldClass(f.Tag.Get(classTag))
}

// unclassified names every field of the shared types that declares no class,
// or one q does not know.
func unclassified() []string {
	known := map[fieldClass]bool{
		classKey: true, classSpec: true, classRun: true, classLocal: true, classMeta: true,
	}

	var missing []string

	for _, typ := range []reflect.Type{
		reflect.TypeFor[Operation](), reflect.TypeFor[Mission](),
		reflect.TypeFor[Repo](), reflect.TypeFor[RepoWork](),
	} {
		for i := range typ.NumField() {
			if field := typ.Field(i); !known[classOf(field)] {
				missing = append(missing, typ.Name()+"."+field.Name)
			}
		}
	}

	return missing
}

// The check runs at start-up as well as under test. A field with no class is
// left out of the projection sent to a peer and out of the merge alike, so the
// failure is silent and lands on another machine; refusing to start is the
// version of that failure someone will actually see.
func init() {
	if missing := unclassified(); len(missing) > 0 {
		panic("mission: fields with no sync class: " + strings.Join(missing, ", "))
	}
}

// copyClass copies every field of the given class from src into dst.
//
// The copy is shallow. Callers that hand the result outside the store clone it
// first, which is the same rule every other read follows.
func copyClass[T any](dst *T, src T, class fieldClass) {
	to := reflect.ValueOf(dst).Elem()
	from := reflect.ValueOf(src)

	for i := range to.NumField() {
		if classOf(to.Type().Field(i)) == class {
			to.Field(i).Set(from.Field(i))
		}
	}
}

// zeroClass clears every field of the given class.
func zeroClass[T any](dst *T, class fieldClass) {
	to := reflect.ValueOf(dst).Elem()

	for i := range to.NumField() {
		if classOf(to.Type().Field(i)) == class {
			to.Field(i).SetZero()
		}
	}
}

// equalClass reports whether a and b agree on every field of the given class.
//
// UpdatedAt is skipped. It records that something changed rather than being a
// thing that changed, so counting it would make every edit to a brief look like
// an edit to the run as well.
func equalClass[T any](a, b T, class fieldClass) bool {
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)

	for i := range av.NumField() {
		field := av.Type().Field(i)
		if classOf(field) != class || field.Name == "UpdatedAt" {
			continue
		}

		if !fieldEqual(av.Field(i), bv.Field(i)) {
			return false
		}
	}

	return true
}

// fieldEqual compares two field values, treating times as instants and an empty
// collection as equal to an absent one.
func fieldEqual(a, b reflect.Value) bool {
	switch a.Type() {
	case timeType:
		at, _ := a.Interface().(time.Time)
		bt, _ := b.Interface().(time.Time)

		return at.Equal(bt)
	case timePtrType:
		at, _ := a.Interface().(*time.Time)
		bt, _ := b.Interface().(*time.Time)

		if at == nil || bt == nil {
			return at == bt
		}

		return at.Equal(*bt)
	}

	// JSON's omitempty turns an empty slice or map into an absent one, so the two
	// must compare equal or a round trip would read as an edit.
	switch a.Kind() {
	case reflect.Slice, reflect.Map:
		if a.Len() == 0 && b.Len() == 0 {
			return true
		}
	default:
	}

	return reflect.DeepEqual(a.Interface(), b.Interface())
}

// Shared returns the mission as its peer should see it: everything true on only
// this host is cleared, in the mission and in the repositories and worktrees
// nested inside it.
func (t Mission) Shared() Mission {
	out := cloneMission(t)
	zeroClass(&out, classLocal)

	out.ExtraRepos = sharedRepos(out.ExtraRepos)
	out.LaunchRepos = sharedRepos(out.LaunchRepos)
	out.Work = sharedWork(out.Work)

	return out
}

// Shared returns the operation as its peer should see it.
func (t Operation) Shared() Operation {
	t.Repos = sharedRepos(t.Repos)

	return t
}

// sharedRepos clears the host-local parts of each repository.
func sharedRepos(repos []Repo) []Repo {
	if len(repos) == 0 {
		return nil
	}

	out := make([]Repo, len(repos))

	for i, r := range repos {
		zeroClass(&r, classLocal)
		out[i] = r
	}

	return out
}

// sharedWork clears the host-local parts of each worktree record.
func sharedWork(work map[string]RepoWork) map[string]RepoWork {
	if len(work) == 0 {
		return nil
	}

	out := make(map[string]RepoWork, len(work))

	for name, w := range work {
		zeroClass(&w, classLocal)
		out[name] = w
	}

	return out
}

// specEqual reports whether two missions carry the same brief.
//
// Both are projected first, so two hosts holding the same repositories at
// different paths agree.
func specEqual(a, b Mission) bool {
	return equalClass(a.Shared(), b.Shared(), classSpec)
}

// runEqual reports whether two missions record the same run.
func runEqual(a, b Mission) bool {
	return equalClass(a.Shared(), b.Shared(), classRun)
}

// operationEqual reports whether two operations say the same thing.
func operationEqual(a, b Operation) bool {
	return equalClass(a.Shared(), b.Shared(), classSpec)
}

// adoptRepos returns the incoming repository list with this host's own paths
// kept for every repository it already knew by name.
//
// The incoming list decides which repositories there are; the local one only
// supplies where they live here. A repository this host has never seen comes
// through with no path, which the daemon then resolves from its URL.
func adoptRepos(local, incoming []Repo) []Repo {
	if len(incoming) == 0 {
		return nil
	}

	known := make(map[string]Repo, len(local))
	for _, r := range local {
		known[r.Name] = r
	}

	out := make([]Repo, len(incoming))

	for i, in := range incoming {
		merged := in
		zeroClass(&merged, classLocal)

		if mine, ok := known[in.Name]; ok && (mine.URL == "" || in.URL == "" || mine.URL == in.URL) {
			copyClass(&merged, mine, classLocal)
		}

		out[i] = merged
	}

	return out
}

// adoptWork returns the holder's worktree records with this host's own
// worktrees kept.
//
// A worktree this host still has on disk is kept even when the holder no longer
// lists it. That happens when the holder closes a mission: its record of the
// work is cleared, but this host's mirror is still there and the record is the
// only thing that says so. Dropping it would orphan the worktree.
func adoptWork(local, incoming map[string]RepoWork) map[string]RepoWork {
	out := make(map[string]RepoWork, len(incoming)+len(local))

	for name, in := range incoming {
		merged := in
		zeroClass(&merged, classLocal)

		if mine, ok := local[name]; ok {
			copyClass(&merged, mine, classLocal)
		}

		merged.RepoName = name
		out[name] = merged
	}

	for name, mine := range local {
		if _, listed := incoming[name]; !listed && mine.Created {
			out[name] = mine
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// adoptSpec replaces a mission's brief with the incoming one, keeping this
// host's paths for the repositories it names.
func adoptSpec(local *Mission, incoming Mission, known []Repo) {
	extra := adoptRepos(known, incoming.ExtraRepos)

	copyClass(local, cloneMission(incoming), classSpec)
	local.ExtraRepos = extra
	local.SpecRev = incoming.SpecRev
}

// adoptRun replaces a mission's run state with the incoming one, keeping this
// host's own paths and worktrees.
func adoptRun(local *Mission, incoming Mission, known []Repo) {
	launch := adoptRepos(known, incoming.LaunchRepos)
	work := adoptWork(local.Work, incoming.Work)

	copyClass(local, cloneMission(incoming), classRun)
	local.LaunchRepos = launch
	local.Work = work
}
