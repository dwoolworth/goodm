package goodm

import (
	"fmt"
	"reflect"
)

// Validate checks a model instance against its schema.
// Returns a slice of ValidationError for any fields that fail validation.
func Validate(model interface{}, schema *Schema) []ValidationError {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	return validateFields(v, schema.Fields, "")
}

// validateFields recursively validates struct fields, producing dotted error paths
// for nested subdocuments (e.g. "address.street", "items[0].name").
func validateFields(v reflect.Value, fields []FieldSchema, pathPrefix string) []ValidationError {
	var errs []ValidationError

	for _, fs := range fields {
		fv := v.FieldByName(fs.Name)
		if !fv.IsValid() {
			continue
		}

		fieldPath := fs.BSONName
		if pathPrefix != "" {
			fieldPath = pathPrefix + "." + fs.BSONName
		}

		// Required: field must be non-zero
		if fs.Required && fv.IsZero() {
			errs = append(errs, ValidationError{
				Field:   fieldPath,
				Message: "field is required",
			})
		}

		// Enum: value must be in the allowed set
		if len(fs.Enum) > 0 && !fv.IsZero() {
			if err := validateEnum(fv, fs.Enum, fieldPath); err != nil {
				errs = append(errs, *err)
			}
		}

		// Min: numeric or string length lower bound
		if fs.Min != nil && !fv.IsZero() {
			if err := validateMin(fv, *fs.Min, fieldPath); err != nil {
				errs = append(errs, *err)
			}
		}

		// Max: numeric or string length upper bound
		if fs.Max != nil && !fv.IsZero() {
			if err := validateMax(fv, *fs.Max, fieldPath); err != nil {
				errs = append(errs, *err)
			}
		}

		// Recurse into subdocuments
		errs = append(errs, validateSubFields(fv, fs, fieldPath)...)
	}

	return errs
}

// validateEnum checks that fv is one of the allowed enum values.
func validateEnum(fv reflect.Value, enum []string, fieldPath string) *ValidationError {
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

// validateMin checks that fv meets the minimum length (strings) or value (numerics).
func validateMin(fv reflect.Value, min int, fieldPath string) *ValidationError {
	if fv.Kind() == reflect.String {
		if fv.Len() < min {
			return &ValidationError{
				Field:   fieldPath,
				Message: fmt.Sprintf("length %d is less than minimum %d", fv.Len(), min),
			}
		}
	} else if intVal, ok := toInt(fv); ok {
		if intVal < min {
			return &ValidationError{
				Field:   fieldPath,
				Message: fmt.Sprintf("value %d is less than minimum %d", intVal, min),
			}
		}
	}
	return nil
}

// validateMax checks that fv does not exceed the maximum length (strings) or value (numerics).
func validateMax(fv reflect.Value, max int, fieldPath string) *ValidationError {
	if fv.Kind() == reflect.String {
		if fv.Len() > max {
			return &ValidationError{
				Field:   fieldPath,
				Message: fmt.Sprintf("length %d exceeds maximum %d", fv.Len(), max),
			}
		}
	} else if intVal, ok := toInt(fv); ok {
		if intVal > max {
			return &ValidationError{
				Field:   fieldPath,
				Message: fmt.Sprintf("value %d exceeds maximum %d", intVal, max),
			}
		}
	}
	return nil
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
	if innerVal.Kind() == reflect.Ptr {
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
		if elemVal.Kind() == reflect.Ptr {
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

// toInt attempts to extract an integer value from a reflect.Value.
func toInt(v reflect.Value) (int, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(v.Uint()), true
	case reflect.Float32, reflect.Float64:
		return int(v.Float()), true
	default:
		return 0, false
	}
}
