// Package centerstate persists per-session pins and the local login cache.
package centerstate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"remote-enet/internal/identity"
)

var (
	// ErrNoPin identifies a session with no saved pin.
	ErrNoPin = errors.New("centerstate: pin not found")
	// ErrCorrupt identifies state that cannot be used for recovery.
	ErrCorrupt = errors.New("centerstate: corrupt state")
)

var centerIDPattern = regexp.MustCompile(`^[a-z0-9.-]{1,32}$`)

// Store is scoped to one role directory and one center.
type Store struct {
	roleDir  string
	centerID string
}

// Open prepares a center directory under the caller's role directory.
func Open(roleDir, centerID string) (*Store, error) {
	if roleDir == "" {
		return nil, fmt.Errorf("centerstate: role directory is required")
	}
	if !centerIDPattern.MatchString(centerID) || !filepath.IsLocal(centerID) ||
		centerID == "." || (runtime.GOOS == "windows" && strings.HasSuffix(centerID, ".")) {
		return nil, fmt.Errorf("centerstate: invalid center ID %q", centerID)
	}
	roleDir, err := filepath.Abs(roleDir)
	if err != nil {
		return nil, fmt.Errorf("centerstate: resolve role directory: %w", err)
	}
	if err := os.MkdirAll(roleDir, 0o700); err != nil {
		return nil, fmt.Errorf("centerstate: create role directory %q: %w", roleDir, err)
	}
	s := &Store{roleDir: roleDir, centerID: centerID}
	root, err := s.openCenter(true)
	if err != nil {
		return nil, s.wrap("open", err)
	}
	defer root.Close()
	pins, err := openSubdir(root, "pins", true)
	if err != nil {
		return nil, s.wrap("open pins", err)
	}
	pins.Close()
	return s, nil
}

// NodeRef identifies the Hub selected for the pinned session.
type NodeRef struct {
	ID    [16]byte
	Addr  string
	Port  uint16
	HubFP identity.Fingerprint
}

// Pin holds the peer fingerprint confirmed for one session.
type Pin struct {
	CenterID string
	Session  [16]byte
	Role     string
	Peer     identity.Fingerprint
	Node     NodeRef
	PairedAt time.Time
}

// Login is a UI cache. Its contents do not establish authorization.
type Login struct {
	DeviceFP     identity.Fingerprint
	DeviceLabel  string
	AccountID    string
	AccountLabel string
	ExpiresAt    time.Time
	LoggedInAt   time.Time
}

// SavePin replaces the pin after the caller has completed pairing.
func (s *Store) SavePin(ctx context.Context, p Pin) error {
	name := pinFilename(p.Session)
	if err := s.validatePin(p.Session, p); err != nil {
		return s.wrap("save "+name, err)
	}
	return s.mutate(ctx, "save "+name, true, func(dir *os.Root) error {
		if err := checkPinName(dir, name); err != nil {
			return err
		}
		return saveRecord(ctx, dir, name, encodePin(p))
	})
}

// Pin returns ErrNoPin for an absent record and ErrCorrupt for an unusable one.
func (s *Store) Pin(id [16]byte) (Pin, error) {
	pins, err := s.openPins()
	if err != nil {
		return Pin{}, s.wrap("read "+pinFilename(id), err)
	}
	defer pins.Close()
	return s.readPin(pins, id)
}

// Pins returns usable records and joins errors for unusable entries.
// Temporary files are ignored. Pins deleted during enumeration are omitted.
func (s *Store) Pins() ([]Pin, error) {
	pins, err := s.openPins()
	if err != nil {
		return nil, s.wrap("list pins", err)
	}
	defer pins.Close()
	entries, err := fs.ReadDir(pins.FS(), ".")
	if err != nil {
		return nil, s.wrap("list pins", err)
	}
	var records []Pin
	var errs []error
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		var id recordID
		if err := id.UnmarshalText([]byte(name)); err != nil {
			errs = append(errs, s.wrap("list pins", fmt.Errorf("%w: invalid pin name %q: %w", ErrCorrupt, entry.Name(), err)))
			continue
		}
		p, err := s.readPin(pins, id)
		switch {
		case err == nil:
			records = append(records, p)
		case !errors.Is(err, ErrNoPin):
			errs = append(errs, err)
		}
	}
	return records, errors.Join(errs...)
}

// DeletePin is idempotent and also removes an unreadable pin for the session.
func (s *Store) DeletePin(ctx context.Context, id [16]byte) error {
	name := pinFilename(id)
	return s.mutate(ctx, "delete "+name, true, func(dir *os.Root) error {
		if err := checkPinName(dir, name); err != nil {
			return err
		}
		return deleteRecord(ctx, dir, name)
	})
}

// SaveLogin replaces the UI cache for the current center.
func (s *Store) SaveLogin(ctx context.Context, l Login) error {
	if err := validateLogin(l); err != nil {
		return s.wrap("save login.json", err)
	}
	return s.mutate(ctx, "save login.json", false, func(dir *os.Root) error {
		return saveRecord(ctx, dir, "login.json", encodeLogin(l))
	})
}

// Login reports whether the cache exists. The caller must refresh it with the center.
func (s *Store) Login() (Login, bool, error) {
	root, err := s.openCenter(false)
	if err != nil {
		return Login{}, false, s.wrap("read login.json", err)
	}
	defer root.Close()
	var record loginRecord
	found, err := readRecord(root, "login.json", &record)
	if err == nil && found {
		var login Login
		login, err = record.login()
		if err == nil {
			return login, true, nil
		}
		err = fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if err != nil {
		return Login{}, false, s.wrap("read login.json", err)
	}
	return Login{}, false, nil
}

// ClearLogin is idempotent and also removes an unreadable cache.
func (s *Store) ClearLogin(ctx context.Context) error {
	return s.mutate(ctx, "clear login.json", false, func(dir *os.Root) error {
		return deleteRecord(ctx, dir, "login.json")
	})
}

func (s *Store) readPin(dir *os.Root, id [16]byte) (Pin, error) {
	name := pinFilename(id)
	var record pinRecord
	found, err := readRecord(dir, name, &record)
	switch {
	case err != nil:
		return Pin{}, s.wrap("read "+name, err)
	case !found:
		return Pin{}, s.wrap("read "+name, ErrNoPin)
	case record.Version != version:
		return Pin{}, s.wrap("read "+name, fmt.Errorf("%w: unsupported version %d", ErrCorrupt, record.Version))
	}
	p := record.pin()
	if err := s.validatePin(id, p); err != nil {
		return Pin{}, s.wrap("read "+name, fmt.Errorf("%w: %w", ErrCorrupt, err))
	}
	return p, nil
}

func (s *Store) openCenter(create bool) (*os.Root, error) {
	role, err := os.OpenRoot(s.roleDir)
	if err != nil {
		return nil, fmt.Errorf("open role directory: %w", err)
	}
	defer role.Close()
	centers, err := openSubdir(role, "centers", create)
	if err != nil {
		return nil, err
	}
	defer centers.Close()
	return openSubdir(centers, s.centerID, create)
}

func (s *Store) openPins() (*os.Root, error) {
	root, err := s.openCenter(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return openSubdir(root, "pins", false)
}

func (s *Store) mutate(ctx context.Context, op string, pinDir bool, change func(*os.Root) error) error {
	if err := ctx.Err(); err != nil {
		return s.wrap(op, err)
	}
	root, err := s.openCenter(false)
	if err != nil {
		return s.wrap(op, err)
	}
	defer root.Close()
	lock, err := acquireLock(ctx, root)
	if err != nil {
		return s.wrap(op, err)
	}
	defer lock.Close()
	dir := root
	if pinDir {
		dir, err = openSubdir(root, "pins", false)
		if err != nil {
			return s.wrap(op, err)
		}
		defer dir.Close()
	}
	return s.wrap(op, change(dir))
}

func (s *Store) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("centerstate: %s in %q: %w", op, filepath.Join(s.roleDir, "centers", s.centerID), err)
}
