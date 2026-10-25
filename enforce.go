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

	// Validator says whether to apply or verify each collection's validator,
	// derived from Schema.JSONSchema. ValidationLevel and ValidationAction
	// are the server settings to use with it; empty means "strict" and
	// "error". A collection registered for more than one model is an error
	// under any policy but ValidatorIgnore.
	Validator        ValidatorPolicy
	ValidationLevel  string
	ValidationAction string
}

// Enforce ensures that all registered schemas are reflected in the database.
// It creates missing indexes, optionally sets or verifies collection
// validators, and optionally detects schema drift based on the provided
// options. If no options are provided, validators are left alone and drift
// detection is skipped.
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
		if err := enforceValidator(ctx, db, schema, opt, otherSchemasForCollection(schema)); err != nil {
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

	existing, err := ListExistingIndexes(ctx, coll)
	if err != nil {
		return &EnforcementError{
			Collection: schema.Collection,
			Message:    fmt.Sprintf("failed to list indexes: %v", err),
		}
	}

	for _, def := range schema.IndexDefinitions() {
		if err := enforceIndex(ctx, coll, schema.Collection, existing, def, policy, probe); err != nil {
			return err
		}
	}
	return nil
}

// enforceIndex ensures one index exists exactly as defined, creating it if
// missing and handling option mismatches per the policy.
func enforceIndex(ctx context.Context, coll *mongo.Collection, collection string, existing map[string]IndexSpec, def IndexDefinition, policy IndexMismatchPolicy, probe *prepareUniqueProbe) error {
	spec, exists := existing[def.Name]
	if !exists {
		return createIndex(ctx, coll, collection, def)
	}

	mismatch := describeIndexMismatch(spec, def)
	if mismatch == "" || policy == IndexMismatchIgnore {
		return nil
	}

	if policy == IndexMismatchError {
		return &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("index %s exists but %s (use IndexMismatchRebuild to rebuild it)", def.Name, mismatch),
		}
	}

	// IndexMismatchRebuild. A unique index can only be built over unique data.
	// Check before touching the index so a failure here is a read-only no-op.
	if def.Unique {
		hasDups, err := hasDuplicateValues(ctx, coll, def)
		if err != nil {
			return &EnforcementError{
				Collection: collection,
				Message:    fmt.Sprintf("failed to check for duplicates before rebuilding index %s: %v", def.Name, err),
			}
		}
		if hasDups {
			return &EnforcementError{
				Collection: collection,
				Message:    fmt.Sprintf("cannot rebuild index %s as unique: collection contains duplicate values; existing index left in place", def.Name),
			}
		}
	}

	if canRevertPrepareUniqueInPlace(spec, def) {
		if err := collModIndex(ctx, coll, def.Name, "prepareUnique", false); err != nil {
			return &EnforcementError{
				Collection: collection,
				Message:    fmt.Sprintf("failed to clear prepareUnique on index %s: %v", def.Name, err),
			}
		}
		return nil
	}

	if canConvertToUniqueInPlace(spec, def) {
		supported, err := probe.ok(ctx)
		if err != nil {
			return &EnforcementError{
				Collection: collection,
				Message:    fmt.Sprintf("cannot determine whether the server supports in-place unique conversion of index %s: %v", def.Name, err),
			}
		}
		if supported {
			converted, err := convertIndexToUnique(ctx, coll, collection, def.Name)
			if err != nil || converted {
				return err
			}
		}
	}

	if err := coll.Indexes().DropOne(ctx, def.Name); err != nil {
		return &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("failed to drop mismatched index %s: %v", def.Name, err),
		}
	}
	if err := createIndex(ctx, coll, collection, def); err != nil {
		return &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("index %s was dropped but recreate failed: %v", def.Name, err),
		}
	}
	return nil
}

// optionsMatchExceptUnique reports whether an existing index matches the
// definition in every respect other than the unique flag.
func optionsMatchExceptUnique(spec IndexSpec, def IndexDefinition) bool {
	return indexKeysEqual(spec.Keys, def.Keys) &&
		spec.Sparse == def.Sparse &&
		!spec.Hidden &&
		ttlEqual(spec.ExpireAfterSeconds, def.ExpireAfterSeconds) &&
		partialFiltersEqual(def.PartialFilter, spec.PartialFilter) &&
		collationsEqual(def.Collation, spec.Collation)
}

// canConvertToUniqueInPlace reports whether the only difference between the
// existing index and the schema is that the schema wants it unique. collMod
// can only add uniqueness; every other difference needs a rebuild.
func canConvertToUniqueInPlace(spec IndexSpec, def IndexDefinition) bool {
	return def.Unique && !spec.Unique && optionsMatchExceptUnique(spec, def)
}

// canRevertPrepareUniqueInPlace reports whether the only mismatch is a stale
// prepareUnique on an index the schema wants non-unique; clearing the flag
// fixes that without a rebuild.
func canRevertPrepareUniqueInPlace(spec IndexSpec, def IndexDefinition) bool {
	return !def.Unique && spec.PrepareUnique && !spec.Unique && optionsMatchExceptUnique(spec, def)
}

func ttlEqual(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
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

func createIndex(ctx context.Context, coll *mongo.Collection, collection string, def IndexDefinition) error {
	if _, err := coll.Indexes().CreateOne(ctx, def.Model()); err != nil {
		return &EnforcementError{
			Collection: collection,
			Message:    fmt.Sprintf("failed to create index %s: %v", def.Name, err),
		}
	}
	return nil
}

// describeIndexMismatch returns a human-readable description of how an existing
// index differs from the definition, or "" if it matches.
func describeIndexMismatch(spec IndexSpec, def IndexDefinition) string {
	if !indexKeysEqual(spec.Keys, def.Keys) {
		return "has different keys than the schema expects"
	}
	if def.Unique && !spec.Unique {
		if spec.PrepareUnique {
			return "is prepareUnique but not yet unique: new duplicates are already rejected (schema requires unique)"
		}
		return "is not unique (schema requires unique)"
	}
	if !def.Unique && spec.Unique {
		return "is unique (schema expects non-unique)"
	}
	if !def.Unique && spec.PrepareUnique {
		return "is prepareUnique: new duplicates are rejected (schema expects non-unique)"
	}
	if spec.Sparse != def.Sparse {
		if spec.Sparse {
			return "is sparse (schema expects non-sparse)"
		}
		return "is not sparse (schema requires sparse)"
	}
	if !ttlEqual(spec.ExpireAfterSeconds, def.ExpireAfterSeconds) {
		switch {
		case def.ExpireAfterSeconds == nil:
			return "is a TTL index (schema expects no TTL)"
		case spec.ExpireAfterSeconds == nil:
			return "is not a TTL index (schema requires ttl)"
		default:
			return fmt.Sprintf("expires after %ds (schema requires %ds)", *spec.ExpireAfterSeconds, *def.ExpireAfterSeconds)
		}
	}
	if !partialFiltersEqual(def.PartialFilter, spec.PartialFilter) {
		if def.PartialFilter == nil {
			return "is partial (schema expects no partial filter)"
		}
		return "has a different partial filter than the schema expects"
	}
	if !collationsEqual(def.Collation, spec.Collation) {
		if def.Collation == nil {
			return "has a collation (schema expects none)"
		}
		return "has a different collation than the schema expects"
	}
	if spec.Hidden {
		return "is hidden"
	}
	return ""
}

// hasDuplicateValues reports whether any two documents would collide under a
// unique index with the given definition. It mirrors unique-index semantics as
// closely as an aggregation can: a partial filter restricts the candidates,
// sparse indexes skip documents missing every key, array fields are unwound so
// each element is its own entry, duplicate elements within one document do not
// count, and missing fields group as null. Empty arrays also group as null, so
// an empty array and a missing field are reported as a collision the server
// would not raise; that only errs toward refusing a rebuild, never toward an
// unsafe one.
func hasDuplicateValues(ctx context.Context, coll *mongo.Collection, def IndexDefinition) (bool, error) {
	pipeline := mongo.Pipeline{}
	if def.PartialFilter != nil {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: def.PartialFilter}})
	}
	if def.Sparse {
		present := bson.A{}
		for _, k := range def.Keys {
			present = append(present, bson.D{{Key: k.Key, Value: bson.D{{Key: "$exists", Value: true}}}})
		}
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.D{{Key: "$or", Value: present}}}})
	}
	perDoc := bson.D{{Key: "doc", Value: "$_id"}}
	perValue := bson.D{}
	for i, k := range def.Keys {
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
	aggOpts := options.Aggregate()
	if def.Collation != nil {
		aggOpts.SetCollation(def.Collation)
	}
	cursor, err := coll.Aggregate(ctx, pipeline, aggOpts)
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
	return detectDriftIn(ctx, db, schema.Collection, knownFields(schema), sampleSize)
}

// knownFields returns the set of top-level bson field names a schema declares.
func knownFields(schema *Schema) map[string]bool {
	known := make(map[string]bool, len(schema.Fields))
	for _, f := range schema.Fields {
		known[f.BSONName] = true
	}
	return known
}

// detectDriftIn samples a collection and reports top-level fields not in known.
func detectDriftIn(ctx context.Context, db *mongo.Database, collection string, known map[string]bool, sampleSize int) []DriftError {
	var drifts []DriftError
	coll := db.Collection(collection)

	if sampleSize <= 0 {
		sampleSize = DefaultDriftSampleSize
	}

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetLimit(int64(sampleSize)))
	if err != nil {
		return drifts
	}
	defer func() { _ = cursor.Close(ctx) }()

	seen := make(map[string]bool)
	for cursor.Next(ctx) {
		var doc bson.D
		if err := cursor.Decode(&doc); err != nil {
			continue
		}
		for _, elem := range doc {
			if !known[elem.Key] && !seen[elem.Key] {
				seen[elem.Key] = true
				drifts = append(drifts, DriftError{
					Collection: collection,
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
	Name               string
	Keys               bson.D // for text indexes, the weighted fields appear as field: "text"
	Unique             bool
	PrepareUnique      bool // collMod prepareUnique is set: not unique, but new duplicates are rejected
	Sparse             bool
	HasPartialFilter   bool // index has a partialFilterExpression
	HasCollation       bool
	HasTTL             bool // index has expireAfterSeconds
	Hidden             bool
	ExpireAfterSeconds *int32   // TTL in seconds when HasTTL
	PartialFilter      bson.Raw // raw partialFilterExpression when HasPartialFilter
	Collation          bson.Raw // raw collation document when HasCollation
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
			ExpireAfterSeconds      *int32   `bson:"expireAfterSeconds"`
			Hidden                  bool     `bson:"hidden"`
			Weights                 bson.D   `bson:"weights"`
		}
		if err := cursor.Decode(&idx); err != nil {
			continue
		}
		if idx.Name == "" {
			continue
		}
		result[idx.Name] = IndexSpec{
			Name:               idx.Name,
			Keys:               normalizeTextKeys(idx.Key, idx.Weights),
			Unique:             idx.Unique,
			PrepareUnique:      idx.PrepareUnique,
			Sparse:             idx.Sparse,
			HasPartialFilter:   len(idx.PartialFilterExpression) > 0,
			HasCollation:       len(idx.Collation) > 0,
			HasTTL:             idx.ExpireAfterSeconds != nil,
			Hidden:             idx.Hidden,
			ExpireAfterSeconds: idx.ExpireAfterSeconds,
			PartialFilter:      idx.PartialFilterExpression,
			Collation:          idx.Collation,
		}
	}

	return result, nil
}

// normalizeTextKeys rewrites the server's internal text index key form
// ({_fts: "text", _ftsx: 1} plus a weights document) back into the
// {field: "text"} form a schema declares, so the two compare equal.
func normalizeTextKeys(keys bson.D, weights bson.D) bson.D {
	if len(weights) == 0 {
		return keys
	}
	out := make(bson.D, 0, len(keys)+len(weights))
	inserted := false
	for _, k := range keys {
		if k.Key == "_fts" || k.Key == "_ftsx" {
			if !inserted {
				for _, w := range weights {
					out = append(out, bson.E{Key: w.Key, Value: "text"})
				}
				inserted = true
			}
			continue
		}
		out = append(out, k)
	}
	return out
}
