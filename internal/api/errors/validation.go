package errors

import "strconv"

// FieldCode names a per-field validation failure. It is its own closed registry for the
// same reason Code is: the frontend renders per-field messages from these, so a typo would
// silently degrade to an unstyled blob (11 §2.4).
type FieldCode struct{ name string }

// String returns the wire form.
func (f FieldCode) String() string { return f.name }

// MarshalJSON renders the field code as its wire name.
func (f FieldCode) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(f.name)), nil }

var (
	FieldRequired         = FieldCode{"required"}
	FieldTooShort         = FieldCode{"too_short"}
	FieldSameAsServerName = FieldCode{"same_as_server_name"}
	FieldInvalid          = FieldCode{"invalid"}
)

// FieldError is one entry of details.fields. Field is a dotted path into the request body,
// so a nested value reads as modifiers.combat.
type FieldError struct {
	Field   string    `json:"field"`
	Code    FieldCode `json:"code"`
	Message string    `json:"message"`
}

// Validation collects every problem in one request. One request, one response, all the
// problems — not one error at a time (11 §2.4).
type Validation struct {
	fields []FieldError
}

// Add records a failure against a dotted field path.
func (v *Validation) Add(field string, code FieldCode, message string) {
	v.fields = append(v.fields, FieldError{Field: field, Code: code, Message: message})
}

// Err returns nil when nothing failed, and otherwise the 422 of 11 §2.4.
func (v *Validation) Err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return New(ValidationFailed).With("fields", v.fields)
}
