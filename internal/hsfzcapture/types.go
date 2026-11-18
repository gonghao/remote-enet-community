// Package hsfzcapture records bounded application byte streams and HSFZ indexes.
package hsfzcapture

import "time"

// Direction is relative to the Operator observation boundary.
type Direction string

const (
	ToVehicle    Direction = "to_vehicle"
	FromVehicle  Direction = "from_vehicle"
	ToOperator   Direction = "to_operator"
	FromOperator Direction = "from_operator"
)

// Metadata identifies one stream without retaining its socket or session.
type Metadata struct {
	Generation uint64
	Network    string
	Channel    string
	Local      string
	Peer       string
	Target     string
}

// Options are clamped to hard maxima; zero selects the default.
type Options struct {
	MaxFileBytes   int64
	QueueBytes     int
	MaxConnections int
	RecentFrames   int
}

// Snapshot owns its recent slices and never exposes mutable recorder storage.
type Snapshot struct {
	Active            bool
	Stopping          bool
	Incomplete        bool
	Path              string
	StartedAt         time.Time
	StoppedAt         time.Time
	BytesWritten      uint64
	ObservedBytes     uint64
	RecordedBytes     uint64
	Frames            uint64
	Markers           uint64
	DroppedEvents     uint64
	ActiveConnections int
	Error             string
	Recent            []Frame
	RecentMarkers     []Marker
}

// Marker is a user annotation at an observed time, independent of stream frames.
type Marker struct {
	Sequence   uint64
	Generation uint64
	At         time.Time
	Label      string
}

// Frame indexes a raw stream; PrefixHex contains at most 256 original bytes.
type Frame struct {
	Sequence        uint64
	Connection      uint64
	Generation      uint64
	Offset          uint64
	At              time.Time
	Network         string
	Channel         string
	Direction       Direction
	Control         uint16
	BodyLength      uint32
	HasAddresses    bool
	Source          byte
	Destination     byte
	PrefixHex       string
	PrefixTruncated bool
	Status          string
}
