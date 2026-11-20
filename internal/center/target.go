package center

import (
	"fmt"
	"net/url"
	"strings"
)

// CanonicalTarget accepts an HTTP(S) URL or an origin-form target. Client
// paths and queries must already be ASCII URL-encoded.
func CanonicalTarget(raw string) (string, error) {
	raw, _, _ = strings.Cut(raw, "#")
	if !strings.HasPrefix(raw, "/") {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "\\") {
			return "", fmt.Errorf("unsupported center URL %q", raw)
		}
		// Read the encoded path verbatim, including the case of escapes.
		_, rest, _ := strings.Cut(raw, "://")
		i := strings.IndexAny(rest, "/?")
		raw = "/"
		if i >= 0 {
			raw = rest[i:]
			if raw[0] == '?' {
				raw = "/" + raw
			}
		}
	}
	path, query, _ := strings.Cut(raw, "?")
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '%' {
			if i+2 >= len(raw) {
				return "", fmt.Errorf("incomplete URL escape")
			}
			if _, err := url.PathUnescape(raw[i : i+3]); err != nil {
				return "", fmt.Errorf("invalid URL escape: %w", err)
			}
			i += 2
			continue
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("-._~!$&()*+,/:;=?@", rune(c)) {
			continue
		}
		return "", fmt.Errorf("unsupported target byte %02x", c)
	}
	segments := strings.Split(path[1:], "/")
	stack := make([]string, 0, len(segments))
	for i, s := range segments {
		switch strings.ReplaceAll(strings.ToLower(s), "%2e", ".") {
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case ".":
		default:
			stack = append(stack, s)
			continue
		}
		if i == len(segments)-1 {
			stack = append(stack, "")
		}
	}
	target := "/" + strings.Join(stack, "/")
	if query != "" {
		target += "?" + query
	}
	return target, nil
}
