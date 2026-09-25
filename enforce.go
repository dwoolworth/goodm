package goodm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// DriftPolicy controls how schema drift is handled during enforcement.
type DriftPolicy int

const (
	DriftIgnore DriftPolicy = iota // skip drift detection entirely
	DriftWarn                      // detect drift, call OnDriftWarning, continue
	DriftFatal                     // detect drift, return error if any found
)

// DefaultDriftSampleSize is the number of documents sampled for drift detection.
const DefaultDriftSampleSize = 100

// IndexMismatchPolicy controls what Enforce does when an index exists under the
// expected name but its options don't match the schema (e.g. a goodm:"unique"
// field whose index is not unique, or is sparse/partial).
type IndexMismatchPolicy int

const (
	// IndexMismatchError returns an EnforcementError describing the mismatch.
	// This is the default: a unique tag whose index isn't actually unique is a
	// silent data-integrity hole, so Enforce fails loudly.
	IndexMismatchError IndexMismatchPolicy = iota
	// IndexMismatchRebuild rebuilds the mismatched index to match the schema.
	// A non-unique index that must become unique is converted in place on
	// MongoDB 6.0+ (collMod prepareUnique, then unique), so the collection is
	// never left without an index and new duplicates are rejected from the
	// moment conversion starts. If the data already contains duplicates the
	// conversion is undone, the existing index is left in place, and an
	// EnforcementError is returned. Other mismatches (different keys, sparse,
	// partial, unique -> non-unique) and servers older than 6.0 fall back to
	// drop and recreate.
	IndexMismatchRebuild
	// IndexMismatchIgnore restores the pre-0.6 behavior: an index is accepted
	// by name alone and its options are never checked.
	IndexMismatchIgnore
)

// EnforceOptions configures the behavior of Enforce.
type EnforceOptions struct {
	DriftPolicy         DriftPolicy
	DriftSampleSize     int                 // documents to sample for drift detection (default 100)
	OnDriftWarning      func(d DriftError)  // called for each drift when policy is DriftWarn
	IndexMismatchPolicy IndexMismatchPolicy // how to handle existing indexes whose options don't match the schema
}

// Enforce ensures that all registered schemas are reflected in the database.
// It creates missing indexes and optionally detects schema drift based on the
// provided options. If no options are provided, drift detection is skipped.
func Enforce(ctx context.Context, db *mongo.Database, opts ...EnforceOptions) error {
	var opt EnforceOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	schemas := GetAll()
	probe := &prepareUniqueProbe{db: db}

	for _, schema := range schemas {
		if err := enforceSchema(ctx, db, schema, opt.IndexMismatchPolicy, probe); err != nil {
			return err
		}

		if opt.DriftPolicy == DriftIgnore {
			continue
		}

		sampleSize := opt.DriftSampleSize
		if sampleSize <= 0 {
			sampleSize = DefaultDriftSampleSize
		}
		drifts := DetectDrift(ctx, db, schema, sampleSize)
		if len(drifts) == 0 {
			continue
		}

		switch opt.DriftPolicy {
		case DriftWarn:
			for _, d := range drifts {
				if opt.OnDriftWarning != nil {
					opt.OnDriftWarning(d)
				}
			}
		case DriftFatal:
			msgs := make([]string, len(drifts))
			for i, d := range drifts {
				msgs[i] = d.Error()
			}
			return &EnforcementError{
				Collection: schema.Collection,
				Message:    fmt.Sprintf("schema drift detected: %s", strings.Join(msgs, "; ")),
			}
		}
	}

	return nil
}

func enforceSchema(ctx context.Context, db *mongo.Database, schema *Schema, policy IndexMismatchPolicy, probe *prepareUniqueProbe) error {
	coll := db.Collection(schema.Collection)

	// Get existing indexes
	existing, err := ListExistingIndexes(ctx, coll)
	if err != nil {
		return &EnforcementError{
			Collection: schema.Collection,
			Message:    fmt.Sprintf("failed to list indexes: %v", err),
		}
	}

	// Single-field indexes from field tags
	for _, field := range schema.Fields {
		if !field.Unique && !field.Index {
			continue
		}
		keys := bson.D{{Key: field.BSONName, Value: 1}}
		if err := enforceIndex(ctx, coll, schema.Collection, existing, field.BSONName+"_1", keys, field.Unique, policy, probe); err != nil {
			return err
		}
	}

	// Compound indexes
	for _, ci := range schema.CompoundIndexes {
		keys := bson.D{}
		for _, f := range ci.Fields {
			keys = append(keys, bson.E{Key: f, Value: 1})
		}
		if err := enforceIndex(ctx, coll, schema.Collection, existing, compoundIndexName(ci), keys, ci.Unique, policy, probe); err != nil {
			return err
		}
	}

	return nil
}

// enforceIndex ensures a single index exists with the expected keys and options,
// creating it if missing and handling option mismatches per the policy.
func enforceIndex(ctx context.Context, coll *mongo.Collection, collection string, existing map[string]IndexSpec, name string, keys bson.D, unique bool, policy IndexMismatchPolicy, probe *prepareUniqueProbe) error {
	spec, exists := existing[name]
	if !exists {
		return createIndex(ctx, coll, collection, name, keys, unique)
	}

	mismatch := describeIndexMismatch(spec, keys, unique)
	if mismatch == "" || policy == IndexMismatchIgnore {
		return nil
	}

	if policy == IndexMismatchError {
		return &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("index %s exists but %s (use IndexMismatchRebuild to rebuild it)", name, mismatch),
		}
	}

	// IndexMismatchRebuild. A unique index can only be built over unique data.
	// Check before touching the index so a failure here is a read-only no-op.
	if unique {
		hasDups, err := hasDuplicateValues(ctx, coll, keys)
		if err != nil {
			return &EnforcementError{
				Collection: collection,
				Message:    fmt.Sprintf("failed to check for duplicates before rebuilding index %s: %v", name, err),
			}
		}
		if hasDups {
			return &EnforcementError{
				Collection: collection,
				Message:    fmt.Sprintf("cannot rebuild index %s as unique: collection contains duplicate values; existing index left in place", name),
			}
		}
	}

	if canRevertPrepareUniqueInPlace(spec, keys, unique) {
		if err := collModIndex(ctx, coll, name, "prepareUnique", false); err != nil {
			return &EnforcementError{
				Collection: collection,
				Message:    fmt.Sprintf("failed to clear prepareUnique on index %s: %v", name, err),
			}
		}
		return nil
	}

	if canConvertToUniqueInPlace(spec, keys, unique) {
		supported, err := probe.ok(ctx)
		if err != nil {
			return &EnforcementError{
				Collection: collection,
				Message:    fmt.Sprintf("cannot determine whether the server supports in-place unique conversion of index %s: %v", name, err),
			}
		}
		if supported {
			converted, err := convertIndexToUnique(ctx, coll, collection, name)
			if err != nil || converted {
				return err
			}
		}
	}

	if err := coll.Indexes().DropOne(ctx, name); err != nil {
		return &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("failed to drop mismatched index %s: %v", name, err),
		}
	}
	if err := createIndex(ctx, coll, collection, name, keys, unique); err != nil {
		return &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("index %s was dropped but recreate failed: %v", name, err),
		}
	}
	return nil
}

// canConvertToUniqueInPlace reports whether the only difference between the
// existing index and the schema is the unique flag. collMod can only add
// uniqueness; it cannot change keys, sparse, partial filters, or remove unique.
// Indexes carrying options the schema never sets (collation, TTL, hidden) are
// excluded so the conversion does not silently keep them; drop+create
// normalizes those to a plain unique index as it always has.
func canConvertToUniqueInPlace(spec IndexSpec, keys bson.D, unique bool) bool {
	return unique && !spec.Unique && !spec.Sparse && !spec.HasPartialFilter &&
		!spec.HasCollation && !spec.HasTTL && !spec.Hidden && indexKeysEqual(spec.Keys, keys)
}

// canRevertPrepareUniqueInPlace reports whether the only mismatch is a stale
// prepareUnique on an index the schema wants non-unique; clearing the flag
// fixes that without a rebuild.
func canRevertPrepareUniqueInPlace(spec IndexSpec, keys bson.D, unique bool) bool {
	return !unique && spec.PrepareUnique && !spec.Unique && indexKeysEqual(spec.Keys, keys)
}

// codeCannotConvertIndexToUnique is the server error code returned by
// collMod {unique: true} when the collection already contains duplicates.
const codeCannotConvertIndexToUnique = 359

// prepareUniqueUnsupported reports whether a collMod prepareUnique failure
// means the server does not recognize the option (as opposed to auth, state,
// or transport failures, which must not trigger a drop+create fallback).
func prepareUniqueUnsupported(err error) bool {
	var cmdErr mongo.CommandError
	if !errors.As(err, &cmdErr) {
		return false
	}
	switch cmdErr.Code {
	case 2, 9, 72, 115, 40415: // BadValue, FailedToParse, InvalidOptions, CommandNotSupported, IDLUnknownField
		return true
	}
	return false
}

// revertTimeout bounds the prepareUnique revert, which must not depend on the
// caller's context still being alive.
const revertTimeout = 30 * time.Second

// convertIndexToUnique makes an existing non-unique index unique without
// dropping it. prepareUnique must be set first: from then on the server
// rejects new duplicate inserts, so the final unique step cannot race a writer.
//
// Returns (false, nil) if the server does not recognize prepareUnique; the
// index is untouched and the caller may fall back to drop and recreate. If the
// final step reports existing duplicates, prepareUnique is reverted so the
// index is left exactly as it was. On any other failure the index is
// deliberately left prepared: that state is safe (index present, new
// duplicates rejected), it may be shared with a concurrent Enforce, and the
// next Enforce finishes the conversion because both collMod steps are
// idempotent.
func convertIndexToUnique(ctx context.Context, coll *mongo.Collection, collection, name string) (bool, error) {
	if err := collModIndex(ctx, coll, name, "prepareUnique", true); err != nil {
		if prepareUniqueUnsupported(err) {
			return false, nil
		}
		return false, &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("failed to prepare index %s for unique conversion: %v", name, err),
		}
	}

	err := collModIndex(ctx, coll, name, "unique", true)
	if err == nil {
		return true, nil
	}

	var cmdErr mongo.CommandError
	if !errors.As(err, &cmdErr) || cmdErr.Code != codeCannotConvertIndexToUnique {
		return false, &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("collMod unique failed on index %s: %v; the index is left prepareUnique (present, new duplicates rejected) and is converted by the next Enforce that succeeds", name, err),
		}
	}

	msg := fmt.Sprintf("cannot convert index %s to unique: collection contains duplicate values; existing index left in place", name)
	revertCtx, cancel := context.WithTimeout(context.Background(), revertTimeout)
	defer cancel()
	if revertErr := collModIndex(revertCtx, coll, name, "prepareUnique", false); revertErr != nil {
		msg += fmt.Sprintf(" (index is still prepareUnique, so new duplicates are rejected; revert failed: %v)", revertErr)
	}
	return false, &EnforcementError{Collection: collection, Message: msg}
}

func collModIndex(ctx context.Context, coll *mongo.Collection, name, option string, value bool) error {
	cmd := bson.D{
		{Key: "collMod", Value: coll.Name()},
		{Key: "index", Value: bson.D{
			{Key: "name", Value: name},
			{Key: option, Value: value},
		}},
	}
	return coll.Database().RunCommand(ctx, cmd).Err()
}

// prepareUniqueProbe checks server support for collMod prepareUnique at most
// once per Enforce run, and only when a conversion is actually needed.
type prepareUniqueProbe struct {
	db        *mongo.Database
	checked   bool
	supported bool
	err       error
}

func (p *prepareUniqueProbe) ok(ctx context.Context) (bool, error) {
	if !p.checked {
		p.supported, p.err = supportsPrepareUnique(ctx, p.db)
		p.checked = true
	}
	return p.supported, p.err
}

// supportsPrepareUnique reports whether the server is 6.0 or newer, the first
// release with collMod prepareUnique. A 6.0+ binary still rejects it while the
// featureCompatibilityVersion is below 6.0 (mid-upgrade), so FCV is checked
// too when the caller has permission to read it. A failed buildInfo is an
// error, not "unsupported": silently downgrading to drop+create would reopen
// the unindexed window this path exists to avoid.
func supportsPrepareUnique(ctx context.Context, db *mongo.Database) (bool, error) {
	var info struct {
		VersionArray []int32 `bson:"versionArray"`
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		return false, fmt.Errorf("buildInfo: %w", err)
	}
	if len(info.VersionArray) == 0 || info.VersionArray[0] < 6 {
		return false, nil
	}

	var fcv struct {
		FCV struct {
			Version string `bson:"version"`
		} `bson:"featureCompatibilityVersion"`
	}
	cmd := bson.D{{Key: "getParameter", Value: 1}, {Key: "featureCompatibilityVersion", Value: 1}}
	if err := db.Client().Database("admin").RunCommand(ctx, cmd).Decode(&fcv); err != nil {
		return true, nil // not permitted to read FCV; trust the binary version
	}
	major, _, _ := strings.Cut(fcv.FCV.Version, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return false, fmt.Errorf("unparseable featureCompatibilityVersion %q", fcv.FCV.Version)
	}
	return n >= 6, nil
}

func createIndex(ctx context.Context, coll *mongo.Collection, collection, name string, keys bson.D, unique bool) error {
	model := mongo.IndexModel{Keys: keys}
	if unique {
		model.Options = options.Index().SetUnique(true)
	}
	if _, err := coll.Indexes().CreateOne(ctx, model); err != nil {
		return &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("failed to create index %s: %v", name, err),
		}
	}
	return nil
}

// describeIndexMismatch returns a human-readable description of how an existing
// index differs from what the schema expects, or "" if it matches.
func describeIndexMismatch(spec IndexSpec, keys bson.D, unique bool) string {
	if !indexKeysEqual(spec.Keys, keys) {
		return "has different keys than the schema expects"
	}
	if unique && !spec.Unique {
		if spec.PrepareUnique {
			return "is prepareUnique but not yet unique: new duplicates are already rejected (schema requires unique)"
		}
		return "is not unique (schema requires unique)"
	}
	if !unique && spec.Unique {
		return "is unique (schema expects non-unique)"
	}
	if !unique && spec.PrepareUnique {
		return "is prepareUnique: new duplicates are rejected (schema expects non-unique)"
	}
	if unique && spec.Sparse {
		return "is sparse (schema requires a plain unique index)"
	}
	if unique && spec.HasPartialFilter {
		return "is partial (schema requires a plain unique index)"
	}
	return ""
}

func indexKeysEqual(a, b bson.D) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || indexDirection(a[i].Value) != indexDirection(b[i].Value) {
			return false
		}
	}
	return true
}

// indexDirection normalizes an index key direction to int. Non-numeric values
// (text/hashed indexes) normalize to 0 and thus never match a schema index.
func indexDirection(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// hasDuplicateValues reports whether any two documents would collide under a
// unique index on keys. It mirrors unique-index semantics as closely as an
// aggregation can: array fields are unwound so each element is its own entry,
// duplicate elements within one document do not count, and missing fields
// group as null. Empty arrays also group as null, so an empty array and a
// missing field are reported as a collision the server would not raise; that
// only errs toward refusing a rebuild, never toward an unsafe one.
func hasDuplicateValues(ctx context.Context, coll *mongo.Collection, keys bson.D) (bool, error) {
	pipeline := mongo.Pipeline{}
	perDoc := bson.D{{Key: "doc", Value: "$_id"}}
	perValue := bson.D{}
	for i, k := range keys {
		// Positional names: $group _id field names cannot contain dots, and
		// mapping "a.b" to "a_b" could collide with a real "a_b" key.
		field := "k" + strconv.Itoa(i)
		pipeline = append(pipeline, bson.D{{Key: "$unwind", Value: bson.D{
			{Key: "path", Value: "$" + k.Key},
			{Key: "preserveNullAndEmptyArrays", Value: true},
		}}})
		perDoc = append(perDoc, bson.E{Key: field, Value: "$" + k.Key})
		perValue = append(perValue, bson.E{Key: field, Value: "$_id." + field})
	}
	pipeline = append(pipeline,
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: perDoc}}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: perValue},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		bson.D{{Key: "$match", Value: bson.D{{Key: "n", Value: bson.D{{Key: "$gt", Value: 1}}}}}},
		bson.D{{Key: "$limit", Value: 1}},
	)
	cursor, err := coll.Aggregate(ctx, pipeline)
	if err != nil {
		return false, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	return cursor.Next(ctx), cursor.Err()
}

// DetectDrift samples documents from the collection and reports fields
// that exist in the database but not in the schema. The sampleSize parameter
// controls how many documents are sampled (use DefaultDriftSampleSize if unsure).
func DetectDrift(ctx context.Context, db *mongo.Database, schema *Schema, sampleSize int) []DriftError {
	var drifts []DriftError
	coll := db.Collection(schema.Collection)

	if sampleSize <= 0 {
		sampleSize = DefaultDriftSampleSize
	}

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetLimit(int64(sampleSize)))
	if err != nil {
		return drifts
	}
	defer func() { _ = cursor.Close(ctx) }()

	knownFields := make(map[string]bool)
	for _, f := range schema.Fields {
		knownFields[f.BSONName] = true
	}

	seen := make(map[string]bool)
	for cursor.Next(ctx) {
		var doc bson.D
		if err := cursor.Decode(&doc); err != nil {
			continue
		}
		for _, elem := range doc {
			if !knownFields[elem.Key] && !seen[elem.Key] {
				seen[elem.Key] = true
				drifts = append(drifts, DriftError{
					Collection: schema.Collection,
					Field:      elem.Key,
					Message:    "field exists in database but not in schema",
				})
			}
		}
	}

	return drifts
}

// IndexSpec describes an existing index on a collection.
type IndexSpec struct {
	Name             string
	Keys             bson.D
	Unique           bool
	PrepareUnique    bool // collMod prepareUnique is set: not unique, but new duplicates are rejected
	Sparse           bool
	HasPartialFilter bool // index has a partialFilterExpression
	HasCollation     bool
	HasTTL           bool // index has expireAfterSeconds
	Hidden           bool
}

// ListExistingIndexes returns the indexes that exist on the collection, keyed
// by index name.
func ListExistingIndexes(ctx context.Context, coll *mongo.Collection) (map[string]IndexSpec, error) {
	result := make(map[string]IndexSpec)

	cursor, err := coll.Indexes().List(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	for cursor.Next(ctx) {
		var idx struct {
			Name                    string   `bson:"name"`
			Key                     bson.D   `bson:"key"`
			Unique                  bool     `bson:"unique"`
			PrepareUnique           bool     `bson:"prepareUnique"`
			Sparse                  bool     `bson:"sparse"`
			PartialFilterExpression bson.Raw `bson:"partialFilterExpression"`
			Collation               bson.Raw `bson:"collation"`
			ExpireAfterSeconds      *int64   `bson:"expireAfterSeconds"`
			Hidden                  bool     `bson:"hidden"`
		}
		if err := cursor.Decode(&idx); err != nil {
			continue
		}
		if idx.Name == "" {
			continue
		}
		result[idx.Name] = IndexSpec{
			Name:             idx.Name,
			Keys:             idx.Key,
			Unique:           idx.Unique,
			PrepareUnique:    idx.PrepareUnique,
			Sparse:           idx.Sparse,
			HasPartialFilter: len(idx.PartialFilterExpression) > 0,
			HasCollation:     len(idx.Collation) > 0,
			HasTTL:           idx.ExpireAfterSeconds != nil,
			Hidden:           idx.Hidden,
		}
	}

	return result, nil
}

func compoundIndexName(ci CompoundIndex) string {
	parts := make([]string, 0, len(ci.Fields)*2)
	for _, f := range ci.Fields {
		parts = append(parts, f, "1")
	}
	return strings.Join(parts, "_")
}
