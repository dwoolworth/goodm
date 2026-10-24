# Changelog

All notable changes to this project will be documented in this file.

## [0.7.0] - 2026-10-24

This release closes the main MongoDB feature gaps found in a review of 0.6.1 and fixes several correctness bugs. It contains breaking changes; see the end of this entry.

### Added
- **Index types and options.** Field tags: `index=-1`, `index=hashed`, `index=text`, `index=2dsphere`, `sparse`, and `ttl=<duration|seconds>`. `CompoundIndex` gained `Sparse`, `Name`, `ExpireAfterSeconds` (with the `TTL(time.Duration)` helper), `PartialFilter`, and `Collation`, and its `Fields` entries accept `-field`, `field:text`, `field:hashed`, and `field:2dsphere` markers. Index tags inside subdocuments now create dotted-path indexes. `Enforce` detects mismatches in sparseness, TTL, partial filter, and collation as well as keys and uniqueness.
- `Schema.IndexDefinitions()` and the `IndexDefinition` type: the single resolved list of indexes a schema expects. `Enforce`, `Migrate`, and `goodm inspect` all work from it.
- `Migrate` now plans an `ActionRebuildIndex` for indexes whose options differ from the schema and executes it through the same in-place unique conversion as `Enforce`. `MigrationAction.Definition` carries the expected index.
- `ErrDuplicateKey` and `*DuplicateKeyError` (with `Collection`, `Index`, and `Fields`) for unique index violations from `Create`, `CreateMany`, `Update`, `UpdateFields`, `UpdateOne`, `UpdateMany`, `FindOneAndUpdate`, `SaveMany`, and `Bulk`.
- Query layer: `FindByID` (accepts hex strings for ObjectID models), `Count`, `Exists`, `Distinct`, `FindOneAndUpdate`, `FindOneAndReplace`, and `FindOneAndDelete`. `FindOptions` gained `Projection`, `Hint`, `Collation`, and `BatchSize`. `UpdateOptions` gained `Upsert` (with the `Upsert()` shorthand), `ArrayFilters`, `Hint`, and `Collation` for the raw operations; `DeleteOptions` gained `Hint` and `Collation`.
- `NewBulk`: a builder for heterogeneous `BulkWrite` batches (`InsertOne`, `UpdateOne`, `UpdateMany`, `ReplaceOne`, `DeleteOne`, `DeleteMany`, `Unordered`) with per-operation failure reporting.
- `SaveMany`: the `Update` lifecycle for a slice of models, with per-document version filters and per-index classification of conflicts (`ErrVersionConflict`), missing documents (`ErrNotFound`), duplicate keys, and write concern or transport failures. Each document is replaced with its own conditional request, issued concurrently outside transactions, because MongoDB's bulk results cannot say which replacement matched. Racing writers always see exactly one success. Transport errors are reported as "outcome unknown" (the server may have applied them) and items not sent because the context was already done as `ErrNotAttempted`, which wraps the context error; inside a transaction the first failure of any kind aborts the transaction and every item is reported as not saved; an `AfterSave` error is reported for its item after the other hooks run and does not undo the save. A batch containing the same `_id` twice is rejected. Middleware may scope the batch through `OpInfo.Filter`, which starts as an empty document.
- `*BulkError` with per-item `BulkWriteError` entries, a `Result`, and a `WriteConcernError`; returned by `CreateMany`, `SaveMany`, and `Bulk.Execute`. `errors.Is`/`errors.As` see every item's error. When the write concern was not met, `WriteConcernError` is set and the affected writes are treated as unknown: `CreateMany` and `Bulk` run no after-hooks and their `Result` is unreliable; `SaveMany` lists each affected item, rolls its version back, and skips its `AfterSave`.
- `CreateOptions.Unordered` for `CreateMany`; `AfterCreate` now runs only for models that were actually inserted (inside a transaction a write failure aborts the transaction, so none are, and `CreateMany` reports every item as not inserted), and an `AfterCreate` error is reported in the `*BulkError` for its item instead of aborting the remaining hooks.
- `BulkResult.UpsertedCount` and `UpsertedIDs`.
- Pipeline: `PipelineOptions.AllowDiskUse`, `Collation`, `Hint`, `BatchSize`; stages `UnwindPreserve`, `LookupPipeline`, `Set`, `Unset`, `ReplaceRoot`, `Facet`, `Sample`. `Execute` and `Cursor` run middleware under the new `OpAggregate`.
- Middleware may now modify `OpInfo.Filter` and the new `OpInfo.Pipeline`; the operation reads them after the chain runs. New op types `OpSaveMany`, `OpAggregate`, `OpBulkWrite`.
- Registry: `SchemaOf(model)` resolves by type, `Unregister(model)`, `Schema.Type`, `Schema.PkgPath`, `Schema.QualifiedName()`.
- Custom `_id` types: the ID field is found by its `bson:"_id"` tag (falling back to a field named `ID`) and may be any type. ObjectIDs are still generated automatically; other types must be set before `Create` or in `BeforeCreate`, and a zero value is rejected.
- `TransactionOptions.Transaction` for driver-level transaction options.
- `goodm discover` records index key directions and kinds, so generated `Indexes()` methods preserve descending and special keys.

### Fixed
- `min`/`max` were skipped for zero values, so `min=1` accepted `0`, and floats were truncated to integers before comparison, so `max=10` accepted `10.9` and `min=0` accepted `-0.5`. Bounds now apply to every present value and compare numerically; only a nil pointer is treated as absent.
- Middleware could not change the filter: operations closed over their original argument. The soft-delete and tenant-scoping patterns in the docs now work.
- Schemas were keyed by bare struct name, so a second `User` type in another package failed to register and an unregistered type with a registered name silently used the wrong collection. The registry is keyed by `reflect.Type`.
- `UpdateFields` skipped validation of the values it set, allowed immutable fields to change, rejected dotted paths into subdocuments, and mutated the caller's map.
- `Migrate` reconstructed index keys by parsing index names and compared by name only; it now shares index definitions and mismatch detection with `Enforce`.
- `Populate` and `BatchPopulate` ignored the referenced model's collection options (read preference and concerns).
- A nested `WithTransaction` started a second session; it now joins the transaction already in the context.
- The hooks documentation listed the immutable check after `BeforeSave`; it runs before.
- The `FindOne` family compares driver errors with `errors.Is` instead of `==`.

### Changed
- **Breaking:** `GetAll()` returns `[]*Schema` sorted by collection and qualified name instead of `map[string]*Schema`; `PlanMigration` takes `[]*Schema`.
- **Breaking:** `Get(name)` returns `false` when more than one registered type has that name. Use `SchemaOf`.
- **Breaking:** `FieldSchema.Min` and `Max` are `*float64`.
- **Breaking:** `CreateMany` returns a `*BulkError` for write failures instead of a wrapped driver error, and unique violations everywhere are `*DuplicateKeyError` (the driver error remains reachable through `errors.As`).
- **Breaking:** `Pipeline.Execute` and `Pipeline.Cursor` now run middleware.
- **Breaking:** validation of `min`/`max` no longer skips zero values (see Fixed). Optional bounded fields should be pointers.
- **Breaking:** `UpdateFields` now returns `ValidationErrors` for values that violate the schema or target immutable fields.
- `IndexSpec` gained `ExpireAfterSeconds`, `PartialFilter`, and `Collation`; text indexes report their keys as `field: "text"`.
- Minimum Go version is 1.21.

## [0.6.1] - 2026-09-25

### Fixed
- `IndexMismatchRebuild` no longer drops a non-unique index before recreating it as unique. On MongoDB 6.0+ (binary and featureCompatibilityVersion) the index is converted in place with `collMod` (`prepareUnique: true`, then `unique: true`), so there is no window where the collection is unindexed and a concurrent writer can insert a duplicate that would break the recreate and every subsequent `Enforce`. The read-only duplicate pre-check still runs first, so known duplicates never trigger a `collMod`. If a duplicate slips in between the pre-check and the conversion, `prepareUnique` is reverted and the existing index is left untouched. On any other failure of the final step the index is left in the prepared state (present, new duplicates rejected) and the next `Enforce` completes the conversion. Drop + recreate remains the fallback for servers that don't support `prepareUnique` and for mismatches `collMod` cannot fix (keys, sparse, partial, unique -> non-unique) (MSA-78).

### Added
- `IndexSpec.PrepareUnique` reports an index left in the `collMod prepareUnique` state; `Enforce` mismatch errors name that state explicitly, and `IndexMismatchRebuild` clears a stale one on a non-unique field in place.
- `IndexSpec.HasCollation`, `IndexSpec.HasTTL`, `IndexSpec.Hidden`. Indexes with these options are never converted in place, so a rebuild still normalizes them to a plain unique index.
- The duplicate pre-check now mirrors unique-index semantics for array (multikey) fields and no longer conflates dotted keys such as `a.b` with `a_b`.

## [0.6.0] - 2026-09-24

### Fixed
- `Enforce` now verifies the options of existing indexes instead of accepting any index by name alone. A `goodm:"unique"` field whose existing index is non-unique, sparse, or partial (and the reverse: a plain `index` field with a unique index) is reported as an `EnforcementError` by default. The same check applies to compound indexes. Previously such mismatches were silently skipped, so `unique` tags guaranteed nothing once any index of that name existed (MSA-76 / BUG-3136).

### Added
- `EnforceOptions.IndexMismatchPolicy`: `IndexMismatchError` (default, fail loudly), `IndexMismatchRebuild` (drop and recreate mismatched indexes; refuses to drop when rebuilding a unique index over duplicate data), and `IndexMismatchIgnore` (legacy name-only behavior).
- `IndexSpec` type describing an existing index (keys, unique, sparse, partial).

### Changed
- **Breaking:** `ListExistingIndexes` now returns `map[string]IndexSpec` instead of `map[string]bool`.

## [0.5.0] - 2026-04-21

### Added
- `WithRetry(n)` option for `Update()` with 3-way field-level merge on version conflict. On conflict, re-reads the document, diffs caller's changes against the other writer's changes, and auto-merges if the changed fields are disjoint. Returns `*MergeConflictError` (with field names) when both sides modified the same field. (#5)
- `*MergeConflictError` error type listing the conflicting bson field names.
- Auto-refresh of in-memory version on version conflict (even without retry) to prevent cascading write failures.

## [0.4.0] - 2026-04-20

### Added
- `UpdateFields()` for partial `$set` updates without optimistic locking — ideal for concurrent writers touching disjoint fields (e.g. progress tracking, heartbeats). Runs middleware, sets `updated_at`, increments version, and reflects changes back onto the struct. (#4)
- `UnsetFields()` option for `Update()` to atomically remove fields from MongoDB documents via `ReplaceOne`. Validates against schema to prevent unsetting required, managed, or unknown fields. (#3)

### Changed
- Reduced cyclomatic complexity across 9 functions (all now ≤15) for Go Report Card gocyclo score improvement.
- Fixed `gofmt -s` formatting in `inspect.go`, `generate.go`, and `middleware.go`.

## [0.3.0] - 2026-03-29

### Added
- Documentation site with mkdocs-material and GitHub Pages deployment.
- CI workflow with multi-version Go and MongoDB matrix testing.
- Example tests and README improvements.

## [0.2.0] - 2026-03-28

### Added
- Subdocument support for nested struct validation, defaults, and schema introspection.
- Schema defaults on Create and versionKey optimistic concurrency.

## [0.1.1] - 2026-03-27

### Added
- Makefile, duplicate registration guard, ldflags version, and BatchPopulate.
- Array ref support in Populate and BatchPopulate.

## [0.1.0] - 2026-03-26

### Added
- Initial release: schema-driven ODM for MongoDB in Go.
- CRUD operations with lifecycle hooks, validation, middleware, and population.
- CLI with discover, migrate, and inspect commands.
