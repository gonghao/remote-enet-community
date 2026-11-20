package centerstate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/settings"
)

func pinFixture(center string, id byte) Pin {
	return Pin{
		CenterID: center,
		Session:  [16]byte{id},
		Role:     settings.RoleOperator,
		Peer:     identity.Fingerprint{1},
		Node:     NodeRef{ID: [16]byte{2}, Addr: "::1", Port: 9443, HubFP: identity.Fingerprint{3}},
		PairedAt: time.Date(2026, 11, 20, 3, 4, 5, 678, time.UTC),
	}
}

func loginFixture() Login {
	now := pinFixture("alpha", 1).PairedAt
	return Login{
		DeviceFP:     identity.Fingerprint{4},
		DeviceLabel:  "",
		AccountID:    "BQAAAAAAAAAAAAAAAAAAAA",
		AccountLabel: "测试账号",
		ExpiresAt:    now.Add(time.Hour),
		LoggedInAt:   now,
	}
}

func openStore(t *testing.T, roleDir, center string) *Store {
	t.Helper()
	s, err := Open(roleDir, center)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func savePin(t *testing.T, s *Store, p Pin) {
	t.Helper()
	if err := s.SavePin(t.Context(), p); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsInvalidID(t *testing.T) {
	roleDir := filepath.Join(t.TempDir(), settings.RoleOperator)
	invalid := []string{"", ".", "..", "../other", `..\other`, "/tmp", `C:\tmp`, "A", "é", "a_b", "%2e%2e", "alpha\n", "a\x00b", strings.Repeat("a", 33)}
	if runtime.GOOS == "windows" {
		invalid = append(invalid, "con", "nul.json", "...", "alpha.")
	}
	for _, id := range invalid {
		if _, err := Open(roleDir, id); err == nil {
			t.Errorf("Open(%q) succeeded", id)
		}
	}
	if _, err := os.Stat(roleDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid Open created the role directory: %v", err)
	}
	openStore(t, roleDir, strings.Repeat("a", 32))
	openStore(t, roleDir, "team-2.example")
}

func TestStateRejectsSymlinks(t *testing.T) {
	roleDir := t.TempDir()
	outside := t.TempDir()
	centers := filepath.Join(roleDir, "centers")
	if err := os.Symlink(outside, centers); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("cannot create symlinks: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := Open(roleDir, "alpha"); err == nil {
		t.Fatal("Open followed the centers symlink")
	}
	if err := os.Remove(centers); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, roleDir, "alpha")
	if err := os.Symlink("alpha", filepath.Join(centers, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(roleDir, "alias"); err == nil {
		t.Fatal("Open followed a symlink to another center")
	}

	trustPath := filepath.Join(roleDir, "trust.json")
	if err := os.WriteFile(trustPath, []byte("unrelated trust state"), 0o600); err != nil {
		t.Fatal(err)
	}
	loginPath := filepath.Join(centers, "alpha", "login.json")
	if err := os.Symlink(trustPath, loginPath); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Login(); found || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Login with symlink = found %v, error %v", found, err)
	}
	if err := s.ClearLogin(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(trustPath); string(got) != "unrelated trust state" || err != nil {
		t.Fatalf("trust file changed: %q, %v", got, err)
	}
}

func TestRecordsAndIsolation(t *testing.T) {
	state := t.TempDir()
	roleDir := filepath.Join(state, settings.RoleOperator)
	alpha := openStore(t, roleDir, "alpha")
	beta := openStore(t, roleDir, "beta")
	client := openStore(t, filepath.Join(state, settings.RoleClient), "alpha")
	p := pinFixture("alpha", 1)
	l := loginFixture()
	savePin(t, alpha, p)
	if err := alpha.SaveLogin(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	for _, other := range []*Store{beta, client} {
		if _, err := other.Pin(p.Session); !errors.Is(err, ErrNoPin) {
			t.Fatalf("other store Pin error = %v", err)
		}
		if _, found, err := other.Login(); found || err != nil {
			t.Fatalf("other store Login = found %v, error %v", found, err)
		}
	}

	other := p
	other.CenterID = "beta"
	other.Peer[0]++
	savePin(t, beta, other)
	alpha = openStore(t, roleDir, "alpha")
	if got, err := alpha.Pin(p.Session); got != p || err != nil {
		t.Fatalf("reopened Pin = %+v, %v", got, err)
	}
	if got, found, err := alpha.Login(); got != l || !found || err != nil {
		t.Fatalf("reopened Login = %+v, %v, %v", got, found, err)
	}

	centerDir := filepath.Join(roleDir, "centers", "alpha")
	pinsDir := filepath.Join(centerDir, "pins")
	pinPath := filepath.Join(pinsDir, "AQAAAAAAAAAAAAAAAAAAAA.json")
	loginPath := filepath.Join(centerDir, "login.json")
	pinJSON := fmt.Sprintf(`{"version":1,"center_id":"alpha","session_id":"AQAAAAAAAAAAAAAAAAAAAA","role":"operator","peer_fp":%q,"node":{"node_id":"AgAAAAAAAAAAAAAAAAAAAA","addr":"::1","port":9443,"hub_fp":%q},"paired_at":"2026-11-20T03:04:05.000000678Z"}`, p.Peer, p.Node.HubFP)
	loginJSON := fmt.Sprintf(`{"version":1,"device_fp":%q,"device_label":"","account_id":"BQAAAAAAAAAAAAAAAAAAAA","account_label":"测试账号","expires_at":"2026-11-20T04:04:05.000000678Z","logged_in_at":"2026-11-20T03:04:05.000000678Z"}`, l.DeviceFP)
	for path, want := range map[string]string{pinPath: pinJSON, loginPath: loginJSON} {
		if got, err := os.ReadFile(path); string(got) != want+"\n" || err != nil {
			t.Fatalf("read %s = %s, %v", path, got, err)
		}
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{roleDir, filepath.Dir(centerDir), centerDir, pinsDir} {
			assertMode(t, path, 0o700)
		}
		for _, path := range []string{pinPath, loginPath, filepath.Join(centerDir, ".lock")} {
			assertMode(t, path, 0o600)
		}
	}

	for range 2 {
		if err := alpha.DeletePin(t.Context(), p.Session); err != nil {
			t.Fatal(err)
		}
		if err := alpha.ClearLogin(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := alpha.Pin(p.Session); !errors.Is(err, ErrNoPin) {
		t.Fatalf("deleted pin error = %v", err)
	}
	if _, found, err := alpha.Login(); found || err != nil {
		t.Fatalf("cleared login = found %v, error %v", found, err)
	}
	if got, err := beta.Pin(p.Session); got != other || err != nil {
		t.Fatalf("other center after deletion = %+v, %v", got, err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

func TestCorruptRecords(t *testing.T) {
	roleDir := t.TempDir()
	s := openStore(t, roleDir, "alpha")
	dir := filepath.Join(roleDir, "centers", "alpha")
	p := pinFixture("alpha", 1)
	good := pinFixture("alpha", 2)
	savePin(t, s, p)
	savePin(t, s, good)
	path := filepath.Join(dir, "pins", pinFilename(p.Session))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := map[string]string{
		"malformed":        "{",
		"trailing":         string(data) + "{}",
		"version":          strings.Replace(string(data), `"version":1`, `"version":2`, 1),
		"duplicate":        strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1),
		"unknown":          strings.Replace(string(data), `"addr"`, `"extra":false,"addr"`, 1),
		"wrong field case": strings.Replace(string(data), `"role"`, `"Role"`, 1),
		"missing role":     strings.Replace(string(data), `"role":"operator",`, "", 1),
		"null peer":        strings.Replace(string(data), fmt.Sprintf("%q", p.Peer), "null", 1),
		"wrong center":     strings.Replace(string(data), `"alpha"`, `"beta"`, 1),
		"wrong session":    strings.Replace(string(data), "AQAAAAAAAAAAAAAAAAAAAA", "AgAAAAAAAAAAAAAAAAAAAA", 1),
		"noncanonical sid": strings.Replace(string(data), "AQAAAAAAAAAAAAAAAAAAAA", "AQAAAAAAAAAAAAAAAAAAAB", 1),
		"oversized":        strings.Repeat(" ", maxRecordBytes) + string(data),
	}
	for name, invalid := range corrupt {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := s.Pin(p.Session); got != (Pin{}) || !errors.Is(err, ErrCorrupt) || errors.Is(err, ErrNoPin) {
				t.Fatalf("Pin = %+v, %v", got, err)
			}
		})
	}
	list, err := s.Pins()
	if len(list) != 1 || list[0] != good || !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), pinFilename(p.Session)) {
		t.Fatalf("Pins = %+v, %v", list, err)
	}
	if err := s.DeletePin(t.Context(), p.Session); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Pins(); len(got) != 1 || got[0] != good || err != nil {
		t.Fatalf("Pins after corrupt pin deletion = %+v, %v", got, err)
	}

	l := loginFixture()
	if err := s.SaveLogin(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	loginPath := filepath.Join(dir, "login.json")
	data, err = os.ReadFile(loginPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"null", strings.Replace(string(data), `"device_label":""`, `"device_label":null`, 1), strings.Replace(string(data), `"device_label":"",`, "", 1)} {
		if err := os.WriteFile(loginPath, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, found, err := s.Login(); got != (Login{}) || found || !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt Login = %+v, %v, %v", got, found, err)
		}
	}
	if err := s.ClearLogin(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentPinReplacements(t *testing.T) {
	roleDir := t.TempDir()
	primary := openStore(t, roleDir, "alpha")
	secondary := openStore(t, roleDir, "alpha")
	p := pinFixture("alpha", 1)
	savePin(t, primary, p)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			s := []*Store{primary, secondary}[i%2]
			candidate := p
			candidate.Peer[0] = byte(i + 2)
			candidate.Node.HubFP[0] = candidate.Peer[0] + 2
			if err := s.SavePin(t.Context(), candidate); err != nil {
				t.Errorf("SavePin: %v", err)
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			got, err := secondary.Pin(p.Session)
			if err != nil || got.Session != p.Session || got.Node.HubFP[0] != got.Peer[0]+2 {
				t.Errorf("incomplete record read during replacement: %+v, %v", got, err)
				return
			}
		}
	})
	wg.Wait()
	if got, err := primary.Pin(p.Session); err != nil || got.Peer[0] < 2 || got.Peer[0] > 13 || got.Node.HubFP[0] != got.Peer[0]+2 {
		t.Fatalf("final Pin = %+v, %v", got, err)
	}
}

func TestInterruptedReplacementPreservesPin(t *testing.T) {
	s := openStore(t, t.TempDir(), "alpha")
	p := pinFixture("alpha", 1)
	savePin(t, s, p)
	dir, err := s.openPins()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	for _, id := range [][16]byte{p.Session, {2}} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := replaceRecord(ctx, dir, pinFilename(id), []byte("{")); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled pre-rename replacement error = %v", err)
		}
	}
	if got, err := s.Pin(p.Session); got != p || err != nil {
		t.Fatalf("pin after interruption = %+v, %v", got, err)
	}
	path := filepath.Join(s.roleDir, "centers", s.centerID, "pins")
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 || entries[0].Name() != pinFilename(p.Session) {
		t.Fatalf("entries after interruption = %v, %v", entries, err)
	}
	orphan := filepath.Join(path, ".state-orphan.tmp")
	if err := os.WriteFile(orphan, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Pins(); len(got) != 1 || got[0] != p || err != nil {
		t.Fatalf("Pins with interrupted process temporary file = %+v, %v", got, err)
	}
}

func TestPinNamesOnCaseInsensitiveFilesystem(t *testing.T) {
	s := openStore(t, t.TempDir(), "alpha")
	p := pinFixture("alpha", 1)
	savePin(t, s, p)
	var alias recordID
	alternate := "a" + idEncoding.EncodeToString(p.Session[:])[1:]
	if err := alias.UnmarshalText([]byte(alternate)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.roleDir, "centers", s.centerID, "pins", alternate+".json")); errors.Is(err, os.ErrNotExist) {
		t.Skip("filesystem distinguishes case")
	} else if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pin(alias); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Pin at case-insensitive alias error = %v", err)
	}
	other := p
	other.Session = alias
	if err := s.SavePin(t.Context(), other); err == nil {
		t.Fatal("SavePin replaced a pin with a different session ID")
	}
	if err := s.DeletePin(t.Context(), alias); err == nil {
		t.Fatal("DeletePin deleted a pin with a different session ID")
	}
	if got, err := s.Pin(p.Session); got != p || err != nil {
		t.Fatalf("pin after alias operations = %+v, %v", got, err)
	}
}

func TestLockAcrossProcesses(t *testing.T) {
	const childEnv = "RENET_CENTERSTATE_LOCK_TEST_DIR"
	if roleDir := os.Getenv(childEnv); roleDir != "" {
		s := openStore(t, roleDir, "alpha")
		root, err := s.openCenter(false)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		lock, err := acquireLock(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		fmt.Println("locked")
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
		return
	}

	roleDir := t.TempDir()
	s := openStore(t, roleDir, "alpha")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockAcrossProcesses$")
	cmd.Env = append(os.Environ(), childEnv+"="+roleDir)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	if ready, err := bufio.NewReader(stdout).ReadString('\n'); ready != "locked\n" || err != nil {
		t.Fatalf("child lock handshake = %q, %v", ready, err)
	}

	p := pinFixture("alpha", 1)
	wait, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if err := s.SavePin(wait, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SavePin with child holding the lock error = %v", err)
	}
	if _, err := s.Pin(p.Session); !errors.Is(err, ErrNoPin) {
		t.Fatalf("Pin after blocked write error = %v", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("lock holder exited successfully after being killed")
	}
	if err := s.SavePin(ctx, p); err != nil {
		t.Fatalf("SavePin after killing lock holder: %v", err)
	}
}
