package goodm

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// BulkResult contains the outcome of a bulk operation.
type BulkResult struct {
	InsertedCount int64
	MatchedCount  int64
	ModifiedCount int64
	DeletedCount  int64
}

// CreateMany inserts multiple documents. It generates IDs, sets timestamps,
// runs BeforeCreate/AfterCreate hooks, and validates each model before
// performing a single InsertMany call.
//
// models must be a slice of structs or struct pointers (e.g. []User or []*User).
//
// Performance: hooks and validation run per-model. For large batches where
// you don't need the ODM lifecycle, use the mongo driver's InsertMany directly.
func CreateMany(ctx context.Context, models interface{}, opts ...CreateOptions) error {
	rv := reflect.ValueOf(models)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Slice {
		return fmt.Errorf("goodm: CreateMany expects a slice, got %T", models)
	}
	if rv.Len() == 0 {
		return nil
	}

	// Get a pointer to the first element for schema lookup
	first := rv.Index(0)
	var elemForSchema interface{}
	if first.Kind() == reflect.Ptr {
		elemForSchema = first.Interface()
	} else {
		elemForSchema = first.Addr().Interface()
	}

	schema, err := getSchemaForModel(elemForSchema)
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

	return runMiddleware(ctx, &OpInfo{
		Operation:  OpCreateMany,
		Collection: schema.Collection,
		ModelName:  schema.ModelName,
	}, func(ctx context.Context) error {
		now := time.Now()
		docs := make([]interface{}, rv.Len())

		for i := 0; i < rv.Len(); i++ {
			elem := rv.Index(i)
			var model interface{}
			if elem.Kind() == reflect.Ptr {
				model = elem.Interface()
			} else {
				model = elem.Addr().Interface()
			}

			// Set ID if zero
			id, err := getModelID(model)
			if err != nil {
				return err
			}
			if id.IsZero() {
				setModelID(model, bson.NewObjectID())
			}

			// Set timestamps
			setTimestamps(model, now)

			// BeforeCreate hook
			if hook, ok := model.(BeforeCreate); ok {
				if err := hook.BeforeCreate(ctx); err != nil {
					return fmt.Errorf("goodm: BeforeCreate failed on item %d: %w", i, err)
				}
			}

			// Validate
			if errs := Validate(model, schema); len(errs) > 0 {
				return fmt.Errorf("goodm: validation failed on item %d: %w", i, ValidationErrors(errs))
			}

			docs[i] = model
		}

		coll := db.Collection(schema.Collection)
		if _, err := coll.InsertMany(ctx, docs); err != nil {
			return fmt.Errorf("goodm: insert many failed: %w", err)
		}

		// AfterCreate hooks
		for i := 0; i < rv.Len(); i++ {
			elem := rv.Index(i)
			var model interface{}
			if elem.Kind() == reflect.Ptr {
				model = elem.Interface()
			} else {
				model = elem.Addr().Interface()
			}
			if hook, ok := model.(AfterCreate); ok {
				if err := hook.AfterCreate(ctx); err != nil {
					return err
				}
			}
		}

		return nil
	})
}

// UpdateMany updates all documents matching filter with the given update document.
// The model parameter is used only for schema/collection lookup (e.g. &User{}).
//
// Performance: This is a direct passthrough to MongoDB's UpdateMany. It bypasses
// hooks, validation, and immutable field enforcement. Use Update for the full
// ODM lifecycle on individual documents.
func UpdateMany(ctx context.Context, filter, update interface{}, model interface{}, opts ...UpdateOptions) (*BulkResult, error) {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return nil, err
	}

	var optDB *mongo.Database
	if len(opts) > 0 {
		optDB = opts[0].DB
	}
	db, err := getDB(optDB)
	if err != nil {
		return nil, err
	}

	var result *BulkResult
	err = runMiddleware(ctx, &OpInfo{
		Operation:  OpUpdateMany,
		Collection: schema.Collection,
		ModelName:  schema.ModelName,
		Model:      model,
		Filter:     filter,
	}, func(ctx context.Context) error {
		coll := db.Collection(schema.Collection)
		res, err := coll.UpdateMany(ctx, filter, update)
		if err != nil {
			return fmt.Errorf("goodm: update many failed: %w", err)
		}
		result = &BulkResult{
			MatchedCount:  res.MatchedCount,
			ModifiedCount: res.ModifiedCount,
		}
		return nil
	})

	return result, err
}

// DeleteMany deletes all documents matching filter.
// The model parameter is used only for schema/collection lookup (e.g. &User{}).
//
// Performance: This is a direct passthrough to MongoDB's DeleteMany. It bypasses
// hooks entirely. Use Delete for the full ODM lifecycle on individual documents.
func DeleteMany(ctx context.Context, filter interface{}, model interface{}, opts ...DeleteOptions) (*BulkResult, error) {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return nil, err
	}

	var optDB *mongo.Database
	if len(opts) > 0 {
		optDB = opts[0].DB
	}
	db, err := getDB(optDB)
	if err != nil {
		return nil, err
	}

	var result *BulkResult
	err = runMiddleware(ctx, &OpInfo{
		Operation:  OpDeleteMany,
		Collection: schema.Collection,
		ModelName:  schema.ModelName,
		Filter:     filter,
	}, func(ctx context.Context) error {
		coll := db.Collection(schema.Collection)
		res, err := coll.DeleteMany(ctx, filter)
		if err != nil {
			return fmt.Errorf("goodm: delete many failed: %w", err)
		}
		result = &BulkResult{
			DeletedCount: res.DeletedCount,
		}
		return nil
	})

	return result, err
}
