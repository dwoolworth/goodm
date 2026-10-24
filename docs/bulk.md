# Bulk Operations

goodm has two tiers of batch operations, mirroring the single-document API. `CreateMany` and `SaveMany` run the full model lifecycle per item and then write once. `UpdateMany`, `DeleteMany`, and the `Bulk` builder are raw passthroughs for speed.

## CreateMany

```go
func CreateMany(ctx context.Context, models interface{}, opts ...CreateOptions) error
```

Inserts multiple documents with the full ODM lifecycle per model: ID generation, timestamps, defaults, `BeforeCreate`, validation, a single `InsertMany`, then `AfterCreate` for every model that was inserted.

```go
users := []User{
    {Email: "alice@example.com", Name: "Alice", Age: 30},
    {Email: "bob@example.com", Name: "Bob", Age: 25},
}
err := goodm.CreateMany(ctx, users)
```

Works with both value slices and pointer slices. After `CreateMany`, each model has its `ID`, `CreatedAt`, and `UpdatedAt` set.

If any model fails its hook or validation, nothing is written:

```go
err := goodm.CreateMany(ctx, users)
// Error: "goodm: item 1: validation error on email: field is required"
```

Write failures such as duplicate keys come back as a `*BulkError`. The insert is ordered by default, so it stops at the first failure. Set `Unordered` to insert everything that can be inserted:

```go
err := goodm.CreateMany(ctx, users, goodm.CreateOptions{Unordered: true})
var be *goodm.BulkError
if errors.As(err, &be) {
    for _, e := range be.Errors {
        fmt.Printf("item %d: %v\n", e.Index, e.Err) // e.Err is *DuplicateKeyError here
    }
    fmt.Printf("%d inserted\n", be.Result.InsertedCount)
}
```

## SaveMany

```go
func SaveMany(ctx context.Context, models interface{}, opts ...UpdateOptions) error
```

Persists changes to many loaded models in one call. Each model goes through the `Update` lifecycle: immutable fields are checked against the stored documents (one query for the whole batch), `BeforeSave` runs, the model is validated, and its `Version` and `UpdatedAt` advance. Each document is then replaced with its own conditional request guarded by its version filter, and `AfterSave` runs for every model that was written.

Outside a transaction the replacements are issued concurrently, up to eight in flight, so a batch costs roughly a few round trips of latency rather than one per document. Inside a transaction they run in order on the session. MongoDB's bulk write is deliberately not used: its results do not say which replacement matched, which is exactly what optimistic locking needs to know, and the client-level bulk write of MongoDB 8 cannot honor database-level codecs or write concern.

```go
var tasks []Task
goodm.Find(ctx, bson.D{{Key: "status", Value: "queued"}}, &tasks)
for i := range tasks {
    tasks[i].Status = "scheduled"
}
err := goodm.SaveMany(ctx, tasks)
```

If any model fails its immutable check, hook, or validation, or the slice contains the same `_id` twice, `SaveMany` returns before writing anything. After the write, per-item problems are reported in a `*BulkError`:

| Item error | Meaning |
|-----------|---------|
| `ErrVersionConflict` | Another writer changed the document first. The model's in-memory version is refreshed from the database, as `Update` does. |
| `ErrNotFound` | The document was deleted. |
| `*DuplicateKeyError` | The replacement violated a unique index. |

| `*WriteConcernError`-style item | The write concern was not met for that item; its outcome is unknown, so its version is rolled back and `AfterSave` skipped. `BulkError.WriteConcernError` is set too. |
| transport or context error | Attributed to the item it happened on; the rest of the batch still runs. |

Models that succeeded are saved regardless of failures elsewhere in the batch; failed models keep their old version. A replacement that matched nothing is resolved by reading the document from the primary: still present means `ErrVersionConflict`, gone means `ErrNotFound`. Two writers racing from the same version therefore always see exactly one success and one conflict. If that resolution read itself fails, only the items it was resolving are reported as failed, with the read error; the items already known to be saved keep their new version.

Middleware may replace `op.Filter` for `OpSaveMany` (it starts as an empty document); it is ANDed into every document's filter and out-of-scope documents are reported as `ErrNotFound`.

```go
err := goodm.SaveMany(ctx, tasks)
var be *goodm.BulkError
if errors.As(err, &be) {
    for _, e := range be.Errors {
        if errors.Is(e.Err, goodm.ErrVersionConflict) {
            // reload tasks[e.Index] and retry
        }
    }
}
```

`errors.Is(err, goodm.ErrVersionConflict)` on the `*BulkError` itself reports whether any item had that error.

## UpdateMany

```go
func UpdateMany(ctx context.Context, filter, update, model interface{}, opts ...UpdateOptions) (*BulkResult, error)
```

Updates all documents matching the filter. The `model` parameter is for collection lookup only. `Upsert`, `ArrayFilters`, `Hint`, and `Collation` from `UpdateOptions` apply.

```go
result, err := goodm.UpdateMany(ctx,
    bson.D{{Key: "role", Value: "user"}},
    bson.D{{Key: "$set", Value: bson.D{{Key: "verified", Value: true}}}},
    &User{},
)
fmt.Printf("Matched: %d, Modified: %d\n", result.MatchedCount, result.ModifiedCount)
```

> **Performance:** Direct passthrough to MongoDB. Bypasses hooks, validation, and immutable enforcement.

## DeleteMany

```go
func DeleteMany(ctx context.Context, filter, model interface{}, opts ...DeleteOptions) (*BulkResult, error)
```

Deletes all documents matching the filter.

```go
result, err := goodm.DeleteMany(ctx,
    bson.D{{Key: "role", Value: "guest"}},
    &User{},
)
fmt.Printf("Deleted: %d\n", result.DeletedCount)
```

> **Performance:** Direct passthrough to MongoDB. Bypasses hooks entirely.

## Bulk Builder

```go
func NewBulk(model interface{}, opts ...BulkOptions) *Bulk
```

`Bulk` accumulates heterogeneous writes against one collection and sends them in a single `BulkWrite`. It is a raw passthrough like `UpdateOne`: hooks, validation, timestamps, and versioning do not run, but middleware does (`OpBulkWrite`).

```go
res, err := goodm.NewBulk(&User{}).
    InsertOne(&User{Email: "carol@example.com", Name: "Carol"}).
    UpdateOne(bson.D{{Key: "email", Value: "alice@example.com"}},
        bson.M{"$set": bson.M{"verified": true}}).
    UpdateOne(bson.D{{Key: "email", Value: "dave@example.com"}},
        bson.M{"$set": bson.M{"name": "Dave"}}, goodm.Upsert()).
    UpdateMany(bson.D{{Key: "role", Value: "guest"}},
        bson.M{"$set": bson.M{"role": "user"}}).
    ReplaceOne(bson.D{{Key: "_id", Value: id}}, replacement).
    DeleteOne(bson.D{{Key: "email", Value: "old@example.com"}}).
    DeleteMany(bson.D{{Key: "disabled", Value: true}}).
    Unordered().
    Execute(ctx)
```

| Method | Operation |
|--------|-----------|
| `InsertOne(doc)` | Insert a model or BSON document |
| `UpdateOne(filter, update, opts...)` | Update the first match; `UpdateOptions` for upsert, array filters, hint, collation |
| `UpdateMany(filter, update, opts...)` | Update every match |
| `ReplaceOne(filter, replacement, opts...)` | Replace the first match |
| `DeleteOne(filter, opts...)` | Delete the first match |
| `DeleteMany(filter, opts...)` | Delete every match |
| `Unordered()` | Continue past failures (default is ordered) |
| `Len()` | Number of queued operations |
| `Execute(ctx)` | Send the batch; returns `*BulkResult` |

The driver splits large batches to fit the server's limits, so there is no need to chunk. Inside `WithTransaction`, the bulk write joins the transaction like any other operation.

## BulkResult

```go
type BulkResult struct {
    InsertedCount int64
    MatchedCount  int64
    ModifiedCount int64
    DeletedCount  int64
    UpsertedCount int64
    UpsertedIDs   map[int64]interface{} // operation index -> inserted _id
}
```

## BulkError

```go
type BulkError struct {
    Collection        string
    Errors            []BulkWriteError // one per failed operation or model
    Result            *BulkResult      // what did succeed
    WriteConcernError error            // set when the write concern was not met for some operation
}

type BulkWriteError struct {
    Index   int    // position in the batch
    Code    int    // server error code, 0 for ODM-level failures
    Message string
    Err     error  // *DuplicateKeyError, ErrNotFound, ErrVersionConflict, ...
}
```

`errors.Is` and `errors.As` on a `*BulkError` see every item's `Err`, and `be.Failed(i)` reports whether item `i` failed.

## Comparison

| Operation | Hooks | Validation | Immutable Check | Versioning | DB Calls |
|-----------|-------|-----------|----------------|-----------|----------|
| `CreateMany` | Yes (per model) | Yes (per model) | No | Sets 0 | 1 InsertMany |
| `SaveMany` | Yes (per model) | Yes (per model) | Yes (1 query) | Per-document version filter | 1 ReplaceOne per document, up to 8 concurrent (+1 read to resolve misses) |
| `UpdateMany` | No | No | No | No | 1 UpdateMany |
| `DeleteMany` | No | No | N/A | No | 1 DeleteMany |
| `Bulk` | No | No | No | No | 1 BulkWrite |
