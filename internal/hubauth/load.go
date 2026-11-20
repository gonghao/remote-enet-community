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
	cfg := defaults()
	if err := DecodeStrict(f, 64<<10, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// DecodeStrict overlays a bounded UTF-8 JSON document onto a tagged struct.
// Destinations support structs, slices, strings, int and int64. Discard dst on error.
func DecodeStrict(r io.Reader, limit int, dst any) error {
	v := reflect.ValueOf(dst)
	if limit < 0 || v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return errors.New("expected nonnegative limit and pointer to struct")
	}
	reader := &io.LimitedReader{R: r, N: int64(limit)}
	b, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	if reader.N == 0 {
		var extra [1]byte
		if _, err := io.ReadFull(r, extra[:]); err != io.EOF {
			if err != nil {
				return fmt.Errorf("read configuration: %w", err)
			}
			return fmt.Errorf("configuration exceeds %d bytes", limit)
		}
	}
	if !utf8.Valid(b) {
		return errors.New("expected UTF-8 configuration")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := decode(d, v.Elem(), ""); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing configuration content")
	}
	return nil
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
				if tag, ok := f.Tag.Lookup("json"); ok && f.IsExported() && tag != "-" && tag == name {
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
