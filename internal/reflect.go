package internal

import (
	"reflect"
	"strings"
)

// StructFields returns all exported fields of a struct, flattening embedded structs.
// It accepts a reflect.Type that must be a struct type.
func StructFields(t reflect.Type) []reflect.StructField {
	if t.Kind() == reflect.Ptr {
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
		// Flatten embedded structs
		if f.Anonymous {
			embedded := StructFields(f.Type)
			fields = append(fields, embedded...)
			continue
		}
		fields = append(fields, f)
	}
	return fields
}

// TypeName returns a human-readable type name for a reflect.Type.
func TypeName(t reflect.Type) string {
	if t.Kind() == reflect.Ptr {
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
