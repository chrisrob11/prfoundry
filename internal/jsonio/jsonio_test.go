package jsonio

import (
	"strings"
	"testing"
)

func TestDecodeStrictRefusesUnknownFields(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	if err := DecodeStrict([]byte(`{"a": 1}`), &v); err != nil || v.A != 1 {
		t.Fatalf("DecodeStrict = %v, %+v", err, v)
	}
	if err := DecodeStrict([]byte(`{"b": 1}`), &v); err == nil {
		t.Fatal("DecodeStrict of an unknown field: want an error")
	}
}

func TestWriteIndents(t *testing.T) {
	var b strings.Builder
	if err := Write(&b, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), "{\n  \"a\": 1\n}\n"; got != want {
		t.Fatalf("Write = %q, want %q", got, want)
	}
}
