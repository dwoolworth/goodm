# goodm

[![Go Reference](https://pkg.go.dev/badge/github.com/dwoolworth/goodm.svg)](https://pkg.go.dev/github.com/dwoolworth/goodm)
[![Go Report Card](https://goreportcard.com/badge/github.com/dwoolworth/goodm)](https://goreportcard.com/report/github.com/dwoolworth/goodm)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-1.19+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MongoDB](https://img.shields.io/badge/MongoDB-4.0+-47A248?logo=mongodb&logoColor=white)](https://www.mongodb.com/)

A schema-driven ODM for MongoDB in Go. Define your models as Go structs, and goodm handles validation, hooks, indexes, immutability, references, middleware, aggregation, bulk operations, and transactions.

```go
type User struct {
    goodm.Model `bson:",inline"`
    Email       string `bson:"email" goodm:"unique,required"`
    Name        string `bson:"name"  goodm:"required,immutable"`
    Age         int    `bson:"age"   goodm:"min=13,max=120"`
    Role        string `bson:"role"  goodm:"enum=admin|user|mod,default=user"`
}

func init() {
    goodm.Register(&User{}, "users")
}
```

## Why goodm?

Go has a strong MongoDB driver, but no mature ODM that treats Go structs as the schema contract. The ecosystem gap is real:

- **Prisma dropped Go support.** If you were looking to Prisma for structured data modeling in Go, that door is closed.
- **The official mongo-driver is low-level.** You get `bson.D` and `Collection.InsertOne` — everything else (validation, hooks, timestamps, immutability, index management) is left to you.
- **Mongoose proved the pattern.** Node.js developers have had schema-as-code with lifecycle hooks, population, and middleware for over a decade. Go deserves the same.

goodm fills that gap. Your Go struct *is* the schema. Tags declare constraints, indexes, and references. The ODM enforces them on every write — no separate schema files, no code generation step, no runtime surprises.

**What you get:**
- **One source of truth.** The struct definition *is* the database contract. Add `goodm:"unique,required"` and the index exists, the validation runs, the error is typed.
- **Full write lifecycle.** Create and Update automatically handle ID generation, timestamps, hook execution, validation, and immutable field enforcement. You don't wire any of it.
- **Escape hatches.** `UpdateOne`, `DeleteOne`, `UpdateMany`, `DeleteMany` bypass the ODM and hit MongoDB directly when you need raw performance.
- **Schema-aware CLI.** `goodm discover` reverse-engineers an existing database into Go structs. `goodm migrate` diffs your structs against a live database and syncs indexes.

## Install

```bash
go get github.com/dwoolworth/goodm
```

CLI (optional):

```bash
go install github.com/dwoolworth/goodm/cmd/goodm@latest
```

## Quick Start

```go
package main

import (
    "context"
    "log"

    "github.com/dwoolworth/goodm"
    "go.mongodb.org/mongo-driver/v2/bson"
)

type User struct {
    goodm.Model `bson:",inline"`
    Email       string `bson:"email" goodm:"unique,required"`
    Name        string `bson:"name"  goodm:"required,immutable"`
    Age         int    `bson:"age"   goodm:"min=13,max=120"`
}

func init() {
    goodm.Register(&User{}, "users")
}

func main() {
    ctx := context.Background()

    // Connect and enforce indexes
    db, _ := goodm.Connect(ctx, "mongodb://localhost:27017", "myapp")
    goodm.Enforce(ctx, db)

    // Create
    user := &User{Email: "alice@example.com", Name: "Alice", Age: 30}
    goodm.Create(ctx, user)

    // Read
    found := &User{}
    goodm.FindOne(ctx, bson.D{{Key: "email", Value: "alice@example.com"}}, found)

    // Update
    found.Age = 31
    goodm.Update(ctx, found)

    // Delete
    goodm.Delete(ctx, found)
}
```

## Features

| Feature | Description |
|---------|-------------|
| **Schema Tags** | `unique`, `index`, `required`, `immutable`, `default`, `enum`, `min`, `max`, `ref` |
| **CRUD** | `Create`, `FindOne`, `Find`, `FindCursor`, `Update`, `Delete` with full lifecycle |
| **Raw Operations** | `UpdateOne`, `DeleteOne`, `UpdateMany`, `DeleteMany` for direct MongoDB access |
| **Hooks** | `BeforeCreate`, `AfterCreate`, `BeforeSave`, `AfterSave`, `BeforeDelete`, `AfterDelete` |
| **Validation** | Automatic on Create/Update: required, enum, min/max, immutable enforcement |
| **Middleware** | Global and per-model middleware chains wrapping all operations |
| **Population** | Resolve `ref=` fields by fetching referenced documents |
| **Aggregation** | Fluent pipeline builder with `Match`, `Group`, `Sort`, `Lookup`, and more |
| **Bulk** | `CreateMany` with hooks/validation, `UpdateMany`/`DeleteMany` passthrough |
| **Transactions** | `WithTransaction` wraps operations in a MongoDB session transaction |
| **CLI** | `goodm discover` (introspect DB), `goodm migrate` (sync indexes), `goodm inspect` (view schemas) |

## Documentation

Detailed guides are in the [`docs/`](docs/) directory:

- [Getting Started](docs/getting-started.md) - Installation, connection, first model
- [Models & Tags](docs/models.md) - Defining models, all tag options, compound indexes
- [CRUD Operations](docs/crud.md) - Create, read, update, delete with examples
- [Hooks](docs/hooks.md) - Lifecycle hooks and when they fire
- [Validation](docs/validation.md) - Required, enum, min/max, immutable fields
- [Middleware](docs/middleware.md) - Global and per-model middleware chains
- [Population](docs/populate.md) - Resolving document references
- [Aggregation](docs/pipeline.md) - Fluent pipeline builder
- [Bulk Operations](docs/bulk.md) - Batch insert, update, delete
- [Transactions](docs/transactions.md) - Multi-document ACID transactions
- [CLI](docs/cli.md) - discover, migrate, inspect commands

## Schema Tags

Tags are added to struct fields via `goodm:"..."`:

```go
type Product struct {
    goodm.Model `bson:",inline"`
    SKU         string        `bson:"sku"      goodm:"unique,required,immutable"`
    Name        string        `bson:"name"     goodm:"required"`
    Price       int           `bson:"price"    goodm:"min=0"`
    Category    string        `bson:"category" goodm:"index,enum=electronics|clothing|food"`
    Stock       int           `bson:"stock"    goodm:"default=0,min=0"`
    BrandID     bson.ObjectID `bson:"brand"    goodm:"ref=brands"`
}
```

| Tag | Effect |
|-----|--------|
| `unique` | Creates a unique index |
| `index` | Creates a non-unique index |
| `required` | Field must be non-zero on Create/Update |
| `immutable` | Field cannot change after creation |
| `default=X` | Default value annotation |
| `enum=a\|b\|c` | Value must be one of the listed options |
| `min=N` | Minimum numeric value |
| `max=N` | Maximum numeric value |
| `ref=collection` | References a document in another collection |

## Middleware

```go
// Global middleware — runs on every operation
goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    start := time.Now()
    err := next(ctx)
    log.Printf("%s %s.%s took %v", op.Operation, op.Collection, op.ModelName, time.Since(start))
    return err
})

// Per-model middleware
goodm.UseFor("User", func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    log.Printf("User operation: %s", op.Operation)
    return next(ctx)
})
```

## Aggregation

```go
var results []bson.M
goodm.NewPipeline(&User{}).
    Match(bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 21}}}}).
    Group(bson.D{
        {Key: "_id", Value: "$role"},
        {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
    }).
    Sort(bson.D{{Key: "count", Value: -1}}).
    Limit(10).
    Execute(ctx, &results)
```

## Transactions

```go
err := goodm.WithTransaction(ctx, func(ctx context.Context) error {
    if err := goodm.Create(ctx, order); err != nil {
        return err
    }
    return goodm.Update(ctx, inventory)
})
```

Requires a MongoDB replica set. All goodm operations inside the callback participate in the transaction automatically.

## Requirements

- Go 1.19+
- MongoDB 4.0+ (6.0+ recommended)
- Replica set required for transactions

## Contributing

Contributions are welcome! Here's how to get started:

1. **Fork** the repository
2. **Clone** your fork: `git clone https://github.com/<you>/goodm.git`
3. **Create a branch**: `git checkout -b my-feature`
4. **Make your changes** and add tests
5. **Run tests**: `go test ./...` (requires a running MongoDB instance)
6. **Commit**: `git commit -m "Add my feature"`
7. **Push**: `git push origin my-feature`
8. **Open a Pull Request** against `main`

### Guidelines

- Follow existing code style and patterns
- Add tests for new features — integration tests use a local MongoDB
- Keep PRs focused on a single change
- Update documentation in `docs/` if you add or change behavior
- Run `go vet ./...` before submitting

### Reporting Issues

Found a bug or have a feature request? [Open an issue](https://github.com/dwoolworth/goodm/issues) with a clear description and, if applicable, steps to reproduce.

## License

[MIT](LICENSE)
