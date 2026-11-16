// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"bytes"
	"fmt"
	"strings"
)

// ParseDotenv reads environment variables from user-data written as
// KEY=VALUE lines:
//
//	# comment
//	LISTEN=:8080
//	export GREETING="hello, world\n"
//	PATTERN='$literal # not a comment'
//	EMPTY=
//
// Keys are [A-Za-z_][A-Za-z0-9_]*. Unquoted values end at " #" and are
// trimmed. Single-quoted values are literal; double-quoted values take the
// escapes \\ \" \$ \n \r \t. There is no variable expansion and a value
// cannot span lines. Later definitions of a key win.
//
// Any other line is an error, so user-data meant for something else, such
// as a shell script or a #cloud-config document, is not mistaken for an
// environment.
func ParseDotenv(b []byte) (map[string]string, error) {
	// Trim the BOM
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	env := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, err := parseDotenvLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		env[k] = v
	}
	return env, nil
}

func parseDotenvLine(line string) (string, string, error) {
	if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		line = strings.TrimSpace(rest)
	}
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", fmt.Errorf("not KEY=VALUE")
	}
	k = strings.TrimRight(k, " \t")
	if !validEnvKey(k) {
		return "", "", fmt.Errorf("invalid name %q", k)
	}
	v = strings.TrimLeft(v, " \t")
	var val, rest string
	switch {
	case strings.HasPrefix(v, "'"):
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", "", fmt.Errorf("%s: unterminated quote", k)
		}
		val, rest = v[1:1+end], v[2+end:]
	case strings.HasPrefix(v, `"`):
		var b strings.Builder
		i := 1
		for ; i < len(v) && v[i] != '"'; i++ {
			if v[i] != '\\' || i+1 == len(v) {
				b.WriteByte(v[i])
				continue
			}
			i++
			switch v[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '\\', '"', '$':
				b.WriteByte(v[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(v[i])
			}
		}
		if i >= len(v) {
			return "", "", fmt.Errorf("%s: unterminated quote", k)
		}
		val, rest = b.String(), v[i+1:]
	default:
		val = v
		if j := strings.Index(val, " #"); j >= 0 {
			val = val[:j]
		} else if j := strings.Index(val, "\t#"); j >= 0 {
			val = val[:j]
		}
		val = strings.TrimRight(val, " \t")
	}
	if rest = strings.TrimSpace(rest); rest != "" && rest[0] != '#' {
		return "", "", fmt.Errorf("%s: unexpected text after the closing quote", k)
	}
	if strings.IndexByte(val, 0) >= 0 {
		return "", "", fmt.Errorf("%s: value contains NUL", k)
	}
	return k, val, nil
}

func validEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i, c := range k {
		switch {
		case c == '_' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z':
		case i > 0 && '0' <= c && c <= '9':
		default:
			return false
		}
	}
	return true
}
