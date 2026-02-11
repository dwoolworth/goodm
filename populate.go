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
// Example:
//
//	user := &User{}
//	goodm.FindOne(ctx, bson.D{{Key: "email", Value: "alice@example.com"}}, user)
//
//	profile := &Profile{}
//	err := goodm.Populate(ctx, user, goodm.Refs{"profile": profile})
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

		refID, ok := fv.Interface().(bson.ObjectID)
		if !ok {
			return fmt.Errorf("goodm: ref field %q is not bson.ObjectID", bsonName)
		}
		if refID.IsZero() {
			continue // skip unset refs
		}

		coll := db.Collection(field.Ref)
		if err := coll.FindOne(ctx, bson.D{{Key: "_id", Value: refID}}).Decode(target); err != nil {
			if err == mongo.ErrNoDocuments {
				continue // referenced document not found, leave target as zero
			}
			return fmt.Errorf("goodm: populate %q failed: %w", bsonName, err)
		}
	}

	return nil
}
