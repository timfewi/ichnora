package core

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"unicode/utf8"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/ingest"
)

type fieldKey struct{ root, path, field string }
type fieldCheck struct{ valueDigest, digest, state, reason string }

func jsonScalar(raw []byte) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if !utf8.Valid(raw) || !json.Valid(raw) || raw[0] == '[' || raw[0] == '{' {
		return "", false
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", false
		}
		return mustJSON(value), true
	}
	return string(raw), true
}

func (c *Core) readField(root, path, field string) fieldCheck {
	unavailable := fieldCheck{state: ApUnknown, reason: "JSON field cannot be read"}
	if root == "" {
		return unavailable
	}
	absolute, err := c.pol.Source(root, path)
	if err != nil {
		return unavailable
	}
	file, err := os.Open(absolute)
	if err != nil {
		return unavailable
	}
	defer file.Close()
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) > limit || !utf8.Valid(data) || !json.Valid(data) {
		return unavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return unavailable
	}
	result := fieldCheck{state: ApStale, reason: "JSON field missing"}
	found := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return unavailable
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return unavailable
		}
		if key != field {
			continue
		}
		if found {
			return fieldCheck{state: ApUnknown, reason: "JSON field occurs more than once"}
		}
		found = true
		scalar, ok := jsonScalar(raw)
		if !ok {
			result.reason = "JSON field is not scalar"
			continue
		}
		result = fieldCheck{valueDigest: ingest.Digest([]byte(scalar)), digest: ingest.Digest(data), state: ApCurrent}
	}
	return result
}

func (c *Core) prepareField(root, role string, r contract.Ref) (preparedRef, error) {
	if r.Field == "" || len(r.Field) > 200 {
		return preparedRef{}, contract.Errf(contract.CodeInvalid, "json_field needs a key of 1 to 200 bytes")
	}
	value, ok := jsonScalar([]byte(r.Value))
	if !ok {
		return preparedRef{}, contract.Errf(contract.CodeInvalid, "json_field value must be a JSON scalar literal")
	}
	field := c.readField(root, r.Path, r.Field)
	if field.state != ApCurrent || field.valueDigest != ingest.Digest([]byte(value)) {
		return preparedRef{}, contract.Errf(contract.CodeInvalid, "JSON field does not match the declared value")
	}
	if r.SHA256 != "" && r.SHA256 != field.digest {
		return preparedRef{}, contract.Errf(contract.CodeInvalid, "JSON file digest differs from the observed file")
	}
	r.SHA256, r.Value = field.digest, value
	return preparedRef{role: role, kind: r.Kind, detail: r, ref: mustJSON([]string{r.Path, r.Field}), root: "file:" + r.Path + "@" + field.digest, state: "ok"}, nil
}

func (e *evaluator) fieldDependency(v *verdict, root string, r contract.Ref) {
	key := fieldKey{root, r.Path, r.Field}
	field, ok := e.fields[key]
	if !ok {
		field = e.c.readField(root, r.Path, r.Field)
		e.fields[key] = field
	}
	switch {
	case field.state != ApCurrent:
		v.mark(field.state, field.reason+": "+r.Path+" / "+r.Field)
	case field.valueDigest != ingest.Digest([]byte(r.Value)):
		v.mark(ApStale, "JSON field changed: "+r.Path+" / "+r.Field)
	default:
		v.reasons = append(v.reasons, "JSON field matched: "+r.Path+" / "+r.Field)
	}
}
