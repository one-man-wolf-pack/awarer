package checkpointjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// errTrailingData reports bytes after the single JSON document a store record must
// contain. It is unexported: callers wrap it with a record-specific label, and the
// repository maps the wrapped result to ErrCorruptStore.
var errTrailingData = errors.New("unexpected trailing data after JSON document")

// probeSchemaVersion leniently reads a persisted record's own schema_version, so the
// incompatible-versus-corrupt line can be drawn before the strict reader runs. The
// error is non-nil when data is not a JSON document carrying a numeric
// schema_version at all — that is corruption, not a format from another generation.
//
// It returns why, not just that, because the two ways to fail here look nothing alike
// to whoever has to fix the store: a truncated file (the likely outcome of a partial
// write) and a well-formed document with the wrong shape both land on this path, and
// a diagnostic that flattened them into one sentence would send the reader looking
// for the wrong problem.
//
// It is deliberately the only concession to a document this build did not write: it
// classifies, it never dispatches. There is exactly one decoder per record, and a
// version other than the current one is refused rather than routed to a second shape.
func probeSchemaVersion(data []byte) (int, error) {
	var probe struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return 0, err
	}
	if probe.SchemaVersion == nil {
		return 0, errors.New("no schema_version")
	}
	return *probe.SchemaVersion, nil
}

// decodeStrictJSON decodes exactly one JSON document from data into dst, rejecting
// unknown fields and any trailing bytes. Every persisted checkpoint record is a
// hostile-input boundary: a hand-edit, a typo, or a record from another format must
// fail loudly here rather than being silently half-trusted, so the store's
// exact-schema-shape invariant holds on read.
func decodeStrictJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errTrailingData
	}
	return nil
}
