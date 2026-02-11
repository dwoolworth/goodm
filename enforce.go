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

// EnforceOptions configures the behavior of Enforce.
type EnforceOptions struct {
	DriftPolicy    DriftPolicy
	OnDriftWarning func(d DriftError) // called for each drift when policy is DriftWarn
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
		if err := enforceSchema(ctx, db, schema); err != nil {
			return err
		}

		if opt.DriftPolicy == DriftIgnore {
			continue
		}

		drifts := DetectDrift(ctx, db, schema)
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

func enforceSchema(ctx context.Context, db *mongo.Database, schema *Schema) error {
	coll := db.Collection(schema.Collection)

	// Get existing indexes
	existing, err := ListExistingIndexes(ctx, coll)
	if err != nil {
		return &EnforcementError{
			Collection: schema.Collection,
			Message:    fmt.Sprintf("failed to list indexes: %v", err),
		}
	}

	// Create single-field indexes from field tags
	for _, field := range schema.Fields {
		if field.Unique {
			indexName := field.BSONName + "_1"
			if !existing[indexName] {
				model := mongo.IndexModel{
					Keys:    bson.D{{Key: field.BSONName, Value: 1}},
					Options: options.Index().SetUnique(true),
				}
				if _, err := coll.Indexes().CreateOne(ctx, model); err != nil {
					return &EnforcementError{
						Collection: schema.Collection,
						Message:    fmt.Sprintf("failed to create unique index on %s: %v", field.BSONName, err),
					}
				}
			}
		} else if field.Index {
			indexName := field.BSONName + "_1"
			if !existing[indexName] {
				model := mongo.IndexModel{
					Keys: bson.D{{Key: field.BSONName, Value: 1}},
				}
				if _, err := coll.Indexes().CreateOne(ctx, model); err != nil {
					return &EnforcementError{
						Collection: schema.Collection,
						Message:    fmt.Sprintf("failed to create index on %s: %v", field.BSONName, err),
					}
				}
			}
		}
	}

	// Create compound indexes
	for _, ci := range schema.CompoundIndexes {
		indexName := compoundIndexName(ci)
		if !existing[indexName] {
			keys := bson.D{}
			for _, f := range ci.Fields {
				keys = append(keys, bson.E{Key: f, Value: 1})
			}
			model := mongo.IndexModel{Keys: keys}
			if ci.Unique {
				model.Options = options.Index().SetUnique(true)
			}
			if _, err := coll.Indexes().CreateOne(ctx, model); err != nil {
				return &EnforcementError{
					Collection: schema.Collection,
					Message:    fmt.Sprintf("failed to create compound index %s: %v", indexName, err),
				}
			}
		}
	}

	return nil
}

// DetectDrift samples documents from the collection and reports fields
// that exist in the database but not in the schema.
func DetectDrift(ctx context.Context, db *mongo.Database, schema *Schema) []DriftError {
	var drifts []DriftError
	coll := db.Collection(schema.Collection)

	// Sample up to 100 documents
	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetLimit(100))
	if err != nil {
		return drifts
	}
	defer cursor.Close(ctx)

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

// ListExistingIndexes returns a set of index names that exist on the collection.
func ListExistingIndexes(ctx context.Context, coll *mongo.Collection) (map[string]bool, error) {
	result := make(map[string]bool)

	cursor, err := coll.Indexes().List(ctx)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	for cursor.Next(ctx) {
		var idx bson.M
		if err := cursor.Decode(&idx); err != nil {
			continue
		}
		if name, ok := idx["name"].(string); ok {
			result[name] = true
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
