// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ParseUserDataEnv reads environment variables from user-data in one of
// three forms, chosen by its first non-blank character:
//
//	{"KEY": "value", ...}                       a JSON object
//	[{"key": "KEY", "value": "value"}, ...]     a JSON array of pairs
//	KEY=value                                   dotenv lines (ParseDotenv)
//
// JSON values may be strings, numbers or booleans; numbers and booleans
// become their JSON text. Field names of the pairs match case-insensitively,
// so AWS-style {"Key": ..., "Value": ...} works too. Later definitions of a
// key win.
func ParseUserDataEnv(b []byte) (map[string]string, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	switch t := bytes.TrimLeft(b, " \t\r\n"); {
	case len(t) > 0 && t[0] == '{':
		return parseJSONObjectEnv(t)
	case len(t) > 0 && t[0] == '[':
		return parseJSONPairsEnv(t)
	}
	return ParseDotenv(b)
}

func parseJSONObjectEnv(b []byte) (map[string]string, error) {
	var obj map[string]json.RawMessage
	if err := decodeJSON(b, &obj); err != nil {
		return nil, err
	}
	env := map[string]string{}
	for k, raw := range obj {
		if err := setJSONEnv(env, k, raw); err != nil {
			return nil, err
		}
	}
	return env, nil
}

func parseJSONPairsEnv(b []byte) (map[string]string, error) {
	var pairs []struct {
		Key   *string         `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := decodeJSON(b, &pairs); err != nil {
		return nil, err
	}
	env := map[string]string{}
	for i, p := range pairs {
		if p.Key == nil {
			return nil, fmt.Errorf("JSON: element %d has no \"key\"", i)
		}
		if p.Value == nil {
			return nil, fmt.Errorf("JSON: %s has no \"value\"", *p.Key)
		}
		if err := setJSONEnv(env, *p.Key, p.Value); err != nil {
			return nil, err
		}
	}
	return env, nil
}

// decodeJSON decodes exactly one JSON value, rejecting unknown fields and
// trailing data.
func decodeJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("JSON: %w", err)
	}
	if dec.More() {
		return errors.New("JSON: unexpected data after the value")
	}
	return nil
}

func setJSONEnv(env map[string]string, k string, raw json.RawMessage) error {
	if !validEnvKey(k) {
		return fmt.Errorf("JSON: invalid name %q", k)
	}
	var v string
	switch raw = bytes.TrimSpace(raw); {
	case len(raw) > 0 && raw[0] == '"':
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("JSON: %s: %w", k, err)
		}
	case len(raw) > 0 && (raw[0] == '-' || '0' <= raw[0] && raw[0] <= '9'),
		string(raw) == "true", string(raw) == "false":
		v = string(raw)
	default:
		return fmt.Errorf("JSON: %s: value must be a string, number or boolean", k)
	}
	if strings.IndexByte(v, 0) >= 0 {
		return fmt.Errorf("JSON: %s: value contains NUL", k)
	}
	env[k] = v
	return nil
}
