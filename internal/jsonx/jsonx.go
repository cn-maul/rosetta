//go:build !nojsonv2

// Package jsonx provides lenient JSON field decoding for third-party
// services whose OpenAI compatibility is imperfect: numbers serialized as
// strings, message content as an array of parts instead of a string,
// nulls where scalars are promised. Decoders never fail the whole payload
// on a single odd field — they fall back to zero values.
//
// The decoders are implemented once on top of encoding/json/v2
// (jsontext token streaming, stable since Go 1.27): values are dispatched
// on their token kind, so no byte round-trips are needed. The v2
// UnmarshalJSONFrom entry point is only invoked by encoding/json when the
// jsonv2 experiment is active (the default). Under GOEXPERIMENT=nojsonv2
// v1 would ignore UnmarshalJSONFrom entirely and silently decode every field
// to its zero value, so the package is built only when jsonv2 is available:
// the constraint turns that silent corruption into a clear build failure
// (audit C12).
package jsonx

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"math"
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

func (f *FlexString) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	f.Value, f.Set = "", true
	switch dec.PeekKind() {
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		f.Value = tok.String()
	case '0', 't', 'f':
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		f.Value = tok.String()
	case 'n':
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		f.Set = false
	default:
		// Objects and arrays leave the field unset instead of failing.
		f.Set = false
		return dec.SkipValue()
	}
	return nil
}

func (f FlexString) MarshalJSON() ([]byte, error) { return json.Marshal(f.Value) }

// FlexInt64 accepts a JSON number or a numeric string ("120" counts the
// same as 120) and yields an int64. Values that cannot be interpreted
// ("N/A") degrade to unset rather than failing the payload.
type FlexInt64 struct {
	Value int64
	Set   bool
}

func (f *FlexInt64) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	f.Value, f.Set = 0, true
	switch dec.PeekKind() {
	case '0':
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		if v, ierr := tok.Int(); ierr == nil {
			f.Value = v
			return nil
		}
		// Tolerate floats like 1.2e3 or 120.0 from sloppy serializers.
		if fv, ferr := tok.Float(); ferr == nil {
			v, ok := clampFloatToInt64(fv)
			f.Value, f.Set = v, ok
			return nil
		}
		f.Set = false
		return nil
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		s := strings.TrimSpace(tok.String())
		if s == "" {
			f.Set = false
			return nil
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			f.Value = v
			return nil
		}
		if fv, ferr := strconv.ParseFloat(s, 64); ferr == nil {
			v, ok := clampFloatToInt64(fv)
			f.Value, f.Set = v, ok
			return nil
		}
		f.Set = false
		return nil
	case 'n':
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		f.Set = false
		return nil
	default:
		f.Set = false
		return dec.SkipValue()
	}
}

func (f FlexInt64) MarshalJSON() ([]byte, error) { return json.Marshal(f.Value) }

// clampFloatToInt64 converts a float to int64 without the silent wraparound
// that a raw int64(f) cast performs on out-of-range or non-finite values:
// ±Inf and overflow pin to the int64 extremes, NaN reports "unset".
func clampFloatToInt64(fv float64) (int64, bool) {
	switch {
	case math.IsNaN(fv):
		return 0, false
	case fv >= math.MaxInt64:
		return math.MaxInt64, true
	case fv <= math.MinInt64:
		return math.MinInt64, true
	default:
		return int64(fv), true
	}
}

// ContentString decodes an OpenAI-style "content" field: usually a plain
// string, but occasionally an array of typed parts on third-party
// services. Text parts are joined with newlines. Unexpected shapes leave
// the field unset instead of failing.
type ContentString struct {
	Value string
	Set   bool
}

func (f *ContentString) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	f.Value, f.Set = "", false
	switch dec.PeekKind() {
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		f.Value, f.Set = tok.String(), true
		return nil
	case '[':
		val, err := dec.ReadValue()
		if err != nil {
			return err
		}
		var parts []struct {
			Text string `json:"text"`
			Type string `json:"type"`
		}
		if err := jsonv2.Unmarshal(val, &parts); err != nil {
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
	case 'n':
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		return nil
	default:
		return dec.SkipValue() // objects and scalars: leave unset
	}
}

func (f ContentString) MarshalJSON() ([]byte, error) { return json.Marshal(f.Value) }

// FlexJSONString decodes a field that is normally a JSON string holding
// serialized JSON (OpenAI tool-call "arguments"), but which Anthropic-style
// gateways sometimes return as a raw object or array. Whatever the shape,
// the caller gets back the JSON text it expects: a string value yields its
// contents verbatim (so incremental stream fragments concatenate cleanly),
// any other value yields its compact re-serialized text, and null yields
// the empty string. It never fails the surrounding payload.
type FlexJSONString struct {
	Value string
	Set   bool
}

func (f *FlexJSONString) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	f.Value, f.Set = "", true
	switch dec.PeekKind() {
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		f.Value = tok.String()
	case 'n':
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		f.Set = false
	default:
		val, err := dec.ReadValue()
		if err != nil {
			return err
		}
		// ReadValue hands back a well-formed value, so compaction cannot
		// fail; strip any incidental whitespace to yield canonical text.
		_ = val.Compact()
		f.Value = string(val)
	}
	return nil
}

func (f FlexJSONString) MarshalJSON() ([]byte, error) { return json.Marshal(f.Value) }
