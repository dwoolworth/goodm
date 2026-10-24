package goodm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
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
			items, wcErr, fatal := splitBulkWriteError(insertErr, schema)
			if fatal != nil {
				return fmt.Errorf("goodm: insert many failed: %w", fatal)
			}
			bulkErr = &BulkError{Collection: schema.Collection, Errors: items, WriteConcernError: wcErr}
			if wcErr != nil {
				// The server may or may not have applied the inserts; nothing
				// can be reported as created.
				return bulkErr
			}
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
// If any model fails its immutable check, hook, or validation, or the batch
// contains the same _id twice, SaveMany returns before writing anything. After
// the write, documents that were deleted or modified by another writer are
// reported in a *BulkError with ErrNotFound or ErrVersionConflict per index
// (the in-memory version of a conflicting model is refreshed from the
// database, as Update does); unique index violations appear as
// *DuplicateKeyError. Successful models are saved regardless of failures
// elsewhere in the batch. A write concern failure is returned as a plain
// error, since the outcome of every item is then unknown.
//
// On MongoDB 8.0 and later the replacements go out in one client-level bulk
// write whose verbose results report the matched count of each operation.
// Older servers only report aggregate counts, so there each document is
// replaced with its own request. Either way the outcome of every item is
// known exactly.
//
// Middleware may replace OpInfo.Filter (initially an empty document) to scope
// the batch; it is ANDed into every document's filter, so out-of-scope
// documents are reported as ErrNotFound.
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

	// Middleware may replace info.Filter to scope the batch (e.g. to a
	// tenant); it is ANDed into every per-document filter and read. It starts
	// as an empty document so middleware can wrap it with $and.
	info := &OpInfo{
		Operation:  OpSaveMany,
		Collection: schema.Collection,
		ModelName:  schema.ModelName,
		Filter:     bson.D{},
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		coll := getCollection(db, schema)
		n := rv.Len()
		scope := info.Filter

		items := make([]interface{}, n)
		ids := make([]interface{}, n)
		seen := make(map[string]int, n)
		for i := 0; i < n; i++ {
			items[i] = elemModel(rv.Index(i))
			id, err := modelID(schema, items[i])
			if err != nil {
				return fmt.Errorf("goodm: item %d: %w", i, err)
			}
			if isZeroValue(id) {
				return fmt.Errorf("goodm: item %d: cannot save document with zero ID", i)
			}
			if prev, dup := seen[idKey(id)]; dup {
				return fmt.Errorf("goodm: items %d and %d have the same _id; a batch may contain each document once", prev, i)
			}
			seen[idKey(id)] = i
			ids[i] = id
		}

		if err := checkImmutableFieldsBatch(ctx, coll, schema, items, ids, scope); err != nil {
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
		filters := make([]interface{}, n)
		for i, model := range items {
			oldVersions[i], _ = getModelVersion(model)
			setModelVersion(model, oldVersions[i]+1)
			setUpdatedAt(model, now)
			filters[i] = scopedFilter(scope, buildVersionFilter(ids[i], oldVersions[i]))
		}

		// failures holds the outcome of every item that was not saved;
		// unresolved lists items whose replacement matched no document.
		var out saveOutcome
		var err error
		if !forceLegacySaveMany && supportsClientBulkWrite(ctx, db) {
			out, err = saveManyClientBulk(ctx, db, schema, items, filters)
		} else {
			out, err = saveManyPerDocument(ctx, coll, schema, items, filters)
		}
		if err != nil {
			rollbackVersions(items, oldVersions, nil)
			return err
		}
		if out.wcErr != nil {
			rollbackVersions(items, oldVersions, nil)
			return &BulkError{Collection: schema.Collection, Errors: out.itemErrors(), WriteConcernError: out.wcErr}
		}

		if len(out.unresolved) > 0 {
			if err := resolveUnmatched(ctx, coll, items, ids, out.unresolved, out.failures, scope); err != nil {
				rollbackVersions(items, oldVersions, nil)
				return err
			}
		}

		rollbackVersions(items, oldVersions, out.failures)
		for i, model := range items {
			if _, failed := out.failures[i]; failed {
				continue
			}
			if hook, ok := model.(AfterSave); ok {
				if err := hook.AfterSave(ctx); err != nil {
					return err
				}
			}
		}

		if len(out.failures) == 0 {
			return nil
		}
		return &BulkError{Collection: schema.Collection, Errors: out.itemErrors(), Result: out.result}
	})
}

// forceLegacySaveMany makes SaveMany use the per-document path even on
// servers that support the client-level bulk write. Tests use it to cover
// both paths on one server.
var forceLegacySaveMany bool

// saveOutcome is what the write phase of SaveMany learned.
type saveOutcome struct {
	failures   map[int]error // items the server rejected or that were classified
	unresolved []int         // items whose replacement matched no document
	result     *BulkResult
	wcErr      error
}

func (o *saveOutcome) itemErrors() []BulkWriteError {
	errs := make([]BulkWriteError, 0, len(o.failures))
	for i, e := range o.failures {
		errs = append(errs, BulkWriteError{Index: i, Message: e.Error(), Err: e})
	}
	sort.Slice(errs, func(a, b int) bool { return errs[a].Index < errs[b].Index })
	return errs
}

// saveManyClientBulk sends every replacement in one client-level bulk write
// (MongoDB 8.0+) with verbose results, which report the matched count of each
// operation individually, so no guessing is needed to tell a saved document
// from one another writer changed first.
func saveManyClientBulk(ctx context.Context, db *mongo.Database, schema *Schema, items, filters []interface{}) (saveOutcome, error) {
	out := saveOutcome{failures: map[int]error{}}
	writes := make([]mongo.ClientBulkWrite, len(items))
	for i, model := range items {
		writes[i] = mongo.ClientBulkWrite{
			Database:   db.Name(),
			Collection: schema.Collection,
			Model:      mongo.NewClientReplaceOneModel().SetFilter(filters[i]).SetReplacement(model),
		}
	}
	opts := options.ClientBulkWrite().SetOrdered(false).SetVerboseResults(true)
	if wc := schema.CollOptions.WriteConcern; wc != nil {
		opts.SetWriteConcern(wc)
	}

	res, err := db.Client().BulkWrite(ctx, writes, opts)
	if err != nil {
		var cbwe mongo.ClientBulkWriteException
		if !errors.As(err, &cbwe) {
			return out, fmt.Errorf("goodm: save many failed: %w", err)
		}
		if cbwe.WriteError != nil {
			return out, fmt.Errorf("goodm: save many failed: %w", err)
		}
		for i, we := range cbwe.WriteErrors {
			we.Index = i
			out.failures[i] = translateBulkItem(we, schema).Err
		}
		if len(cbwe.WriteConcernErrors) > 0 {
			wce := cbwe.WriteConcernErrors[0]
			out.wcErr = fmt.Errorf("goodm: write concern error (%d): %s: %w", wce.Code, wce.Message, err)
			return out, nil
		}
		res = cbwe.PartialResult
	}
	if res == nil || !res.HasVerboseResults {
		return out, fmt.Errorf("goodm: save many failed: server returned no per-operation results")
	}
	out.result = &BulkResult{MatchedCount: res.MatchedCount, ModifiedCount: res.ModifiedCount}
	for i := range items {
		if _, failed := out.failures[i]; failed {
			continue
		}
		if r, ok := res.UpdateResults[i]; !ok || r.MatchedCount == 0 {
			out.unresolved = append(out.unresolved, i)
		}
	}
	return out, nil
}

// saveManyPerDocument is the path for servers before 8.0, whose bulk writes
// report only aggregate counts: each document is replaced with its own
// request so the matched count of every replacement is known exactly.
// Failures do not stop the batch.
func saveManyPerDocument(ctx context.Context, coll *mongo.Collection, schema *Schema, items, filters []interface{}) (saveOutcome, error) {
	out := saveOutcome{failures: map[int]error{}, result: &BulkResult{}}
	for i, model := range items {
		res, err := coll.ReplaceOne(ctx, filters[i], model)
		if err != nil {
			var we mongo.WriteException
			switch {
			case errors.As(err, &we) && we.WriteConcernError != nil:
				out.wcErr = fmt.Errorf("goodm: write concern error (%d): %s: %w", we.WriteConcernError.Code, we.WriteConcernError.Message, err)
				return out, nil
			case mongo.IsDuplicateKeyError(err):
				out.failures[i] = newDuplicateKeyError(err, schema, err.Error())
			case errors.As(err, &we):
				out.failures[i] = fmt.Errorf("goodm: write error: %w", err)
			default:
				return out, fmt.Errorf("goodm: save many failed on item %d: %w", i, err)
			}
			continue
		}
		out.result.MatchedCount += res.MatchedCount
		out.result.ModifiedCount += res.ModifiedCount
		if res.MatchedCount == 0 {
			out.unresolved = append(out.unresolved, i)
		}
	}
	return out, nil
}

// resolveUnmatched classifies items whose replacement matched no document:
// a document that still exists within scope is a version conflict (and the
// model's version is refreshed from it); anything else is not found. The
// read goes to the primary so it cannot observe a stale secondary.
func resolveUnmatched(ctx context.Context, coll *mongo.Collection, items, ids []interface{}, unresolved []int, failures map[int]error, scope interface{}) error {
	lookup := make([]interface{}, 0, len(unresolved))
	for _, i := range unresolved {
		lookup = append(lookup, ids[i])
	}
	primary := coll.Database().Collection(coll.Name(), options.Collection().SetReadPreference(readpref.Primary()))
	cursor, err := primary.Find(ctx,
		scopedFilter(scope, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: lookup}}}}),
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}, {Key: "__v", Value: 1}}))
	if err != nil {
		return fmt.Errorf("goodm: failed to resolve unmatched documents: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	versions := map[string]int{}
	for cursor.Next(ctx) {
		var doc struct {
			ID      interface{} `bson:"_id"`
			Version int         `bson:"__v"`
		}
		if err := cursor.Decode(&doc); err != nil {
			return fmt.Errorf("goodm: failed to resolve unmatched documents: %w", err)
		}
		versions[idKey(doc.ID)] = doc.Version
	}
	if err := cursor.Err(); err != nil {
		return fmt.Errorf("goodm: failed to resolve unmatched documents: %w", err)
	}

	for _, i := range unresolved {
		v, exists := versions[idKey(ids[i])]
		if !exists {
			failures[i] = ErrNotFound
			continue
		}
		setModelVersion(items[i], v)
		failures[i] = ErrVersionConflict
	}
	return nil
}

// rollbackVersions restores the pre-save version on failed items. Items whose
// failure is a version conflict keep the version resolveUnmatched refreshed
// from the database. With failures == nil every item is rolled back.
func rollbackVersions(items []interface{}, oldVersions []int, failures map[int]error) {
	for i, model := range items {
		if failures == nil {
			setModelVersion(model, oldVersions[i])
			continue
		}
		if failure, failed := failures[i]; failed && !errors.Is(failure, ErrVersionConflict) {
			setModelVersion(model, oldVersions[i])
		}
	}
}

// scopedFilter ANDs a middleware-supplied scope into a filter. A nil or
// empty scope leaves the filter unchanged.
func scopedFilter(scope interface{}, filter bson.D) interface{} {
	if scope == nil {
		return filter
	}
	if d, ok := scope.(bson.D); ok && len(d) == 0 {
		return filter
	}
	if m, ok := scope.(bson.M); ok && len(m) == 0 {
		return filter
	}
	return bson.D{{Key: "$and", Value: bson.A{scope, filter}}}
}

// serverMajorVersions caches the major server version per client.
var serverMajorVersions sync.Map // *mongo.Client -> int

// supportsClientBulkWrite reports whether the server accepts the client-level
// bulkWrite command (MongoDB 8.0+). A failed probe reports false so SaveMany
// takes the path that works everywhere.
func supportsClientBulkWrite(ctx context.Context, db *mongo.Database) bool {
	client := db.Client()
	if v, ok := serverMajorVersions.Load(client); ok {
		return v.(int) >= 8
	}
	var info struct {
		VersionArray []int32 `bson:"versionArray"`
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil || len(info.VersionArray) == 0 {
		return false
	}
	major := int(info.VersionArray[0])
	serverMajorVersions.Store(client, major)
	return major >= 8
}

// checkImmutableFieldsBatch loads the stored documents for all items in one
// query and compares immutable fields. It is a no-op when the schema has no
// immutable fields. Items whose document no longer exists are skipped here;
// the write phase reports them as ErrNotFound.
func checkImmutableFieldsBatch(ctx context.Context, coll *mongo.Collection, schema *Schema, items, ids []interface{}, scope interface{}) error {
	if !hasImmutableFields(schema) {
		return nil
	}
	cursor, err := coll.Find(ctx, scopedFilter(scope, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}))
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
			items, wcErr, fatal := splitBulkWriteError(err, schema)
			if fatal != nil {
				return fmt.Errorf("goodm: bulk write failed: %w", fatal)
			}
			return &BulkError{Collection: schema.Collection, Errors: items, Result: result, WriteConcernError: wcErr}
		}
		return nil
	})
	return result, err
}
