# Middleware

Middleware wraps CRUD operations with composable before/after logic. Use it for logging, metrics, tracing, access control, or any cross-cutting concern.

## Registering Middleware

### Global Middleware

Runs on every CRUD operation for all models:

```go
goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    start := time.Now()
    err := next(ctx)
    log.Printf("[%s] %s.%s took %v err=%v",
        op.Operation, op.Collection, op.ModelName, time.Since(start), err)
    return err
})
```

### Per-Model Middleware

Runs only for operations on the named model:

```go
goodm.UseFor("User", func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    log.Printf("User operation: %s", op.Operation)
    return next(ctx)
})
```

## Execution Order

1. Global middleware (in registration order)
2. Per-model middleware (in registration order)
3. The actual CRUD operation

Each middleware calls `next(ctx)` to continue the chain. Not calling `next` aborts the operation.

```go
// Middleware 1 (registered first)
goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    fmt.Println("1 before")
    err := next(ctx)
    fmt.Println("1 after")
    return err
})

// Middleware 2 (registered second)
goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    fmt.Println("2 before")
    err := next(ctx)
    fmt.Println("2 after")
    return err
})

// Output for any operation:
// 1 before
// 2 before
// (operation executes)
// 2 after
// 1 after
```

## OpInfo

The `OpInfo` struct provides context about the current operation:

```go
type OpInfo struct {
    Operation  OpType      // "create", "find", "update", "delete", etc.
    Collection string      // MongoDB collection name
    ModelName  string      // Go struct name
    Model      interface{} // The model instance (may be nil for filter-based ops)
    Filter     interface{} // The query filter (may be nil for Create)
    Pipeline   []bson.D    // The aggregation stages (OpAggregate) or change stream stages (OpWatch)
}
```

### Operation Types

| OpType | Operations |
|--------|-----------|
| `OpCreate` | `Create` |
| `OpFind` | `FindOne`, `FindByID`, `Find`, `FindCursor`, `Count`, `Exists`, `Distinct` |
| `OpUpdate` | `Update`, `UpdateFields`, `UpdateOne`, `FindOneAndUpdate`, `FindOneAndReplace` |
| `OpDelete` | `Delete`, `DeleteOne`, `FindOneAndDelete` |
| `OpCreateMany` | `CreateMany` |
| `OpSaveMany` | `SaveMany` |
| `OpUpdateMany` | `UpdateMany` |
| `OpDeleteMany` | `DeleteMany` |
| `OpAggregate` | `Pipeline.Execute`, `Pipeline.Cursor` |
| `OpBulkWrite` | `Bulk.Execute` |
| `OpWatch` | `Watch` |

## Modifying Filters and Pipelines

The operation reads `op.Filter` (and `op.Pipeline` for aggregations) after the middleware chain has run, so middleware can replace them. This is how to scope every query to a tenant or hide soft-deleted documents without touching call sites:

```go
goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    tenant, ok := tenantFromContext(ctx)
    if !ok {
        return next(ctx)
    }
    scope := bson.E{Key: "tenant", Value: tenant}
    switch op.Operation {
    case goodm.OpFind, goodm.OpUpdate, goodm.OpDelete, goodm.OpUpdateMany, goodm.OpDeleteMany, goodm.OpSaveMany:
        if op.Filter == nil {
            op.Filter = bson.D{scope}
        } else {
            op.Filter = bson.D{scope, {Key: "$and", Value: bson.A{op.Filter}}}
        }
    case goodm.OpAggregate:
        op.Pipeline = append([]bson.D{{{Key: "$match", Value: bson.D{scope}}}}, op.Pipeline...)
    case goodm.OpWatch:
        // Change events nest the document: scope on fullDocument (delete
        // events carry none and are dropped unless the stream asks for
        // pre-images and the scope also checks fullDocumentBeforeChange).
        match := bson.D{{Key: "$or", Value: bson.A{
            bson.D{{Key: "fullDocument.tenant", Value: tenant}},
            bson.D{{Key: "fullDocumentBeforeChange.tenant", Value: tenant}},
        }}}
        op.Pipeline = append([]bson.D{{{Key: "$match", Value: match}}}, op.Pipeline...)
    }
    return next(ctx)
})
```

Which operations honor a rewritten `op.Filter`:

| Operations | Behavior |
|-----------|----------|
| `Find`, `FindOne`, `FindByID`, `FindCursor`, `Count`, `Exists`, `Distinct`, `UpdateOne`, `UpdateMany`, `DeleteOne`, `DeleteMany`, `FindOneAndUpdate`, `FindOneAndReplace`, `FindOneAndDelete` | Use `op.Filter` as the query |
| `Update`, `UpdateFields`, `Delete`, `SaveMany` | AND `op.Filter` with their `_id` (and version) filters; an out-of-scope document is reported as `ErrNotFound` |
| `Pipeline.Execute`, `Pipeline.Cursor` | Use `op.Pipeline` |
| `Watch` | Use `op.Pipeline` as the change stream's stages; a `$match` on `fullDocument.*` drops delete events, which carry no document |
| `Bulk.Execute` | Not scoped: each queued operation carries its own filter |
| `Populate`, `BatchPopulate` | Not scoped: no middleware runs |

A tenant guard therefore needs to cover the update and delete op types, `OpAggregate`, and `OpWatch` as well as `OpFind`, and cannot rely on middleware for `Bulk` or `Populate`.

## Aborting Operations

Return an error without calling `next` to abort:

```go
goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    if op.Operation == goodm.OpDelete && op.ModelName == "AuditLog" {
        return fmt.Errorf("audit logs cannot be deleted")
    }
    return next(ctx)
})
```

## Modifying Context

Pass a modified context to `next` for tracing, request IDs, etc.:

```go
goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    ctx = context.WithValue(ctx, "trace_id", generateTraceID())
    return next(ctx)
})
```

## Clearing Middleware

Remove all registered middleware (useful in tests):

```go
goodm.ClearMiddleware()
```

## Examples

### Request Timing

```go
goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    start := time.Now()
    err := next(ctx)
    duration := time.Since(start)
    metrics.RecordDBLatency(string(op.Operation), op.Collection, duration)
    return err
})
```

### Error Wrapping

```go
goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    err := next(ctx)
    if err != nil {
        return fmt.Errorf("[%s/%s] %w", op.Collection, op.Operation, err)
    }
    return nil
})
```
