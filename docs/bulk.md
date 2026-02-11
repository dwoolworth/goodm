# Bulk Operations

goodm provides batch operations for inserting, updating, and deleting multiple documents.

## CreateMany

```go
func CreateMany(ctx context.Context, models interface{}, opts ...CreateOptions) error
```

Inserts multiple documents with the full ODM lifecycle per model: ID generation, timestamps, hooks, and validation. Uses a single `InsertMany` call to MongoDB.

```go
users := []User{
    {Email: "alice@example.com", Name: "Alice", Age: 30},
    {Email: "bob@example.com", Name: "Bob", Age: 25},
    {Email: "carol@example.com", Name: "Carol", Age: 28},
}

err := goodm.CreateMany(ctx, users)
```

Works with both value slices and pointer slices:

```go
// Also valid
users := []*User{
    {Email: "alice@example.com", Name: "Alice", Age: 30},
    {Email: "bob@example.com", Name: "Bob", Age: 25},
}
err := goodm.CreateMany(ctx, users)
```

After `CreateMany`, each model in the slice has its `ID`, `CreatedAt`, and `UpdatedAt` set.

> **Performance:** Hooks (`BeforeCreate`/`AfterCreate`) and validation run per-model. For large batches where you don't need the ODM lifecycle, use the mongo driver's `InsertMany` directly.

### Validation

If any model fails validation, the entire operation is aborted before the database write:

```go
users := []User{
    {Email: "ok@example.com", Name: "OK", Age: 30},
    {Email: "", Name: "Bad", Age: 30}, // fails: email required
}
err := goodm.CreateMany(ctx, users)
// Error: "goodm: validation failed on item 1: ..."
```

## UpdateMany

```go
func UpdateMany(ctx context.Context, filter, update, model interface{}, opts ...UpdateOptions) (*BulkResult, error)
```

Updates all documents matching the filter. The `model` parameter is for collection lookup only.

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

## BulkResult

`UpdateMany` and `DeleteMany` return a `BulkResult`:

```go
type BulkResult struct {
    InsertedCount int64
    MatchedCount  int64
    ModifiedCount int64
    DeletedCount  int64
}
```

## Comparison

| Operation | Hooks | Validation | Immutable Check | DB Calls |
|-----------|-------|-----------|----------------|----------|
| `CreateMany` | Yes (per model) | Yes (per model) | No | 1 InsertMany |
| `UpdateMany` | No | No | No | 1 UpdateMany |
| `DeleteMany` | No | No | N/A | 1 DeleteMany |
