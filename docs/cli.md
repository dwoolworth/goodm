# CLI

The goodm CLI provides tools for database introspection, schema migration, and schema inspection.

## Installation

```bash
go install github.com/dwoolworth/goodm/cmd/goodm@latest
```

## Commands

### goodm discover

Introspect an existing MongoDB database and generate Go model files.

```bash
goodm discover --db myapp
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--uri` | `mongodb://localhost:27017` | MongoDB connection URI |
| `--db` | (required) | Database name |
| `--collection` | (all) | Specific collection to discover |
| `--output` | `./models` | Output directory for generated files |
| `--package` | `models` | Go package name for generated files |
| `--sample-size` | `500` | Documents to sample per collection |

**What it does:**

1. Connects to the database
2. Samples documents from each collection to infer field types
3. Reads existing indexes
4. Generates Go struct definitions with:
   - `bson` tags matching field names
   - `goodm` tags for `unique`, `index`, `required` (inferred from indexes and field prevalence)
   - Compound index declarations
   - `init()` registration function

**Example output** (`models/users.go`):

```go
package models

import (
    "github.com/dwoolworth/goodm"
    "go.mongodb.org/mongo-driver/v2/bson"
)

type User struct {
    goodm.Model `bson:",inline"`
    Email       string        `bson:"email"   goodm:"unique,required"`
    Name        string        `bson:"name"    goodm:"required"`
    Age         int           `bson:"age"`
    Profile     bson.ObjectID `bson:"profile"`
}

func init() {
    if err := goodm.Register(&User{}, "users"); err != nil {
        panic(err)
    }
}
```

### goodm migrate

Compare registered schemas against a live database and synchronize indexes.

```bash
goodm migrate --db myapp
goodm migrate --db myapp --dry-run
goodm migrate --db myapp --drop-extras
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--uri` | `mongodb://localhost:27017` | MongoDB connection URI |
| `--db` | (required) | Database name |
| `--dry-run` | `false` | Show planned changes without applying |
| `--drop-extras` | `false` | Drop indexes in DB but not in schema |

**What it does:**

1. Reads all registered model schemas
2. Compares expected indexes vs actual indexes in the database
3. Shows a migration plan:
   - `+` indexes to create
   - `-` indexes to drop (if `--drop-extras`)
   - Warning for field drift (fields in DB not in schema)
4. Executes the plan (unless `--dry-run`)

**Example output:**

```
Collection: users
  + email_1 (unique)
  + role_1
  - old_field_1 (drop)
  ⚠ field "legacy_data" exists in DB but not in schema

Summary: 2 to create, 1 to drop, 1 warning
Executing migration...
Done: 2 created, 1 dropped, 0 errors
```

### goodm inspect

Display all registered model schemas.

```bash
goodm inspect
goodm inspect --diff --db myapp
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--diff` | `false` | Compare schemas against live database |
| `--uri` | `mongodb://localhost:27017` | MongoDB connection URI |
| `--db` | (required with `--diff`) | Database name |

**What it does:**

Shows each registered model with:
- Fields: bson name, Go type, attributes
- References to other collections
- Indexes (single and compound)
- Hooks

With `--diff`, also reports schema drift.

### goodm version

```bash
goodm version
# goodm v0.1.0
```

## Using with Registered Models

The `migrate` and `inspect` commands require models to be registered. Since Go only runs `init()` for imported packages, you need to import your model packages.

For the CLI to work with your models, you can create a custom entry point:

```go
// cmd/migrate/main.go
package main

import (
    _ "myapp/models" // triggers init() registration

    "github.com/dwoolworth/goodm/cmd/goodm/commands"
)

func main() {
    commands.Execute()
}
```

Or use `discover` to generate models from an existing database first, then use `migrate` to keep indexes in sync going forward.
