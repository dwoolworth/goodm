# Getting Started

## Installation

Add goodm to your Go module:

```bash
go get github.com/dwoolworth/goodm
```

For the CLI tools (optional):

```bash
go install github.com/dwoolworth/goodm/cmd/goodm@latest
```

## Connecting to MongoDB

```go
package main

import (
    "context"
    "log"

    "github.com/dwoolworth/goodm"
)

func main() {
    ctx := context.Background()

    db, err := goodm.Connect(ctx, "mongodb://localhost:27017", "myapp")
    if err != nil {
        log.Fatal(err)
    }

    // Enforce creates indexes defined in your schemas
    if err := goodm.Enforce(ctx, db); err != nil {
        log.Fatal(err)
    }
}
```

`Connect` stores the database reference globally. All CRUD functions use it by default, or you can pass a specific database via options:

```go
goodm.Create(ctx, user, goodm.CreateOptions{DB: otherDB})
```

## Defining Your First Model

Every model embeds `goodm.Model` (which provides `_id`, `created_at`, `updated_at`) and must be registered with a collection name:

```go
type User struct {
    goodm.Model `bson:",inline"`
    Email       string `bson:"email" goodm:"unique,required"`
    Name        string `bson:"name"  goodm:"required"`
    Age         int    `bson:"age"   goodm:"min=0,max=200"`
}

func init() {
    if err := goodm.Register(&User{}, "users"); err != nil {
        panic(err)
    }
}
```

The `init()` function runs automatically when the package is imported, ensuring models are registered before any operations.

## Your First CRUD Operations

```go
ctx := context.Background()

// Create — generates ID, sets timestamps, validates
user := &User{Email: "alice@example.com", Name: "Alice", Age: 30}
if err := goodm.Create(ctx, user); err != nil {
    log.Fatal(err)
}
fmt.Println(user.ID)        // generated ObjectID
fmt.Println(user.CreatedAt) // set automatically

// Read
found := &User{}
err := goodm.FindOne(ctx, bson.D{{Key: "email", Value: "alice@example.com"}}, found)

// Update — validates, enforces immutable fields, updates timestamp
found.Age = 31
err = goodm.Update(ctx, found)

// Delete
err = goodm.Delete(ctx, found)
```

## Enforcing Schemas

`Enforce` ensures your database indexes match your schema definitions:

```go
// Basic — create missing indexes
goodm.Enforce(ctx, db)

// With drift detection — warn about fields in DB not in schema
goodm.Enforce(ctx, db, goodm.EnforceOptions{
    DriftPolicy: goodm.DriftWarn,
    OnDriftWarning: func(d goodm.DriftError) {
        log.Printf("drift: %v", d)
    },
})

// Strict — fail on drift
goodm.Enforce(ctx, db, goodm.EnforceOptions{
    DriftPolicy: goodm.DriftFatal,
})
```

## Error Handling

goodm provides typed errors:

```go
err := goodm.FindOne(ctx, filter, result)

if errors.Is(err, goodm.ErrNotFound) {
    // document doesn't exist
}

if errors.Is(err, goodm.ErrNoDatabase) {
    // Connect() hasn't been called
}

var ve goodm.ValidationErrors
if errors.As(err, &ve) {
    for _, e := range ve {
        fmt.Printf("field %s: %s\n", e.Field, e.Message)
    }
}
```

## Project Layout

A typical goodm project looks like:

```
myapp/
├── models/
│   ├── user.go      # User struct + init() registration
│   ├── post.go      # Post struct + init() registration
│   └── profile.go
├── main.go           # Connect, Enforce, app logic
└── go.mod
```

Import the models package in `main.go` (even if unused directly) to trigger `init()` registration:

```go
import _ "myapp/models"
```

## Next Steps

- [Models & Tags](models.md) — All tag options and compound indexes
- [CRUD Operations](crud.md) — Full CRUD API reference
- [Hooks](hooks.md) — Lifecycle hooks
- [CLI](cli.md) — Database introspection and migration tools
