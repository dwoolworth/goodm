package goodm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// BulkResult contains the outcome of a bulk operation.
type BulkResult struct {
	InsertedCount int64
	MatchedCount  int64
	ModifiedCount int64
	DeletedCount  int64
	UpsertedCount int64

	// UpsertedIDs maps the index of each upserting operation to the _id of the
	// document it inserted.
	UpsertedIDs map[int64]interface{}
}

func bulkResultFrom(res *mongo.BulkWriteResult) *BulkResult {
	if res == nil {
		return &BulkResult{}
	}
	return &BulkResult{
		InsertedCount: res.InsertedCount,
		MatchedCount:  res.MatchedCount,
		ModifiedCount: res.ModifiedCount,
		DeletedCount:  res.DeletedCount,
		UpsertedCount: res.UpsertedCount,
		UpsertedIDs:   res.UpsertedIDs,
	}
}

// CreateMany inserts multiple documents. It generates IDs, sets timestamps,
// applies defaults, runs BeforeCreate hooks, and validates each model before
// performing a single InsertMany call, then runs AfterCreate for every model
// that was inserted.
//
// models must be a slice of structs or struct pointers (e.g. []User or []*User).
//
// If any model fails its hook or validation the call returns before writing.
// Write failures (such as duplicate keys) are returned as a *BulkError listing
// the failed indexes. By default the insert is ordered and stops at the first
// failure; set CreateOptions.Unordered to insert everything that can be.
//
// Performance: hooks and validation run per-model. For large batches where
// you don't need the ODM lifecycle, use Bulk or the driver's InsertMany directly.
func CreateMany(ctx context.Context, models interface{}, opts ...CreateOptions) error {
	rv, schema, err := modelSlice(models, "CreateMany")
	if err != nil || rv.Len() == 0 {
		return err
	}

	var opt CreateOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	db, err := getDB(opt.DB)
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
			model := elemModel(rv.Index(i))
			if err := prepareCreate(ctx, schema, model, now); err != nil {
				return fmt.Errorf("goodm: item %d: %w", i, err)
			}
			docs[i] = model
		}

		coll := getCollection(db, schema)
		insertOpts := options.InsertMany()
		if opt.Unordered {
			insertOpts.SetOrdered(false)
		}
		_, insertErr := coll.InsertMany(ctx, docs, insertOpts)

		var bulkErr *BulkError
		if insertErr != nil {
			items, ok := bulkWriteErrors(insertErr, schema)
			if !ok {
				return fmt.Errorf("goodm: insert many failed: %w", insertErr)
			}
			bulkErr = &BulkError{Collection: schema.Collection, Errors: items}
		}

		// AfterCreate hooks for inserted models. An ordered insert stops at the
		// first failure, so nothing after that index was written.
		inserted := int64(0)
		stopAt := rv.Len()
		if bulkErr != nil && !opt.Unordered && len(bulkErr.Errors) > 0 {
			stopAt = bulkErr.Errors[0].Index
		}
		for i := 0; i < stopAt; i++ {
			if bulkErr != nil && bulkErr.Failed(i) {
				continue
			}
			inserted++
			if hook, ok := docs[i].(AfterCreate); ok {
				if err := hook.AfterCreate(ctx); err != nil {
					return err
				}
			}
		}

		if bulkErr != nil {
			bulkErr.Result = &BulkResult{InsertedCount: inserted}
			return bulkErr
		}
		return nil
	})
}

// modelSlice validates that models is a slice (or pointer to one) of a
// registered model type and returns it with the schema.
func modelSlice(models interface{}, op string) (reflect.Value, *Schema, error) {
	rv := reflect.ValueOf(models)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Slice {
		return rv, nil, fmt.Errorf("goodm: %s expects a slice, got %T", op, models)
	}
	schema, err := getSchemaForModel(models)
	if err != nil {
		return rv, nil, err
	}
	return rv, schema, nil
}

// elemModel returns a pointer-to-struct interface from a reflect.Value,
// whether the value is already a pointer or a plain struct.
func elemModel(v reflect.Value) interface{} {
	if v.Kind() == reflect.Pointer {
		return v.Interface()
	}
	return v.Addr().Interface()
}

// SaveMany persists changes to many loaded models in one round trip. Each
// model goes through the Update lifecycle: immutable fields are checked
// against the stored documents (one query for the whole batch), BeforeSave
// runs, the model is validated, and its version and UpdatedAt advance. The
// replacements are then sent as a single unordered bulk write, each guarded
// by its own version filter, and AfterSave runs for every model that was
// written.
//
// If any model fails its immutable check, hook, or validation, SaveMany
// returns before writing anything. After the write, documents that were
// deleted or modified by another writer are reported in a *BulkError with
// ErrNotFound or ErrVersionConflict per index (the in-memory version of a
// conflicting model is refreshed from the database, as Update does); unique
// index violations appear as *DuplicateKeyError. Successful models are saved
// regardless of failures elsewhere in the batch.
//
// models must be a slice of structs or struct pointers (e.g. []User or []*User).
func SaveMany(ctx context.Context, models interface{}, opts ...UpdateOptions) error {
	rv, schema, err := modelSlice(models, "SaveMany")
	if err != nil || rv.Len() == 0 {
		return err
	}

	var opt UpdateOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	db, err := getDB(opt.DB)
	if err != nil {
		return err
	}

	return runMiddleware(ctx, &OpInfo{
		Operation:  OpSaveMany,
		Collection: schema.Collection,
		ModelName:  schema.ModelName,
	}, func(ctx context.Context) error {
		coll := getCollection(db, schema)
		n := rv.Len()

		items := make([]interface{}, n)
		ids := make([]interface{}, n)
		for i := 0; i < n; i++ {
			items[i] = elemModel(rv.Index(i))
			id, err := modelID(schema, items[i])
			if err != nil {
				return fmt.Errorf("goodm: item %d: %w", i, err)
			}
			if isZeroValue(id) {
				return fmt.Errorf("goodm: item %d: cannot save document with zero ID", i)
			}
			ids[i] = id
		}

		if err := checkImmutableFieldsBatch(ctx, coll, schema, items, ids); err != nil {
			return err
		}

		for i, model := range items {
			if hook, ok := model.(BeforeSave); ok {
				if err := hook.BeforeSave(ctx); err != nil {
					return fmt.Errorf("goodm: BeforeSave failed on item %d: %w", i, err)
				}
			}
			if errs := Validate(model, schema); len(errs) > 0 {
				return fmt.Errorf("goodm: validation failed on item %d: %w", i, ValidationErrors(errs))
			}
		}

		now := time.Now()
		oldVersions := make([]int, n)
		writes := make([]mongo.WriteModel, n)
		for i, model := range items {
			oldVersions[i], _ = getModelVersion(model)
			setModelVersion(model, oldVersions[i]+1)
			setUpdatedAt(model, now)
			writes[i] = mongo.NewReplaceOneModel().
				SetFilter(buildVersionFilter(ids[i], oldVersions[i])).
				SetReplacement(model)
		}

		res, writeErr := coll.BulkWrite(ctx, writes, options.BulkWrite().SetOrdered(false))
		failures := map[int]error{}
		if writeErr != nil {
			errs, ok := bulkWriteErrors(writeErr, schema)
			if !ok {
				for i, model := range items {
					setModelVersion(model, oldVersions[i])
				}
				return fmt.Errorf("goodm: save many failed: %w", writeErr)
			}
			for _, e := range errs {
				failures[e.Index] = e.Err
			}
		}

		// Replacements that matched nothing are not errors to the server, so
		// classify them by re-reading version and updated_at.
		attempted := int64(n - len(failures))
		if res == nil || res.MatchedCount < attempted {
			if err := classifyUnmatched(ctx, coll, items, ids, failures, now); err != nil {
				return err
			}
		}

		for i, model := range items {
			if failure, failed := failures[i]; failed {
				// classifyUnmatched already refreshed conflicting models from
				// the database; roll every other failure back to its old version.
				if !errors.Is(failure, ErrVersionConflict) {
					setModelVersion(model, oldVersions[i])
				}
				continue
			}
			if hook, ok := model.(AfterSave); ok {
				if err := hook.AfterSave(ctx); err != nil {
					return err
				}
			}
		}

		if len(failures) == 0 {
			return nil
		}
		bulkErr := &BulkError{Collection: schema.Collection, Result: bulkResultFrom(res)}
		for i := 0; i < n; i++ {
			if e, ok := failures[i]; ok {
				bulkErr.Errors = append(bulkErr.Errors, BulkWriteError{Index: i, Message: e.Error(), Err: e})
			}
		}
		return bulkErr
	})
}

// checkImmutableFieldsBatch loads the stored documents for all items in one
// query and compares immutable fields. It is a no-op when the schema has no
// immutable fields. Items whose document no longer exists are skipped here;
// the write phase reports them as ErrNotFound.
func checkImmutableFieldsBatch(ctx context.Context, coll *mongo.Collection, schema *Schema, items, ids []interface{}) error {
	if !hasImmutableFields(schema) {
		return nil
	}
	cursor, err := coll.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return fmt.Errorf("goodm: failed to fetch existing documents: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	existing := map[string]interface{}{}
	for cursor.Next(ctx) {
		doc := reflect.New(schema.Type).Interface()
		if err := cursor.Decode(doc); err != nil {
			return fmt.Errorf("goodm: failed to decode existing document: %w", err)
		}
		id, err := modelID(schema, doc)
		if err != nil {
			return err
		}
		existing[idKey(id)] = doc
	}
	if err := cursor.Err(); err != nil {
		return fmt.Errorf("goodm: failed to fetch existing documents: %w", err)
	}

	for i, model := range items {
		old, ok := existing[idKey(ids[i])]
		if !ok {
			continue // reported as ErrNotFound for this item after the write
		}
		if errs := validateImmutable(old, model, schema); len(errs) > 0 {
			return fmt.Errorf("goodm: item %d: %w", i, ValidationErrors(errs))
		}
	}
	return nil
}

// classifyUnmatched decides, for every item not already failed by the server,
// whether its replacement was applied. A document whose stored version and
// updated_at equal what this call wrote was saved; a missing document is
// ErrNotFound; anything else is ErrVersionConflict, and the model's version
// is refreshed from the stored document.
func classifyUnmatched(ctx context.Context, coll *mongo.Collection, items, ids []interface{}, failures map[int]error, now time.Time) error {
	var lookup []interface{}
	for i := range items {
		if _, failed := failures[i]; !failed {
			lookup = append(lookup, ids[i])
		}
	}
	if len(lookup) == 0 {
		return nil
	}

	cursor, err := coll.Find(ctx,
		bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: lookup}}}},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}, {Key: "__v", Value: 1}, {Key: "updated_at", Value: 1}}))
	if err != nil {
		return fmt.Errorf("goodm: failed to verify saved documents: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	type stored struct {
		Version   int       `bson:"__v"`
		UpdatedAt time.Time `bson:"updated_at"`
	}
	found := map[string]stored{}
	for cursor.Next(ctx) {
		var doc struct {
			ID        interface{} `bson:"_id"`
			Version   int         `bson:"__v"`
			UpdatedAt time.Time   `bson:"updated_at"`
		}
		if err := cursor.Decode(&doc); err != nil {
			return fmt.Errorf("goodm: failed to decode saved document: %w", err)
		}
		found[idKey(doc.ID)] = stored{Version: doc.Version, UpdatedAt: doc.UpdatedAt}
	}
	if err := cursor.Err(); err != nil {
		return fmt.Errorf("goodm: failed to verify saved documents: %w", err)
	}

	wrote := now.Truncate(time.Millisecond)
	for i, model := range items {
		if _, failed := failures[i]; failed {
			continue
		}
		st, ok := found[idKey(ids[i])]
		if !ok {
			failures[i] = ErrNotFound
			continue
		}
		want, _ := getModelVersion(model)
		if st.Version == want && st.UpdatedAt.Equal(wrote) {
			continue
		}
		setModelVersion(model, st.Version)
		failures[i] = ErrVersionConflict
	}
	return nil
}

// idKey renders an _id as a map key. ObjectIDs use their hex form so values
// decoded from the server match values held in models.
func idKey(id interface{}) string {
	if oid, ok := id.(bson.ObjectID); ok {
		return oid.Hex()
	}
	raw, err := bson.Marshal(bson.D{{Key: "k", Value: id}})
	if err != nil {
		return fmt.Sprint(id)
	}
	return string(raw)
}

// UpdateMany updates all documents matching filter with the given update document.
// The model parameter is used only for schema/collection lookup (e.g. &User{}).
// Upsert, ArrayFilters, Hint, and Collation from UpdateOptions apply.
//
// Performance: This is a direct passthrough to MongoDB's UpdateMany. It bypasses
// hooks, validation, and immutable field enforcement. Use Update for the full
// ODM lifecycle on individual documents.
func UpdateMany(ctx context.Context, filter, update interface{}, model interface{}, opts ...UpdateOptions) (*BulkResult, error) {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return nil, err
	}

	var opt UpdateOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	db, err := getDB(opt.DB)
	if err != nil {
		return nil, err
	}

	var result *BulkResult
	info := &OpInfo{
		Operation:  OpUpdateMany,
		Collection: schema.Collection,
		ModelName:  schema.ModelName,
		Model:      model,
		Filter:     filter,
	}
	err = runMiddleware(ctx, info, func(ctx context.Context) error {
		coll := getCollection(db, schema)
		res, err := coll.UpdateMany(ctx, info.Filter, update, updateManyOptions(opt))
		if err != nil {
			return translateWriteError(err, schema, "update many failed")
		}
		result = &BulkResult{
			MatchedCount:  res.MatchedCount,
			ModifiedCount: res.ModifiedCount,
			UpsertedCount: res.UpsertedCount,
		}
		if res.UpsertedID != nil {
			result.UpsertedIDs = map[int64]interface{}{0: res.UpsertedID}
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

	var opt DeleteOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	db, err := getDB(opt.DB)
	if err != nil {
		return nil, err
	}

	var result *BulkResult
	info := &OpInfo{
		Operation:  OpDeleteMany,
		Collection: schema.Collection,
		ModelName:  schema.ModelName,
		Filter:     filter,
	}
	err = runMiddleware(ctx, info, func(ctx context.Context) error {
		coll := getCollection(db, schema)
		res, err := coll.DeleteMany(ctx, info.Filter, deleteManyOptions(opt))
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

// BulkOptions configures a Bulk.
type BulkOptions struct {
	DB        *mongo.Database
	Unordered bool // continue past failures; equivalent to calling Unordered()
}

// Bulk accumulates heterogeneous write operations against one collection and
// sends them in a single BulkWrite. It is a raw passthrough like UpdateOne:
// hooks, validation, timestamps, and versioning do not run. Use CreateMany
// and SaveMany for the lifecycle-aware batch operations.
//
// Operations run in order and stop at the first failure unless Unordered is
// set. Failures are returned as a *BulkError listing each failed operation by
// its position.
//
// Example:
//
//	res, err := goodm.NewBulk(&User{}).
//	    InsertOne(&User{Email: "a@example.com", Name: "A"}).
//	    UpdateOne(bson.D{{Key: "email", Value: "b@example.com"}},
//	        bson.D{{Key: "$set", Value: bson.D{{Key: "verified", Value: true}}}}, goodm.Upsert()).
//	    DeleteMany(bson.D{{Key: "role", Value: "guest"}}).
//	    Unordered().
//	    Execute(ctx)
type Bulk struct {
	model   interface{}
	opt     BulkOptions
	ordered bool
	ops     []mongo.WriteModel
}

// NewBulk creates a bulk write builder bound to the given model's collection.
func NewBulk(model interface{}, opts ...BulkOptions) *Bulk {
	b := &Bulk{model: model, ordered: true}
	if len(opts) > 0 {
		b.opt = opts[0]
		if b.opt.Unordered {
			b.ordered = false
		}
	}
	return b
}

// InsertOne queues an insert of doc (a model value or any BSON document).
func (b *Bulk) InsertOne(doc interface{}) *Bulk {
	b.ops = append(b.ops, mongo.NewInsertOneModel().SetDocument(doc))
	return b
}

// UpdateOne queues an update of the first document matching filter.
func (b *Bulk) UpdateOne(filter, update interface{}, opts ...UpdateOptions) *Bulk {
	m := mongo.NewUpdateOneModel().SetFilter(filter).SetUpdate(update)
	if len(opts) > 0 {
		o := opts[0]
		if o.Upsert {
			m.SetUpsert(true)
		}
		if o.ArrayFilters != nil {
			m.SetArrayFilters(o.ArrayFilters)
		}
		if o.Hint != nil {
			m.SetHint(o.Hint)
		}
		if o.Collation != nil {
			m.SetCollation(o.Collation)
		}
	}
	b.ops = append(b.ops, m)
	return b
}

// UpdateMany queues an update of every document matching filter.
func (b *Bulk) UpdateMany(filter, update interface{}, opts ...UpdateOptions) *Bulk {
	m := mongo.NewUpdateManyModel().SetFilter(filter).SetUpdate(update)
	if len(opts) > 0 {
		o := opts[0]
		if o.Upsert {
			m.SetUpsert(true)
		}
		if o.ArrayFilters != nil {
			m.SetArrayFilters(o.ArrayFilters)
		}
		if o.Hint != nil {
			m.SetHint(o.Hint)
		}
		if o.Collation != nil {
			m.SetCollation(o.Collation)
		}
	}
	b.ops = append(b.ops, m)
	return b
}

// ReplaceOne queues a replacement of the first document matching filter.
func (b *Bulk) ReplaceOne(filter, replacement interface{}, opts ...UpdateOptions) *Bulk {
	m := mongo.NewReplaceOneModel().SetFilter(filter).SetReplacement(replacement)
	if len(opts) > 0 {
		o := opts[0]
		if o.Upsert {
			m.SetUpsert(true)
		}
		if o.Hint != nil {
			m.SetHint(o.Hint)
		}
		if o.Collation != nil {
			m.SetCollation(o.Collation)
		}
	}
	b.ops = append(b.ops, m)
	return b
}

// DeleteOne queues a delete of the first document matching filter.
func (b *Bulk) DeleteOne(filter interface{}, opts ...DeleteOptions) *Bulk {
	m := mongo.NewDeleteOneModel().SetFilter(filter)
	if len(opts) > 0 {
		if opts[0].Hint != nil {
			m.SetHint(opts[0].Hint)
		}
		if opts[0].Collation != nil {
			m.SetCollation(opts[0].Collation)
		}
	}
	b.ops = append(b.ops, m)
	return b
}

// DeleteMany queues a delete of every document matching filter.
func (b *Bulk) DeleteMany(filter interface{}, opts ...DeleteOptions) *Bulk {
	m := mongo.NewDeleteManyModel().SetFilter(filter)
	if len(opts) > 0 {
		if opts[0].Hint != nil {
			m.SetHint(opts[0].Hint)
		}
		if opts[0].Collation != nil {
			m.SetCollation(opts[0].Collation)
		}
	}
	b.ops = append(b.ops, m)
	return b
}

// Unordered lets the server continue past failed operations and run them in
// any order. The default is ordered.
func (b *Bulk) Unordered() *Bulk {
	b.ordered = false
	return b
}

// Len returns the number of queued operations.
func (b *Bulk) Len() int {
	return len(b.ops)
}

// Execute sends the queued operations. With no operations it returns an empty
// result and no error. Middleware runs under OpBulkWrite.
func (b *Bulk) Execute(ctx context.Context) (*BulkResult, error) {
	schema, err := getSchemaForModel(b.model)
	if err != nil {
		return nil, err
	}
	if len(b.ops) == 0 {
		return &BulkResult{}, nil
	}

	var result *BulkResult
	err = runMiddleware(ctx, &OpInfo{
		Operation:  OpBulkWrite,
		Collection: schema.Collection,
		ModelName:  schema.ModelName,
		Model:      b.model,
	}, func(ctx context.Context) error {
		db, err := getDB(b.opt.DB)
		if err != nil {
			return err
		}
		coll := getCollection(db, schema)
		res, err := coll.BulkWrite(ctx, b.ops, options.BulkWrite().SetOrdered(b.ordered))
		result = bulkResultFrom(res)
		if err != nil {
			items, ok := bulkWriteErrors(err, schema)
			if !ok {
				return fmt.Errorf("goodm: bulk write failed: %w", err)
			}
			return &BulkError{Collection: schema.Collection, Errors: items, Result: result}
		}
		return nil
	})
	return result, err
}
