package goodm

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"
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

	// ErrDuplicateKey is matched by errors.Is for any write rejected by a unique
	// index. The returned error is a *DuplicateKeyError carrying the index and
	// field names.
	ErrDuplicateKey = errors.New("goodm: duplicate key")
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

// DuplicateKeyError is returned when a write violates a unique index.
// errors.Is(err, ErrDuplicateKey) matches it.
type DuplicateKeyError struct {
	Collection string
	Index      string   // name of the violated index, when the server reports it
	Fields     []string // bson field names of the violated index, when known
	Err        error    // the underlying driver error
}

func (e *DuplicateKeyError) Error() string {
	switch {
	case len(e.Fields) > 0:
		return fmt.Sprintf("goodm: duplicate key on %s (%s)", e.Collection, strings.Join(e.Fields, ", "))
	case e.Index != "":
		return fmt.Sprintf("goodm: duplicate key on %s (index %s)", e.Collection, e.Index)
	default:
		return fmt.Sprintf("goodm: duplicate key on %s", e.Collection)
	}
}

func (e *DuplicateKeyError) Unwrap() error { return e.Err }

// Is reports true for ErrDuplicateKey so callers can use errors.Is.
func (e *DuplicateKeyError) Is(target error) bool { return target == ErrDuplicateKey }

// BulkWriteError describes the failure of one operation in a bulk write.
type BulkWriteError struct {
	Index   int    // position of the operation (or model) in the batch
	Code    int    // server error code, 0 for ODM-level failures
	Message string // server message, or the ODM error text
	Err     error  // typed error: *DuplicateKeyError, ErrNotFound, ErrVersionConflict, ValidationErrors
}

func (e BulkWriteError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("item %d: %v", e.Index, e.Err)
	}
	return fmt.Sprintf("item %d: %s", e.Index, e.Message)
}

func (e BulkWriteError) Unwrap() error { return e.Err }

// BulkError is returned by CreateMany, SaveMany, and Bulk.Execute when some
// operations failed. Result reports what did succeed. errors.Is matches the
// typed error of any failed item (e.g. ErrDuplicateKey, ErrVersionConflict).
type BulkError struct {
	Collection string
	Errors     []BulkWriteError
	Result     *BulkResult
}

func (e *BulkError) Error() string {
	msgs := make([]string, 0, len(e.Errors))
	for i, we := range e.Errors {
		if i == 3 && len(e.Errors) > 4 {
			msgs = append(msgs, fmt.Sprintf("... and %d more", len(e.Errors)-3))
			break
		}
		msgs = append(msgs, we.Error())
	}
	return fmt.Sprintf("goodm: bulk write on %s: %d of the operations failed: %s", e.Collection, len(e.Errors), strings.Join(msgs, "; "))
}

// Unwrap exposes each item's error to errors.Is and errors.As.
func (e *BulkError) Unwrap() []error {
	out := make([]error, 0, len(e.Errors))
	for _, we := range e.Errors {
		if we.Err != nil {
			out = append(out, we.Err)
		}
	}
	return out
}

// Failed reports whether the item at index i failed.
func (e *BulkError) Failed(i int) bool {
	for _, we := range e.Errors {
		if we.Index == i {
			return true
		}
	}
	return false
}

var (
	dupIndexRe  = regexp.MustCompile(`index: (\S+) dup key`)
	dupFieldsRe = regexp.MustCompile(`dup key: \{(.*)\}`)
	dupKeyRe    = regexp.MustCompile(`([A-Za-z_$][\w.$]*):`)
)

// translateWriteError converts driver errors into goodm's typed errors. A
// unique index violation becomes a *DuplicateKeyError; anything else is
// wrapped with the given context.
func translateWriteError(err error, schema *Schema, context string) error {
	if err == nil {
		return nil
	}
	if mongo.IsDuplicateKeyError(err) {
		return newDuplicateKeyError(err, schema, err.Error())
	}
	return fmt.Errorf("goodm: %s: %w", context, err)
}

// newDuplicateKeyError builds a DuplicateKeyError from a server message such as
// `E11000 duplicate key error collection: db.users index: email_1 dup key: { email: "a" }`.
func newDuplicateKeyError(err error, schema *Schema, msg string) *DuplicateKeyError {
	dke := &DuplicateKeyError{Err: err}
	if schema != nil {
		dke.Collection = schema.Collection
	}
	if m := dupIndexRe.FindStringSubmatch(msg); m != nil {
		dke.Index = m[1]
	}
	if schema != nil && dke.Index != "" {
		for _, def := range schema.IndexDefinitions() {
			if def.Name == dke.Index {
				for _, k := range def.Keys {
					dke.Fields = append(dke.Fields, k.Key)
				}
				break
			}
		}
	}
	if len(dke.Fields) == 0 {
		if m := dupFieldsRe.FindStringSubmatch(msg); m != nil {
			for _, k := range dupKeyRe.FindAllStringSubmatch(m[1], -1) {
				dke.Fields = append(dke.Fields, k[1])
			}
		}
	}
	return dke
}

// isDuplicateKeyCode reports whether a server write error code is a unique
// index violation.
func isDuplicateKeyCode(code int) bool {
	return code == 11000 || code == 11001 || code == 12582
}

// bulkWriteErrors converts a driver BulkWriteException into per-item errors,
// translating duplicate key violations. Returns nil if err is not a bulk
// write exception.
func bulkWriteErrors(err error, schema *Schema) ([]BulkWriteError, bool) {
	var bwe mongo.BulkWriteException
	if !errors.As(err, &bwe) {
		return nil, false
	}
	out := make([]BulkWriteError, 0, len(bwe.WriteErrors))
	for _, we := range bwe.WriteErrors {
		item := BulkWriteError{Index: we.Index, Code: we.Code, Message: we.Message}
		if isDuplicateKeyCode(we.Code) {
			item.Err = newDuplicateKeyError(we, schema, we.Message)
		} else {
			item.Err = fmt.Errorf("goodm: write error %d: %s", we.Code, we.Message)
		}
		out = append(out, item)
	}
	return out, true
}
