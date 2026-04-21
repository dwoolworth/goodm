package goodm

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNotFound is returned when a document is not found.
	ErrNotFound = errors.New("goodm: document not found")

	// ErrNoDatabase is returned when no database connection is available.
	ErrNoDatabase = errors.New("goodm: no database connection (call Connect first)")

	// ErrVersionConflict is returned when an update fails due to a version mismatch
	// (optimistic concurrency control). This means another process modified the
	// document between your read and write.
	ErrVersionConflict = errors.New("goodm: version conflict (document was modified by another process)")
)

// DriftError indicates a field exists in the database but not in the schema.
type DriftError struct {
	Collection string
	Field      string
	Message    string
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("drift in %s.%s: %s", e.Collection, e.Field, e.Message)
}

// EnforcementError indicates a schema enforcement failure (e.g., missing index).
type EnforcementError struct {
	Collection string
	Message    string
}

func (e *EnforcementError) Error() string {
	return fmt.Sprintf("enforcement error on %s: %s", e.Collection, e.Message)
}

// ValidationError indicates a field failed validation.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("validation error on %s: %s", e.Field, e.Message)
}

// MergeConflictError is returned when a retry-with-merge detects that both the
// caller and another writer modified the same fields. The conflicting field names
// (bson names) are listed so the caller can decide how to resolve.
type MergeConflictError struct {
	Fields []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("goodm: merge conflict on fields: %s", strings.Join(e.Fields, ", "))
}

// ValidationErrors is a slice of ValidationError that implements error.
type ValidationErrors []ValidationError

func (ve ValidationErrors) Error() string {
	msgs := make([]string, len(ve))
	for i, e := range ve {
		msgs[i] = e.Error()
	}
	return strings.Join(msgs, "; ")
}
