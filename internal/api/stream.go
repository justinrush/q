package api

// Event names on the daemon's server-sent event stream.

const (
	// EventSnapshot carries the full state and is sent once on connect.
	EventSnapshot = "snapshot"
	// EventMission carries a single changed mission.
	EventMission = "mission"
	// EventOperation carries a single changed operation.
	EventOperation = "operation"
	// EventDeleted reports that an entity is gone.
	EventDeleted = "deleted"
	// EventJob reports progress of a long-running action such as a launch.
	EventJob = "job"
	// EventPing is a heartbeat, so a client can notice a socket that has died
	// without the far end telling it.
	EventPing = "ping"
)
