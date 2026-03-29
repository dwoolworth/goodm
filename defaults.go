package goodm

import (
	"fmt"
	"reflect"
	"strconv"
)

// applyDefaults sets zero-valued fields to their schema defaults.
// Only called during Create — defaults are a creation-time concern.
func applyDefaults(model interface{}, schema *Schema) error {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	return applyFieldDefaults(v, schema.Fields)
}

// applyFieldDefaults recursively applies default values to zero-valued fields,
// including fields inside subdocuments and slice elements.
func applyFieldDefaults(v reflect.Value, fields []FieldSchema) error {
	for _, field := range fields {
		fv := v.FieldByName(field.Name)
		if !fv.IsValid() || !fv.CanSet() {
			continue
		}

		// Apply default to primitive fields
		if field.Default != "" && fv.IsZero() {
			if err := setFieldFromString(fv, field.Default); err != nil {
				return fmt.Errorf("goodm: cannot apply default %q to field %s: %w", field.Default, field.Name, err)
			}
		}

		// Recurse into subdocuments
		if len(field.SubFields) > 0 {
			if err := applySubFieldDefaults(fv, field); err != nil {
				return err
			}
		}
	}

	return nil
}

// applySubFieldDefaults applies defaults to nested struct or slice-of-struct fields.
func applySubFieldDefaults(fv reflect.Value, field FieldSchema) error {
	if field.IsSlice {
		for i := 0; i < fv.Len(); i++ {
			elemVal := fv.Index(i)
			if elemVal.Kind() == reflect.Ptr {
				if elemVal.IsNil() {
					continue
				}
				elemVal = elemVal.Elem()
			}
			if err := applyFieldDefaults(elemVal, field.SubFields); err != nil {
				return err
			}
		}
		return nil
	}
	// Single struct or *struct
	innerVal := fv
	if innerVal.Kind() == reflect.Ptr {
		if innerVal.IsNil() {
			return nil
		}
		innerVal = innerVal.Elem()
	}
	return applyFieldDefaults(innerVal, field.SubFields)
}

// setFieldFromString parses a string value and sets it on a reflect.Value.
func setFieldFromString(fv reflect.Value, s string) error {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)

	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		fv.SetBool(b)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return err
		}
		fv.SetUint(n)

	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		fv.SetFloat(f)

	default:
		return fmt.Errorf("unsupported type %s", fv.Type())
	}

	return nil
}
