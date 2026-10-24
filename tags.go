package goodm

import (
	"strconv"
	"strings"
)

// ParseGoodmTag parses a `goodm:"..."` struct tag value into FieldSchema attributes.
// Supported tags: unique, index[=kind], sparse, ttl=duration|seconds, required,
// immutable, default=val, enum=a|b|c, min=N, max=N, ref=collection.
//
// index accepts an optional kind: index=-1 (descending), index=hashed,
// index=text, index=2dsphere. A bare index is ascending.
func ParseGoodmTag(tag string) FieldSchema {
	var fs FieldSchema
	if tag == "" {
		return fs
	}

	parts := strings.Split(tag, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if k, v, ok := strings.Cut(part, "="); ok {
			parseTagKeyValue(&fs, k, v)
		} else {
			parseTagFlag(&fs, part)
		}
	}

	return fs
}

// parseTagKeyValue applies a key=value tag directive to a FieldSchema.
func parseTagKeyValue(fs *FieldSchema, key, value string) {
	switch key {
	case "default":
		fs.Default = value
	case "enum":
		fs.Enum = strings.Split(value, "|")
	case "min":
		if n, err := strconv.ParseFloat(value, 64); err == nil {
			fs.Min = &n
		}
	case "max":
		if n, err := strconv.ParseFloat(value, 64); err == nil {
			fs.Max = &n
		}
	case "ref":
		fs.Ref = value
	case "index":
		fs.Index = true
		fs.IndexKind = value
	case "ttl":
		if ttl, err := parseTTL(value); err == nil {
			fs.TTL = ttl
		}
	}
}

// parseTagFlag applies a boolean flag tag directive to a FieldSchema.
func parseTagFlag(fs *FieldSchema, flag string) {
	switch flag {
	case "unique":
		fs.Unique = true
	case "index":
		fs.Index = true
	case "required":
		fs.Required = true
	case "immutable":
		fs.Immutable = true
	case "sparse":
		fs.Sparse = true
	}
}

// ParseBSONTag extracts the BSON field name from a `bson:"..."` struct tag.
// Returns the field name and whether the field should be omitted when empty.
func ParseBSONTag(tag string) (name string, omitempty bool) {
	if tag == "" {
		return "", false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, p := range parts[1:] {
		if strings.TrimSpace(p) == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty
}

// bsonTagInline reports whether a `bson:"..."` tag carries the inline option.
func bsonTagInline(tag string) bool {
	parts := strings.Split(tag, ",")
	for _, p := range parts[1:] {
		if strings.TrimSpace(p) == "inline" {
			return true
		}
	}
	return false
}
