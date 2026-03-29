# Design Decisions

This document explains Mongoose features that goodm intentionally omits and why. Go's type system and idioms make many of these unnecessary or counterproductive.

## Omitted Features

### Virtuals

**What they are in Mongoose:** Computed properties defined on the schema that exist in memory but are never persisted to MongoDB. Common example: `fullName` derived from `firstName` + `lastName`.

**Why goodm skips them:** Go methods on structs already do this natively:

```go
func (u *User) FullName() string {
    return u.FirstName + " " + u.LastName
}
```

Methods are not struct fields, so they're automatically excluded from BSON writes. They're type-safe, discoverable via IDE tooling, and testable with no framework involvement. A goodm virtual system would add complexity to replicate what the language already provides.

For JSON serialization of computed fields, implement `json.Marshaler` on your struct.

### Aliases

**What they are in Mongoose:** Alternative names for fields, allowing `doc.name` to map to a field stored as `n` in the database.

**Why goodm skips them:** Go struct tags already solve this:

```go
Name string `bson:"n" json:"name"`
```

The `bson` tag controls the database field name, and the Go field name is the alias. The only gap is querying by Go field name instead of BSON name, but this is intentional — Go developers expect filters to use the BSON name since that's what MongoDB uses. Abstracting this away would create confusion about which name to use where.

### Soft Deletes

**What they are:** Instead of removing a document, set a `deletedAt` timestamp and filter it out of queries automatically.

**Why goodm skips them (for now):** Soft deletes require implicit query modification on every find operation, which conflicts with goodm's principle of being explicit and predictable. The transparent filtering can lead to surprising behavior when working directly with MongoDB tools or the raw driver.

If you need soft deletes, implement them with a `BeforeDelete` hook that sets a timestamp and cancels the delete, plus middleware that injects the filter on reads:

```go
func (u *User) BeforeDelete(ctx context.Context) error {
    u.DeletedAt = time.Now()
    goodm.Update(ctx, u)
    return fmt.Errorf("soft delete applied") // cancel the hard delete
}

goodm.Use(func(ctx context.Context, op *goodm.OpInfo, next func(context.Context) error) error {
    if op.Operation == goodm.OpFind {
        // Add deletedAt filter to queries
    }
    return next(ctx)
})
```

### Schema-Level Defaults Applied on Read

**What they are in Mongoose:** When a field is missing from a document in the database, Mongoose fills in the schema default when reading.

**What goodm does instead:** goodm applies `default=X` values at creation time — during `Create` and `CreateMany`, zero-valued fields are set to their schema default before hooks and validation run. This means the default is persisted to the database, so reads always return exactly what's stored. Read-time defaults (silently filling in missing values on query) are still omitted because they create a mismatch between what's in the database and what's in memory, making debugging harder.

### Discriminators (Single-Collection Inheritance)

**What they are in Mongoose:** Store multiple model types in one collection, differentiated by a `__t` type field. Queries automatically filter by type.

**Why goodm skips them:** Go doesn't have class inheritance. The idiomatic Go pattern is to use separate collections for separate types, or to use a shared struct with a type field and handle the branching explicitly. Discriminators add implicit query filtering that would be surprising in Go codebases.

### Query Casting

**What it is in Mongoose:** Automatically casts query values to match schema types (e.g., string `"5"` becomes number `5`).

**Why goodm skips it:** Go is statically typed. If you pass an `int` to a filter, it's already an `int`. Query casting exists in Mongoose because JavaScript is dynamically typed and values from HTTP requests arrive as strings. In Go, you parse input at the boundary (HTTP handler) and work with typed values from there.

## Mongoose Schema Options

Mongoose schemas accept many options. Here's how goodm handles each:

| Option | Status | Rationale |
|--------|--------|-----------|
| `strict` | N/A | Go structs are inherently strict — only declared fields are serialized. There's no "loose mode" to toggle. |
| `versionKey` | **Implemented** | goodm uses `__v` (same as Mongoose) for optimistic concurrency control. See [CRUD docs](crud.md) for details. |
| `autoIndex` | Omitted | goodm uses explicit `Enforce()` to create indexes on demand, giving you full control over when index creation happens (e.g., deploy scripts vs. app startup). |
| `toJSON` / `toObject` | Omitted | Use Go's `json.Marshaler` interface or custom methods on your struct. The language already provides this. |
| `minimize` | Omitted | Use `bson:",omitempty"` on struct tags to skip zero-valued fields. This is more granular than a schema-level flag. |
| `capped` | Omitted | Capped collections are a MongoDB admin concern. Create them using the MongoDB driver directly: `db.CreateCollection(ctx, name, options.CreateCollection().SetCapped(true).SetSizeInBytes(size))`. |
| `read` / `writeConcern` | **Implemented** | Per-schema read/write concern via the `Configurable` interface. Implement `CollectionOptions()` on your model to set read preference, read concern, and write concern. All CRUD, bulk, and pipeline operations automatically use the configured options. |
| `shardKey` | Omitted | Shard keys are a database administration concern configured at the MongoDB level, not the application level. |
| `validateBeforeSave` | Always on | goodm always validates before Create and Update. To bypass validation, use raw passthroughs (`UpdateOne`, `DeleteOne`, `UpdateMany`, `DeleteMany`). |
| `selectPopulatedPaths` | N/A | goodm's `Populate` is explicit — you specify exactly which fields to populate. There's no automatic path selection to configure. |

## Implemented: Subdocuments

Nested structs are parsed recursively during registration. All `goodm` tags on inner struct fields are enforced — validation, defaults, and schema introspection work at any nesting depth.

**Why inline sub-schemas instead of separate registration?** Subdocuments aren't models — they don't have collections, hooks, or CRUD operations. They're structural components of their parent document. Requiring separate registration would add ceremony for no benefit and would imply capabilities (like independent CRUD) that don't exist.

**Circular reference safety:** During parsing, goodm tracks which types are currently being parsed. If a type references itself (e.g., `type Node struct { Child *Node }`), the recursive reference gets empty SubFields rather than causing infinite recursion.

**Leaf types:** `time.Time`, `bson.ObjectID`, and `bson.Decimal128` are structs in Go but serialize as atomic BSON values. goodm treats them as leaf types and never recurses into their fields.

**What's deferred:** `ref=` tags inside subdocuments are parsed and stored but `Populate()` only operates on top-level fields. Dotted-path indexes (`unique`/`index` on inner fields) are parsed but not acted on by `Enforce()`. Both are future enhancements.

## Design Principles

These omissions follow a few core principles:

1. **Don't duplicate the language.** If Go already provides a mechanism (methods, struct tags, zero values, static types), don't add a framework-level abstraction on top.

2. **Be explicit over implicit.** Transparent query modification, automatic type casting, and silent data filling create hard-to-debug behavior. goodm prefers operations that do exactly what they say.

3. **Hooks and middleware are the escape hatch.** Features that require custom behavior per-model (soft deletes, computed fields on save, default injection) can be implemented via hooks and middleware without adding framework complexity.

4. **Keep the API surface small.** Every feature added is a feature that must be maintained, documented, and understood. A smaller API is easier to learn and harder to misuse.
