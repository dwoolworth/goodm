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
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/session"
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
// If any model fails its BeforeCreate hook or validation the call returns
// before writing. Write failures (such as duplicate keys) are returned as a
// *BulkError listing the failed indexes. By default the insert is ordered and
// stops at the first failure; set CreateOptions.Unordered to insert
// everything that can be. An AfterCreate hook that returns an error is
// reported for its item in the *BulkError, after the other hooks have run;
// the document stays inserted. Inside a transaction a server rejection has
// aborted the transaction, so CreateMany reports every item as not inserted
// (items an ordered insert never sent as ErrNotAttempted) and runs no
// AfterCreate.
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
			bulkErr = &BulkError{Collection: schema.Collection, Errors: items, WriteConcernError: wcErr, Result: &BulkResult{}}
			if wcErr != nil {
				// The server may or may not have applied the inserts; nothing
				// can be reported as created.
				return bulkErr
			}
			if inTransaction(ctx) {
				// The server rejection aborted the transaction, so no item
				// will commit: report every other item as not inserted (or
				// as not attempted, if an ordered insert never sent it) and
				// run no hooks.
				first := items[0].Index
				for i := 0; i < rv.Len(); i++ {
					if bulkErr.Failed(i) {
						continue
					}
					var itemErr error
					if !opt.Unordered && i > first {
						itemErr = fmt.Errorf("%w: ordered insert stopped at item %d", ErrNotAttempted, first)
					} else {
						itemErr = fmt.Errorf("goodm: not inserted: transaction aborted by item %d: %v", first, items[0].Err)
					}
					bulkErr.Errors = append(bulkErr.Errors, BulkWriteError{Index: i, Message: itemErr.Error(), Err: itemErr})
				}
				sort.Slice(bulkErr.Errors, func(a, b int) bool { return bulkErr.Errors[a].Index < bulkErr.Errors[b].Index })
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
					// The document is inserted; report the hook failure for
					// its item after the other hooks have run.
					if bulkErr == nil {
						bulkErr = &BulkError{Collection: schema.Collection}
					}
					hookErr := fmt.Errorf("goodm: created, but AfterCreate failed: %w", err)
					bulkErr.Errors = append(bulkErr.Errors, BulkWriteError{Index: i, Message: hookErr.Error(), Err: hookErr})
				}
			}
		}

		if bulkErr != nil {
			sort.Slice(bulkErr.Errors, func(a, b int) bool { return bulkErr.Errors[a].Index < bulkErr.Errors[b].Index })
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

// SaveMany persists changes to many loaded models in one call. Each model
// goes through the Update lifecycle: immutable fields are checked against the
// stored documents (one query for the whole batch), BeforeSave runs, the
// model is validated, and its version and UpdatedAt advance. Each document is
// then replaced with its own request guarded by its version filter, and
// AfterSave runs for every model that was written. Of UpdateOptions only DB
// is used.
//
// If any model fails its immutable check, hook, or validation, or the batch
// contains the same _id twice, SaveMany returns before writing anything. After
// the write, documents that were deleted or modified by another writer are
// reported in a *BulkError with ErrNotFound or ErrVersionConflict per index
// (the in-memory version of a conflicting model is refreshed from the
// database, as Update does); unique index violations appear as
// *DuplicateKeyError. An item whose write concern was not met, or whose
// request failed in transport or by context cancellation, is listed with that
// error marked "outcome unknown": the server may have applied it, so its
// version is rolled back and its AfterSave skipped (a later save of it then
// reports a conflict rather than losing anything). Once the context is done
// the remaining items are not attempted. BulkError.WriteConcernError is set
// when any item hit a write concern error. Successful models are saved
// regardless of failures elsewhere in the batch; an AfterSave hook that
// returns an error is reported for its item as well, after the other hooks
// have run, and does not undo the save.
//
// Inside a transaction (goodm's WithTransaction or one started on the
// session by the driver) a server error on any item (a duplicate key, a
// write conflict, a validator failure) has aborted the transaction, so
// SaveMany stops there, restores every model's version, runs no AfterSave,
// and reports every item as not saved; a server error on the resolution read
// does the same. Version conflicts, missing documents, and client-side
// failures (a marshal error, a done context) leave the transaction running
// and are reported per item as usual. SaveMany never aborts a transaction
// itself. With a session that has no transaction running, items are sent
// one at a time and failures do not stop the batch.
//
// Each document is replaced with its own conditional request, so every
// replacement's matched count is known exactly and every failure is
// attributed to one item. Outside a transaction the requests are issued
// concurrently (up to 8 in flight, so custom BSON marshalers run on several
// goroutines at once); inside one they run in order. A bulk write would be
// fewer round trips, but MongoDB's bulk results do not say which replacement
// matched, and the client-level bulkWrite of MongoDB 8 cannot honor
// database-level codecs or write concern.
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

		out := saveManyReplace(ctx, coll, schema, items, filters)
		if out.aborted {
			// The transaction is dead: nothing in this batch will commit.
			for i := range items {
				setModelVersion(items[i], oldVersions[i])
			}
			return &BulkError{Collection: schema.Collection, Errors: out.itemErrors(), Result: &BulkResult{}, WriteConcernError: out.wcErr}
		}

		if len(out.unresolved) > 0 {
			if err := resolveUnmatched(ctx, coll, schema, items, ids, out.unresolved, out.failures, scope); err != nil {
				if out.inTxn && isServerError(err) {
					// The server failed the read, which aborted the
					// transaction: nothing in the batch will commit. The read
					// error is wrapped so its labels (e.g. the driver's
					// transient-transaction label) stay visible.
					for i := range items {
						setModelVersion(items[i], oldVersions[i])
						out.failures[i] = fmt.Errorf("goodm: not saved: transaction aborted while resolving the batch: %w", err)
					}
					return &BulkError{Collection: schema.Collection, Errors: out.itemErrors(), Result: &BulkResult{}, WriteConcernError: out.wcErr}
				}
				// The replacements already happened; only the outcome of the
				// unresolved items is unknown. Report those, keep the rest.
				for _, i := range out.unresolved {
					out.failures[i] = fmt.Errorf("goodm: outcome unknown: %w", err)
				}
			}
		}

		rollbackVersions(items, oldVersions, out.failures)
		for i, model := range items {
			if _, failed := out.failures[i]; failed {
				continue
			}
			if hook, ok := model.(AfterSave); ok {
				if err := hook.AfterSave(ctx); err != nil {
					out.failures[i] = fmt.Errorf("goodm: saved, but AfterSave failed: %w", err)
				}
			}
		}

		if len(out.failures) == 0 {
			return nil
		}
		return &BulkError{Collection: schema.Collection, Errors: out.itemErrors(), Result: out.result, WriteConcernError: out.wcErr}
	})
}

// saveManyConcurrency bounds the number of replacements in flight at once
// when SaveMany runs outside a transaction.
const saveManyConcurrency = 8

// saveOutcome is what the write phase of SaveMany learned.
type saveOutcome struct {
	failures   map[int]error // items the server rejected, or that were classified
	codes      map[int]int   // server error codes for failures that have one
	unresolved []int         // items whose replacement matched no document
	result     *BulkResult
	wcErr      error // first write concern error, if any
	inTxn      bool  // the batch ran inside a transaction
	aborted    bool  // the batch ran in a transaction that a failure aborted
}

func (o *saveOutcome) itemErrors() []BulkWriteError {
	errs := make([]BulkWriteError, 0, len(o.failures))
	for i, e := range o.failures {
		errs = append(errs, BulkWriteError{Index: i, Code: o.codes[i], Message: e.Error(), Err: e})
	}
	sort.Slice(errs, func(a, b int) bool { return errs[a].Index < errs[b].Index })
	return errs
}

// replaceResult is the outcome of one document's replacement.
type replaceResult struct {
	matched, modified int64
	err               error // server rejection, write concern, or transport error
	code              int   // server error code, if any
	wc                error // write concern error, if any
	server            bool  // the server produced the error, so inside a transaction it has aborted it
}

// ErrNotAttempted marks items SaveMany or CreateMany did not send: the
// context was done, an earlier item's server error aborted the transaction,
// or an ordered insert stopped at an earlier failure. In the context case the
// context error is wrapped too, so errors.Is(err, context.Canceled) works.
var ErrNotAttempted = errors.New("goodm: not attempted")

// isServerError reports whether err came back through the driver's server
// error types (a command or write error), as opposed to a failure raised
// before anything was sent, such as a marshal error or a context that was
// already done. The driver also wraps network errors and in-flight timeouts
// as command errors, so inside a transaction those count as aborting too:
// the transaction may or may not survive them, and treating it as aborted
// errs toward a later conflict rather than a lost write.
func isServerError(err error) bool {
	var se mongo.ServerError
	return errors.As(err, &se)
}

// isTransportError reports whether err is a network failure or an in-flight
// context error, after which the server may or may not have applied the
// write.
func isTransportError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var le mongo.LabeledError
	return errors.As(err, &le) && le.HasErrorLabel("NetworkError")
}

// inTransaction reports whether ctx carries a session with a transaction in
// progress, whether it was started by goodm's WithTransaction or by the
// driver directly. The driver exposes no query for this, so it is probed with
// StartTransaction, which is purely local: it fails with ErrTransactInProgress
// when a transaction is running, and otherwise starts one that is aborted
// again locally before any operation is sent. (The probe assumes the session
// has not been ended; using an ended session is a caller bug the driver
// would otherwise report on the first operation.)
func inTransaction(ctx context.Context) bool {
	sess := mongo.SessionFromContext(ctx)
	if sess == nil {
		return false
	}
	switch err := sess.StartTransaction(); {
	case errors.Is(err, session.ErrTransactInProgress):
		return true
	case err == nil:
		_ = sess.AbortTransaction(ctx)
	}
	return false
}

// saveManyReplace replaces each document with its own conditional request,
// so every replacement's matched count is known exactly and every failure is
// attributed to one item. Outside a transaction the requests are issued
// concurrently and a failure never stops the batch; once the context is done
// the remaining items are reported as not attempted. Inside a transaction
// the requests run in order on the session, and the first failure stops the
// batch because the server has aborted the transaction.
func saveManyReplace(ctx context.Context, coll *mongo.Collection, schema *Schema, items, filters []interface{}) saveOutcome {
	n := len(items)
	results := make([]replaceResult, n)
	replace := func(i int) {
		if err := ctx.Err(); err != nil {
			results[i] = replaceResult{err: fmt.Errorf("%w: %w", ErrNotAttempted, err)}
			return
		}
		res, err := coll.ReplaceOne(ctx, filters[i], items[i])
		if err != nil {
			results[i] = classifyReplaceError(err, schema)
			return
		}
		results[i] = replaceResult{matched: res.MatchedCount, modified: res.ModifiedCount}
	}

	out := saveOutcome{failures: map[int]error{}, codes: map[int]int{}, result: &BulkResult{}}
	hasSession := mongo.SessionFromContext(ctx) != nil
	out.inTxn = hasSession && inTransaction(ctx)
	switch {
	case out.inTxn:
		// A server error has aborted the transaction, so stop: earlier items
		// will not commit. They are reported with the cause described but
		// not wrapped, so only the failing item matches its error type. A
		// client-side failure (marshal error, done context) leaves the
		// transaction running and is handled like any other item failure;
		// nothing is aborted on the client, because resetting the session's
		// state would let later writes in the transaction commit on their own.
		for i := range items {
			replace(i)
			if results[i].err != nil && results[i].server {
				out.aborted = true
				for j := i + 1; j < n; j++ {
					results[j] = replaceResult{err: fmt.Errorf("%w: transaction aborted by item %d", ErrNotAttempted, i)}
				}
				for j := 0; j < i; j++ {
					if results[j].err != nil {
						continue // keep the item's own failure
					}
					results[j] = replaceResult{err: fmt.Errorf("goodm: not saved: transaction aborted by item %d: %v", i, results[i].err)}
				}
				break
			}
		}
	case hasSession || n == 1:
		// A session cannot be used from several goroutines.
		for i := range items {
			replace(i)
		}
	default:
		var wg sync.WaitGroup
		work := make(chan int)
		workers := saveManyConcurrency
		if n < workers {
			workers = n
		}
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range work {
					replace(i)
				}
			}()
		}
		for i := range items {
			work <- i
		}
		close(work)
		wg.Wait()
	}

	for i, r := range results {
		if r.wc != nil && out.wcErr == nil {
			out.wcErr = r.wc
		}
		if r.err != nil {
			out.failures[i] = r.err
			if r.code != 0 {
				out.codes[i] = r.code
			}
			continue
		}
		out.result.MatchedCount += r.matched
		out.result.ModifiedCount += r.modified
		if r.matched == 0 {
			out.unresolved = append(out.unresolved, i)
		}
	}
	return out
}

// classifyReplaceError turns a ReplaceOne failure into the item's error. A
// server write error (such as a duplicate key) is translated with its code;
// a write concern error is recorded as well, and stands alone when there is
// no write error. Anything else (transport, context) means the server may or
// may not have applied the write, so it is reported as outcome unknown.
func classifyReplaceError(err error, schema *Schema) replaceResult {
	// Decide from the driver's error, before translation, whether the server
	// produced it. Inside a transaction a server error has aborted the
	// transaction; network, timeout, and server-selection failures are
	// wrapped the same way by the driver and are treated as aborting too,
	// which errs toward reporting a conflict later rather than a lost write.
	r := replaceResult{server: isServerError(err)}
	var we mongo.WriteException
	if !errors.As(err, &we) {
		var ce mongo.CommandError
		if errors.As(err, &ce) && !isTransportError(err) {
			// A command error from the server (write conflict, no such
			// transaction) is known not to have applied.
			r.err = fmt.Errorf("goodm: replace failed: %w", err)
			r.code = int(ce.Code)
			return r
		}
		r.err = fmt.Errorf("goodm: outcome unknown: %w", err)
		return r
	}
	if we.WriteConcernError != nil {
		r.wc = fmt.Errorf("goodm: write concern error (%d): %s: %w", we.WriteConcernError.Code, we.WriteConcernError.Message, err)
		r.code = we.WriteConcernError.Code
	}
	if len(we.WriteErrors) > 0 {
		item := translateBulkItem(we.WriteErrors[0], schema)
		r.err = item.Err
		r.code = item.Code
	} else if r.wc != nil {
		r.err = fmt.Errorf("goodm: outcome unknown: %w", r.wc)
	} else {
		r.err = fmt.Errorf("goodm: outcome unknown: %w", err)
	}
	return r
}

// resolveUnmatched classifies items whose replacement matched no document:
// a document that still exists within scope is a version conflict (and the
// model's version is refreshed from it); anything else is not found. The
// read goes to the primary so it cannot observe a stale secondary, and the
// documents are decoded into the model type so their _id values compare
// with the models' through the same Go type.
func resolveUnmatched(ctx context.Context, coll *mongo.Collection, schema *Schema, items, ids []interface{}, unresolved []int, failures map[int]error, scope interface{}) error {
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
		doc := reflect.New(schema.Type).Interface()
		if err := cursor.Decode(doc); err != nil {
			return fmt.Errorf("goodm: failed to resolve unmatched documents: %w", err)
		}
		id, err := modelID(schema, doc)
		if err != nil {
			return err
		}
		v, _ := getModelVersion(doc)
		versions[idKey(id)] = v
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
// from the database.
func rollbackVersions(items []interface{}, oldVersions []int, failures map[int]error) {
	for i, model := range items {
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
