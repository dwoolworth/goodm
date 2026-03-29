# goodm

**A schema-driven ODM for MongoDB in Go.**

Define your models as Go structs, and goodm handles validation, hooks, indexes, immutability, references, middleware, aggregation, bulk operations, and transactions.

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

Your struct **is** the schema. Tags declare constraints, indexes, and references. The ODM enforces them on every write.

## Why goodm?

Go has a strong MongoDB driver, but no mature ODM that treats Go structs as the schema contract:

- **Prisma dropped Go support.** That door is closed.
- **The official mongo-driver is low-level.** You get `bson.D` and `Collection.InsertOne` — everything else is left to you.
- **Mongoose proved the pattern.** Node.js developers have had schema-as-code with lifecycle hooks, population, and middleware for over a decade. Go deserves the same.

## Quick Start

### Install

```bash
go get github.com/dwoolworth/goodm
```

CLI (optional):

```bash
go install github.com/dwoolworth/goodm/cmd/goodm@latest
```

### Connect and Create

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

| Feature | What it does |
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

## How does goodm compare?

| Feature | goodm | mongo-driver | mgm | mongox |
|---|---|---|---|---|
| Schema-as-struct tags | Yes | No | Partial | No |
| Lifecycle hooks | 6 hooks | No | 3 hooks | No |
| Validation (required, enum, min/max) | Yes | No | No | No |
| Immutable fields | Yes | No | No | No |
| Optimistic concurrency (versioning) | Yes | No | No | No |
| Population (ref resolution) | Yes | No | No | No |
| Aggregation builder | Fluent API | Manual | No | No |
| Middleware (global + per-model) | Yes | No | No | No |
| CLI: discover (DB → Go structs) | Yes | No | No | No |
| CLI: migrate (sync indexes) | Yes | No | No | No |
| Default values | Yes | No | No | No |
| Escape hatches (raw MongoDB) | Yes | N/A | Yes | Yes |

## Requirements

- Go 1.19+
- MongoDB 4.0+ (6.0+ recommended)
- Replica set required for transactions

## License

[MIT](https://github.com/dwoolworth/goodm/blob/main/LICENSE)
