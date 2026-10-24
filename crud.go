package goodm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

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
//
// Example:
//
//	goodm.Update(ctx, &server, goodm.UnsetFields("agent_id"))
func UnsetFields(fields ...string) UpdateOptions {
	return UpdateOptions{Unset: fields}
}

// WithRetry returns UpdateOptions that will automatically retry on version
// conflict using a 3-way field-level merge. On conflict, the document is re-read
// from the database, and the caller's changed fields are merged onto the fresh
// state — but only if no other writer modified the same fields. If both sides
// changed the same field, a *MergeConflictError is returned.
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
		if err := coll.FindOne(ctx, info.Filter, findOneOptions(opt)).Decode(result); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return ErrNotFound
			}
			return fmt.Errorf("goodm: find one failed: %w", err)
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

	var opt UpdateOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	// Validate unset fields against the schema before proceeding.
	if err := validateUnsetFields(schema, opt.Unset); err != nil {
		return err
	}

	// Middleware may replace info.Filter (e.g. to scope by tenant); it is
	// ANDed with the _id and version filters of every read and write below.
	info := &OpInfo{
		Operation: OpUpdate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model,
		Filter: bson.D{{Key: "_id", Value: id}},
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}

		coll := getCollection(db, schema)
		scope := info.Filter

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

// andFilter ANDs a middleware-supplied scope filter into filter. A nil scope
// leaves filter unchanged.
func andFilter(scope interface{}, filter bson.D) interface{} {
	if scope == nil {
		return filter
	}
	return bson.D{{Key: "$and", Value: bson.A{scope, filter}}}
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
// Each value is validated against the field's schema (required, enum, min,
// max); subdocument values may be structs, bson.M, bson.D, or slices of those.
// Immutable fields are rejected. The caller's map is not modified.
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

	if err := validateUpdateFields(schema, fields); err != nil {
		return err
	}

	info := &OpInfo{
		Operation: OpUpdate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model,
		Filter: bson.D{{Key: "_id", Value: id}},
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		var optDB *mongo.Database
		if len(opts) > 0 {
			optDB = opts[0].DB
		}
		db, err := getDB(optDB)
		if err != nil {
			return err
		}

		// Work on a copy so the caller's map is left untouched.
		set := make(bson.M, len(fields)+1)
		for k, v := range fields {
			set[k] = v
		}
		now := time.Now()
		set["updated_at"] = now
		oldVersion, _ := getModelVersion(model)

		coll := getCollection(db, schema)
		result, err := coll.UpdateOne(ctx, andFilter(info.Filter, bson.D{{Key: "_id", Value: id}}), bson.D{
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
		applyFieldsToModel(model, fields)

		return nil
	})
}

// validateUpdateFields checks that every key in fields is a known, mutable,
// non-managed schema field and that its value satisfies the field's rules.
func validateUpdateFields(schema *Schema, fields bson.M) error {
	var errs ValidationErrors
	for name, val := range fields {
		if managedFields[name] {
			return fmt.Errorf("goodm: cannot set managed field %q via UpdateFields", name)
		}
		fs, immutable, isElement := resolveFieldPath(schema, name)
		if fs == nil {
			return fmt.Errorf("goodm: unknown field %q in %s", name, schema.ModelName)
		}
		if immutable {
			return ValidationErrors{{Field: name, Message: "field is immutable and cannot be changed"}}
		}
		errs = append(errs, validateFieldValue(fs, val, name, isElement)...)
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// resolveFieldPath walks a dotted bson path through the schema. A numeric
// segment after a slice field selects one element. It returns the leaf
// field, whether the leaf or any ancestor is immutable, and whether the path
// ends on a slice element rather than the slice itself; the field is nil if
// the path is unknown.
func resolveFieldPath(schema *Schema, path string) (field *FieldSchema, immutable, isElement bool) {
	fields := schema.Fields
	var leaf *FieldSchema
	for _, seg := range strings.Split(path, ".") {
		if _, err := strconv.Atoi(seg); err == nil && leaf != nil && !isElement && isSliceField(leaf) {
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
	return fs.IsSlice || strings.HasPrefix(fs.Type, "[]")
}

// indirectValue follows interfaces and non-nil pointers to the concrete value.
func indirectValue(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

// validateFieldValue checks one value against a field's required, enum, min,
// max, and subdocument rules. Subdocument values may be Go structs, bson.M,
// bson.D, or any map with string keys; slices of those for slice fields.
// isElement means the value is one element of a slice field.
func validateFieldValue(fs *FieldSchema, val interface{}, path string, isElement bool) []ValidationError {
	rv := indirectValue(reflect.ValueOf(val))
	if !rv.IsValid() {
		if fs.Required {
			return []ValidationError{{Field: path, Message: "field is required"}}
		}
		return nil
	}

	if len(fs.SubFields) > 0 {
		if fs.IsSlice && !isElement {
			return validateSubdocSlice(fs, rv, path)
		}
		return validateSubdocValue(fs.SubFields, rv, path)
	}
	if isElement {
		return nil // an element of a scalar slice has no per-element rules
	}

	var errs []ValidationError
	if fs.Required && rv.IsZero() {
		errs = append(errs, ValidationError{Field: path, Message: "field is required"})
	}
	if len(fs.Enum) > 0 && !rv.IsZero() {
		if err := validateEnum(rv, fs.Enum, path); err != nil {
			errs = append(errs, *err)
		}
	}
	if fs.Min != nil {
		if err := validateMin(rv, *fs.Min, path); err != nil {
			errs = append(errs, *err)
		}
	}
	if fs.Max != nil {
		if err := validateMax(rv, *fs.Max, path); err != nil {
			errs = append(errs, *err)
		}
	}
	return errs
}

// validateSubdocSlice validates a value set on a []struct field: its length
// against min/max and each element as a subdocument.
func validateSubdocSlice(fs *FieldSchema, rv reflect.Value, path string) []ValidationError {
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return []ValidationError{{Field: path, Message: fmt.Sprintf("expected an array of subdocuments, got %s", rv.Type())}}
	}
	var errs []ValidationError
	if fs.Min != nil {
		if err := validateMin(rv, *fs.Min, path); err != nil {
			errs = append(errs, *err)
		}
	}
	if fs.Max != nil {
		if err := validateMax(rv, *fs.Max, path); err != nil {
			errs = append(errs, *err)
		}
	}
	for i := 0; i < rv.Len(); i++ {
		elem := indirectValue(rv.Index(i))
		if !elem.IsValid() {
			continue
		}
		errs = append(errs, validateSubdocValue(fs.SubFields, elem, fmt.Sprintf("%s[%d]", path, i))...)
	}
	return errs
}

// validateSubdocValue validates one subdocument given as a struct, bson.D,
// bson.M, or string-keyed map.
func validateSubdocValue(fields []FieldSchema, rv reflect.Value, path string) []ValidationError {
	switch {
	case rv.Kind() == reflect.Struct:
		return validateFields(rv, fields, path)
	case rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String:
		get := func(name string) (interface{}, bool) {
			v := rv.MapIndex(reflect.ValueOf(name).Convert(rv.Type().Key()))
			if !v.IsValid() {
				return nil, false
			}
			return v.Interface(), true
		}
		return validateSubdocFields(fields, get, path)
	case rv.Type() == reflect.TypeOf(bson.D{}):
		doc := rv.Interface().(bson.D)
		get := func(name string) (interface{}, bool) {
			for _, e := range doc {
				if e.Key == name {
					return e.Value, true
				}
			}
			return nil, false
		}
		return validateSubdocFields(fields, get, path)
	default:
		return []ValidationError{{Field: path, Message: fmt.Sprintf("expected a subdocument, got %s", rv.Type())}}
	}
}

// validateSubdocFields applies each field's rules to the value looked up by
// bson name. Missing fields fail only when required.
func validateSubdocFields(fields []FieldSchema, get func(string) (interface{}, bool), path string) []ValidationError {
	var errs []ValidationError
	for i := range fields {
		fs := &fields[i]
		fieldPath := path + "." + fs.BSONName
		val, ok := get(fs.BSONName)
		if !ok {
			if fs.Required {
				errs = append(errs, ValidationError{Field: fieldPath, Message: "field is required"})
			}
			continue
		}
		errs = append(errs, validateFieldValue(fs, val, fieldPath, false)...)
	}
	return errs
}

// applyFieldsToModel reflects $set values back onto the Go struct so the caller
// sees the updated state without a re-read. Dotted paths are followed through
// nested structs, pointers, and slice indices; values that cannot be assigned
// are skipped.
func applyFieldsToModel(model interface{}, fields bson.M) {
	root := reflect.ValueOf(model)
	if root.Kind() == reflect.Pointer {
		root = root.Elem()
	}
	for path, val := range fields {
		target := root
		ok := true
		for _, seg := range strings.Split(path, ".") {
			target, ok = stepInto(target, seg)
			if !ok {
				break
			}
		}
		if !ok || !target.CanSet() {
			continue
		}
		assignValue(target, val)
	}
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
		f := fieldByBSONName(v, seg)
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

// fieldByBSONName finds the struct field whose bson name matches, descending
// into embedded structs.
func fieldByBSONName(v reflect.Value, name string) reflect.Value {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.Anonymous {
			inner := v.Field(i)
			if inner.Kind() == reflect.Pointer {
				if inner.IsNil() {
					continue
				}
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				if f := fieldByBSONName(inner, name); f.IsValid() {
					return f
				}
			}
			continue
		}
		if !sf.IsExported() {
			continue
		}
		bsonName, _ := ParseBSONTag(sf.Tag.Get("bson"))
		if bsonName == "" {
			bsonName = strings.ToLower(sf.Name)
		}
		if bsonName == name {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

// assignValue sets val onto target when it is assignable or convertible.
func assignValue(target reflect.Value, val interface{}) {
	if val == nil {
		target.Set(reflect.Zero(target.Type()))
		return
	}
	rv := reflect.ValueOf(val)
	switch {
	case rv.Type().AssignableTo(target.Type()):
		target.Set(rv)
	case rv.Type().ConvertibleTo(target.Type()) && rv.Kind() != reflect.String && target.Kind() != reflect.String:
		target.Set(rv.Convert(target.Type()))
	case rv.Kind() == reflect.String && target.Kind() == reflect.String:
		target.SetString(rv.String())
	}
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
		var opt UpdateOptions
		if len(opts) > 0 {
			opt = opts[0]
		}
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
		err = coll.FindOneAndUpdate(ctx, info.Filter, managed, fo).Decode(result)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return ErrNotFound
			}
			return translateWriteError(err, schema, "find one and update failed")
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
		err = coll.FindOneAndReplace(ctx, info.Filter, replacement, fo).Decode(result)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return ErrNotFound
			}
			return translateWriteError(err, schema, "find one and replace failed")
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

	info := &OpInfo{
		Operation: OpDelete, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model,
		Filter: bson.D{{Key: "_id", Value: id}},
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
		result, err := coll.DeleteOne(ctx, andFilter(info.Filter, bson.D{{Key: "_id", Value: id}}), deleteOneOptions(opt))
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

// setTimestamps sets CreatedAt (if zero) and UpdatedAt on a model via reflection.
func setTimestamps(model interface{}, now time.Time) {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if f := v.FieldByName("CreatedAt"); f.IsValid() && f.CanSet() && f.Type() == reflect.TypeOf(time.Time{}) {
		if f.Interface().(time.Time).IsZero() {
			f.Set(reflect.ValueOf(now))
		}
	}
	setUpdatedAt(model, now)
}

// setUpdatedAt sets only UpdatedAt on a model via reflection.
func setUpdatedAt(model interface{}, now time.Time) {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if f := v.FieldByName("UpdatedAt"); f.IsValid() && f.CanSet() && f.Type() == reflect.TypeOf(time.Time{}) {
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

// getModelVersion extracts the Version field from a model via reflection.
func getModelVersion(model interface{}) (int, error) {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	f := v.FieldByName("Version")
	if !f.IsValid() || !f.CanInt() {
		return 0, fmt.Errorf("goodm: model has no Version field")
	}
	return int(f.Int()), nil
}

// setModelVersion sets the Version field on a model via reflection.
func setModelVersion(model interface{}, version int) {
	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if f := v.FieldByName("Version"); f.IsValid() && f.CanSet() && f.CanInt() {
		f.SetInt(int64(version))
	}
}

// managedFields are internal fields that must not be unset or set directly.
var managedFields = map[string]bool{
	"_id": true, "created_at": true, "updated_at": true, "__v": true,
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
