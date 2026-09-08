package jsonx

import (
	"encoding/json"
	"testing"
)

func TestFlexString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		value string
		set   bool
	}{
		{"string", `"hello"`, "hello", true},
		{"number", `123`, "123", true},
		{"float", `1.5`, "1.5", true},
		{"bool", `true`, "true", true},
		{"null", `null`, "", false},
		{"object", `{"a":1}`, "", false},
		{"array", `[1,2]`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f FlexString
			if err := json.Unmarshal([]byte(tt.input), &f); err != nil {
				t.Fatalf("Unmarshal(%s) error: %v", tt.input, err)
			}
			if f.Value != tt.value || f.Set != tt.set {
				t.Fatalf("got (%q, %v), want (%q, %v)", f.Value, f.Set, tt.value, tt.set)
			}
		})
	}
}

func TestFlexInt64(t *testing.T) {
	tests := []struct {
		name  string
		input string
		value int64
		set   bool
	}{
		{"number", `120`, 120, true},
		{"numeric string", `"120"`, 120, true},
		{"float string", `"120.0"`, 120, true},
		{"float truncation", `12.9`, 12, true},
		{"exponent string", `"1.5e3"`, 1500, true},
		{"null", `null`, 0, false},
		{"empty string", `""`, 0, false},
		// Contract: a single odd field must never fail the whole payload.
		{"non-numeric string", `"N/A"`, 0, false},
		{"dash string", `"-"`, 0, false},
		{"bool", `true`, 0, false},
		{"object", `{"a":1}`, 0, false},
		{"array", `[1,2]`, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f FlexInt64
			if err := json.Unmarshal([]byte(tt.input), &f); err != nil {
				t.Fatalf("Unmarshal(%s) error: %v", tt.input, err)
			}
			if f.Value != tt.value || f.Set != tt.set {
				t.Fatalf("got (%d, %v), want (%d, %v)", f.Value, f.Set, tt.value, tt.set)
			}
		})
	}
}

func TestContentString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		value string
		set   bool
	}{
		{"string", `"hi"`, "hi", true},
		{"null", `null`, "", false},
		{"object", `{"foo":"bar"}`, "", false},
		{"number", `123`, "", false},
		{"array of text parts", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb", true},
		{"array without text", `[{"type":"image_url"}]`, "", true},
		{"array of scalars", `[1,2]`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f ContentString
			if err := json.Unmarshal([]byte(tt.input), &f); err != nil {
				t.Fatalf("Unmarshal(%s) error: %v", tt.input, err)
			}
			if f.Value != tt.value || f.Set != tt.set {
				t.Fatalf("got (%q, %v), want (%q, %v)", f.Value, f.Set, tt.value, tt.set)
			}
		})
	}
}

// The package contract promises the whole payload survives one odd field.
func TestPayloadSurvivesOddField(t *testing.T) {
	var doc struct {
		Content ContentString `json:"content"`
		Tokens  FlexInt64     `json:"prompt_tokens"`
		ID      string        `json:"id"`
	}
	body := `{"id":"x","content":"hi","prompt_tokens":"N/A"}`
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("payload failed to decode: %v", err)
	}
	if doc.ID != "x" || doc.Content.Value != "hi" || doc.Tokens.Value != 0 {
		t.Fatalf("unexpected decode result: %+v", doc)
	}
}

// The MarshalJSON entry points stay alive under the v2 backend: they are
// what the v1 encoding/json API calls for these types.
func TestMarshal(t *testing.T) {
	tests := []struct {
		name string
		f    any
		want string
	}{
		{"FlexString", FlexString{Value: "hi", Set: true}, `"hi"`},
		{"FlexString empty", FlexString{}, `""`},
		{"FlexInt64", FlexInt64{Value: 42, Set: true}, `42`},
		{"FlexInt64 unset", FlexInt64{}, `0`},
		{"ContentString", ContentString{Value: "a\nb", Set: true}, `"a\nb"`},
		{"ContentString unset", ContentString{}, `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.f)
			if err != nil {
				t.Fatalf("Marshal err: %v", err)
			}
			if string(b) != tt.want {
				t.Fatalf("Marshal(%+v) = %s, want %s", tt.f, b, tt.want)
			}
		})
	}
}
