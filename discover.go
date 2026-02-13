package goodm

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// DiscoverOptions controls how database discovery is performed.
type DiscoverOptions struct {
	SampleSize  int      // documents to sample per collection (default 500)
	Collections []string // empty = all collections
}

// DiscoveredField describes a single field found in a collection's documents.
type DiscoveredField struct {
	BSONName   string
	GoType     string // inferred Go type
	IsRequired bool   // appears in every sampled doc
	IsUnique   bool   // has a unique index
	IsIndexed  bool   // has a non-unique index
}

// DiscoveredIndex describes an index found on a collection.
type DiscoveredIndex struct {
	Name   string
	Keys   []string // field names in order
	Unique bool
}

// DiscoveredCollection holds the discovery results for a single collection.
type DiscoveredCollection struct {
	Name     string
	Fields   []DiscoveredField
	Indexes  []DiscoveredIndex
	DocCount int64
}

// Discover introspects a MongoDB database by sampling documents and reading indexes.
func Discover(ctx context.Context, db *mongo.Database, opts DiscoverOptions) ([]DiscoveredCollection, error) {
	if opts.SampleSize <= 0 {
		opts.SampleSize = 500
	}

	var collNames []string
	if len(opts.Collections) > 0 {
		collNames = opts.Collections
	} else {
		names, err := db.ListCollectionNames(ctx, bson.D{})
		if err != nil {
			return nil, fmt.Errorf("goodm discover: failed to list collections: %w", err)
		}
		collNames = names
	}

	var results []DiscoveredCollection
	for _, name := range collNames {
		coll := db.Collection(name)
		dc, err := discoverCollection(ctx, coll, opts)
		if err != nil {
			return nil, fmt.Errorf("goodm discover: collection %s: %w", name, err)
		}
		results = append(results, dc)
	}

	return results, nil
}

func discoverCollection(ctx context.Context, coll *mongo.Collection, opts DiscoverOptions) (DiscoveredCollection, error) {
	dc := DiscoveredCollection{
		Name: coll.Name(),
	}

	// Get document count
	count, err := coll.CountDocuments(ctx, bson.D{})
	if err != nil {
		return dc, fmt.Errorf("failed to count documents: %w", err)
	}
	dc.DocCount = count

	// Sample documents to infer fields
	fields, err := sampleDocuments(ctx, coll, opts.SampleSize)
	if err != nil {
		return dc, err
	}
	dc.Fields = fields

	// Detect indexes
	indexes, err := detectIndexes(ctx, coll)
	if err != nil {
		return dc, err
	}
	dc.Indexes = indexes

	// Merge index info into fields
	for i := range dc.Fields {
		for _, idx := range dc.Indexes {
			if len(idx.Keys) == 1 && idx.Keys[0] == dc.Fields[i].BSONName {
				if idx.Unique {
					dc.Fields[i].IsUnique = true
				} else {
					dc.Fields[i].IsIndexed = true
				}
			}
		}
	}

	return dc, nil
}

// fieldTracker accumulates type information across sampled documents.
type fieldTracker struct {
	types map[string]bool // set of observed Go types
	count int             // number of docs containing this field
}

func sampleDocuments(ctx context.Context, coll *mongo.Collection, sampleSize int) ([]DiscoveredField, error) {
	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetLimit(int64(sampleSize)))
	if err != nil {
		return nil, fmt.Errorf("failed to sample documents: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	trackers := make(map[string]*fieldTracker) // bsonName → tracker
	fieldOrder := []string{}                   // preserve insertion order
	totalDocs := 0

	for cursor.Next(ctx) {
		var doc bson.D
		if err := cursor.Decode(&doc); err != nil {
			continue
		}
		totalDocs++

		for _, elem := range doc {
			ft, exists := trackers[elem.Key]
			if !exists {
				ft = &fieldTracker{types: make(map[string]bool)}
				trackers[elem.Key] = ft
				fieldOrder = append(fieldOrder, elem.Key)
			}
			ft.count++
			goType := inferGoType(elem.Value)
			ft.types[goType] = true
		}
	}

	if totalDocs == 0 {
		return nil, nil
	}

	var fields []DiscoveredField
	for _, name := range fieldOrder {
		ft := trackers[name]
		goType := resolveType(ft.types)
		fields = append(fields, DiscoveredField{
			BSONName:   name,
			GoType:     goType,
			IsRequired: ft.count == totalDocs,
		})
	}

	return fields, nil
}

func detectIndexes(ctx context.Context, coll *mongo.Collection) ([]DiscoveredIndex, error) {
	cursor, err := coll.Indexes().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list indexes: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var indexes []DiscoveredIndex
	for cursor.Next(ctx) {
		var raw bson.M
		if err := cursor.Decode(&raw); err != nil {
			continue
		}

		name, _ := raw["name"].(string)

		// Parse key document
		var keys []string
		if keyDoc, ok := raw["key"].(bson.D); ok {
			for _, k := range keyDoc {
				keys = append(keys, k.Key)
			}
		}

		unique := false
		if u, ok := raw["unique"].(bool); ok {
			unique = u
		}

		indexes = append(indexes, DiscoveredIndex{
			Name:   name,
			Keys:   keys,
			Unique: unique,
		})
	}

	return indexes, nil
}

// inferGoType maps a BSON runtime value to a Go type string.
func inferGoType(v interface{}) string {
	switch v := v.(type) {
	case string:
		return "string"
	case int32:
		return "int32"
	case int64:
		return "int64"
	case float64:
		return "float64"
	case bool:
		return "bool"
	case bson.ObjectID:
		return "bson.ObjectID"
	case time.Time:
		return "time.Time"
	case bson.D:
		return "bson.M"
	case bson.A:
		return inferArrayType(v)
	case []byte:
		return "[]byte"
	case bson.Decimal128:
		return "bson.Decimal128"
	case nil:
		return "null"
	default:
		_ = v // used by type switch
		return "interface{}"
	}
}

func inferArrayType(arr bson.A) string {
	if len(arr) == 0 {
		return "bson.A"
	}
	firstType := inferGoType(arr[0])
	for _, elem := range arr[1:] {
		if inferGoType(elem) != firstType {
			return "bson.A"
		}
	}
	if firstType == "null" {
		return "bson.A"
	}
	return "[]" + firstType
}

// resolveType picks the best Go type from a set of observed types.
func resolveType(types map[string]bool) string {
	// Remove null and track if it was present
	hasNull := types["null"]
	delete(types, "null")

	if len(types) == 0 {
		return "interface{}"
	}

	// Numeric promotion: int32 + int64 → int64
	if types["int32"] && types["int64"] {
		delete(types, "int32")
	}
	// int + float → float64
	if (types["int32"] || types["int64"]) && types["float64"] {
		delete(types, "int32")
		delete(types, "int64")
	}

	if len(types) == 1 {
		var t string
		for t = range types {
		}
		if hasNull {
			return "*" + t
		}
		return t
	}

	// Multiple non-null types — fall back to interface{}
	// Sort for deterministic output
	typeList := make([]string, 0, len(types))
	for t := range types {
		typeList = append(typeList, t)
	}
	sort.Strings(typeList)
	_ = typeList
	return "interface{}"
}
