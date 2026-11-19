package hub

import "time"

// SetupLimits bounds pending outer handshakes and routing exchanges on an
// embedded listener. Nonpositive fields use 16 slots, 8/s, burst 16 and 10s.
type SetupLimits struct {
	Concurrent int
	PerSecond  int
	Burst      int
	Timeout    time.Duration
}

type setupAdmission struct {
	SetupLimits
	slots  chan struct{}
	tokens float64
	last   time.Time
}

func newSetupAdmission(limits SetupLimits) *setupAdmission {
	if limits.Concurrent <= 0 {
		limits.Concurrent = 16
	}
	if limits.PerSecond <= 0 {
		limits.PerSecond = 8
	}
	if limits.Burst <= 0 {
		limits.Burst = 16
	}
	if limits.Timeout <= 0 {
		limits.Timeout = setupTimeout
	}
	return &setupAdmission{SetupLimits: limits, slots: make(chan struct{}, limits.Concurrent), tokens: float64(limits.Burst)}
}

// Only the accept loop charges tokens. Workers release slots independently.
func (a *setupAdmission) acquire(now time.Time) bool {
	a.tokens = min(float64(a.Burst), a.tokens+now.Sub(a.last).Seconds()*float64(a.PerSecond))
	a.last = now
	if a.tokens < 1 {
		return false
	}
	select {
	case a.slots <- struct{}{}:
		a.tokens--
		return true
	default:
		return false
	}
}

func (a *setupAdmission) release() { <-a.slots }
