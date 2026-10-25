package goodm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/dwoolworth/goodm/internal"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// getCollection returns a *mongo.Collection for the schema, applying any
// per-schema read/write concern or read preference configured via the
// Configurable interface.
func getCollection(db *mongo.Database, schema *Schema) *mongo.Collection {
	opts := schema.CollOptions
	if opts.ReadPreference == nil && opts.ReadConcern == nil && opts.WriteConcern == nil {
		return db.Collection(schema.Collection)
	}
	collOpts := options.Collection()
	if opts.ReadPreference != nil {
		collOpts.SetReadPreference(opts.ReadPreference)
	}
	if opts.ReadConcern != nil {
		collOpts.SetReadConcern(opts.ReadConcern)
	}
	if opts.WriteConcern != nil {
		collOpts.SetWriteConcern(opts.WriteConcern)
	}
	return db.Collection(schema.Collection, collOpts)
}

// CreateOptions configures Create and CreateMany.
type CreateOptions struct {
	DB *mongo.Database

	// Unordered lets CreateMany continue inserting after a failure instead of
	// stopping at the first error. Failures are reported in a *BulkError.
	Unordered bool
}

// FindOptions configures Find, FindOne, FindCursor, FindByID, Count, Exists,
// Distinct, and FindOneAndDelete.
type FindOptions struct {
	DB         *mongo.Database
	Limit      int64
	Skip       int64
	Sort       bson.D
	Projection interface{}        // fields to include or exclude, e.g. bson.D{{Key: "email", Value: 1}}
	Hint       interface{}        // index name or key document to use
	Collation  *options.Collation // collation for string comparison
	BatchSize  int32              // documents per cursor batch (Find, FindCursor)
}

// UpdateOptions configures Update, UpdateFields, UpdateOne, UpdateMany, and
// the update operations of a Bulk.
type UpdateOptions struct {
	DB         *mongo.Database
	Unset      []string // Update only: bson field names to remove from the document
	MaxRetries int      // Update only: retry with 3-way merge on version conflict (0 = no retry)

	// Raw operations (UpdateOne, UpdateMany, Bulk) only:
	Upsert       bool               // insert a new document when the filter matches nothing
	ArrayFilters []interface{}      // filters for positional $[<identifier>] operators
	Hint         interface{}        // index name or key document to use
	Collation    *options.Collation // collation for string comparison
}

// UnsetFields returns UpdateOptions that will remove the specified fields from
// the MongoDB document. Field names should be bson names (e.g. "agent_id").
// After a successful save the fields are zeroed on the model as well. Update
// writes the whole struct, so a later Update stores the field's zero value
// again unless the field is tagged omitempty.
//
// Several UpdateOptions values may be passed to one call and are combined:
//
//	goodm.Update(ctx, &server, goodm.UnsetFields("agent_id"), goodm.WithRetry(2))
func UnsetFields(fields ...string) UpdateOptions {
	return UpdateOptions{Unset: fields}
}

// mergeUpdateOptions combines several UpdateOptions values: the first DB,
// Hint, and Collation set win, Unset lists and ArrayFilters are concatenated,
// MaxRetries is the largest given, and Upsert is set if any value sets it.
func mergeUpdateOptions(opts []UpdateOptions) UpdateOptions {
	var out UpdateOptions
	for _, o := range opts {
		if out.DB == nil {
			out.DB = o.DB
		}
		if out.Hint == nil {
			out.Hint = o.Hint
		}
		if out.Collation == nil {
			out.Collation = o.Collation
		}
		out.Unset = append(out.Unset, o.Unset...)
		out.ArrayFilters = append(out.ArrayFilters, o.ArrayFilters...)
		if o.MaxRetries > out.MaxRetries {
			out.MaxRetries = o.MaxRetries
		}
		out.Upsert = out.Upsert || o.Upsert
	}
	return out
}

// WithRetry returns UpdateOptions that will automatically retry on version
// conflict using a 3-way field-level merge. On conflict, the document is re-read
// from the database, and the caller's changed fields are merged onto the fresh
// state — but only if no other writer modified the same fields. If both sides
// changed the same field, a *MergeConflictError is returned.
//
// The caller's changes are those that differ from the document as it was last
// loaded or saved through goodm (FindOne, FindByID, Find, FindOneAndUpdate,
// FindOneAndReplace, Create, Update, UpdateFields, SaveMany), which goodm.Model
// remembers. A model built by hand, loaded through FindCursor, or without an
// embedded goodm.Model has no such base; a conflict then returns
// ErrVersionConflict without merging.
//
// Example:
//
//	goodm.Update(ctx, &task, goodm.WithRetry(3))
func WithRetry(maxRetries int) UpdateOptions {
	return UpdateOptions{MaxRetries: maxRetries}
}

// Upsert returns UpdateOptions that insert a new document when the filter
// matches nothing. For UpdateOne, UpdateMany, FindOneAndUpdate, and Bulk.
//
// Example:
//
//	goodm.UpdateOne(ctx, filter, update, &User{}, goodm.Upsert())
func Upsert() UpdateOptions {
	return UpdateOptions{Upsert: true}
}

// DeleteOptions configures Delete, DeleteOne, DeleteMany, and the delete
// operations of a Bulk.
type DeleteOptions struct {
	DB        *mongo.Database
	Hint      interface{}        // index name or key document to use
	Collation *options.Collation // collation for string comparison
}

// FindOneAndUpdateOptions configures FindOneAndUpdate and FindOneAndReplace.
type FindOneAndUpdateOptions struct {
	DB             *mongo.Database
	Upsert         bool               // insert when the filter matches nothing
	ReturnOriginal bool               // decode the document as it was before the update (default: after)
	Sort           bson.D             // which document to update when several match
	Projection     interface{}        // fields to decode into result
	ArrayFilters   []interface{}      // FindOneAndUpdate only
	Hint           interface{}        // index name or key document to use
	Collation      *options.Collation // collation for string comparison
}

// Create inserts a new document. It generates an ObjectID if the ID is zero,
// sets timestamps, applies defaults, runs BeforeCreate/AfterCreate hooks, and
// validates against the schema.
//
// Models whose _id is not a bson.ObjectID must set it themselves, either
// before calling Create or in a BeforeCreate hook.
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

		if err := prepareCreate(ctx, schema, model, time.Now()); err != nil {
			return err
		}

		coll := getCollection(db, schema)
		if _, err := coll.InsertOne(ctx, model); err != nil {
			return translateWriteError(err, schema, "insert failed")
		}
		recordBaseAfterWrite(ctx, schema, model, nil)

		// AfterCreate hook
		if hook, ok := model.(AfterCreate); ok {
			if err := hook.AfterCreate(ctx); err != nil {
				return err
			}
		}

		return nil
	})
}

// prepareCreate runs the pre-insert lifecycle on a model: ID generation,
// timestamps, defaults, version, BeforeCreate, and validation.
func prepareCreate(ctx context.Context, schema *Schema, model interface{}, now time.Time) error {
	if err := assignObjectID(schema, model); err != nil {
		return err
	}
	setTimestamps(model, now)
	if err := applyDefaults(model, schema); err != nil {
		return err
	}
	setModelVersion(model, 0)

	if hook, ok := model.(BeforeCreate); ok {
		if err := hook.BeforeCreate(ctx); err != nil {
			return err
		}
	}

	if err := requireID(schema, model); err != nil {
		return err
	}
	if errs := Validate(model, schema); len(errs) > 0 {
		return ValidationErrors(errs)
	}
	return nil
}

// FindOne finds a single document matching filter and decodes it into result.
// Returns ErrNotFound if no document matches.
func FindOne(ctx context.Context, filter interface{}, result interface{}, opts ...FindOptions) error {
	schema, err := getSchemaForModel(result)
	if err != nil {
		return err
	}

	info := &OpInfo{
		Operation: OpFind, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: result, Filter: filter,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt FindOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		coll := getCollection(db, schema)
		res := coll.FindOne(ctx, info.Filter, findOneOptions(opt))
		if err := res.Decode(result); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return ErrNotFound
			}
			return fmt.Errorf("goodm: find one failed: %w", err)
		}
		if raw, err := res.Raw(); err == nil {
			setModelBase(schema, result, raw)
		}

		return nil
	})
}

// FindByID finds the document with the given _id and decodes it into result.
// When the model's ID is a bson.ObjectID, id may also be its hex string.
// Returns ErrNotFound if no document matches.
func FindByID(ctx context.Context, id interface{}, result interface{}, opts ...FindOptions) error {
	schema, err := getSchemaForModel(result)
	if err != nil {
		return err
	}
	id, err = coerceID(schema, id)
	if err != nil {
		return err
	}
	return FindOne(ctx, bson.D{{Key: "_id", Value: id}}, result, opts...)
}

// Find finds all documents matching filter and decodes them into results.
// results must be a pointer to a slice (e.g. *[]User).
func Find(ctx context.Context, filter interface{}, results interface{}, opts ...FindOptions) error {
	// results must be *[]T
	rv := reflect.ValueOf(results)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("goodm: results must be a pointer to a slice, got %T", results)
	}

	schema, err := getSchemaForModel(results)
	if err != nil {
		return err
	}

	info := &OpInfo{
		Operation: OpFind, Collection: schema.Collection,
		ModelName: schema.ModelName, Filter: filter,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt FindOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		coll := getCollection(db, schema)
		cursor, err := coll.Find(ctx, info.Filter, findOptions(opt))
		if err != nil {
			return fmt.Errorf("goodm: find failed: %w", err)
		}
		defer func() { _ = cursor.Close(ctx) }()

		return decodeAll(ctx, cursor, schema, rv.Elem())
	})
}

// decodeAll decodes every document of a cursor into the slice value (of
// element type T or *T), recording each document's raw form as the model's
// merge base.
func decodeAll(ctx context.Context, cursor *mongo.Cursor, schema *Schema, slice reflect.Value) error {
	elemType := slice.Type().Elem()
	isPtr := elemType.Kind() == reflect.Pointer
	if isPtr {
		elemType = elemType.Elem()
	}
	out := reflect.MakeSlice(slice.Type(), 0, 0)
	for cursor.Next(ctx) {
		elem := reflect.New(elemType)
		if err := cursor.Decode(elem.Interface()); err != nil {
			return fmt.Errorf("goodm: cursor decode failed: %w", err)
		}
		setModelBase(schema, elem.Interface(), copyRaw(cursor.Current))
		if isPtr {
			out = reflect.Append(out, elem)
		} else {
			out = reflect.Append(out, elem.Elem())
		}
	}
	if err := cursor.Err(); err != nil {
		return fmt.Errorf("goodm: cursor failed: %w", err)
	}
	slice.Set(out)
	return nil
}

// FindCursor returns a raw *mongo.Cursor for streaming large result sets.
// The model parameter is used only for schema/collection lookup (e.g. &User{}).
func FindCursor(ctx context.Context, filter interface{}, model interface{}, opts ...FindOptions) (*mongo.Cursor, error) {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return nil, err
	}

	var cursor *mongo.Cursor
	info := &OpInfo{
		Operation: OpFind, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: filter,
	}
	err = runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt FindOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		coll := getCollection(db, schema)
		c, err := coll.Find(ctx, info.Filter, findOptions(opt))
		if err != nil {
			return fmt.Errorf("goodm: find cursor failed: %w", err)
		}
		cursor = c
		return nil
	})

	return cursor, err
}

// Count returns the number of documents matching filter. Limit, Skip, Hint,
// and Collation from FindOptions apply.
func Count(ctx context.Context, filter interface{}, model interface{}, opts ...FindOptions) (int64, error) {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return 0, err
	}

	var n int64
	info := &OpInfo{
		Operation: OpFind, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: filter,
	}
	err = runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt FindOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}
		countOpts := options.Count()
		if opt.Limit > 0 {
			countOpts.SetLimit(opt.Limit)
		}
		if opt.Skip > 0 {
			countOpts.SetSkip(opt.Skip)
		}
		if opt.Hint != nil {
			countOpts.SetHint(opt.Hint)
		}
		if opt.Collation != nil {
			countOpts.SetCollation(opt.Collation)
		}
		coll := getCollection(db, schema)
		c, err := coll.CountDocuments(ctx, info.Filter, countOpts)
		if err != nil {
			return fmt.Errorf("goodm: count failed: %w", err)
		}
		n = c
		return nil
	})
	return n, err
}

// Exists reports whether at least one document matches filter.
func Exists(ctx context.Context, filter interface{}, model interface{}, opts ...FindOptions) (bool, error) {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return false, err
	}

	var found bool
	info := &OpInfo{
		Operation: OpFind, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: filter,
	}
	err = runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt FindOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}
		opt.Projection = bson.D{{Key: "_id", Value: 1}}
		coll := getCollection(db, schema)
		var doc bson.Raw
		err = coll.FindOne(ctx, info.Filter, findOneOptions(opt)).Decode(&doc)
		if err == nil {
			found = true
			return nil
		}
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil
		}
		return fmt.Errorf("goodm: exists failed: %w", err)
	})
	return found, err
}

// Distinct finds the distinct values of a bson field among the documents
// matching filter and decodes them into results, a pointer to a slice
// (e.g. *[]string).
func Distinct(ctx context.Context, field string, filter interface{}, model interface{}, results interface{}, opts ...FindOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	info := &OpInfo{
		Operation: OpFind, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: filter,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt FindOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}
		distinctOpts := options.Distinct()
		if opt.Collation != nil {
			distinctOpts.SetCollation(opt.Collation)
		}
		if opt.Hint != nil {
			distinctOpts.SetHint(opt.Hint)
		}
		coll := getCollection(db, schema)
		if err := coll.Distinct(ctx, field, info.Filter, distinctOpts).Decode(results); err != nil {
			return fmt.Errorf("goodm: distinct failed: %w", err)
		}
		return nil
	})
}

// Update replaces an existing document. It fetches the current document to enforce
// immutable fields, runs BeforeSave/AfterSave hooks, validates, and sets UpdatedAt.
func Update(ctx context.Context, model interface{}, opts ...UpdateOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	id, err := modelID(schema, model)
	if err != nil {
		return err
	}
	if isZeroValue(id) {
		return fmt.Errorf("goodm: cannot update document with zero ID")
	}

	opt := mergeUpdateOptions(opts)

	// Validate unset fields against the schema before proceeding.
	if err := validateUnsetFields(schema, opt.Unset); err != nil {
		return err
	}

	// Middleware may replace info.Filter (e.g. to scope by tenant); it is
	// ANDed with the _id and version filters of every read and write below.
	base := bson.D{{Key: "_id", Value: id}}
	info := &OpInfo{
		Operation: OpUpdate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: base,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		coll := getCollection(db, schema)
		scope := middlewareScope(info, base)

		if err := checkImmutableFields(ctx, coll, scope, id, model, schema); err != nil {
			return err
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

		// Save with optional retry-with-merge on version conflict.
		if err := saveWithRetry(ctx, coll, schema, model, opt, id, scope); err != nil {
			return err
		}
		if len(opt.Unset) > 0 {
			// The stored document lacks the unset fields: zero them on the
			// model (a later Update then writes their zero value, or omits
			// them if they are omitempty).
			zeroUnsetFields(model, opt.Unset)
		}
		recordBaseAfterWrite(ctx, schema, model, opt.Unset)

		// AfterSave hook
		if hook, ok := model.(AfterSave); ok {
			if err := hook.AfterSave(ctx); err != nil {
				return err
			}
		}

		return nil
	})
}

// checkImmutableFields verifies that immutable fields have not been modified.
// Skips the check entirely if no fields are marked immutable.
func checkImmutableFields(ctx context.Context, coll *mongo.Collection, scope interface{}, id interface{}, model interface{}, schema *Schema) error {
	if !hasImmutableFields(schema) {
		return nil
	}
	existing := reflect.New(reflect.TypeOf(model).Elem()).Interface()
	if err := coll.FindOne(ctx, andFilter(scope, bson.D{{Key: "_id", Value: id}})).Decode(existing); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return ErrNotFound
		}
		return fmt.Errorf("goodm: failed to fetch existing document: %w", err)
	}
	if immutableErrs := validateImmutable(existing, model, schema); len(immutableErrs) > 0 {
		return ValidationErrors(immutableErrs)
	}
	return nil
}

// buildVersionFilter constructs a filter with optimistic concurrency version checking.
// When oldVersion == 0, also matches documents without __v (legacy compat).
func buildVersionFilter(id interface{}, oldVersion int) bson.D {
	if oldVersion == 0 {
		return bson.D{
			{Key: "_id", Value: id},
			{Key: "$or", Value: bson.A{
				bson.D{{Key: "__v", Value: 0}},
				bson.D{{Key: "__v", Value: bson.D{{Key: "$exists", Value: false}}}},
			}},
		}
	}
	return bson.D{
		{Key: "_id", Value: id},
		{Key: "__v", Value: oldVersion},
	}
}

// andFilter ANDs a middleware-supplied scope filter into filter. A nil,
// typed-nil, or empty scope leaves filter unchanged.
func andFilter(scope interface{}, filter bson.D) interface{} {
	if isEmptyFilter(scope) {
		return filter
	}
	return bson.D{{Key: "$and", Value: bson.A{scope, filter}}}
}

// isEmptyFilter reports whether a filter value is nil, a typed nil, or an
// empty document.
func isEmptyFilter(f interface{}) bool {
	if f == nil {
		return true
	}
	v := reflect.ValueOf(f)
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.IsNil() || v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	}
	return false
}

// middlewareScope returns the filter middleware left in info, or nil when it
// is still the base filter the operation started with, so an untouched
// operation keeps its plain _id query.
func middlewareScope(info *OpInfo, base bson.D) interface{} {
	if reflect.DeepEqual(info.Filter, base) {
		return nil
	}
	return info.Filter
}

// checkUpdateConflict disambiguates between a missing (or out-of-scope)
// document and a version conflict when an update matched zero documents.
func checkUpdateConflict(ctx context.Context, coll *mongo.Collection, scope interface{}, id interface{}) error {
	count, err := coll.CountDocuments(ctx, andFilter(scope, bson.D{{Key: "_id", Value: id}}))
	if err != nil {
		return fmt.Errorf("goodm: update failed: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return ErrVersionConflict
}

// UpdateFields performs a partial $set update on specific fields of a document,
// identified by the model's ID. It runs middleware, sets UpdatedAt, and increments
// the version — but does NOT enforce optimistic locking (last-write-wins).
//
// Use this instead of Update when concurrent writers touch disjoint fields and
// version conflicts are acceptable (e.g. progress tracking, heartbeats).
//
// fields maps bson field names to their new values. Dotted paths into
// subdocuments and array elements are allowed ("address.city", "items.0.qty").
// Each value is converted to the field's Go type (see convertValue for the
// conversions allowed) and validated against the field's schema (required,
// enum, min, max); the path must exist on the model, so an index past the end
// of a slice is rejected. Immutable fields are rejected. The caller's map is
// not modified.
//
//	err := goodm.UpdateFields(ctx, &task, bson.M{"step": 5, "tokens_used": 12000})
func UpdateFields(ctx context.Context, model interface{}, fields bson.M, opts ...UpdateOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	id, err := modelID(schema, model)
	if err != nil {
		return err
	}
	if isZeroValue(id) {
		return fmt.Errorf("goodm: cannot update document with zero ID")
	}

	values, stored, err := prepareUpdateFields(schema, model, fields)
	if err != nil {
		return err
	}

	base := bson.D{{Key: "_id", Value: id}}
	info := &OpInfo{
		Operation: OpUpdate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: base,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		db, err := getDB(mergeUpdateOptions(opts).DB)
		if err != nil {
			return err
		}

		// Send the converted values (not the caller's map, which is left
		// untouched) so the stored document matches what the model will hold.
		set := make(bson.M, len(stored)+1)
		for k, v := range stored {
			set[k] = v
		}
		now := time.Now()
		set["updated_at"] = now
		oldVersion, _ := getModelVersion(model)

		coll := getCollection(db, schema)
		result, err := coll.UpdateOne(ctx, andFilter(middlewareScope(info, base), base), bson.D{
			{Key: "$set", Value: set},
			{Key: "$inc", Value: bson.D{{Key: "__v", Value: 1}}},
		})
		if err != nil {
			return translateWriteError(err, schema, "update fields failed")
		}
		if result.MatchedCount == 0 {
			return ErrNotFound
		}

		// Reflect the changes back onto the struct
		setUpdatedAt(model, now)
		setModelVersion(model, oldVersion+1)
		applyFieldsToModel(model, values)
		if inTransaction(ctx) {
			setModelBase(schema, model, nil)
		} else {
			advanceBaseAfterUpdateFields(schema, model, values, now, oldVersion+1)
		}

		return nil
	})
}

// advanceBaseAfterUpdateFields moves the model's merge base forward by the
// fields UpdateFields wrote, starting from the old base rather than from the
// model, so edits the caller made in memory but did not save are still seen
// as changes by a later WithRetry merge. A model with no base keeps none.
func advanceBaseAfterUpdateFields(schema *Schema, model interface{}, values map[string]reflect.Value, now time.Time, version int) {
	old := modelBase(schema, model)
	if old == nil {
		return
	}
	stored := reflect.New(schema.Type)
	if err := bson.Unmarshal(old, stored.Interface()); err != nil {
		setModelBase(schema, model, nil)
		return
	}
	if !applyFieldsToModel(stored.Interface(), values) {
		// The stored document was shaped differently (e.g. the caller wrote
		// through an index its stored copy did not have): the base can no
		// longer be reconstructed, so drop it rather than keep a wrong one.
		setModelBase(schema, model, nil)
		return
	}
	setUpdatedAt(stored.Interface(), now)
	setModelVersion(stored.Interface(), version)
	raw, err := bson.Marshal(stored.Interface())
	if err != nil {
		setModelBase(schema, model, nil)
		return
	}
	setModelBase(schema, model, raw)
}

// prepareUpdateFields checks that every key in fields is a known, mutable,
// non-managed schema field whose path exists on the model, converts each
// value to the field's Go type, and validates it. The returned values are
// copied onto the model after the write; the matching stored values are
// returned separately (a nil stays a BSON null).
func prepareUpdateFields(schema *Schema, model interface{}, fields bson.M) (values map[string]reflect.Value, stored bson.M, err error) {
	var errs ValidationErrors
	values = make(map[string]reflect.Value, len(fields))
	stored = make(bson.M, len(fields))
	root := reflect.ValueOf(model)
	if root.Kind() == reflect.Pointer {
		root = root.Elem()
	}
	for name, val := range fields {
		if managedFields[name] {
			return nil, nil, fmt.Errorf("goodm: cannot set managed field %q via UpdateFields", name)
		}
		fs, immutable, isElement := resolveFieldPath(schema, name)
		if fs == nil {
			return nil, nil, fmt.Errorf("goodm: unknown field %q in %s", name, schema.ModelName)
		}
		if immutable {
			return nil, nil, ValidationErrors{{Field: name, Message: "field is immutable and cannot be changed"}}
		}
		if _, ok := walkPath(root, name); !ok {
			errs = append(errs, ValidationError{Field: name, Message: "path does not exist on the model (index out of range or nil subdocument)"})
			continue
		}
		target := fs.goType
		if isElement {
			target = target.Elem()
		}
		v, err := convertValue(val, target)
		if err != nil {
			errs = append(errs, ValidationError{Field: name, Message: fmt.Sprintf("cannot assign %T to %s: %v", val, target, err)})
			continue
		}
		if isElement {
			errs = append(errs, validateSliceElement(v, fs, name)...)
			if len(fs.Enum) > 0 && !v.IsZero() {
				if e := validateEnum(v, fs.Enum, name); e != nil {
					errs = append(errs, *e)
				}
			}
		} else {
			errs = append(errs, validateOneField(v, fs, name)...)
		}
		values[name] = v
		if isNilValue(val) {
			stored[name] = nil
		} else {
			stored[name] = v.Interface()
		}
	}
	if len(errs) > 0 {
		return nil, nil, errs
	}
	return values, stored, nil
}

// isNilValue reports whether val is nil or a typed nil pointer/interface.
func isNilValue(val interface{}) bool {
	if val == nil {
		return true
	}
	rv := reflect.ValueOf(val)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
		return rv.IsNil()
	}
	return false
}

// walkPath follows a dotted path from root and reports whether it addresses
// a settable value. A segment declared by a nil inline pointer embed is still
// a top-level key of the document, so from there the rest of the path is
// checked against the types alone; applyFieldsToModel allocates the embed
// after the write.
func walkPath(root reflect.Value, path string) (reflect.Value, bool) {
	segs := strings.Split(path, ".")
	target := root
	for i, seg := range segs {
		next, ok := stepInto(target, seg)
		if ok {
			target = next
			continue
		}
		cur := target
		if cur.Kind() == reflect.Pointer && !cur.IsNil() {
			cur = cur.Elem()
		}
		if cur.Kind() == reflect.Struct && typePathExists(cur.Type(), segs[i:]) {
			return reflect.Value{}, true
		}
		return reflect.Value{}, false
	}
	return target, target.IsValid() && target.CanSet()
}

// typePathExists reports whether a dotted path can be resolved on a type
// whose value is entirely zero (it sits behind a nil inline embed), meaning
// none of its fields exist in the stored document either. The path may only
// pass through struct values and inline embeds: a numeric segment on an
// absent array makes the server create an object in its place, and a nil
// pointer cannot be descended into. The last segment may name anything.
func typePathExists(t reflect.Type, segs []string) bool {
	for i, seg := range segs {
		last := i == len(segs)-1
		if t.Kind() == reflect.Pointer {
			if i > 0 {
				return false // a nil pointer on the path
			}
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return false // arrays, slices, maps, scalars: absent, nothing to descend into
		}
		var found *reflect.StructField
		for _, f := range internal.StructFields(t) {
			if bsonFieldName(f) == seg {
				f := f
				found = &f
				break
			}
		}
		if found == nil {
			return false
		}
		t = found.Type
		if !last && t.Kind() == reflect.Pointer {
			return false
		}
	}
	return true
}

// convertValue converts a caller-supplied value to a field's Go type. Only
// conversions that preserve meaning are allowed: a value of the right type;
// numbers to numbers of another width when the value is representable;
// documents (structs, bson.D, bson.M, string-keyed maps) to structs and
// maps, matched by bson name with undeclared keys dropped; arrays to slices
// and arrays, element by element; hex strings to ObjectIDs; and anything
// into an interface{} field. nil becomes the zero value.
func convertValue(val interface{}, t reflect.Type) (reflect.Value, error) {
	if isNilValue(val) {
		return reflect.Zero(t), nil
	}
	rv := reflect.ValueOf(val)
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return reflect.Zero(t), nil // a pointer chain ending in nil is nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return reflect.Zero(t), nil
	}

	if t.Kind() == reflect.Pointer {
		inner, err := convertValue(val, t.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		ptr := reflect.New(t.Elem())
		ptr.Elem().Set(inner)
		return ptr, nil
	}
	if (rv.Kind() == reflect.Float32 || rv.Kind() == reflect.Float64) && math.IsNaN(rv.Float()) {
		return reflect.Value{}, fmt.Errorf("NaN is not a value")
	}
	if rv.Type().AssignableTo(t) {
		return rv, nil
	}
	if t.Kind() == reflect.Interface {
		return rv, nil
	}

	switch valueClass(t) {
	case classNumber:
		if valueClass(rv.Type()) != classNumber {
			return reflect.Value{}, fmt.Errorf("not a number")
		}
		return convertNumber(rv, t)
	case classBSOND:
		pairs, err := documentPairs(rv)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(pairs).Convert(t), nil
	case classString:
		if rv.Kind() != reflect.String {
			return reflect.Value{}, fmt.Errorf("not a string")
		}
		return rv.Convert(t), nil
	case classBool:
		if rv.Kind() != reflect.Bool {
			return reflect.Value{}, fmt.Errorf("not a bool")
		}
		return rv.Convert(t), nil
	case classObjectID:
		if rv.Kind() == reflect.String {
			oid, err := bson.ObjectIDFromHex(rv.String())
			if err != nil {
				return reflect.Value{}, err
			}
			return reflect.ValueOf(oid).Convert(t), nil
		}
		if valueClass(rv.Type()) == classObjectID {
			return rv.Convert(t), nil
		}
		return reflect.Value{}, fmt.Errorf("not an ObjectID")
	case classTime:
		if rv.Type() == dateTimeType {
			return reflect.ValueOf(rv.Interface().(bson.DateTime).Time()).Convert(t), nil
		}
		if valueClass(rv.Type()) == classTime && rv.Type().ConvertibleTo(t) {
			return rv.Convert(t), nil
		}
		return reflect.Value{}, fmt.Errorf("not a time.Time")
	case classBinary:
		if t == bsonBinaryType && rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			return reflect.ValueOf(bson.Binary{Data: rv.Bytes()}), nil
		}
		if rv.Type() == bsonBinaryType {
			bin := rv.Interface().(bson.Binary)
			if t == bsonBinaryType {
				return rv, nil
			}
			if bin.Subtype != 0 && bin.Subtype != 2 {
				return reflect.Value{}, fmt.Errorf("binary subtype %d does not convert to %s", bin.Subtype, t)
			}
			if !reflect.TypeOf(bin.Data).ConvertibleTo(t) {
				return reflect.Value{}, fmt.Errorf("binary data does not convert to %s", t)
			}
			return reflect.ValueOf(bin.Data).Convert(t), nil
		}
		if valueClass(rv.Type()) == classBinary && rv.Type().ConvertibleTo(t) {
			return rv.Convert(t), nil
		}
		return reflect.Value{}, fmt.Errorf("not a byte slice")
	case classDecimal:
		if valueClass(rv.Type()) == classDecimal && rv.Type().ConvertibleTo(t) {
			return rv.Convert(t), nil
		}
		return reflect.Value{}, fmt.Errorf("not a Decimal128")
	case classDocument:
		return convertDocument(rv, t)
	case classArray:
		return convertArray(rv, t)
	}
	return reflect.Value{}, fmt.Errorf("unsupported field type %s", t)
}

type typeClass int

const (
	classOther typeClass = iota
	classNumber
	classString
	classBool
	classTime
	classObjectID
	classDecimal
	classBinary
	classDocument
	classArray
	classBSOND // a bson.D field: an ordered document that accepts any document source
)

var (
	timeType       = reflect.TypeOf(time.Time{})
	dateTimeType   = reflect.TypeOf(bson.DateTime(0))
	objectIDType   = reflect.TypeOf(bson.ObjectID{})
	decimalType    = reflect.TypeOf(bson.Decimal128{})
	bsonBinaryType = reflect.TypeOf(bson.Binary{})
	bsonDType      = reflect.TypeOf(bson.D{})
	bsonEType      = reflect.TypeOf(bson.E{})
	bsonRawType    = reflect.TypeOf(bson.Raw{})
)

// bsonScalarStructs are struct types that encode as BSON scalars, so they
// are never documents.
var bsonScalarStructs = map[reflect.Type]bool{
	reflect.TypeOf(bson.Regex{}):         true,
	reflect.TypeOf(bson.Timestamp{}):     true,
	reflect.TypeOf(bson.Undefined{}):     true,
	reflect.TypeOf(bson.Null{}):          true,
	reflect.TypeOf(bson.MinKey{}):        true,
	reflect.TypeOf(bson.MaxKey{}):        true,
	reflect.TypeOf(bson.CodeWithScope{}): true,
	reflect.TypeOf(bson.DBPointer{}):     true,
}

// valueClass groups Go types by the BSON shape they take.
func valueClass(t reflect.Type) typeClass {
	if bsonScalarStructs[t] {
		return classOther
	}
	switch {
	case t == timeType, t == dateTimeType:
		return classTime
	case t == objectIDType:
		return classObjectID
	case t == decimalType:
		return classDecimal
	case t == bsonBinaryType || (t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8):
		return classBinary
	case t == bsonDType:
		return classBSOND
	case t == bsonRawType:
		return classDocument
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return classNumber
	case reflect.String:
		return classString
	case reflect.Bool:
		return classBool
	case reflect.Struct, reflect.Map:
		return classDocument
	case reflect.Slice:
		if t.Elem() == bsonEType {
			return classBSOND
		}
		return classArray
	case reflect.Array:
		return classArray
	}
	return classOther
}

// convertNumber converts between numeric kinds, rejecting values the target
// cannot represent: fractions into integers, negatives into unsigned,
// overflow, and infinities.
func convertNumber(rv reflect.Value, t reflect.Type) (reflect.Value, error) {
	out := reflect.New(t).Elem()
	switch {
	case out.CanInt():
		var n int64
		switch {
		case rv.CanInt():
			n = rv.Int()
		case rv.CanUint():
			if rv.Uint() > math.MaxInt64 {
				return reflect.Value{}, fmt.Errorf("value overflows %s", t)
			}
			n = int64(rv.Uint())
		default:
			f := rv.Float()
			if f != math.Trunc(f) || math.IsInf(f, 0) || math.IsNaN(f) {
				return reflect.Value{}, fmt.Errorf("value %v is not an integer", f)
			}
			if f < -0x1p63 || f >= 0x1p63 { // float64(MaxInt64) rounds up to 2^63
				return reflect.Value{}, fmt.Errorf("value overflows %s", t)
			}
			n = int64(f)
		}
		if out.OverflowInt(n) {
			return reflect.Value{}, fmt.Errorf("value %d overflows %s", n, t)
		}
		out.SetInt(n)
	case out.CanUint():
		var n uint64
		switch {
		case rv.CanUint():
			n = rv.Uint()
		case rv.CanInt():
			if rv.Int() < 0 {
				return reflect.Value{}, fmt.Errorf("value %d is negative", rv.Int())
			}
			n = uint64(rv.Int())
		default:
			f := rv.Float()
			if f != math.Trunc(f) || math.IsInf(f, 0) || math.IsNaN(f) || f < 0 {
				return reflect.Value{}, fmt.Errorf("value %v is not a non-negative integer", f)
			}
			if f >= 0x1p64 {
				return reflect.Value{}, fmt.Errorf("value overflows %s", t)
			}
			n = uint64(f)
		}
		if out.OverflowUint(n) {
			return reflect.Value{}, fmt.Errorf("value %d overflows %s", n, t)
		}
		out.SetUint(n)
	default: // float target
		var f float64
		switch {
		case rv.CanInt():
			f = float64(rv.Int())
			if f >= 0x1p63 || int64(f) != rv.Int() {
				return reflect.Value{}, fmt.Errorf("value %d is not exactly representable as %s", rv.Int(), t)
			}
		case rv.CanUint():
			f = float64(rv.Uint())
			if f >= 0x1p64 || uint64(f) != rv.Uint() {
				return reflect.Value{}, fmt.Errorf("value %d is not exactly representable as %s", rv.Uint(), t)
			}
		default:
			f = rv.Float()
			if math.IsNaN(f) {
				return reflect.Value{}, fmt.Errorf("NaN is not a value")
			}
		}
		if out.OverflowFloat(f) {
			return reflect.Value{}, fmt.Errorf("value %v overflows %s", f, t)
		}
		if t.Kind() == reflect.Float32 && (rv.CanInt() || rv.CanUint()) && float64(float32(f)) != f {
			return reflect.Value{}, fmt.Errorf("value %v is not exactly representable as %s", f, t)
		}
		out.SetFloat(f)
	}
	return out, nil
}

// convertDocument converts a struct, bson.D, bson.M, or string-keyed map into
// a struct or map type by bson field name.
func convertDocument(rv reflect.Value, t reflect.Type) (reflect.Value, error) {
	pairs, err := documentPairs(rv)
	if err != nil {
		return reflect.Value{}, err
	}
	out := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.Struct:
		for _, p := range pairs {
			f := fieldByBSONName(out, p.Key, true)
			if !f.IsValid() || !f.CanSet() {
				continue // undeclared key: dropped, as Update drops it
			}
			v, err := convertValue(p.Value, f.Type())
			if err != nil {
				return reflect.Value{}, fmt.Errorf("%s: %w", p.Key, err)
			}
			f.Set(v)
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return reflect.Value{}, fmt.Errorf("map keys must be strings")
		}
		out = reflect.MakeMapWithSize(t, len(pairs))
		for _, p := range pairs {
			v, err := convertValue(p.Value, t.Elem())
			if err != nil {
				return reflect.Value{}, fmt.Errorf("%s: %w", p.Key, err)
			}
			out.SetMapIndex(reflect.ValueOf(p.Key).Convert(t.Key()), v)
		}
	default:
		return reflect.Value{}, fmt.Errorf("not a document type")
	}
	return out, nil
}

// documentPairs flattens a document-shaped value into key/value pairs. Struct
// sources are read field by field through reflection (not marshaled), so
// their values keep their Go types.
func documentPairs(rv reflect.Value) (bson.D, error) {
	switch {
	case rv.Type() == bsonDType || (rv.Kind() == reflect.Slice && rv.Type().Elem() == bsonEType):
		out := make(bson.D, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface().(bson.E)
		}
		return out, nil
	case rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String:
		out := make(bson.D, 0, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out = append(out, bson.E{Key: iter.Key().String(), Value: iter.Value().Interface()})
		}
		return out, nil
	case rv.Type() == bsonRawType:
		var out bson.D
		if err := bson.Unmarshal(rv.Interface().(bson.Raw), &out); err != nil {
			return nil, err
		}
		return out, nil
	case rv.Kind() == reflect.Struct && valueClass(rv.Type()) == classDocument:
		return structPairs(rv), nil
	}
	return nil, fmt.Errorf("not a document")
}

// structPairs lists a struct's fields by bson name as the driver would
// encode them: exported fields only, inline embeds flattened, fields tagged
// "-" skipped, and omitempty fields skipped when zero.
func structPairs(v reflect.Value) bson.D {
	var out bson.D
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		f := v.Field(i)
		if internal.IsInlineStruct(sf) {
			if f.Kind() == reflect.Pointer {
				if f.IsNil() {
					continue
				}
				f = f.Elem()
			}
			out = append(out, structPairs(f)...)
			continue
		}
		if bsonTagInline(sf.Tag.Get("bson")) && f.Kind() == reflect.Map && f.Type().Key().Kind() == reflect.String {
			// An inline map's entries are stored beside the struct's fields.
			iter := f.MapRange()
			for iter.Next() {
				out = append(out, bson.E{Key: iter.Key().String(), Value: iter.Value().Interface()})
			}
			continue
		}
		name := bsonFieldName(sf)
		if name == "-" {
			continue
		}
		if internal.HasOmitEmpty(sf.Tag.Get("bson")) && f.IsZero() {
			continue
		}
		out = append(out, bson.E{Key: name, Value: f.Interface()})
	}
	return out
}

// convertArray converts a slice or array element by element. A fixed-size
// target array accepts at most its length and is zero-padded.
func convertArray(rv reflect.Value, t reflect.Type) (reflect.Value, error) {
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return reflect.Value{}, fmt.Errorf("not an array")
	}
	if rv.Type() == bsonDType || (rv.Kind() == reflect.Slice && rv.Type().Elem() == bsonEType) {
		return reflect.Value{}, fmt.Errorf("a document is not an array")
	}
	n := rv.Len()
	var out reflect.Value
	switch t.Kind() {
	case reflect.Slice:
		out = reflect.MakeSlice(t, n, n)
	case reflect.Array:
		if n > t.Len() {
			return reflect.Value{}, fmt.Errorf("%d elements do not fit in %s", n, t)
		}
		out = reflect.New(t).Elem()
	default:
		return reflect.Value{}, fmt.Errorf("not an array type")
	}
	for i := 0; i < n; i++ {
		v, err := convertValue(rv.Index(i).Interface(), t.Elem())
		if err != nil {
			return reflect.Value{}, fmt.Errorf("[%d]: %w", i, err)
		}
		out.Index(i).Set(v)
	}
	return out, nil
}

// validateSliceElement validates one element of a slice field: a subdocument
// element is validated by its inner fields. Scalar elements are subject only
// to enum, which the caller applies; min and max are length rules for the
// whole slice.
func validateSliceElement(v reflect.Value, fs *FieldSchema, path string) []ValidationError {
	if len(fs.SubFields) == 0 {
		return nil
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	return validateFields(v, fs.SubFields, path)
}

// resolveFieldPath walks a dotted bson path through the schema. A numeric
// segment after a slice or array field selects one element. It returns the
// leaf field, whether the leaf or any ancestor is immutable, and whether the
// path ends on an element rather than the field itself; the field is nil if
// the path is unknown.
func resolveFieldPath(schema *Schema, path string) (field *FieldSchema, immutable, isElement bool) {
	fields := schema.Fields
	var leaf *FieldSchema
	for _, seg := range strings.Split(path, ".") {
		if n, err := strconv.Atoi(seg); err == nil && leaf != nil && !isElement && isSliceField(leaf) {
			if n < 0 {
				return nil, false, false
			}
			isElement = true
			continue
		}
		if fields == nil {
			return nil, false, false
		}
		var next *FieldSchema
		for i := range fields {
			if fields[i].BSONName == seg {
				next = &fields[i]
				break
			}
		}
		if next == nil {
			return nil, false, false
		}
		leaf = next
		isElement = false
		immutable = immutable || next.Immutable
		fields = next.SubFields
	}
	return leaf, immutable, isElement
}

// isSliceField reports whether a schema field holds a slice or array.
func isSliceField(fs *FieldSchema) bool {
	if fs.goType == nil {
		return fs.IsSlice || strings.HasPrefix(fs.Type, "[]")
	}
	k := fs.goType.Kind()
	return k == reflect.Slice || k == reflect.Array
}

// applyFieldsToModel copies the converted values onto the Go struct so the
// caller sees the updated state without a re-read. Dotted paths are followed
// through nested structs, pointers, and slice indices; a path that cannot be
// reached on the current model (e.g. an index past the end of a slice) is
// skipped, and the result reports whether every path was applied.
func applyFieldsToModel(model interface{}, values map[string]reflect.Value) bool {
	root := reflect.ValueOf(model)
	if root.Kind() == reflect.Pointer {
		root = root.Elem()
	}
	all := true
	for path, val := range values {
		target := root
		ok := true
		for _, seg := range strings.Split(path, ".") {
			target, ok = stepIntoAlloc(target, seg)
			if !ok {
				break
			}
		}
		if !ok || !target.CanSet() || !val.Type().AssignableTo(target.Type()) {
			all = false
			continue
		}
		target.Set(val)
	}
	return all
}

// stepIntoAlloc is stepInto for a write that has already happened: a nil
// inline pointer embed declaring the segment is allocated so the value can
// be copied onto the model.
func stepIntoAlloc(v reflect.Value, seg string) (reflect.Value, bool) {
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		f := fieldByBSONName(v, seg, true)
		return f, f.IsValid()
	}
	return stepInto(v, seg)
}

// stepInto resolves one path segment from v: a bson field name on a struct or
// a numeric index on a slice/array. Nil pointers end the walk.
func stepInto(v reflect.Value, seg string) (reflect.Value, bool) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return v, false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		f := fieldByBSONName(v, seg, false)
		return f, f.IsValid()
	case reflect.Slice, reflect.Array:
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= v.Len() {
			return v, false
		}
		return v.Index(i), true
	}
	return v, false
}

// fieldByBSONName finds the struct field the driver stores under name: a
// direct field first, then fields flattened from inline struct fields, which
// is the driver's own precedence. With alloc set, a nil inline pointer is
// allocated when it declares the name; otherwise the walk stops there.
func fieldByBSONName(v reflect.Value, name string, alloc bool) reflect.Value {
	if name == "-" {
		return reflect.Value{}
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() || bsonTagInline(sf.Tag.Get("bson")) {
			continue // inline structs are searched below; inline maps have no name of their own
		}
		if bsonFieldName(sf) == name {
			return v.Field(i)
		}
	}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !internal.IsInlineStruct(sf) {
			continue
		}
		inner := v.Field(i)
		if inner.Kind() == reflect.Pointer {
			if inner.IsNil() {
				if !alloc || !inner.CanSet() || !typeHasBSONName(inner.Type().Elem(), name) {
					continue
				}
				inner.Set(reflect.New(inner.Type().Elem()))
			}
			inner = inner.Elem()
		}
		if f := fieldByBSONName(inner, name, alloc); f.IsValid() {
			return f
		}
	}
	return reflect.Value{}
}

// typeHasBSONName reports whether a struct type stores a field under name,
// directly or through inline embeds.
func typeHasBSONName(t reflect.Type, name string) bool {
	for _, f := range internal.StructFields(t) {
		if bsonFieldName(f) == name {
			return true
		}
	}
	return false
}

// bsonFieldName is the name the driver stores a field under: its tag, or the
// lowercased Go name; "-" for fields the driver skips.
func bsonFieldName(sf reflect.StructField) string {
	name, _ := ParseBSONTag(sf.Tag.Get("bson"))
	if name == "" {
		return strings.ToLower(sf.Name)
	}
	return name
}

// fieldByIndex follows a FieldSchema's index path from a struct value,
// tolerating nil pointer embeds along the way (returns an invalid Value).
func fieldByIndex(v reflect.Value, index []int) reflect.Value {
	cur := v
	for _, i := range index {
		if cur.Kind() == reflect.Pointer {
			if cur.IsNil() {
				return reflect.Value{}
			}
			cur = cur.Elem()
		}
		if cur.Kind() != reflect.Struct || i >= cur.NumField() {
			return reflect.Value{}
		}
		cur = cur.Field(i)
	}
	return cur
}

// schemaField returns the value of fs within v, by index path when the
// schema recorded one and by Go name otherwise.
func schemaField(v reflect.Value, fs *FieldSchema) reflect.Value {
	if len(fs.index) > 0 {
		return fieldByIndex(v, fs.index)
	}
	return v.FieldByName(fs.Name)
}

// UpdateOne performs a partial update on a single document matching filter.
// The model parameter is used only for schema/collection lookup (e.g. &User{}).
// The update parameter should be a MongoDB update document (e.g. bson.D{{"$set", bson.D{...}}}).
// Upsert, ArrayFilters, Hint, and Collation from UpdateOptions apply.
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

	info := &OpInfo{
		Operation: OpUpdate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: filter,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		opt := mergeUpdateOptions(opts)
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		coll := getCollection(db, schema)
		result, err := coll.UpdateOne(ctx, info.Filter, update, updateOneOptions(opt))
		if err != nil {
			return translateWriteError(err, schema, "update one failed")
		}
		if result.MatchedCount == 0 && result.UpsertedCount == 0 {
			return ErrNotFound
		}

		return nil
	})
}

// FindOneAndUpdate atomically updates the first document matching filter and
// decodes it into result (the updated document by default; set ReturnOriginal
// to get the pre-update state). It sets updated_at, increments __v, and on an
// upsert that inserts, sets created_at, unless the update already touches
// those fields. The update is otherwise applied as given: hooks, validation,
// and immutable enforcement do not run.
//
// Returns ErrNotFound if nothing matched and Upsert is false.
func FindOneAndUpdate(ctx context.Context, filter interface{}, update interface{}, result interface{}, opts ...FindOneAndUpdateOptions) error {
	schema, err := getSchemaForModel(result)
	if err != nil {
		return err
	}

	info := &OpInfo{
		Operation: OpUpdate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: result, Filter: filter,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt FindOneAndUpdateOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		fo := options.FindOneAndUpdate().SetReturnDocument(options.After)
		if opt.ReturnOriginal {
			fo.SetReturnDocument(options.Before)
		}
		if opt.Upsert {
			fo.SetUpsert(true)
		}
		if opt.Sort != nil {
			fo.SetSort(opt.Sort)
		}
		if opt.Projection != nil {
			fo.SetProjection(opt.Projection)
		}
		if opt.ArrayFilters != nil {
			fo.SetArrayFilters(opt.ArrayFilters)
		}
		if opt.Hint != nil {
			fo.SetHint(opt.Hint)
		}
		if opt.Collation != nil {
			fo.SetCollation(opt.Collation)
		}

		coll := getCollection(db, schema)
		managed := withManagedUpdate(update, time.Now(), opt.Upsert)
		res := coll.FindOneAndUpdate(ctx, info.Filter, managed, fo)
		if err := res.Decode(result); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return ErrNotFound
			}
			return translateWriteError(err, schema, "find one and update failed")
		}
		if opt.ReturnOriginal {
			setModelBase(schema, result, nil) // pre-update content is not a merge base
		} else if raw, err := res.Raw(); err == nil {
			setModelBase(schema, result, raw)
		}
		return nil
	})
}

// FindOneAndReplace atomically replaces the first document matching filter
// with replacement and decodes the resulting document into result (the new
// document by default). Like Update it sets updated_at on the replacement
// when it is a registered model, but hooks, validation, versioning, and
// immutable enforcement do not run.
//
// Returns ErrNotFound if nothing matched and Upsert is false.
func FindOneAndReplace(ctx context.Context, filter interface{}, replacement interface{}, result interface{}, opts ...FindOneAndUpdateOptions) error {
	schema, err := getSchemaForModel(result)
	if err != nil {
		return err
	}

	info := &OpInfo{
		Operation: OpUpdate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: result, Filter: filter,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt FindOneAndUpdateOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		fo := options.FindOneAndReplace().SetReturnDocument(options.After)
		if opt.ReturnOriginal {
			fo.SetReturnDocument(options.Before)
		}
		if opt.Upsert {
			fo.SetUpsert(true)
		}
		if opt.Sort != nil {
			fo.SetSort(opt.Sort)
		}
		if opt.Projection != nil {
			fo.SetProjection(opt.Projection)
		}
		if opt.Hint != nil {
			fo.SetHint(opt.Hint)
		}
		if opt.Collation != nil {
			fo.SetCollation(opt.Collation)
		}

		if _, ok := SchemaOf(replacement); ok && reflect.ValueOf(replacement).Kind() == reflect.Pointer {
			setUpdatedAt(replacement, time.Now())
		}

		coll := getCollection(db, schema)
		res := coll.FindOneAndReplace(ctx, info.Filter, replacement, fo)
		if err := res.Decode(result); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return ErrNotFound
			}
			return translateWriteError(err, schema, "find one and replace failed")
		}
		if opt.ReturnOriginal {
			setModelBase(schema, result, nil)
		} else if raw, err := res.Raw(); err == nil {
			setModelBase(schema, result, raw)
		}
		return nil
	})
}

// FindOneAndDelete atomically deletes the first document matching filter and
// decodes the deleted document into result. Sort, Projection, Hint, and
// Collation from FindOptions apply. Hooks do not run.
//
// Returns ErrNotFound if nothing matched.
func FindOneAndDelete(ctx context.Context, filter interface{}, result interface{}, opts ...FindOptions) error {
	schema, err := getSchemaForModel(result)
	if err != nil {
		return err
	}

	info := &OpInfo{
		Operation: OpDelete, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: result, Filter: filter,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt FindOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		fo := options.FindOneAndDelete()
		if opt.Sort != nil {
			fo.SetSort(opt.Sort)
		}
		if opt.Projection != nil {
			fo.SetProjection(opt.Projection)
		}
		if opt.Hint != nil {
			fo.SetHint(opt.Hint)
		}
		if opt.Collation != nil {
			fo.SetCollation(opt.Collation)
		}

		coll := getCollection(db, schema)
		err = coll.FindOneAndDelete(ctx, info.Filter, fo).Decode(result)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return ErrNotFound
			}
			return fmt.Errorf("goodm: find one and delete failed: %w", err)
		}
		return nil
	})
}

// withManagedUpdate adds goodm's managed fields to an update document:
// $set updated_at, $inc __v, and on upsert $setOnInsert created_at. Fields the
// caller already sets are left alone. Updates that are not bson.D or bson.M
// (e.g. aggregation pipelines) are returned unchanged.
func withManagedUpdate(update interface{}, now time.Time, upsert bool) interface{} {
	doc, ok := toUpdateDoc(update)
	if !ok {
		return update
	}

	touched := func(field string, ops ...string) bool {
		for _, op := range ops {
			if d, _ := operatorDoc(doc, op); d != nil {
				for _, e := range d {
					if e.Key == field {
						return true
					}
				}
			}
		}
		return false
	}

	if !touched("updated_at", "$set", "$currentDate", "$unset", "$setOnInsert") {
		doc = addToOperator(doc, "$set", bson.E{Key: "updated_at", Value: now})
	}
	if !touched("__v", "$set", "$inc", "$unset", "$setOnInsert") {
		doc = addToOperator(doc, "$inc", bson.E{Key: "__v", Value: 1})
	}
	if upsert && !touched("created_at", "$set", "$currentDate", "$setOnInsert") {
		doc = addToOperator(doc, "$setOnInsert", bson.E{Key: "created_at", Value: now})
	}
	return doc
}

// toUpdateDoc converts a bson.D or bson.M update into a bson.D copy.
func toUpdateDoc(update interface{}) (bson.D, bool) {
	switch u := update.(type) {
	case bson.D:
		out := make(bson.D, len(u))
		copy(out, u)
		return out, true
	case bson.M:
		out := make(bson.D, 0, len(u))
		for k, v := range u {
			out = append(out, bson.E{Key: k, Value: v})
		}
		return out, true
	}
	return nil, false
}

// operatorDoc returns the document under an update operator as a bson.D and
// its position, or nil and -1 if the operator is absent.
func operatorDoc(doc bson.D, op string) (bson.D, int) {
	for i, e := range doc {
		if e.Key != op {
			continue
		}
		switch v := e.Value.(type) {
		case bson.D:
			return v, i
		case bson.M:
			out := make(bson.D, 0, len(v))
			for k, val := range v {
				out = append(out, bson.E{Key: k, Value: val})
			}
			return out, i
		default:
			if m, err := toBsonMap(v); err == nil {
				out := make(bson.D, 0, len(m))
				for k, val := range m {
					out = append(out, bson.E{Key: k, Value: val})
				}
				return out, i
			}
			return bson.D{}, i
		}
	}
	return nil, -1
}

// addToOperator appends a field to the given operator's document, creating the
// operator if needed.
func addToOperator(doc bson.D, op string, field bson.E) bson.D {
	existing, i := operatorDoc(doc, op)
	if i < 0 {
		return append(doc, bson.E{Key: op, Value: bson.D{field}})
	}
	doc[i] = bson.E{Key: op, Value: append(existing, field)}
	return doc
}

// Delete removes a document by its ID.
// Runs BeforeDelete/AfterDelete hooks.
func Delete(ctx context.Context, model interface{}, opts ...DeleteOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	id, err := modelID(schema, model)
	if err != nil {
		return err
	}
	if isZeroValue(id) {
		return fmt.Errorf("goodm: cannot delete document with zero ID")
	}

	base := bson.D{{Key: "_id", Value: id}}
	info := &OpInfo{
		Operation: OpDelete, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: base,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt DeleteOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		// BeforeDelete hook
		if hook, ok := model.(BeforeDelete); ok {
			if err := hook.BeforeDelete(ctx); err != nil {
				return err
			}
		}

		coll := getCollection(db, schema)
		result, err := coll.DeleteOne(ctx, andFilter(middlewareScope(info, base), base), deleteOneOptions(opt))
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

	info := &OpInfo{
		Operation: OpDelete, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Filter: filter,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		var opt DeleteOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		coll := getCollection(db, schema)
		result, err := coll.DeleteOne(ctx, info.Filter, deleteOneOptions(opt))
		if err != nil {
			return fmt.Errorf("goodm: delete one failed: %w", err)
		}
		if result.DeletedCount == 0 {
			return ErrNotFound
		}

		return nil
	})
}

// --- option builders ---

func findOptions(opt FindOptions) *options.FindOptionsBuilder {
	fo := options.Find()
	if opt.Limit > 0 {
		fo.SetLimit(opt.Limit)
	}
	if opt.Skip > 0 {
		fo.SetSkip(opt.Skip)
	}
	if opt.Sort != nil {
		fo.SetSort(opt.Sort)
	}
	if opt.Projection != nil {
		fo.SetProjection(opt.Projection)
	}
	if opt.Hint != nil {
		fo.SetHint(opt.Hint)
	}
	if opt.Collation != nil {
		fo.SetCollation(opt.Collation)
	}
	if opt.BatchSize > 0 {
		fo.SetBatchSize(opt.BatchSize)
	}
	return fo
}

func findOneOptions(opt FindOptions) *options.FindOneOptionsBuilder {
	fo := options.FindOne()
	if opt.Skip > 0 {
		fo.SetSkip(opt.Skip)
	}
	if opt.Sort != nil {
		fo.SetSort(opt.Sort)
	}
	if opt.Projection != nil {
		fo.SetProjection(opt.Projection)
	}
	if opt.Hint != nil {
		fo.SetHint(opt.Hint)
	}
	if opt.Collation != nil {
		fo.SetCollation(opt.Collation)
	}
	return fo
}

func updateOneOptions(opt UpdateOptions) *options.UpdateOneOptionsBuilder {
	uo := options.UpdateOne()
	if opt.Upsert {
		uo.SetUpsert(true)
	}
	if opt.ArrayFilters != nil {
		uo.SetArrayFilters(opt.ArrayFilters)
	}
	if opt.Hint != nil {
		uo.SetHint(opt.Hint)
	}
	if opt.Collation != nil {
		uo.SetCollation(opt.Collation)
	}
	return uo
}

func updateManyOptions(opt UpdateOptions) *options.UpdateManyOptionsBuilder {
	uo := options.UpdateMany()
	if opt.Upsert {
		uo.SetUpsert(true)
	}
	if opt.ArrayFilters != nil {
		uo.SetArrayFilters(opt.ArrayFilters)
	}
	if opt.Hint != nil {
		uo.SetHint(opt.Hint)
	}
	if opt.Collation != nil {
		uo.SetCollation(opt.Collation)
	}
	return uo
}

func deleteOneOptions(opt DeleteOptions) *options.DeleteOneOptionsBuilder {
	do := options.DeleteOne()
	if opt.Hint != nil {
		do.SetHint(opt.Hint)
	}
	if opt.Collation != nil {
		do.SetCollation(opt.Collation)
	}
	return do
}

func deleteManyOptions(opt DeleteOptions) *options.DeleteManyOptionsBuilder {
	do := options.DeleteMany()
	if opt.Hint != nil {
		do.SetHint(opt.Hint)
	}
	if opt.Collation != nil {
		do.SetCollation(opt.Collation)
	}
	return do
}

// --- helpers ---

// getSchemaForModel resolves the schema for a model value, pointer, or slice
// from the registry by struct type.
func getSchemaForModel(model interface{}) (*Schema, error) {
	schema, ok := SchemaOf(model)
	if !ok {
		t := modelType(model)
		if t == nil {
			return nil, fmt.Errorf("goodm: %T is not a struct model", model)
		}
		return nil, fmt.Errorf("goodm: model %q is not registered", t.Name())
	}
	return schema, nil
}

// idFieldValue returns the settable reflect.Value of the model's _id field.
func idFieldValue(schema *Schema, model interface{}) (reflect.Value, error) {
	if schema.idField == nil {
		return reflect.Value{}, fmt.Errorf("goodm: model %s has no _id field", schema.ModelName)
	}
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	f, err := v.FieldByIndexErr(schema.idField)
	if err != nil {
		return reflect.Value{}, fmt.Errorf("goodm: cannot access _id field of %s: %w", schema.ModelName, err)
	}
	return f, nil
}

// modelID extracts the _id value from a model.
func modelID(schema *Schema, model interface{}) (interface{}, error) {
	f, err := idFieldValue(schema, model)
	if err != nil {
		return nil, err
	}
	return f.Interface(), nil
}

// isZeroValue reports whether an _id value is unset.
func isZeroValue(id interface{}) bool {
	if id == nil {
		return true
	}
	return reflect.ValueOf(id).IsZero()
}

// assignObjectID generates a new ObjectID for models whose _id is a zero
// bson.ObjectID. Other ID types are left for the caller to populate.
func assignObjectID(schema *Schema, model interface{}) error {
	f, err := idFieldValue(schema, model)
	if err != nil {
		return err
	}
	if f.Type() == reflect.TypeOf(bson.ObjectID{}) && f.IsZero() && f.CanSet() {
		f.Set(reflect.ValueOf(bson.NewObjectID()))
	}
	return nil
}

// requireID fails if the model's _id is still zero after hooks ran.
func requireID(schema *Schema, model interface{}) error {
	f, err := idFieldValue(schema, model)
	if err != nil {
		return err
	}
	if f.IsZero() {
		return fmt.Errorf("goodm: %s has a zero _id of type %s; set it before Create or in BeforeCreate", schema.ModelName, f.Type())
	}
	return nil
}

// coerceID converts an id supplied by the caller to the model's ID type where
// that is unambiguous: a hex string becomes a bson.ObjectID.
func coerceID(schema *Schema, id interface{}) (interface{}, error) {
	if schema.idField == nil {
		return id, nil
	}
	idType := schema.Type.FieldByIndex(schema.idField).Type
	if s, ok := id.(string); ok && idType == reflect.TypeOf(bson.ObjectID{}) {
		oid, err := bson.ObjectIDFromHex(s)
		if err != nil {
			return nil, fmt.Errorf("goodm: invalid ObjectID %q: %w", s, err)
		}
		return oid, nil
	}
	return id, nil
}

// managedField returns the model's field stored under one of goodm's managed
// bson names, resolved through the registered schema so a promoted Go field
// of the same name elsewhere cannot be mistaken for it. Unregistered values
// fall back to the Go field name.
func managedField(model interface{}, bsonName, goName string) reflect.Value {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	if schema, ok := SchemaOf(model); ok {
		var path []int
		switch bsonName {
		case "created_at":
			path = schema.createdAt
		case "updated_at":
			path = schema.updatedAt
		case "__v":
			path = schema.version
		}
		if path == nil {
			return reflect.Value{}
		}
		return fieldByIndex(v, path)
	}
	return v.FieldByName(goName)
}

// setTimestamps sets created_at (if zero) and updated_at on a model.
func setTimestamps(model interface{}, now time.Time) {
	if f := managedField(model, "created_at", "CreatedAt"); f.IsValid() && f.CanSet() && f.Type() == reflect.TypeOf(time.Time{}) {
		if f.Interface().(time.Time).IsZero() {
			f.Set(reflect.ValueOf(now))
		}
	}
	setUpdatedAt(model, now)
}

// embeddedModel returns the model's embedded goodm.Model, allocating a nil
// pointer embed, or nil if the struct has none.
func embeddedModel(schema *Schema, model interface{}) *Model {
	if schema == nil || schema.modelPath == nil {
		return nil
	}
	v := reflect.ValueOf(model)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil
	}
	cur := v.Elem()
	for _, i := range schema.modelPath {
		if cur.Kind() == reflect.Pointer {
			if cur.IsNil() {
				if !cur.CanSet() {
					return nil
				}
				cur.Set(reflect.New(cur.Type().Elem()))
			}
			cur = cur.Elem()
		}
		cur = cur.Field(i)
	}
	if cur.Kind() == reflect.Pointer {
		if cur.IsNil() {
			if !cur.CanSet() {
				return nil
			}
			cur.Set(reflect.New(cur.Type().Elem()))
		}
		return cur.Interface().(*Model)
	}
	if !cur.CanAddr() {
		return nil
	}
	return cur.Addr().Interface().(*Model)
}

// recordBaseAfterWrite records the written document as the model's merge
// base, unless the write ran inside a transaction: until that commits the
// write is not stored state, and a base from a transaction that later aborts
// would be merged against as if it were. Inside a transaction the base is
// cleared instead, so a later conflict reports ErrVersionConflict.
func recordBaseAfterWrite(ctx context.Context, schema *Schema, model interface{}, unset []string) {
	if inTransaction(ctx) {
		setModelBase(schema, model, nil)
		return
	}
	if len(unset) == 0 {
		setModelBaseFromModel(schema, model)
		return
	}
	if written, err := buildReplacement(model, unset); err == nil {
		if raw, err := bson.Marshal(written); err == nil {
			setModelBase(schema, model, raw)
		}
	}
}

// setModelBase records raw as the model's last known stored state.
func setModelBase(schema *Schema, model interface{}, raw bson.Raw) {
	if m := embeddedModel(schema, model); m != nil {
		m.setBase(raw)
	}
}

// setModelBaseFromModel records the model's current state as its last known
// stored state, after a successful write.
func setModelBaseFromModel(schema *Schema, model interface{}) {
	m := embeddedModel(schema, model)
	if m == nil {
		return
	}
	if raw, err := bson.Marshal(model); err == nil {
		m.setBase(raw)
	}
}

// modelBase returns the model's last known stored state, or nil.
func modelBase(schema *Schema, model interface{}) bson.Raw {
	if m := embeddedModel(schema, model); m != nil {
		return m.baseSnapshot()
	}
	return nil
}

// copyRaw returns an owned copy of a cursor's current document.
func copyRaw(raw bson.Raw) bson.Raw {
	return append(bson.Raw(nil), raw...)
}

// getUpdatedAt returns a model's updated_at, or the zero time if it has none.
func getUpdatedAt(model interface{}) time.Time {
	if f := managedField(model, "updated_at", "UpdatedAt"); f.IsValid() && f.Type() == reflect.TypeOf(time.Time{}) {
		return f.Interface().(time.Time)
	}
	return time.Time{}
}

// setUpdatedAt sets only updated_at on a model.
func setUpdatedAt(model interface{}, now time.Time) {
	if f := managedField(model, "updated_at", "UpdatedAt"); f.IsValid() && f.CanSet() && f.Type() == reflect.TypeOf(time.Time{}) {
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
	if oldV.Kind() == reflect.Pointer {
		oldV = oldV.Elem()
	}
	newV := reflect.ValueOf(new)
	if newV.Kind() == reflect.Pointer {
		newV = newV.Elem()
	}

	for _, field := range schema.Fields {
		if !field.Immutable {
			continue
		}
		oldField := schemaField(oldV, &field)
		newField := schemaField(newV, &field)
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

// getModelVersion extracts the __v field from a model.
func getModelVersion(model interface{}) (int, error) {
	f := managedField(model, "__v", "Version")
	if !f.IsValid() || !f.CanInt() {
		return 0, fmt.Errorf("goodm: model has no Version field")
	}
	return int(f.Int()), nil
}

// setModelVersion sets the __v field on a model.
func setModelVersion(model interface{}, version int) {
	if f := managedField(model, "__v", "Version"); f.IsValid() && f.CanSet() && f.CanInt() {
		f.SetInt(int64(version))
	}
}

// managedFields are internal fields that must not be unset or set directly.
var managedFields = map[string]bool{
	"_id": true, "created_at": true, "updated_at": true, "__v": true,
}

// zeroUnsetFields sets the fields stored under the given bson names to their
// zero values on the model.
func zeroUnsetFields(model interface{}, unset []string) {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	for _, name := range unset {
		if f := fieldByBSONName(v, name, false); f.IsValid() && f.CanSet() {
			f.Set(reflect.Zero(f.Type()))
		}
	}
}

// validateUnsetFields checks that unset field names are valid schema fields,
// not managed by the ODM, and not required.
func validateUnsetFields(schema *Schema, fields []string) error {
	for _, name := range fields {
		if managedFields[name] {
			return fmt.Errorf("goodm: cannot unset managed field %q", name)
		}
		f := schema.GetField(name)
		if f == nil {
			return fmt.Errorf("goodm: cannot unset unknown field %q in %s", name, schema.ModelName)
		}
		if f.Required {
			return fmt.Errorf("goodm: cannot unset required field %q", name)
		}
	}
	return nil
}

// replaceWithUnset builds the replacement document, strips any unset fields, and
// performs the ReplaceOne. Returns the number of matched documents.
func replaceWithUnset(ctx context.Context, coll *mongo.Collection, schema *Schema, filter interface{}, model interface{}, unsetFields []string) (int64, error) {
	replacement, err := buildReplacement(model, unsetFields)
	if err != nil {
		return 0, err
	}
	result, err := coll.ReplaceOne(ctx, filter, replacement)
	if err != nil {
		return 0, translateWriteError(err, schema, "update failed")
	}
	return result.MatchedCount, nil
}

// buildReplacement marshals a model to bson.M and removes unset fields.
// When there are no unset fields, returns the model as-is to avoid the
// marshal/unmarshal overhead.
func buildReplacement(model interface{}, unsetFields []string) (interface{}, error) {
	if len(unsetFields) == 0 {
		return model, nil
	}

	raw, err := bson.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("goodm: failed to marshal model for unset: %w", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("goodm: failed to unmarshal model for unset: %w", err)
	}

	for _, field := range unsetFields {
		delete(doc, field)
	}

	return doc, nil
}
