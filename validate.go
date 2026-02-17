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
			strVal := stringValue(fv)
			found := false
			for _, allowed := range fs.Enum {
				if strVal == allowed {
					found = true
					break
				}
			}
			if !found {
				errs = append(errs, ValidationError{
					Field:   fieldPath,
					Message: fmt.Sprintf("value %q is not in enum %v", strVal, fs.Enum),
				})
			}
		}

		// Min/Max: numeric or string length boundaries
		if fs.Min != nil && !fv.IsZero() {
			if fv.Kind() == reflect.String {
				if fv.Len() < *fs.Min {
					errs = append(errs, ValidationError{
						Field:   fieldPath,
						Message: fmt.Sprintf("length %d is less than minimum %d", fv.Len(), *fs.Min),
					})
				}
			} else if intVal, ok := toInt(fv); ok {
				if intVal < *fs.Min {
					errs = append(errs, ValidationError{
						Field:   fieldPath,
						Message: fmt.Sprintf("value %d is less than minimum %d", intVal, *fs.Min),
					})
				}
			}
		}

		if fs.Max != nil && !fv.IsZero() {
			if fv.Kind() == reflect.String {
				if fv.Len() > *fs.Max {
					errs = append(errs, ValidationError{
						Field:   fieldPath,
						Message: fmt.Sprintf("length %d exceeds maximum %d", fv.Len(), *fs.Max),
					})
				}
			} else if intVal, ok := toInt(fv); ok {
				if intVal > *fs.Max {
					errs = append(errs, ValidationError{
						Field:   fieldPath,
						Message: fmt.Sprintf("value %d exceeds maximum %d", intVal, *fs.Max),
					})
				}
			}
		}

		// Recurse into subdocuments
		if len(fs.SubFields) > 0 {
			if fs.IsSlice {
				// Slice of structs: validate each element
				for i := 0; i < fv.Len(); i++ {
					elemVal := fv.Index(i)
					if elemVal.Kind() == reflect.Ptr {
						if elemVal.IsNil() {
							continue
						}
						elemVal = elemVal.Elem()
					}
					elemPath := fmt.Sprintf("%s[%d]", fieldPath, i)
					errs = append(errs, validateFields(elemVal, fs.SubFields, elemPath)...)
				}
			} else {
				// Single struct or *struct
				innerVal := fv
				if innerVal.Kind() == reflect.Ptr {
					if innerVal.IsNil() {
						continue // skip nil pointer subdocs
					}
					innerVal = innerVal.Elem()
				}
				errs = append(errs, validateFields(innerVal, fs.SubFields, fieldPath)...)
			}
		}
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
