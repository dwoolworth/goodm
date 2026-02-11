package goodm

import (
	"strconv"
	"strings"
)

// ParseGoodmTag parses a `goodm:"..."` struct tag value into FieldSchema attributes.
// Supported tags: unique, index, required, immutable, default=val, enum=a|b|c,
// min=N, max=N, ref=collection
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
			switch k {
			case "default":
				fs.Default = v
			case "enum":
				fs.Enum = strings.Split(v, "|")
			case "min":
				if n, err := strconv.Atoi(v); err == nil {
					fs.Min = &n
				}
			case "max":
				if n, err := strconv.Atoi(v); err == nil {
					fs.Max = &n
				}
			case "ref":
				fs.Ref = v
			}
		} else {
			switch part {
			case "unique":
				fs.Unique = true
			case "index":
				fs.Index = true
			case "required":
				fs.Required = true
			case "immutable":
				fs.Immutable = true
			}
		}
	}

	return fs
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
