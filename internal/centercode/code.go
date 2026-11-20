package centercode

import (
	"errors"
	"strings"
	"unicode"
)

var (
	// ErrCodeLength indicates the code does not have eight symbols after normalization.
	ErrCodeLength = errors.New("code must contain eight symbols")
	// ErrCodeCharacter indicates a symbol outside the code's alphabet.
	ErrCodeCharacter = errors.New("code contains an invalid character")
)

const accountAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NormalizeAccountCode returns eight Crockford symbols, folding ASCII case and aliases.
func NormalizeAccountCode(s string) (string, error) {
	var code [8]byte
	n := 0
	for _, r := range s {
		if r == '-' || unicode.IsSpace(r) {
			continue
		}
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		switch r {
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if !strings.ContainsRune(accountAlphabet, r) {
			return "", ErrCodeCharacter
		}
		if n == len(code) {
			return "", ErrCodeLength
		}
		code[n] = byte(r)
		n++
	}
	if n != len(code) {
		return "", ErrCodeLength
	}
	return string(code[:]), nil
}

// FormatAccountCode accepts the same input as NormalizeAccountCode and returns
// an empty string for invalid input.
func FormatAccountCode(s string) string {
	code, err := NormalizeAccountCode(s)
	if err != nil {
		return ""
	}
	return code[:4] + "-" + code[4:]
}

// NormalizeNumericCode preserves leading zeros and folds only full-width digits to ASCII.
func NormalizeNumericCode(s string) (string, error) {
	var code [8]byte
	n := 0
	for _, r := range s {
		if r == '-' || unicode.IsSpace(r) {
			continue
		}
		if r >= '０' && r <= '９' {
			r -= '０' - '0'
		}
		if r < '0' || r > '9' {
			return "", ErrCodeCharacter
		}
		if n == len(code) {
			return "", ErrCodeLength
		}
		code[n] = byte(r)
		n++
	}
	if n != len(code) {
		return "", ErrCodeLength
	}
	return string(code[:]), nil
}
