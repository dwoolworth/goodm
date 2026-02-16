package goodm

import (
	"context"
	"fmt"
	"reflect"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Refs maps bson field names to destination pointers for population.
// Keys must correspond to fields tagged with goodm:"ref=collection".
type Refs map[string]interface{}

// PopulateOptions configures the Populate operation.
type PopulateOptions struct {
	DB *mongo.Database
}

// Populate resolves ref fields on a loaded model by fetching referenced documents
// from their respective collections. Each key in refs is a bson field name tagged
// with goodm:"ref=collection", and the corresponding value is a pointer to a struct
// where the referenced document will be decoded.
//
// For single refs (bson.ObjectID), the target should be a pointer to a struct:
//
//	profile := &Profile{}
//	err := goodm.Populate(ctx, user, goodm.Refs{"profile": profile})
//
// For array refs ([]bson.ObjectID), the target should be a pointer to a slice:
//
//	var tags []Tag
//	err := goodm.Populate(ctx, post, goodm.Refs{"tags": &tags})
func Populate(ctx context.Context, model interface{}, refs Refs, opts ...PopulateOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	var optDB *mongo.Database
	if len(opts) > 0 {
		optDB = opts[0].DB
	}
	db, err := getDB(optDB)
	if err != nil {
		return err
	}

	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	for bsonName, target := range refs {
		field := schema.GetField(bsonName)
		if field == nil {
			return fmt.Errorf("goodm: field %q not found in schema for %s", bsonName, schema.ModelName)
		}
		if field.Ref == "" {
			return fmt.Errorf("goodm: field %q has no ref tag", bsonName)
		}

		fv := v.FieldByName(field.Name)
		if !fv.IsValid() {
			return fmt.Errorf("goodm: field %q not found in model struct", field.Name)
		}

		coll := db.Collection(field.Ref)

		// Array ref: []bson.ObjectID → fetch all via $in
		if refIDs, ok := fv.Interface().([]bson.ObjectID); ok {
			ids := filterNonZeroIDs(refIDs)
			if len(ids) == 0 {
				continue
			}
			cursor, err := coll.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
			if err != nil {
				return fmt.Errorf("goodm: populate %q failed: %w", bsonName, err)
			}
			if err := cursor.All(ctx, target); err != nil {
				_ = cursor.Close(ctx)
				return fmt.Errorf("goodm: populate %q decode failed: %w", bsonName, err)
			}
			_ = cursor.Close(ctx)
			continue
		}

		// Single ref: bson.ObjectID → fetch one
		refID, ok := fv.Interface().(bson.ObjectID)
		if !ok {
			return fmt.Errorf("goodm: ref field %q is not bson.ObjectID or []bson.ObjectID", bsonName)
		}
		if refID.IsZero() {
			continue // skip unset refs
		}

		if err := coll.FindOne(ctx, bson.D{{Key: "_id", Value: refID}}).Decode(target); err != nil {
			if err == mongo.ErrNoDocuments {
				continue // referenced document not found, leave target as zero
			}
			return fmt.Errorf("goodm: populate %q failed: %w", bsonName, err)
		}
	}

	return nil
}

// filterNonZeroIDs returns a new slice with zero ObjectIDs removed.
func filterNonZeroIDs(ids []bson.ObjectID) []bson.ObjectID {
	var result []bson.ObjectID
	for _, id := range ids {
		if !id.IsZero() {
			result = append(result, id)
		}
	}
	return result
}

// BatchPopulate resolves a single ref field across a slice of models in one query.
// It collects unique IDs from the ref field and fetches all referenced documents
// using a single $in query, avoiding N+1 overhead.
//
// models must be a slice or pointer to a slice (e.g. []Post or *[]Post).
// field is the bson name of the ref field (e.g. "author").
// results must be a pointer to a slice of the referenced type (e.g. *[]User).
//
// Example:
//
//	var posts []Post
//	goodm.Find(ctx, bson.D{}, &posts)
//
//	var authors []User
//	err := goodm.BatchPopulate(ctx, posts, "author", &authors)
func BatchPopulate(ctx context.Context, models interface{}, field string, results interface{}, opts ...PopulateOptions) error {
	// Validate results is *[]T
	rv := reflect.ValueOf(results)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("goodm: results must be a pointer to a slice, got %T", results)
	}

	// Normalize models to a reflect.Value of a slice
	mv := reflect.ValueOf(models)
	if mv.Kind() == reflect.Ptr {
		mv = mv.Elem()
	}
	if mv.Kind() != reflect.Slice {
		return fmt.Errorf("goodm: models must be a slice, got %T", models)
	}
	if mv.Len() == 0 {
		return nil
	}

	// Get schema from the first element
	elem := mv.Index(0)
	if elem.Kind() == reflect.Ptr {
		elem = elem.Elem()
	}
	tmpPtr := reflect.New(elem.Type())
	schema, err := getSchemaForModel(tmpPtr.Interface())
	if err != nil {
		return err
	}

	// Validate the field has a ref tag
	fs := schema.GetField(field)
	if fs == nil {
		return fmt.Errorf("goodm: field %q not found in schema for %s", field, schema.ModelName)
	}
	if fs.Ref == "" {
		return fmt.Errorf("goodm: field %q has no ref tag", field)
	}

	// Collect unique non-zero IDs (supports both bson.ObjectID and []bson.ObjectID)
	seen := make(map[bson.ObjectID]bool)
	var ids []bson.ObjectID
	for i := 0; i < mv.Len(); i++ {
		el := mv.Index(i)
		if el.Kind() == reflect.Ptr {
			el = el.Elem()
		}
		fv := el.FieldByName(fs.Name)
		if !fv.IsValid() {
			continue
		}

		// Array ref: []bson.ObjectID
		if refIDs, ok := fv.Interface().([]bson.ObjectID); ok {
			for _, refID := range refIDs {
				if !refID.IsZero() && !seen[refID] {
					seen[refID] = true
					ids = append(ids, refID)
				}
			}
			continue
		}

		// Single ref: bson.ObjectID
		refID, ok := fv.Interface().(bson.ObjectID)
		if !ok || refID.IsZero() || seen[refID] {
			continue
		}
		seen[refID] = true
		ids = append(ids, refID)
	}

	if len(ids) == 0 {
		return nil
	}

	// Fetch all referenced documents in one query
	var optDB *mongo.Database
	if len(opts) > 0 {
		optDB = opts[0].DB
	}
	db, err := getDB(optDB)
	if err != nil {
		return err
	}

	coll := db.Collection(fs.Ref)
	cursor, err := coll.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return fmt.Errorf("goodm: batch populate %q failed: %w", field, err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	if err := cursor.All(ctx, results); err != nil {
		return fmt.Errorf("goodm: batch populate decode failed: %w", err)
	}

	return nil
}
