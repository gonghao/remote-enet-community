package diagnostics

import (
	"bytes"
	"encoding/json"
	"math"
	"net/netip"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/observe"
)

func validInput() Input {
	return Input{Version: "dev", Permission: Permission{Platform: runtime.GOOS}}
}

func decode(t *testing.T, in Input) (report, []byte) {
	t.Helper()
	data, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 64*1024 {
		t.Fatal("report exceeds budget")
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r, data
}

// Fill every exported string, including fields added to upstream snapshots later.
func injectStrings(v reflect.Value, secret string) {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(secret)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				injectStrings(v.Field(i), secret)
			}
		}
	case reflect.Slice:
		if v.CanSet() {
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			injectStrings(v.Index(0), secret)
		}
	case reflect.Pointer:
		if v.CanSet() {
			v.Set(reflect.New(v.Type().Elem()))
			injectStrings(v.Elem(), secret)
		}
	}
}

func TestSecretsAndInputOwnership(t *testing.T) {
	const secret = "SECRET-/identity/invite?VIN=123&ip=192.0.2.123"
	in := validInput()
	injectStrings(reflect.ValueOf(&in).Elem(), secret)
	// Version and permission are validation gates, tested independently below.
	in.Version, in.Permission.Platform = "1.2.3-rc.1", runtime.GOOS
	in.Runtime.Gateway = netip.MustParseAddr("192.0.2.123")
	in.Runtime.Diagnostic = netip.MustParseAddrPort("192.0.2.123:6801")
	in.Runtime.Control, in.Runtime.Discovery = in.Runtime.Diagnostic, in.Runtime.Diagnostic
	in.Runtime.Direct.Address, in.Runtime.Direct.Local = in.Runtime.Gateway, in.Runtime.Gateway
	in.Runtime.NIC.Address = in.Runtime.Gateway
	for i := range in.Runtime.Device {
		in.Runtime.Device[i], in.Runtime.Peer[i] = 0xab, 0xcd
	}
	in.Runtime.Exclusive.Owner.PID, in.Runtime.Exclusive.Owner.Created = 123456789, 987654321
	before := cloneInput(t, in)
	r, data := decode(t, in)
	if !reflect.DeepEqual(in, before) {
		t.Fatal("Build modified input")
	}
	for _, forbidden := range []string{secret, "192.0.2.123", "abababab", "cdcdcdcd", "123456789", "987654321", "SECRET", "Events", "Target", "Adapter"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("report leaked %q", forbidden)
		}
	}
	if r.Role != "unknown" || r.Runtime.Role != "unknown" || r.Runtime.Phase != "unknown" || r.Runtime.Path != "unknown" || r.Runtime.Direct.Phase != "unknown" {
		t.Fatal("unknown enum escaped normalization")
	}
	if !r.Runtime.HasError || !r.Runtime.Direct.HasError || !r.Runtime.Metrics.HasError {
		t.Fatal("missing error presence")
	}
	// No input collection is retained and no output buffer is shared between calls.
	in.Runtime.Metrics.Events[0].Message = "changed"
	in.Runtime.Metrics.LastError.Error = "changed"
	_, again := decode(t, before)
	if !bytes.Equal(data, again) {
		t.Fatal("input collection mutation changed report")
	}
	data[0] = '!'
	_, independent := decode(t, before)
	if !bytes.Equal(again, independent) {
		t.Fatal("output buffers alias")
	}
}

func cloneInput(t *testing.T, in Input) Input {
	t.Helper()
	// JSON is used only by the test to establish a deep ownership baseline.
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Input
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestValidationReturnsNoPartialOutput(t *testing.T) {
	for _, version := range []string{"", "secret", "1.2", "01.2.3", "1.2.3+secret", "1.2.3-secret", "1.2.3\n", "1.2.3/secret", strings.Repeat("1", 65), "1000000000.2.3", "1.2.3-rc.01"} {
		in := validInput()
		in.Version = version
		assertFailure(t, in, "diagnostics: invalid version")
	}
	for _, version := range []string{"dev", "0.0.0", "v1.2.3", "1.2.3-alpha.0", "1.2.3-beta.2", "999999999.999999999.999999999-rc.999999999"} {
		in := validInput()
		in.Version = version
		decode(t, in)
	}
	for _, platform := range []string{"", "SECRET", "linux", "darwin", "windows"} {
		if platform == runtime.GOOS {
			continue
		}
		in := validInput()
		in.Permission.Platform = platform
		assertFailure(t, in, "diagnostics: permission platform mismatch")
	}
	for _, number := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, inbound := range []bool{false, true} {
			in := validInput()
			if inbound {
				in.Runtime.Metrics.Rate.InBytesPerSecond = number
			} else {
				in.Runtime.Metrics.Rate.OutBytesPerSecond = number
			}
			assertFailure(t, in, "diagnostics: non-finite rate")
		}
	}
}

func assertFailure(t *testing.T, in Input, message string) {
	t.Helper()
	data, err := Build(in)
	if data != nil || err == nil || err.Error() != message {
		t.Fatalf("expected nil output and fixed error %q, got %q, %v", message, data, err)
	}
}

func TestProjectionAndPartialObservations(t *testing.T) {
	in := validInput()
	in.Role = "operator"
	in.Running, in.Stopping, in.Testing, in.DirectWorking = true, true, true, true
	in.Permission.Elevated, in.Permission.HelperAvailable, in.Permission.AuthorizationRequired = true, true, true
	in.Runtime.Phase, in.Runtime.Direct.Phase = app.PhaseConnected, app.DirectReady
	in.Runtime.Role, in.Runtime.Path.Kind = "client", "relay"
	in.Runtime.Generation, in.Runtime.RTT, in.Runtime.RTTValid = 8, 0, true
	in.Runtime.MetricsIncomplete = true
	m := &in.Runtime.Metrics
	m.ActiveConnections, m.Session.Active, m.Session.Generation = 3, true, 8
	m.Totals, m.Session.Bytes = observe.Bytes{In: 123, Out: 456}, observe.Bytes{In: 12, Out: 34}
	m.Rate = observe.Rate{InBytesPerSecond: 1.25, OutBytesPerSecond: 2.5, Window: time.Second}
	m.Session.RTT = observe.RTT{Samples: 2, TotalSamples: 5, Latest: 3 * time.Millisecond, P50: time.Millisecond, P95: 3 * time.Millisecond}
	r, _ := decode(t, in)
	if r.SchemaVersion != 1 || r.System.GOOS != runtime.GOOS || r.System.GOARCH != runtime.GOARCH || r.Role != "operator" || r.Runtime.Role != "client" || r.Runtime.Phase != "connected" || r.Runtime.Path != "relay" || r.Runtime.Direct.Phase != "ready" {
		t.Fatal("incorrect metadata")
	}
	if !r.Permission.Elevated || !r.Permission.HelperAvailable || !r.Permission.AuthorizationRequired || r.Permission.Platform != runtime.GOOS || !r.Lifecycle.Running || !r.Lifecycle.Stopping || !r.Lifecycle.Testing || !r.Lifecycle.DirectWorking {
		t.Fatal("lost observed booleans")
	}
	got := r.Runtime.Metrics
	if !got.Incomplete || got.ActiveConnections != 3 || !got.SessionActive || got.SessionGeneration != 8 || got.TotalIn != 123 || got.TotalOut != 456 || got.SessionIn != 12 || got.SessionOut != 34 || got.InBytesPerSecond != 1.25 || got.OutBytesPerSecond != 2.5 || got.RateWindowNanoseconds != int64(time.Second) || got.RTTSamples != 2 || got.RTTTotalSamples != 5 || got.RTTLatestNanoseconds != int64(3*time.Millisecond) || got.RTTP50Nanoseconds != int64(time.Millisecond) || got.RTTP95Nanoseconds != int64(3*time.Millisecond) {
		t.Fatalf("incorrect metrics: %+v", got)
	}
	if !r.Runtime.RTTValid || r.Runtime.RTTNanoseconds != 0 || r.Runtime.Generation != 8 {
		t.Fatal("zero measured RTT lost")
	}
	in.Runtime.RTTValid, in.Runtime.RTT = false, time.Hour
	m.Session.RTT.Samples = 0
	r, _ = decode(t, in)
	if r.Runtime.RTTNanoseconds != 0 || r.Runtime.Metrics.RTTLatestNanoseconds != 0 || r.Runtime.Metrics.RTTP50Nanoseconds != 0 || r.Runtime.Metrics.RTTP95Nanoseconds != 0 {
		t.Fatal("unmeasured RTT reported")
	}
	empty, _ := decode(t, validInput())
	if empty.Permission.Elevated || empty.Permission.HelperAvailable || empty.Permission.AuthorizationRequired || empty.Runtime.Metrics.SessionActive || empty.Runtime.RTTValid {
		t.Fatal("inferred observation from empty input")
	}
}

func TestConcurrentIndependentBuilds(t *testing.T) {
	in := validInput()
	_, want := decode(t, in)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			data, err := Build(in)
			if err != nil || !bytes.Equal(data, want) {
				t.Error("concurrent build differs")
			}
			if len(data) != 0 {
				data[0] = '!'
			}
		})
	}
	wg.Wait()
}

func TestExactFieldAllowlist(t *testing.T) {
	_, data := decode(t, validInput())
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"":                  "schemaVersion version system role runtime lifecycle permission",
		"system":            "goos goarch",
		"permission":        "platform elevated helperAvailable authorizationRequired",
		"lifecycle":         "running stopping testing directWorking",
		"runtime":           "role phase generation path hasError rttNanoseconds rttValid metrics exclusive direct",
		"runtime.direct":    "phase hasError",
		"runtime.exclusive": "enabled active",
		"runtime.metrics":   "incomplete activeConnections hasError totalIn totalOut sessionGeneration sessionActive sessionIn sessionOut inBytesPerSecond outBytesPerSecond rateWindowNanoseconds rttSamples rttTotalSamples rttLatestNanoseconds rttP50Nanoseconds rttP95Nanoseconds",
	}
	var visit func(string, map[string]any)
	visit = func(path string, object map[string]any) {
		allowed, ok := expected[path]
		if !ok {
			t.Fatalf("unexpected object %q", path)
		}
		keys := strings.Fields(allowed)
		if len(keys) != len(object) {
			t.Fatalf("unexpected field count at %q: %v", path, object)
		}
		for _, key := range keys {
			value, ok := object[key]
			if !ok {
				t.Fatalf("missing field %q at %q", key, path)
			}
			if child, ok := value.(map[string]any); ok {
				next := key
				if path != "" {
					next = path + "." + key
				}
				visit(next, child)
			}
		}
	}
	visit("", root)
}

func TestFixedPreviewVersions(t *testing.T) {
	for _, version := range []string{
		"0.1.0-preview.wave9.2-worktree", "0.1.0-preview.wave10.0",
		"0.1.0-preview.wave0", "0.1.0-preview.wave999.999-worktree",
		"0.1.0-preview.abcdef0-worktree", "0.1.0-preview.0123456789abcdef0123456789abcdef01234567",
		"0.1.0-preview." + strings.Repeat("a", 40) + "-worktree",
	} {
		in := validInput()
		in.Version = version
		r, _ := decode(t, in)
		if r.Version != version {
			t.Fatal("preview version changed")
		}
	}
	for _, version := range []string{
		"0.1.0-previewXwave10X0", "0.1.0-preview.wave10X0", "0.1.0-preview.wave",
		"0.1.0-preview.wave1000", "0.1.0-preview.wave1.1000", "0.1.0-preview.wave1.2.3",
		"0.1.0-preview.abcdef", "0.1.0-preview." + strings.Repeat("a", 41),
		"0.1.0-preview.ABCDEF0", "0.1.0-preview.abcdefg", "0.1.0-preview.wave10.0-SECRET",
		"0.1.0-preview.wave10.0-worktree-SECRET", "0.1.0-preview.abcdef0-worktree-SECRET",
		"0.1.0-preview.abcdef0+SECRET", "0.1.0-preview.wave10.0/SECRET", "0.1.0-preview.wave10.0\nSECRET",
		"0.1.0-preview.secret", "0.1.0-preview.192.0.2.1", "0.1.0-preview.wave10.0-worktree\n",
		"999999999.999999999.999999999-preview." + strings.Repeat("a", 40),
	} {
		in := validInput()
		in.Version = version
		assertFailure(t, in, "diagnostics: invalid version")
	}
}
