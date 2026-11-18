package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

// This runs real loopback TLS and sessions with memory packet devices. It
// deliberately makes no claim about native TUN, OS sleep or Internet failures.
func TestManualRecoverySoak(t *testing.T) {
	if os.Getenv("RENET_MANUAL_RECOVERY_SOAK") != "1" {
		t.Skip("set RENET_MANUAL_RECOVERY_SOAK=1 and RENET_RECOVERY_SOAK_DURATION=1m..8h for the loopback recovery soak")
	}
	duration, err := time.ParseDuration(os.Getenv("RENET_RECOVERY_SOAK_DURATION"))
	if err != nil || duration < time.Minute || duration > 8*time.Hour {
		t.Fatal("RENET_RECOVERY_SOAK_DURATION must be between 1m and 8h")
	}
	start := time.Now()
	result := struct {
		Schema        string            `json:"schema"`
		GOOS          string            `json:"goos"`
		GOARCH        string            `json:"goarch"`
		RequestedMS   int64             `json:"requestedMs"`
		ElapsedMS     int64             `json:"elapsedMs"`
		Cycles        int               `json:"cycles"`
		Modes         map[string]int    `json:"modes"`
		Paths         map[string]int    `json:"paths"`
		PayloadHashes map[string]string `json:"payloadHashes"`
		BytesEachWay  int64             `json:"bytesEachWay"`
		WorkersJoined bool              `json:"workersJoined"`
		NoReplay      bool              `json:"noReplay"`
	}{Schema: "recovery-soak/v1", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		RequestedMS: duration.Milliseconds(), Modes: map[string]int{}, Paths: map[string]int{}, PayloadHashes: map[string]string{}}
	for time.Since(start) < duration {
		mode := []string{"cancel", "transport", "device"}[result.Cycles%3]
		size := []int{1 << 20, 8 << 20}[result.Cycles%2]
		var n int64
		var hash, path string
		// Run includes all fixture/Hub/transport cleanup callbacks. Failed or
		// interrupted cycles never produce a successful summary tag.
		if !t.Run(fmt.Sprintf("cycle-%04d-%s-%dMiB", result.Cycles+1, mode, size>>20), func(t *testing.T) {
			n, hash, path = runL3Recovery(t, mode, size)
		}) {
			return
		}
		result.Cycles++
		result.Modes[mode]++
		result.Paths[path]++
		result.PayloadHashes[fmt.Sprint(n)] = hash
		result.BytesEachWay += n
		if t.Context().Err() != nil {
			t.Fatal("recovery soak interrupted")
		}
	}
	result.ElapsedMS = time.Since(start).Milliseconds()
	result.WorkersJoined, result.NoReplay = true, true
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RECOVERY_SOAK_RESULT %s", data)
}
