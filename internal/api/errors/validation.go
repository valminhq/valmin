package errors

import "strconv"

// FieldCode names a per-field validation failure, from a closed registry the frontend
// renders per-field messages from (11 §2.4).
type FieldCode struct{ name string }

// String returns the wire form.
func (f FieldCode) String() string { return f.name }

// MarshalJSON renders the field code as its wire name.
func (f FieldCode) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(f.name)), nil }

var (
	FieldRequired         = FieldCode{"required"}
	FieldTooShort         = FieldCode{"too_short"}
	FieldSameAsServerName = FieldCode{"same_as_server_name"}
	FieldPasswordInName   = FieldCode{"password_in_name"}
	FieldInvalid          = FieldCode{"invalid"}

	// Config edits: a `.cfg` declares each setting's type and constraints, so a rejection can
	// name the rule the value broke (03 §9).
	FieldUnknownSetting = FieldCode{"unknown_setting"}
	FieldWrongType      = FieldCode{"wrong_type"}
	FieldOutOfRange     = FieldCode{"out_of_range"}
	FieldNotAnOption    = FieldCode{"not_an_option"}
)

// FieldError is one entry of details.fields. Field is a dotted path into the request body,
// so a nested value reads as modifiers.combat.
type FieldError struct {
	Field   string    `json:"field"`
	Code    FieldCode `json:"code"`
	Message string    `json:"message"`
}

// Validation collects every problem in one request, so a response reports them all at once
// (11 §2.4).
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
