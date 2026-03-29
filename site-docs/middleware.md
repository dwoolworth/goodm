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
}
```

### Operation Types

| OpType | Operations |
|--------|-----------|
| `OpCreate` | `Create` |
| `OpFind` | `FindOne`, `Find`, `FindCursor` |
| `OpUpdate` | `Update`, `UpdateOne` |
| `OpDelete` | `Delete`, `DeleteOne` |
| `OpCreateMany` | `CreateMany` |
| `OpUpdateMany` | `UpdateMany` |
| `OpDeleteMany` | `DeleteMany` |

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
