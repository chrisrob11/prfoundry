// Package jsonio decodes JSON strictly, so a typo in a config file is an
// error rather than a silently ignored field, and writes JSON indented.
package jsonio

import (
	"bytes"
	"encoding/json"
	"io"
)

// DecodeStrict decodes JSON b into v, refusing unknown fields.
func DecodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Write writes v to w as indented JSON.
func Write(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
