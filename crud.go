package goodm

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// CreateOptions configures the Create operation.
type CreateOptions struct {
	DB *mongo.Database
}

// FindOptions configures Find, FindOne, and FindCursor operations.
type FindOptions struct {
	DB    *mongo.Database
	Limit int64
	Skip  int64
	Sort  bson.D
}

// UpdateOptions configures the Update operation.
type UpdateOptions struct {
	DB *mongo.Database
}

// DeleteOptions configures the Delete operation.
type DeleteOptions struct {
	DB *mongo.Database
}

// Create inserts a new document. It generates an ID if zero, sets timestamps,
// runs BeforeCreate/AfterCreate hooks, and validates against the schema.
func Create(ctx context.Context, model interface{}, opts ...CreateOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	return runMiddleware(ctx, &OpInfo{
		Operation: OpCreate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model,
	}, func(ctx context.Context) error {
		var optDB *mongo.Database
		if len(opts) > 0 {
			optDB = opts[0].DB
		}
		db, err := getDB(optDB)
		if err != nil {
			return err
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
		setTimestamps(model, time.Now())

		// BeforeCreate hook
		if hook, ok := model.(BeforeCreate); ok {
			if err := hook.BeforeCreate(ctx); err != nil {
				return err
			}
		}

		// Validate
		if errs := Validate(model, schema); len(errs) > 0 {
			return ValidationErrors(errs)
		}

		// Insert
		coll := db.Collection(schema.Collection)
		if _, err := coll.InsertOne(ctx, model); err != nil {
			return fmt.Errorf("goodm: insert failed: %w", err)
		}

		// AfterCreate hook
		if hook, ok := model.(AfterCreate); ok {
			if err := hook.AfterCreate(ctx); err != nil {
				return err
			}
		}

		return nil
	})
}

// FindOne finds a single document matching filter and decodes it into result.
// Returns ErrNotFound if no document matches.
func FindOne(ctx context.Context, filter interface{}, result interface{}, opts ...FindOptions) error {
	schema, err := getSchemaForModel(result)
	if err != nil {
		return err
	}

	return runMiddleware(ctx, &OpInfo{
		Operation: OpFind, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: result, Filter: filter,
	}, func(ctx context.Context) error {
		var optDB *mongo.Database
		if len(opts) > 0 {
			optDB = opts[0].DB
		}
		db, err := getDB(optDB)
		if err != nil {
			return err
		}

		coll := db.Collection(schema.Collection)
		if err := coll.FindOne(ctx, filter).Decode(result); err != nil {
			if err == mongo.ErrNoDocuments {
				return ErrNotFound
			}
			return fmt.Errorf("goodm: find one failed: %w", err)
		}

		return nil
	})
}

// Find finds all documents matching filter and decodes them into results.
// results must be a pointer to a slice (e.g. *[]User).
func Find(ctx context.Context, filter interface{}, results interface{}, opts ...FindOptions) error {
	// results must be *[]T
	rv := reflect.ValueOf(results)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("goodm: results must be a pointer to a slice, got %T", results)
	}

	elemType := rv.Elem().Type().Elem()
	tmpPtr := reflect.New(elemType)
	schema, err := getSchemaForModel(tmpPtr.Interface())
	if err != nil {
		return err
	}

	return runMiddleware(ctx, &OpInfo{
		Operation: OpFind, Collection: schema.Collection,
		ModelName: schema.ModelName, Filter: filter,
	}, func(ctx context.Context) error {
		var opt FindOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		findOpts := options.Find()
		if opt.Limit > 0 {
			findOpts.SetLimit(opt.Limit)
		}
		if opt.Skip > 0 {
			findOpts.SetSkip(opt.Skip)
		}
		if opt.Sort != nil {
			findOpts.SetSort(opt.Sort)
		}

		coll := db.Collection(schema.Collection)
		cursor, err := coll.Find(ctx, filter, findOpts)
		if err != nil {
			return fmt.Errorf("goodm: find failed: %w", err)
		}
		defer func() { _ = cursor.Close(ctx) }()

		if err := cursor.All(ctx, results); err != nil {
			return fmt.Errorf("goodm: cursor decode failed: %w", err)
		}

		return nil
	})
}

// FindCursor returns a raw *mongo.Cursor for streaming large result sets.
// The model parameter is used only for schema/collection lookup (e.g. &User{}).
func FindCursor(ctx context.Context, filter interface{}, model interface{}, opts ...FindOptions) (*mongo.Cursor, error) {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return nil, err
	}

	var cursor *mongo.Cursor
	err = runMiddleware(ctx, &OpInfo{
		Operation: OpFind, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: filter,
	}, func(ctx context.Context) error {
		var opt FindOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		findOpts := options.Find()
		if opt.Limit > 0 {
			findOpts.SetLimit(opt.Limit)
		}
		if opt.Skip > 0 {
			findOpts.SetSkip(opt.Skip)
		}
		if opt.Sort != nil {
			findOpts.SetSort(opt.Sort)
		}

		coll := db.Collection(schema.Collection)
		c, err := coll.Find(ctx, filter, findOpts)
		if err != nil {
			return fmt.Errorf("goodm: find cursor failed: %w", err)
		}
		cursor = c
		return nil
	})

	return cursor, err
}

// Update replaces an existing document. It fetches the current document to enforce
// immutable fields, runs BeforeSave/AfterSave hooks, validates, and sets UpdatedAt.
func Update(ctx context.Context, model interface{}, opts ...UpdateOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	id, err := getModelID(model)
	if err != nil {
		return err
	}
	if id.IsZero() {
		return fmt.Errorf("goodm: cannot update document with zero ID")
	}

	return runMiddleware(ctx, &OpInfo{
		Operation: OpUpdate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model,
		Filter: bson.D{{Key: "_id", Value: id}},
	}, func(ctx context.Context) error {
		var optDB *mongo.Database
		if len(opts) > 0 {
			optDB = opts[0].DB
		}
		db, err := getDB(optDB)
		if err != nil {
			return err
		}

		coll := db.Collection(schema.Collection)

		// Only fetch the existing document if immutable fields need checking.
		// This avoids an extra query when no fields are marked immutable.
		if hasImmutableFields(schema) {
			existing := reflect.New(reflect.TypeOf(model).Elem()).Interface()
			if err := coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(existing); err != nil {
				if err == mongo.ErrNoDocuments {
					return ErrNotFound
				}
				return fmt.Errorf("goodm: failed to fetch existing document: %w", err)
			}

			if immutableErrs := validateImmutable(existing, model, schema); len(immutableErrs) > 0 {
				return ValidationErrors(immutableErrs)
			}
		}

		// BeforeSave hook
		if hook, ok := model.(BeforeSave); ok {
			if err := hook.BeforeSave(ctx); err != nil {
				return err
			}
		}

		// Validate
		if errs := Validate(model, schema); len(errs) > 0 {
			return ValidationErrors(errs)
		}

		// Set UpdatedAt
		setUpdatedAt(model, time.Now())

		// Replace
		result, err := coll.ReplaceOne(ctx, bson.D{{Key: "_id", Value: id}}, model)
		if err != nil {
			return fmt.Errorf("goodm: update failed: %w", err)
		}
		if result.MatchedCount == 0 {
			return ErrNotFound
		}

		// AfterSave hook
		if hook, ok := model.(AfterSave); ok {
			if err := hook.AfterSave(ctx); err != nil {
				return err
			}
		}

		return nil
	})
}

// UpdateOne performs a partial update on a single document matching filter.
// The model parameter is used only for schema/collection lookup (e.g. &User{}).
// The update parameter should be a MongoDB update document (e.g. bson.D{{"$set", bson.D{...}}}).
//
// Performance: This is a direct passthrough to MongoDB's UpdateOne. It bypasses
// hooks, validation, and immutable field enforcement. Use Update for the full
// ODM lifecycle, or use this when you need raw performance and accept responsibility
// for data integrity.
func UpdateOne(ctx context.Context, filter interface{}, update interface{}, model interface{}, opts ...UpdateOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	return runMiddleware(ctx, &OpInfo{
		Operation: OpUpdate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: filter,
	}, func(ctx context.Context) error {
		var optDB *mongo.Database
		if len(opts) > 0 {
			optDB = opts[0].DB
		}
		db, err := getDB(optDB)
		if err != nil {
			return err
		}

		coll := db.Collection(schema.Collection)
		result, err := coll.UpdateOne(ctx, filter, update)
		if err != nil {
			return fmt.Errorf("goodm: update one failed: %w", err)
		}
		if result.MatchedCount == 0 {
			return ErrNotFound
		}

		return nil
	})
}

// Delete removes a document by its ID.
// Runs BeforeDelete/AfterDelete hooks.
func Delete(ctx context.Context, model interface{}, opts ...DeleteOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	id, err := getModelID(model)
	if err != nil {
		return err
	}
	if id.IsZero() {
		return fmt.Errorf("goodm: cannot delete document with zero ID")
	}

	return runMiddleware(ctx, &OpInfo{
		Operation: OpDelete, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model,
		Filter: bson.D{{Key: "_id", Value: id}},
	}, func(ctx context.Context) error {
		var optDB *mongo.Database
		if len(opts) > 0 {
			optDB = opts[0].DB
		}
		db, err := getDB(optDB)
		if err != nil {
			return err
		}

		// BeforeDelete hook
		if hook, ok := model.(BeforeDelete); ok {
			if err := hook.BeforeDelete(ctx); err != nil {
				return err
			}
		}

		coll := db.Collection(schema.Collection)
		result, err := coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
		if err != nil {
			return fmt.Errorf("goodm: delete failed: %w", err)
		}
		if result.DeletedCount == 0 {
			return ErrNotFound
		}

		// AfterDelete hook
		if hook, ok := model.(AfterDelete); ok {
			if err := hook.AfterDelete(ctx); err != nil {
				return err
			}
		}

		return nil
	})
}

// DeleteOne deletes a single document matching filter.
// The model parameter is used only for schema/collection lookup (e.g. &User{}).
//
// Performance: This is a direct passthrough to MongoDB's DeleteOne. It bypasses
// hooks entirely. Use Delete for the full ODM lifecycle with BeforeDelete/AfterDelete
// hooks, or use this when you need raw performance and don't require hook execution.
func DeleteOne(ctx context.Context, filter interface{}, model interface{}, opts ...DeleteOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	return runMiddleware(ctx, &OpInfo{
		Operation: OpDelete, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: filter,
	}, func(ctx context.Context) error {
		var optDB *mongo.Database
		if len(opts) > 0 {
			optDB = opts[0].DB
		}
		db, err := getDB(optDB)
		if err != nil {
			return err
		}

		coll := db.Collection(schema.Collection)
		result, err := coll.DeleteOne(ctx, filter)
		if err != nil {
			return fmt.Errorf("goodm: delete one failed: %w", err)
		}
		if result.DeletedCount == 0 {
			return ErrNotFound
		}

		return nil
	})
}

// --- helpers ---

// getSchemaForModel resolves the schema for a model instance from the registry.
func getSchemaForModel(model interface{}) (*Schema, error) {
	t := reflect.TypeOf(model)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice {
		t = t.Elem()
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
	}

	schema, ok := Get(t.Name())
	if !ok {
		return nil, fmt.Errorf("goodm: model %q is not registered", t.Name())
	}
	return schema, nil
}

// getModelID extracts the ID field from a model via reflection.
func getModelID(model interface{}) (bson.ObjectID, error) {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	idField := v.FieldByName("ID")
	if !idField.IsValid() {
		return bson.ObjectID{}, fmt.Errorf("goodm: model has no ID field")
	}
	id, ok := idField.Interface().(bson.ObjectID)
	if !ok {
		return bson.ObjectID{}, fmt.Errorf("goodm: ID field is not bson.ObjectID")
	}
	return id, nil
}

// setModelID sets the ID field on a model via reflection.
func setModelID(model interface{}, id bson.ObjectID) {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	idField := v.FieldByName("ID")
	if idField.IsValid() && idField.CanSet() {
		idField.Set(reflect.ValueOf(id))
	}
}

// setTimestamps sets CreatedAt (if zero) and UpdatedAt on a model via reflection.
func setTimestamps(model interface{}, now time.Time) {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if f := v.FieldByName("CreatedAt"); f.IsValid() && f.CanSet() {
		if f.Interface().(time.Time).IsZero() {
			f.Set(reflect.ValueOf(now))
		}
	}
	if f := v.FieldByName("UpdatedAt"); f.IsValid() && f.CanSet() {
		f.Set(reflect.ValueOf(now))
	}
}

// setUpdatedAt sets only UpdatedAt on a model via reflection.
func setUpdatedAt(model interface{}, now time.Time) {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if f := v.FieldByName("UpdatedAt"); f.IsValid() && f.CanSet() {
		f.Set(reflect.ValueOf(now))
	}
}

// getDB returns the provided database or falls back to the global DB().
func getDB(optDB *mongo.Database) (*mongo.Database, error) {
	if optDB != nil {
		return optDB, nil
	}
	db := DB()
	if db == nil {
		return nil, ErrNoDatabase
	}
	return db, nil
}

// validateImmutable checks that immutable fields have not changed between old and new.
func validateImmutable(old, new interface{}, schema *Schema) []ValidationError {
	var errs []ValidationError

	oldV := reflect.ValueOf(old)
	if oldV.Kind() == reflect.Ptr {
		oldV = oldV.Elem()
	}
	newV := reflect.ValueOf(new)
	if newV.Kind() == reflect.Ptr {
		newV = newV.Elem()
	}

	for _, field := range schema.Fields {
		if !field.Immutable {
			continue
		}
		oldField := oldV.FieldByName(field.Name)
		newField := newV.FieldByName(field.Name)
		if !oldField.IsValid() || !newField.IsValid() {
			continue
		}
		if !reflect.DeepEqual(oldField.Interface(), newField.Interface()) {
			errs = append(errs, ValidationError{
				Field:   field.BSONName,
				Message: "field is immutable and cannot be changed",
			})
		}
	}

	return errs
}

// hasImmutableFields returns true if any field in the schema is marked immutable.
func hasImmutableFields(schema *Schema) bool {
	for _, f := range schema.Fields {
		if f.Immutable {
			return true
		}
	}
	return false
}
