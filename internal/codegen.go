package internal

import (
	"strings"
	"unicode"
)

// ToExportedName converts a snake_case or lowercase name to an exported Go name.
// Example: "user_name" → "UserName", "email" → "Email"
func ToExportedName(name string) string {
	parts := strings.Split(name, "_")
	var result strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		// Handle common acronyms
		upper := strings.ToUpper(p)
		if isAcronym(upper) {
			result.WriteString(upper)
		} else {
			runes := []rune(p)
			runes[0] = unicode.ToUpper(runes[0])
			result.WriteString(string(runes))
		}
	}
	return result.String()
}

// SanitizeStructName converts a collection name to a singular exported Go struct name.
// Example: "blog_posts" → "BlogPost", "users" → "User"
func SanitizeStructName(collectionName string) string {
	singular := singularize(collectionName)
	return ToExportedName(singular)
}

// FormatGoodmTag builds the `goodm:"..."` tag value from field attributes.
func FormatGoodmTag(unique, index, required bool) string {
	var parts []string
	if unique {
		parts = append(parts, "unique")
	}
	if index {
		parts = append(parts, "index")
	}
	if required {
		parts = append(parts, "required")
	}
	return strings.Join(parts, ",")
}

// isAcronym returns true for common acronyms that should be all-caps.
func isAcronym(s string) bool {
	switch s {
	case "ID", "URL", "URI", "API", "HTTP", "HTTPS", "JSON", "XML", "SQL", "HTML", "CSS", "IP", "TCP", "UDP", "DNS":
		return true
	}
	return false
}

// singularize performs a simple singularization by stripping trailing "s".
// Handles common cases: "posts" → "post", "statuses" → "status", "iries" → not touched
func singularize(s string) string {
	if len(s) < 3 {
		return s
	}
	if strings.HasSuffix(s, "ies") {
		// "categories" → "category"
		return s[:len(s)-3] + "y"
	}
	if strings.HasSuffix(s, "ses") || strings.HasSuffix(s, "xes") || strings.HasSuffix(s, "zes") || strings.HasSuffix(s, "ches") || strings.HasSuffix(s, "shes") {
		// "statuses" → "status", "boxes" → "box"
		if strings.HasSuffix(s, "ches") || strings.HasSuffix(s, "shes") {
			return s[:len(s)-2]
		}
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss") {
		return s[:len(s)-1]
	}
	return s
}
