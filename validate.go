package goodm

import (
	"fmt"
	"reflect"
	"strconv"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Validate checks a model instance against its schema.
// Returns a slice of ValidationError for any fields that fail validation.
func Validate(model interface{}, schema *Schema) []ValidationError {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}

	return validateFields(v, schema.Fields, "")
}

// validateFields recursively validates struct fields, producing dotted error paths
// for nested subdocuments (e.g. "address.street", "items[0].name").
func validateFields(v reflect.Value, fields []FieldSchema, pathPrefix string) []ValidationError {
	var errs []ValidationError

	for i := range fields {
		fs := &fields[i]
		fieldPath := fs.BSONName
		if pathPrefix != "" {
			fieldPath = pathPrefix + "." + fs.BSONName
		}
		fv := schemaField(v, fs)
		if !fv.IsValid() {
			// The field sits behind a nil inline pointer: it is absent from
			// the document, which only a required rule can object to.
			if fs.Required {
				errs = append(errs, ValidationError{Field: fieldPath, Message: "field is required"})
			}
			continue
		}
		errs = append(errs, validateOneField(fv, fs, fieldPath)...)
	}

	return errs
}

// validateOneField applies a field's rules to a value of the field's Go type.
func validateOneField(fv reflect.Value, fs *FieldSchema, fieldPath string) []ValidationError {
	var errs []ValidationError

	// Required: field must be non-zero
	if fs.Required && isZeroField(fv) {
		errs = append(errs, ValidationError{
			Field:   fieldPath,
			Message: "field is required",
		})
	}

	// Enum: value must be in the allowed set. A zero value is treated as
	// unset here; combine with required to reject it.
	if len(fs.Enum) > 0 && !fv.IsZero() {
		if err := validateEnum(fv, fs.Enum, fieldPath); err != nil {
			errs = append(errs, *err)
		}
	}

	// Min/Max apply to every present value, including the zero value: a
	// field tagged min=1 must reject 0. Only a nil pointer counts as absent.
	if (fs.Min != nil || fs.Max != nil) && !isNilPointer(fv) {
		if fs.Min != nil {
			if err := validateMin(fv, *fs.Min, fieldPath); err != nil {
				errs = append(errs, *err)
			}
		}
		if fs.Max != nil {
			if err := validateMax(fv, *fs.Max, fieldPath); err != nil {
				errs = append(errs, *err)
			}
		}
	}

	// Recurse into subdocuments
	return append(errs, validateSubFields(fv, *fs, fieldPath)...)
}

var zeroerType = reflect.TypeOf((*bson.Zeroer)(nil)).Elem()

// isZeroField reports whether a field value is zero as the driver sees it:
// the Go zero value, or a value whose static type has an IsZero method that
// says so (such as a non-nil *time.Time pointing at the zero time), which
// omitempty omits. As in the driver, the method is looked up on the field's
// declared type, so an interface field is judged by whether it is nil.
func isZeroField(v reflect.Value) bool {
	if v.IsZero() {
		return true
	}
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return true
	}
	if v.Type().Implements(zeroerType) && v.CanInterface() {
		return v.Interface().(bson.Zeroer).IsZero()
	}
	return false
}

// validateEnum checks that fv is one of the allowed enum values.
// For a slice or array field every element must be in the set.
func validateEnum(fv reflect.Value, enum []string, fieldPath string) *ValidationError {
	if isNilPointer(fv) {
		return nil // a nil element is absent, like an unset scalar
	}
	fv = derefValue(fv)
	if fv.Kind() == reflect.Slice || fv.Kind() == reflect.Array {
		for i := 0; i < fv.Len(); i++ {
			if err := validateEnum(fv.Index(i), enum, fmt.Sprintf("%s[%d]", fieldPath, i)); err != nil {
				return err
			}
		}
		return nil
	}
	strVal := stringValue(fv)
	for _, allowed := range enum {
		if strVal == allowed {
			return nil
		}
	}
	return &ValidationError{
		Field:   fieldPath,
		Message: fmt.Sprintf("value %q is not in enum %v", strVal, enum),
	}
}

// isNilPointer reports whether v is a nil pointer (an absent optional value).
func isNilPointer(v reflect.Value) bool {
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// validateMin checks that fv meets the minimum length (strings, slices, maps)
// or value (numerics). Non-numeric, non-measurable kinds are skipped.
func validateMin(fv reflect.Value, min float64, fieldPath string) *ValidationError {
	fv = derefValue(fv)
	if n, ok := lengthOf(fv); ok {
		if float64(n) < min {
			return &ValidationError{
				Field:   fieldPath,
				Message: fmt.Sprintf("length %d is less than minimum %s", n, formatBound(min)),
			}
		}
		return nil
	}
	if num, ok := toFloat(fv); ok && num < min {
		return &ValidationError{
			Field:   fieldPath,
			Message: fmt.Sprintf("value %s is less than minimum %s", formatBound(num), formatBound(min)),
		}
	}
	return nil
}

// validateMax checks that fv does not exceed the maximum length (strings,
// slices, maps) or value (numerics).
func validateMax(fv reflect.Value, max float64, fieldPath string) *ValidationError {
	fv = derefValue(fv)
	if n, ok := lengthOf(fv); ok {
		if float64(n) > max {
			return &ValidationError{
				Field:   fieldPath,
				Message: fmt.Sprintf("length %d exceeds maximum %s", n, formatBound(max)),
			}
		}
		return nil
	}
	if num, ok := toFloat(fv); ok && num > max {
		return &ValidationError{
			Field:   fieldPath,
			Message: fmt.Sprintf("value %s exceeds maximum %s", formatBound(num), formatBound(max)),
		}
	}
	return nil
}

// derefValue follows a non-nil pointer to its element.
func derefValue(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		return v.Elem()
	}
	return v
}

// lengthOf returns the length of strings (in characters, as the server's
// minLength/maxLength count them), slices, arrays, and maps.
func lengthOf(v reflect.Value) (int, bool) {
	switch v.Kind() {
	case reflect.String:
		return utf8.RuneCountInString(v.String()), true
	case reflect.Slice, reflect.Array, reflect.Map:
		return v.Len(), true
	}
	return 0, false
}

// formatBound renders a bound without a trailing ".0" for whole numbers.
func formatBound(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// validateSubFields dispatches subdocument validation for struct and slice fields.
func validateSubFields(fv reflect.Value, fs FieldSchema, fieldPath string) []ValidationError {
	if len(fs.SubFields) == 0 {
		return nil
	}
	if fs.IsSlice {
		return validateSliceElements(fv, fs.SubFields, fieldPath)
	}
	// Single struct or *struct
	innerVal := fv
	if innerVal.Kind() == reflect.Pointer {
		if innerVal.IsNil() {
			return nil
		}
		innerVal = innerVal.Elem()
	}
	return validateFields(innerVal, fs.SubFields, fieldPath)
}

// validateSliceElements validates each element in a slice of structs.
func validateSliceElements(fv reflect.Value, subFields []FieldSchema, fieldPath string) []ValidationError {
	var errs []ValidationError
	for i := 0; i < fv.Len(); i++ {
		elemVal := fv.Index(i)
		if elemVal.Kind() == reflect.Pointer {
			if elemVal.IsNil() {
				continue
			}
			elemVal = elemVal.Elem()
		}
		elemPath := fmt.Sprintf("%s[%d]", fieldPath, i)
		errs = append(errs, validateFields(elemVal, subFields, elemPath)...)
	}
	return errs
}

// stringValue extracts a string representation of a value for enum comparison.
// For string kinds, returns the string directly. For other types, uses fmt.Sprintf.
func stringValue(v reflect.Value) string {
	if v.Kind() == reflect.String {
		return v.String()
	}
	return fmt.Sprintf("%v", v.Interface())
}

// toFloat extracts a numeric value from a reflect.Value as float64.
func toFloat(v reflect.Value) (float64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(v.Uint()), true
	case reflect.Float32, reflect.Float64:
		return v.Float(), true
	default:
		return 0, false
	}
}
