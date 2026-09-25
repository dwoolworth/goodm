package goodm

import (
	"context"
	"fmt"
	"strings"

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
	// IndexMismatchRebuild drops the mismatched index and recreates it to match
	// the schema. Before dropping an index that must become unique, the data is
	// checked for duplicates; if any exist the existing index is left in place
	// and an EnforcementError is returned.
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

	for _, schema := range schemas {
		if err := enforceSchema(ctx, db, schema, opt.IndexMismatchPolicy); err != nil {
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

func enforceSchema(ctx context.Context, db *mongo.Database, schema *Schema, policy IndexMismatchPolicy) error {
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
		if err := enforceIndex(ctx, coll, schema.Collection, existing, field.BSONName+"_1", keys, field.Unique, policy); err != nil {
			return err
		}
	}

	// Compound indexes
	for _, ci := range schema.CompoundIndexes {
		keys := bson.D{}
		for _, f := range ci.Fields {
			keys = append(keys, bson.E{Key: f, Value: 1})
		}
		if err := enforceIndex(ctx, coll, schema.Collection, existing, compoundIndexName(ci), keys, ci.Unique, policy); err != nil {
			return err
		}
	}

	return nil
}

// enforceIndex ensures a single index exists with the expected keys and options,
// creating it if missing and handling option mismatches per the policy.
func enforceIndex(ctx context.Context, coll *mongo.Collection, collection string, existing map[string]IndexSpec, name string, keys bson.D, unique bool, policy IndexMismatchPolicy) error {
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

	// IndexMismatchRebuild: a unique index can only be built if the data is
	// actually unique. Check before dropping so a failure leaves the existing
	// index in place instead of leaving the collection unindexed.
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
		return "is not unique (schema requires unique)"
	}
	if !unique && spec.Unique {
		return "is unique (schema expects non-unique)"
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

// hasDuplicateValues reports whether any two documents share the same value for
// the given index keys. Missing fields group as null, matching MongoDB's unique
// index semantics.
func hasDuplicateValues(ctx context.Context, coll *mongo.Collection, keys bson.D) (bool, error) {
	groupID := bson.D{}
	for _, k := range keys {
		// Dots are not allowed in $group _id field names.
		groupID = append(groupID, bson.E{Key: strings.ReplaceAll(k.Key, ".", "_"), Value: "$" + k.Key})
	}
	pipeline := mongo.Pipeline{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: groupID},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		{{Key: "$match", Value: bson.D{{Key: "n", Value: bson.D{{Key: "$gt", Value: 1}}}}}},
		{{Key: "$limit", Value: 1}},
	}
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
	Sparse           bool
	HasPartialFilter bool // index has a partialFilterExpression
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
			Sparse                  bool     `bson:"sparse"`
			PartialFilterExpression bson.Raw `bson:"partialFilterExpression"`
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
			Sparse:           idx.Sparse,
			HasPartialFilter: len(idx.PartialFilterExpression) > 0,
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
