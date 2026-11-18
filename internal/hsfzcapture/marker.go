package hsfzcapture

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Marker admission errors never contain annotation text.
var (
	ErrNotRecording = errors.New("capture_inactive")
	ErrMarkerLabel  = errors.New("capture_marker_label")
	ErrMarkerFull   = errors.New("capture_marker_full")
)

// Mark queues an annotation synchronously with stream observations, without
// file I/O or a task queue. Success confirms admission; Snapshot counts only
// markers written successfully. An empty label receives an automatic number.
func (r *Recorder) Mark(label string, generation uint64) (Marker, error) {
	if len(label) > 256 || !utf8.ValidString(label) || strings.ContainsFunc(label, unicode.IsControl) {
		return Marker{}, ErrMarkerLabel
	}
	label = strings.TrimSpace(label)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.run
	if s == nil || s.stopping {
		return Marker{}, ErrNotRecording
	}
	if s.queued+eventCost > r.opt.QueueBytes {
		r.drop(s)
		return Marker{}, ErrMarkerFull
	}
	s.nextMarker++
	if label == "" {
		label = fmt.Sprintf("Marker %d", s.nextMarker)
	}
	m := Marker{Sequence: s.nextMarker, Generation: generation, At: time.Now(), Label: label}
	s.queue = append(s.queue, event{Type: "marker", At: m.At, Marker: &m})
	s.queued += eventCost
	r.signal(s)
	return m, nil
}
