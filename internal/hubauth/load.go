package hubauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"unicode/utf8"
)

// Load bounds the read before decoding, then validates the effective config.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()
	const limit = 64 << 10
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	if len(b) > limit || !utf8.Valid(b) {
		return Config{}, errors.New("expected UTF-8 configuration of at most 64 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	cfg := defaults()
	if err := decode(d, reflect.ValueOf(&cfg).Elem(), ""); err != nil {
		return Config{}, err
	}
	if _, err := d.Token(); err != io.EOF {
		return Config{}, errors.New("trailing configuration content")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Match exact JSON tags and track duplicate keys before assigning values.
// The destination type bounds recursion, even for malformed input.
func decode(d *json.Decoder, v reflect.Value, path string) error {
	tok, err := d.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	switch v.Kind() {
	case reflect.Struct:
		if tok != json.Delim('{') {
			break
		}
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			name := key.(string)
			child := name
			if path != "" {
				child = path + "." + name
			}
			if seen[name] {
				return fmt.Errorf("%s: duplicate field", child)
			}
			seen[name] = true
			index := -1
			for i := range v.NumField() {
				f := v.Type().Field(i)
				if f.Tag.Get("json") == name {
					index = i
					break
				}
			}
			if index < 0 {
				return fmt.Errorf("%s: unknown field", child)
			}
			if err := decode(d, v.Field(index), child); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case reflect.Slice:
		if tok != json.Delim('[') {
			break
		}
		for d.More() {
			elem := reflect.New(v.Type().Elem()).Elem()
			if err := decode(d, elem, fmt.Sprintf("%s[%d]", path, v.Len())); err != nil {
				return err
			}
			v.Set(reflect.Append(v, elem))
		}
		_, err = d.Token()
		return err
	case reflect.String:
		if s, ok := tok.(string); ok {
			v.SetString(s)
			return nil
		}
	case reflect.Int, reflect.Int64:
		if n, ok := tok.(json.Number); ok {
			x, err := strconv.ParseInt(string(n), 10, v.Type().Bits())
			if err == nil {
				v.SetInt(x)
				return nil
			}
		}
	}
	return fmt.Errorf("%s: expected %s", path, v.Type())
}
