// Package jsonx provides lenient JSON field decoding for third-party
// services whose OpenAI compatibility is imperfect: numbers serialized as
// strings, message content as an array of parts instead of a string,
// nulls where scalars are promised. Decoders never fail the whole payload
// on a single odd field — they fall back to zero values.
package jsonx

import (
	"encoding/json"
	"strconv"
	"strings"
)

// FlexString accepts a JSON string, number, boolean or null and always
// yields a string. Numbers and booleans are taken verbatim from their raw
// text ("123" -> "123").
type FlexString struct {
	Value string
	Set   bool
}

func (f *FlexString) UnmarshalJSON(b []byte) error {
	f.Value, f.Set = "", true
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		f.Set = false
		return nil
	}
	if s[0] == '"' {
		return json.Unmarshal(b, &f.Value)
	}
	f.Value = strings.Trim(s, `"`)
	return nil
}

func (f FlexString) MarshalJSON() ([]byte, error) { return json.Marshal(f.Value) }

// FlexInt64 accepts a JSON number or a numeric string ("120" counts the
// same as 120) and yields an int64.
type FlexInt64 struct {
	Value int64
	Set   bool
}

func (f *FlexInt64) UnmarshalJSON(b []byte) error {
	f.Value, f.Set = 0, true
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		f.Set = false
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
		if s == "" {
			f.Set = false
			return nil
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		f.Value = v
		return nil
	}
	// Tolerate floats like 1.2e3 or 120.0 from sloppy serializers.
	if fv, ferr := strconv.ParseFloat(s, 64); ferr == nil {
		f.Value = int64(fv)
		return nil
	}
	return err
}

func (f FlexInt64) MarshalJSON() ([]byte, error) { return json.Marshal(f.Value) }

// ContentString decodes an OpenAI-style "content" field: usually a plain
// string, but occasionally an array of typed parts on third-party
// services. Text parts are joined with newlines. Unexpected shapes leave
// the field unset instead of failing.
type ContentString struct {
	Value string
	Set   bool
}

func (f *ContentString) UnmarshalJSON(b []byte) error {
	f.Value, f.Set = "", false
	s := strings.TrimSpace(string(b))
	switch {
	case s == "null" || s == "":
		return nil
	case s[0] == '"':
		f.Set = true
		return json.Unmarshal(b, &f.Value)
	case s[0] == '[':
		var parts []struct {
			Text string `json:"text"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &parts); err != nil {
			return nil // unexpected element shapes: leave unset
		}
		var sb strings.Builder
		for _, p := range parts {
			if p.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
		f.Value, f.Set = sb.String(), true
		return nil
	default:
		return nil
	}
}

func (f ContentString) MarshalJSON() ([]byte, error) { return json.Marshal(f.Value) }
