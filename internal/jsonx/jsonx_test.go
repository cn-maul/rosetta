package jsonx

import (
	"encoding/json"
	"testing"
)

func unmarshal[T any](t *testing.T, data string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	return v
}

func TestFlexString(t *testing.T) {
	cases := []struct {
		data string
		want string
		set  bool
	}{
		{`"hello"`, "hello", true},
		{`123`, "123", true},
		{`true`, "true", true},
		{`null`, "", false},
		{`"123"`, "123", true},
	}
	for _, tc := range cases {
		got := unmarshal[FlexString](t, tc.data)
		if got.Value != tc.want || got.Set != tc.set {
			t.Errorf("FlexString(%s) = %+v, want value=%q set=%v", tc.data, got, tc.want, tc.set)
		}
	}
}

func TestFlexInt64(t *testing.T) {
	cases := []struct {
		data string
		want int64
		set  bool
	}{
		{`120`, 120, true},
		{`"120"`, 120, true},
		{`" 120 "`, 120, true},
		{`120.0`, 120, true},
		{`null`, 0, false},
		{`""`, 0, false},
	}
	for _, tc := range cases {
		got := unmarshal[FlexInt64](t, tc.data)
		if got.Value != tc.want || got.Set != tc.set {
			t.Errorf("FlexInt64(%s) = %+v, want value=%d set=%v", tc.data, got, tc.want, tc.set)
		}
	}
	if err := json.Unmarshal([]byte(`"abc"`), &FlexInt64{}); err == nil {
		t.Errorf("non-numeric string must error")
	}
}

func TestContentString(t *testing.T) {
	cases := []struct {
		data string
		want string
		set  bool
	}{
		{`"hi"`, "hi", true},
		{`null`, "", false},
		{`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb", true},
		{`[{"type":"image_url"}]`, "", true},
		{`{"weird":"object"}`, "", false},
		{`[1,2]`, "", false},
	}
	for _, tc := range cases {
		got := unmarshal[ContentString](t, tc.data)
		if got.Value != tc.want || got.Set != tc.set {
			t.Errorf("ContentString(%s) = %+v, want value=%q set=%v", tc.data, got, tc.want, tc.set)
		}
	}
}
