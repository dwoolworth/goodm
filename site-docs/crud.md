# CRUD Operations

goodm provides full-lifecycle CRUD functions that handle ID generation, timestamps, hooks, validation, and immutable field enforcement. It also provides raw passthrough operations for performance-critical paths.

## Create

```go
func Create(ctx context.Context, model interface{}, opts ...CreateOptions) error
```

Inserts a new document with the full ODM lifecycle:

1. Generates `ID` (if zero)
2. Sets `CreatedAt` (if zero) and `UpdatedAt`
3. Applies schema defaults to zero-valued fields
4. Sets `Version` to 0
5. Runs `BeforeCreate` hook
6. Validates against schema (required, enum, min/max)
7. Inserts into MongoDB
8. Runs `AfterCreate` hook

```go
user := &User{Email: "alice@example.com", Name: "Alice", Age: 30}
err := goodm.Create(ctx, user)
// user.ID, user.CreatedAt, user.UpdatedAt are now set
```

## FindOne

```go
func FindOne(ctx context.Context, filter interface{}, result interface{}, opts ...FindOptions) error
```

Finds a single document matching the filter. Returns `ErrNotFound` if no document matches.

```go
user := &User{}
err := goodm.FindOne(ctx, bson.D{{Key: "email", Value: "alice@example.com"}}, user)
if errors.Is(err, goodm.ErrNotFound) {
    // not found
}
```

## FindByID

```go
func FindByID(ctx context.Context, id interface{}, result interface{}, opts ...FindOptions) error
```

Finds a document by `_id`. When the model's ID is a `bson.ObjectID`, a hex string is accepted too.

```go
user := &User{}
err := goodm.FindByID(ctx, "665f1c2e9b1e8a3d4c5b6a70", user)
```

## Find

```go
func Find(ctx context.Context, filter interface{}, results interface{}, opts ...FindOptions) error
```

Finds all matching documents. `results` must be a pointer to a slice.

```go
var users []User
err := goodm.Find(ctx, bson.D{{Key: "role", Value: "admin"}}, &users)
```

With options:

```go
var users []User
err := goodm.Find(ctx, bson.D{}, &users, goodm.FindOptions{
    Limit:      20,
    Skip:       40,
    Sort:       bson.D{{Key: "created_at", Value: -1}},
    Projection: bson.D{{Key: "email", Value: 1}, {Key: "name", Value: 1}},
})
```

`FindOptions`:

| Field | Applies to | Effect |
|-------|-----------|--------|
| `Limit`, `Skip` | Find, FindCursor, Count | Pagination |
| `Sort` | Find, FindOne, FindCursor, FindOneAndDelete | Sort order |
| `Projection` | all finds | Fields to include or exclude |
| `Hint` | all | Index name or key document to use |
| `Collation` | all | Collation for string comparison |
| `BatchSize` | Find, FindCursor | Documents per cursor batch |
| `DB` | all | Override the global database |

## FindCursor

```go
func FindCursor(ctx context.Context, filter interface{}, model interface{}, opts ...FindOptions) (*mongo.Cursor, error)
```

Returns a raw `*mongo.Cursor` for streaming large result sets. The `model` parameter is used only for collection lookup.

```go
cursor, err := goodm.FindCursor(ctx, bson.D{}, &User{}, goodm.FindOptions{
    Sort: bson.D{{Key: "created_at", Value: 1}},
})
if err != nil {
    log.Fatal(err)
}
defer cursor.Close(ctx)

for cursor.Next(ctx) {
    var user User
    cursor.Decode(&user)
    // process user
}
```

## Count, Exists, Distinct

```go
func Count(ctx context.Context, filter, model interface{}, opts ...FindOptions) (int64, error)
func Exists(ctx context.Context, filter, model interface{}, opts ...FindOptions) (bool, error)
func Distinct(ctx context.Context, field string, filter, model, results interface{}, opts ...FindOptions) error
```

```go
n, err := goodm.Count(ctx, bson.D{{Key: "role", Value: "admin"}}, &User{})
ok, err := goodm.Exists(ctx, bson.D{{Key: "email", Value: email}}, &User{})

var roles []string
err = goodm.Distinct(ctx, "role", bson.D{}, &User{}, &roles)
```

## Update

```go
func Update(ctx context.Context, model interface{}, opts ...UpdateOptions) error
```

Replaces an existing document with the full ODM lifecycle:

1. Requires non-zero `ID`
2. Fetches existing document from DB (only if immutable fields exist)
3. Validates immutable fields (compares with existing doc)
4. Runs `BeforeSave` hook
5. Validates against schema
6. Increments `Version` and sets `UpdatedAt`
7. Replaces document in MongoDB with version check (`__v` filter)
8. Returns `ErrVersionConflict` if another process modified the document
9. Runs `AfterSave` hook

```go
user.Age = 31
err := goodm.Update(ctx, user)
```

If an immutable field has changed, Update returns a `ValidationErrors` with the offending field.

## UpdateFields

```go
func UpdateFields(ctx context.Context, model interface{}, fields bson.M, opts ...UpdateOptions) error
```

Partial `$set` of named fields on the document identified by the model's ID, without optimistic locking (last write wins). Use it when concurrent writers touch disjoint fields, such as progress counters or heartbeats. It sets `updated_at`, increments `__v`, and reflects the new values onto the struct.

Each value is first converted to the field's Go type, then validated against the field's schema (`required`, `enum`, `min`, `max`). Only conversions that keep the value's meaning are allowed: numbers to numbers of another width when the value is exactly representable (a `float64` into a `float32` field is the one rounding conversion allowed), documents (Go structs, `bson.M`, `bson.D`, string-keyed maps) to structs, maps, and `bson.D` fields by bson name, arrays to slices and arrays element by element, hex strings to ObjectIDs, `bson.DateTime` to `time.Time`, generic `bson.Binary` and `[]byte` to each other, `bson.Raw` to any document field, and anything into an `interface{}` field. Anything else (`true` for an `int`, a string for a `time.Time`, a `bson.D` for a `[]struct`, a fractional number for an `int`) is a `ValidationError`. Keys a document carries that the schema does not declare are dropped, as `Update` drops them. The converted value is what is stored and what is copied onto the model, so the two always agree; `nil` is stored as null and leaves the model's field at its zero value. The path must exist on the model, so an index past the end of a slice is rejected. Immutable fields are rejected, dotted paths into subdocuments and array elements are allowed, and the map you pass is not modified.

```go
err := goodm.UpdateFields(ctx, task, bson.M{
    "step":           5,
    "progress.pct":   42,
    "items.0.status": "done",
})
```

## FindOneAndUpdate, FindOneAndReplace, FindOneAndDelete

```go
func FindOneAndUpdate(ctx context.Context, filter, update, result interface{}, opts ...FindOneAndUpdateOptions) error
func FindOneAndReplace(ctx context.Context, filter, replacement, result interface{}, opts ...FindOneAndUpdateOptions) error
func FindOneAndDelete(ctx context.Context, filter, result interface{}, opts ...FindOptions) error
```

Atomic read-modify-write. `FindOneAndUpdate` applies a MongoDB update document and decodes the updated document into `result` (set `ReturnOriginal` for the pre-update state). For `bson.D` and `bson.M` updates goodm adds `updated_at`, increments `__v`, and on an upsert that inserts sets `created_at`, unless your update already touches those fields; aggregation-pipeline updates (`[]bson.D`) are sent exactly as given. Hooks, validation, and immutable enforcement do not run.

```go
var job Job
err := goodm.FindOneAndUpdate(ctx,
    bson.D{{Key: "status", Value: "queued"}},
    bson.M{"$set": bson.M{"status": "running", "worker": id}},
    &job,
    goodm.FindOneAndUpdateOptions{Sort: bson.D{{Key: "created_at", Value: 1}}},
)
if errors.Is(err, goodm.ErrNotFound) {
    // nothing queued
}

// Upsert a counter
var c Counter
err = goodm.FindOneAndUpdate(ctx,
    bson.D{{Key: "name", Value: "visits"}},
    bson.M{"$inc": bson.M{"n": 1}},
    &c, goodm.FindOneAndUpdateOptions{Upsert: true},
)
```

`FindOneAndUpdateOptions`: `Upsert`, `ReturnOriginal`, `Sort`, `Projection`, `ArrayFilters`, `Hint`, `Collation`, `DB`.

## Delete

```go
func Delete(ctx context.Context, model interface{}, opts ...DeleteOptions) error
```

Removes a document by its ID with hooks:

1. Requires non-zero `ID`
2. Runs `BeforeDelete` hook
3. Deletes from MongoDB
4. Runs `AfterDelete` hook

```go
err := goodm.Delete(ctx, user)
```

## Raw Operations

These bypass hooks, validation, and immutable enforcement. Use them when you need direct MongoDB access for performance. Middleware still runs.

### UpdateOne

```go
func UpdateOne(ctx context.Context, filter, update, model interface{}, opts ...UpdateOptions) error
```

Partial update using MongoDB update operators. The `model` parameter is for collection lookup only. `UpdateOptions.Upsert`, `ArrayFilters`, `Hint`, and `Collation` apply; `goodm.Upsert()` is a shorthand.

```go
err := goodm.UpdateOne(ctx,
    bson.D{{Key: "email", Value: "alice@example.com"}},
    bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: 31}}}},
    &User{},
)

// Insert if missing
err = goodm.UpdateOne(ctx,
    bson.D{{Key: "email", Value: "bob@example.com"}},
    bson.M{"$set": bson.M{"name": "Bob"}},
    &User{}, goodm.Upsert(),
)
```

Returns `ErrNotFound` when nothing matched and nothing was upserted. Unique index violations return a `*DuplicateKeyError`.

> **Performance:** Bypasses hooks, validation, and immutable field enforcement. You are responsible for data integrity.

### DeleteOne

```go
func DeleteOne(ctx context.Context, filter, model interface{}, opts ...DeleteOptions) error
```

Deletes by filter without requiring a loaded model. `DeleteOptions.Hint` and `Collation` apply.

```go
err := goodm.DeleteOne(ctx,
    bson.D{{Key: "email", Value: "alice@example.com"}},
    &User{},
)
```

> **Performance:** Bypasses hooks entirely.

For batches of mixed writes see [Bulk Operations](bulk.md).

## Options

All operations accept an optional `DB` field to override the global database:

```go
goodm.Create(ctx, user, goodm.CreateOptions{DB: otherDB})
goodm.FindOne(ctx, filter, result, goodm.FindOptions{DB: otherDB})
goodm.Update(ctx, user, goodm.UpdateOptions{DB: otherDB})
goodm.Delete(ctx, user, goodm.DeleteOptions{DB: otherDB})
```

## Error Types

| Error | When |
|-------|------|
| `ErrNotFound` | FindOne/FindByID/Update/Delete and the FindOneAnd* family find no matching document |
| `ErrNoDatabase` | No database connection (Connect not called) |
| `ErrVersionConflict` | Update detects another process modified the document (optimistic concurrency) |
| `ErrDuplicateKey` / `*DuplicateKeyError` | A write violated a unique index; the error carries `Collection`, `Index`, and `Fields` |
| `ValidationErrors` | Validation or immutable check failure |
| `*MergeConflictError` | `WithRetry` could not merge because both writers changed the same fields |
| `*BulkError` | Some operations of CreateMany, SaveMany, or Bulk failed; lists each failed index |

```go
var dke *goodm.DuplicateKeyError
if errors.As(err, &dke) {
    fmt.Printf("%s already taken", strings.Join(dke.Fields, ", "))
}
```
