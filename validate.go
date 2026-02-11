package goodm

import (
	"fmt"
	"reflect"

	"github.com/dwoolworth/goodm/internal"
)

// Validate checks a model instance against its schema.
// Returns a slice of ValidationError for any fields that fail validation.
func Validate(model interface{}, schema *Schema) []ValidationError {
	var errs []ValidationError

	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	t := v.Type()

	// Build a map from Go field name to reflect.Value for fast lookup
	fieldMap := make(map[string]reflect.Value)
	structFields := internal.StructFields(t)
	for _, sf := range structFields {
		fieldMap[sf.Name] = v.FieldByName(sf.Name)
	}

	for _, fs := range schema.Fields {
		fv, ok := fieldMap[fs.Name]
		if !ok {
			continue
		}

		// Required: field must be non-zero
		if fs.Required && fv.IsZero() {
			errs = append(errs, ValidationError{
				Field:   fs.BSONName,
				Message: "field is required",
			})
		}

		// Enum: value must be in the allowed set
		if len(fs.Enum) > 0 && !fv.IsZero() {
			strVal := fmt.Sprintf("%v", fv.Interface())
			found := false
			for _, allowed := range fs.Enum {
				if strVal == allowed {
					found = true
					break
				}
			}
			if !found {
				errs = append(errs, ValidationError{
					Field:   fs.BSONName,
					Message: fmt.Sprintf("value %q is not in enum %v", strVal, fs.Enum),
				})
			}
		}

		// Min/Max: numeric boundaries
		if fs.Min != nil && !fv.IsZero() {
			if intVal, ok := toInt(fv); ok {
				if intVal < *fs.Min {
					errs = append(errs, ValidationError{
						Field:   fs.BSONName,
						Message: fmt.Sprintf("value %d is less than minimum %d", intVal, *fs.Min),
					})
				}
			}
		}

		if fs.Max != nil && !fv.IsZero() {
			if intVal, ok := toInt(fv); ok {
				if intVal > *fs.Max {
					errs = append(errs, ValidationError{
						Field:   fs.BSONName,
						Message: fmt.Sprintf("value %d exceeds maximum %d", intVal, *fs.Max),
					})
				}
			}
		}
	}

	return errs
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
