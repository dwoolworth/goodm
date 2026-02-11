# CRUD Operations

goodm provides full-lifecycle CRUD functions that handle ID generation, timestamps, hooks, validation, and immutable field enforcement. It also provides raw passthrough operations for performance-critical paths.

## Create

```go
func Create(ctx context.Context, model interface{}, opts ...CreateOptions) error
```

Inserts a new document with the full ODM lifecycle:

1. Generates `ID` (if zero)
2. Sets `CreatedAt` (if zero) and `UpdatedAt`
3. Runs `BeforeCreate` hook
4. Validates against schema (required, enum, min/max)
5. Inserts into MongoDB
6. Runs `AfterCreate` hook

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
    Limit: 20,
    Skip:  40,
    Sort:  bson.D{{Key: "created_at", Value: -1}},
})
```

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

## Update

```go
func Update(ctx context.Context, model interface{}, opts ...UpdateOptions) error
```

Replaces an existing document with the full ODM lifecycle:

1. Requires non-zero `ID`
2. Fetches existing document from DB
3. Runs `BeforeSave` hook
4. Validates immutable fields (compares with existing doc)
5. Validates against schema
6. Sets `UpdatedAt`
7. Replaces document in MongoDB
8. Runs `AfterSave` hook

```go
user.Age = 31
err := goodm.Update(ctx, user)
```

If an immutable field has changed, Update returns a `ValidationErrors` with the offending field.

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

These bypass hooks, validation, and immutable enforcement. Use them when you need direct MongoDB access for performance.

### UpdateOne

```go
func UpdateOne(ctx context.Context, filter, update, model interface{}, opts ...UpdateOptions) error
```

Partial update using MongoDB update operators. The `model` parameter is for collection lookup only.

```go
err := goodm.UpdateOne(ctx,
    bson.D{{Key: "email", Value: "alice@example.com"}},
    bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: 31}}}},
    &User{},
)
```

> **Performance:** Bypasses hooks, validation, and immutable field enforcement. You are responsible for data integrity.

### DeleteOne

```go
func DeleteOne(ctx context.Context, filter, model interface{}, opts ...DeleteOptions) error
```

Deletes by filter without requiring a loaded model.

```go
err := goodm.DeleteOne(ctx,
    bson.D{{Key: "email", Value: "alice@example.com"}},
    &User{},
)
```

> **Performance:** Bypasses hooks entirely.

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
| `ErrNotFound` | FindOne/Update/Delete finds no matching document |
| `ErrNoDatabase` | No database connection (Connect not called) |
| `ValidationErrors` | Validation or immutable check failure |
