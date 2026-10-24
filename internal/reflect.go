package internal

import (
	"reflect"
	"strings"
)

// StructFields returns all exported fields of a struct, flattening inline
// fields the way the BSON driver's default registry does: any exported struct
// or pointer-to-struct field tagged bson:",inline", embedded or named, is
// flattened and its fields are returned with Index set to their full path
// from the root struct; an inline map is skipped (it declares no fields). Any
// other exported embedded struct is returned as a field of its own, which the
// driver stores as a subdocument named by its tag or its lowercased type
// name. Unexported fields, embedded or not, are skipped as the driver skips
// them. It accepts a reflect.Type that must be a struct type.
func StructFields(t reflect.Type) []reflect.StructField {
	return structFields(t, nil)
}

func structFields(t reflect.Type, prefix []int) []reflect.StructField {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	var fields []reflect.StructField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		path := append(append([]int{}, prefix...), i)
		if IsInlineTag(f.Tag.Get("bson")) {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.Struct:
				fields = append(fields, structFields(ft, path)...)
				continue
			case reflect.Map:
				// An inline map holds arbitrary extra keys; it declares no
				// schema fields of its own.
				continue
			}
		}
		f.Index = path
		fields = append(fields, f)
	}
	return fields
}

// IsInlineTag reports whether a `bson:"..."` tag carries the inline option.
func IsInlineTag(tag string) bool {
	parts := strings.Split(tag, ",")
	for _, p := range parts[1:] {
		if strings.TrimSpace(p) == "inline" {
			return true
		}
	}
	return false
}

// HasOmitEmpty reports whether a `bson:"..."` tag carries the omitempty option.
func HasOmitEmpty(tag string) bool {
	parts := strings.Split(tag, ",")
	for _, p := range parts[1:] {
		if strings.TrimSpace(p) == "omitempty" {
			return true
		}
	}
	return false
}

// TypeName returns a human-readable type name for a reflect.Type.
func TypeName(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		return "*" + TypeName(t.Elem())
	}
	if t.Kind() == reflect.Slice {
		return "[]" + TypeName(t.Elem())
	}
	// Use the short name for types in known packages
	name := t.String()
	// Strip package path for common types
	if strings.Contains(name, ".") {
		parts := strings.Split(name, ".")
		pkg := parts[len(parts)-2]
		typeName := parts[len(parts)-1]
		// Keep just the package short name and type
		lastSlash := strings.LastIndex(pkg, "/")
		if lastSlash >= 0 {
			pkg = pkg[lastSlash+1:]
		}
		return pkg + "." + typeName
	}
	return name
}

// IsInlineStruct reports whether the driver flattens f: an exported struct or
// pointer-to-struct field tagged bson:",inline".
func IsInlineStruct(f reflect.StructField) bool {
	if !f.IsExported() || !IsInlineTag(f.Tag.Get("bson")) {
		return false
	}
	ft := f.Type
	if ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	return ft.Kind() == reflect.Struct
}
