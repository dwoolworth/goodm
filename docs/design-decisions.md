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

**Why goodm skips them:** Go structs already have zero values, and the `default` tag in goodm is informational metadata used by code generation and the CLI inspect command. Silently mutating data on read creates a mismatch between what's in the database and what's in memory, making debugging harder. If you need defaults applied on creation, use a `BeforeCreate` hook.

### Discriminators (Single-Collection Inheritance)

**What they are in Mongoose:** Store multiple model types in one collection, differentiated by a `__t` type field. Queries automatically filter by type.

**Why goodm skips them:** Go doesn't have class inheritance. The idiomatic Go pattern is to use separate collections for separate types, or to use a shared struct with a type field and handle the branching explicitly. Discriminators add implicit query filtering that would be surprising in Go codebases.

### Query Casting

**What it is in Mongoose:** Automatically casts query values to match schema types (e.g., string `"5"` becomes number `5`).

**Why goodm skips it:** Go is statically typed. If you pass an `int` to a filter, it's already an `int`. Query casting exists in Mongoose because JavaScript is dynamically typed and values from HTTP requests arrive as strings. In Go, you parse input at the boundary (HTTP handler) and work with typed values from there.

## Design Principles

These omissions follow a few core principles:

1. **Don't duplicate the language.** If Go already provides a mechanism (methods, struct tags, zero values, static types), don't add a framework-level abstraction on top.

2. **Be explicit over implicit.** Transparent query modification, automatic type casting, and silent data filling create hard-to-debug behavior. goodm prefers operations that do exactly what they say.

3. **Hooks and middleware are the escape hatch.** Features that require custom behavior per-model (soft deletes, computed fields on save, default injection) can be implemented via hooks and middleware without adding framework complexity.

4. **Keep the API surface small.** Every feature added is a feature that must be maintained, documented, and understood. A smaller API is easier to learn and harder to misuse.
