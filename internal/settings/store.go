package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

const maxConfigBytes = 64 << 10

// Set with -ldflags '-X remote-enet/internal/settings.defaultCenterJSON=...'.
var defaultCenterJSON string

// Load returns defaults only when the file is absent. Existing files must be
// regular files containing one complete versioned object, at most 64 KiB.
func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, fmt.Errorf("settings: config path is required")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := checkMissingConfigParents(path); err != nil {
			return Config{}, err
		}
		cfg := baseConfig()
		cfg.Center, err = loadDefaultCenter()
		if err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("settings: inspect config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("settings: config must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("settings: open config: %w", err)
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return Config{}, fmt.Errorf("settings: stat config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxConfigBytes {
		return Config{}, fmt.Errorf("settings: config must be a regular file of at most 64 KiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("settings: read config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("settings: config exceeds 64 KiB")
	}
	if !utf8.Valid(data) {
		return Config{}, fmt.Errorf("settings: config must be valid UTF-8")
	}
	return decodeConfig(data)
}

func decodeConfig(data []byte) (Config, error) {
	const name = "config"
	object, err := readObject(data, name)
	if err != nil {
		return Config{}, fmt.Errorf("settings: decode config: %w", err)
	}
	var cfg Config
	if err := decodeField(object["version"], "config.version", &cfg.Version); err != nil {
		return Config{}, fmt.Errorf("settings: decode config: %w", err)
	}
	if cfg.Version != 1 && cfg.Version != Version {
		return Config{}, fmt.Errorf("settings: unsupported version %d", cfg.Version)
	}
	operator := map[string]any{
		"listen":          &cfg.Operator.Listen,
		"endpoint":        &cfg.Operator.Endpoint,
		"bind":            &cfg.Operator.Bind,
		"diagnostic_port": &cfg.Operator.DiagnosticPort,
		"control_port":    &cfg.Operator.ControlPort,
		"discovery_port":  &cfg.Operator.DiscoveryPort,
		"exclusive":       &cfg.Operator.Exclusive,
	}
	fields := map[string]any{
		"version":         &cfg.Version,
		"role":            &cfg.Role,
		"allow_link_test": &cfg.AllowLinkTest,
		"client": map[string]any{
			"nic_name": &cfg.Client.NICName,
			"nic_addr": &cfg.Client.NICAddr,
			"peer":     &cfg.Client.Peer,
		},
		"operator": operator,
	}
	if cfg.Version == Version {
		operator["mode"] = &cfg.Operator.Mode
		operator["hub"] = map[string]any{"addr": &cfg.Operator.Hub.Addr, "fp": &cfg.Operator.Hub.FP, "share": &cfg.Operator.Hub.Share}
		fields["center"] = centerFields(&cfg.Center)
	}
	if err := decodeFields(object, name, fields); err != nil {
		return Config{}, fmt.Errorf("settings: decode config: %w", err)
	}
	center, err := loadDefaultCenter()
	if err != nil {
		return Config{}, err
	}
	if cfg.Version == 1 {
		cfg.Version = Version
		cfg.Operator.Mode = "direct"
		cfg.Center = center
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Windows may report a non-directory ancestor as ErrNotExist. Walk back to
// an existing directory, resolving symlinks so a dangling parent is not absent.
func checkMissingConfigParents(path string) error {
	for dir := configParent(path); ; {
		info, err := os.Lstat(dir)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				info, err = os.Stat(dir)
				if err != nil {
					return fmt.Errorf("settings: resolve config directory %q: %w", dir, err)
				}
			}
			if !info.IsDir() {
				return fmt.Errorf("settings: config parent %q must be a directory", dir)
			}
			return nil
		}
		parent := configParent(dir)
		if !errors.Is(err, os.ErrNotExist) || parent == dir {
			return fmt.Errorf("settings: inspect config directory %q: %w", dir, err)
		}
		dir = parent
	}
}

// Keep traversal through ".." intact; cleaning it can hide a broken symlink.
func configParent(path string) string {
	dir, _ := filepath.Split(path)
	if dir == "" {
		return "."
	}
	if len(dir) > len(filepath.VolumeName(dir))+1 {
		dir = dir[:len(dir)-1]
	}
	return dir
}

func loadDefaultCenter() (Center, error) {
	var center Center
	if defaultCenterJSON == "" {
		return center, nil
	}
	data := []byte(defaultCenterJSON)
	if !utf8.Valid(data) {
		return Center{}, fmt.Errorf("settings: default center must be valid UTF-8")
	}
	if err := decodeField(data, "center", centerFields(&center)); err != nil {
		return Center{}, fmt.Errorf("settings: decode default center: %w", err)
	}
	if err := center.validate(); err != nil {
		return Center{}, fmt.Errorf("settings: invalid default center: %w", err)
	}
	return center, nil
}

func centerFields(c *Center) map[string]any {
	return map[string]any{
		"id": &c.ID,
		"api": func(raw json.RawMessage, name string) error {
			var entries []*string
			if err := json.Unmarshal(raw, &entries); err != nil {
				return fmt.Errorf("%s: invalid value: %w", name, err)
			}
			c.API = make([]string, len(entries))
			for i, entry := range entries {
				if entry == nil {
					return fmt.Errorf("%s[%d]: null is not allowed", name, i)
				}
				c.API[i] = *entry
			}
			return nil
		},
	}
}

// readObject preserves exact keys and checks duplicates before selecting a schema.
func readObject(data []byte, name string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil {
		return nil, fmt.Errorf("%s: read object: %w", name, err)
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("%s: expected an object", name)
	}
	object := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("%s: read field: %w", name, err)
		}
		key := token.(string)
		if _, ok := object[key]; ok {
			return nil, fmt.Errorf("%s: duplicate field %q", name, key)
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, fmt.Errorf("%s.%s: read value: %w", name, key, err)
		}
		object[key] = raw
	}
	if _, err := d.Token(); err != nil {
		return nil, fmt.Errorf("%s: close object: %w", name, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("%s: trailing data", name)
	}
	return object, nil
}

func decodeFields(object map[string]json.RawMessage, name string, fields map[string]any) error {
	for key := range object {
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("%s: unknown field %q", name, key)
		}
	}
	for key, target := range fields {
		if err := decodeField(object[key], name+"."+key, target); err != nil {
			return err
		}
	}
	return nil
}

func decodeField(raw json.RawMessage, name string, target any) error {
	if raw == nil {
		return fmt.Errorf("%s: missing required field", name)
	}
	if bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("%s: null is not allowed", name)
	}
	switch target := target.(type) {
	case map[string]any:
		object, err := readObject(raw, name)
		if err != nil {
			return err
		}
		return decodeFields(object, name, target)
	case func(json.RawMessage, string) error:
		return target(raw, name)
	default:
		if err := json.Unmarshal(raw, target); err != nil {
			return fmt.Errorf("%s: invalid value: %w", name, err)
		}
		return nil
	}
}

// Save validates before touching disk and replaces the file only after the
// temporary file is synced and closed. Any failure before rename preserves it.
func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if path == "" {
		return fmt.Errorf("settings: config path is required")
	}
	if cfg.Center.API == nil {
		cfg.Center.API = []string{}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: encode config: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxConfigBytes {
		return fmt.Errorf("settings: config exceeds 64 KiB")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("settings: config must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("settings: inspect config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("settings: create config directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("settings: create temporary config: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("settings: write temporary config: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("settings: sync temporary config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("settings: close temporary config: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("settings: replace config: %w", err)
	}
	return nil
}

// DefaultPaths uses the OS config directory without creating it or depending on
// the working directory. Role-specific identity directories belong under state.
func DefaultPaths() (configPath, stateDir string, err error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", "", fmt.Errorf("settings: user config directory: %w", err)
	}
	if !filepath.IsAbs(base) {
		return "", "", fmt.Errorf("settings: user config directory must be absolute")
	}
	dir := filepath.Join(base, "remote-enet")
	return filepath.Join(dir, "settings.json"), filepath.Join(dir, "state"), nil
}

// DefaultPathsForRole isolates GUI preferences while retaining the shared
// state root; callers append the role to stateDir for identity and trust.
func DefaultPathsForRole(role string) (configPath, stateDir string, err error) {
	if role != RoleClient && role != RoleOperator {
		return "", "", fmt.Errorf("settings: role must be client or operator")
	}
	configPath, stateDir, err = DefaultPaths()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(filepath.Dir(configPath), role, "settings.json"), stateDir, nil
}
