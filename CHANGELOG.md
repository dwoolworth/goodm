# Changelog

All notable changes to this project will be documented in this file.

## [0.7.1] - 2026-10-24

### Fixed
- **`Update` with `WithRetry` lost the caller's changes.** The merge base was taken from the model at the moment `Update` was called, which already held the caller's edits, so the 3-way diff found nothing to apply: on a version conflict the retry wrote the database copy back and returned nil, and a genuine same-field conflict never produced a `MergeConflictError`. `goodm.Model` now remembers the document as last loaded or saved through goodm (`FindOne`, `FindByID`, `Find`, `FindOneAndUpdate`, `FindOneAndReplace`, `Create`, `Update`, `UpdateFields`, `SaveMany`) and the merge diffs against that. A model with no such base (built by hand, loaded through `FindCursor`, or without an embedded `goodm.Model`) gets `ErrVersionConflict` on conflict instead of a merge. The base is normalized through the model type, so stored documents that differ from the struct in missing or extra fields, integer width, or map key order do not look like changes, and it advances after each merge round so later rounds of `WithRetry(n)` work. The merged document is applied over zeroed BSON fields, so map keys and `omitempty` values another writer removed are not resurrected from the model while fields tagged `-`, unexported fields, and pointer identities are kept; `UpdateFields` advances the base by the fields it wrote rather than by the whole model, so unsaved in-memory edits still count as changes; fields named in `UnsetFields` count as the caller's removals, so a concurrent change to one of them is a conflict instead of being deleted; and a NaN field no longer causes a false conflict on every merge. `Find` decodes documents one at a time to record them, and each loaded model keeps a copy of its stored form.
- `enum` on a slice or array field compared the whole slice's string form against the allowed values, so every non-empty value was rejected; each element is now checked (nil pointer elements are skipped) and an offending one is reported as `field[i]`. `UpdateFields` applies `enum` to an element path such as `tags.1` as well.
- `GenerateModel` with `EmbedModel: false` emitted code that did not import goodm although its `init()` calls `goodm.Register`.
- `Update` and `SaveMany` left a failed model's `UpdatedAt` at the attempted time while restoring its version; `UpdatedAt` is restored too.
- `goodm discover` typed date fields as `interface{}` because BSON dates decode as `bson.DateTime`; they are now `time.Time`.
- A snapshot session passed to `WithTransaction` is documented to return an error.

### Changed
- The two internal filter-scoping helpers added by #8 and #9 are merged into one.

## [0.7.0] - 2026-10-24

This release closes the main MongoDB feature gaps found in a review of 0.6.1 and fixes several correctness bugs. It contains breaking changes; see the end of this entry.

Tested against MongoDB 6.0.28, 7.0.45, and 8.0.34 running as single-node replica sets, with Go 1.21, 1.22, and 1.23 in CI and Go 1.26 locally; the suite runs with the race detector on every combination.

### Added
- **Index types and options.** Field tags: `index=-1`, `index=hashed`, `index=text`, `index=2dsphere`, `sparse`, and `ttl=<duration|seconds>`. `CompoundIndex` gained `Sparse`, `Name`, `ExpireAfterSeconds` (with the `TTL(time.Duration)` helper), `PartialFilter`, and `Collation`, and its `Fields` entries accept `-field`, `field:text`, `field:hashed`, and `field:2dsphere` markers. Index tags inside subdocuments now create dotted-path indexes (see Changed). `Enforce` detects mismatches in sparseness, TTL, partial filter, and collation as well as keys and uniqueness; the fields of a compound text index are compared as a set.
- `Schema.IndexDefinitions()` and the `IndexDefinition` type: the single resolved list of indexes a schema expects. `Enforce`, `Migrate`, and `goodm inspect` all work from it.
- `Migrate` now plans an `ActionRebuildIndex` for indexes whose options differ from the schema and executes it through the same in-place unique conversion as `Enforce`. `MigrationAction.Definition` carries the expected index.
- `ErrDuplicateKey` and `*DuplicateKeyError` (with `Collection`, `Index`, and `Fields`) for unique index violations from `Create`, `CreateMany`, `Update`, `UpdateFields`, `UpdateOne`, `UpdateMany`, `FindOneAndUpdate`, `SaveMany`, and `Bulk`.
- Query layer: `FindByID` (accepts hex strings for ObjectID models), `Count`, `Exists`, `Distinct`, `FindOneAndUpdate`, `FindOneAndReplace`, and `FindOneAndDelete`. `FindOneAndUpdate` adds `updated_at`, `__v`, and on upsert `created_at` to `bson.D`/`bson.M` updates; aggregation-pipeline updates are passed through unchanged. `FindOptions` gained `Projection`, `Hint`, `Collation`, and `BatchSize`. `UpdateOptions` gained `Upsert` (with the `Upsert()` shorthand), `ArrayFilters`, `Hint`, and `Collation` for the raw operations; `DeleteOptions` gained `Hint` and `Collation`.
- `NewBulk`: a builder for heterogeneous `BulkWrite` batches (`InsertOne`, `UpdateOne`, `UpdateMany`, `ReplaceOne`, `DeleteOne`, `DeleteMany`, `Unordered`) with per-operation failure reporting.
- `SaveMany`: the `Update` lifecycle for a slice of models, with per-document version filters and per-index classification of conflicts (`ErrVersionConflict`), missing documents (`ErrNotFound`), duplicate keys, and write concern or transport failures. Each document is replaced with its own conditional request, issued concurrently outside transactions, because MongoDB's bulk results cannot say which replacement matched. Racing writers always see exactly one success. Transport errors are reported as "outcome unknown" (the server may have applied them) and items not sent because the context was already done as `ErrNotAttempted`, which wraps the context error; inside a transaction a server error on any item has aborted the transaction, so every item is reported as not saved, while conflicts, missing documents, and client-side failures leave it running; an `AfterSave` error is reported for its item after the other hooks run and does not undo the save. A batch containing the same `_id` twice is rejected. Middleware may scope the batch through `OpInfo.Filter`, which starts as an empty document.
- `*BulkError` with per-item `BulkWriteError` entries, a `Result`, and a `WriteConcernError`; returned by `CreateMany`, `SaveMany`, and `Bulk.Execute`. `errors.Is`/`errors.As` see every item's error. When the write concern was not met, `WriteConcernError` is set and the affected writes are treated as unknown: `CreateMany` and `Bulk` run no after-hooks and their `Result` is unreliable; `SaveMany` lists each affected item, rolls its version back, and skips its `AfterSave`.
- `CreateOptions.Unordered` for `CreateMany`; `AfterCreate` now runs only for models that were actually inserted (inside a transaction a server rejection has aborted the transaction, so none are, and `CreateMany` reports every item as not inserted), and an `AfterCreate` error is reported in the `*BulkError` for its item instead of aborting the remaining hooks.
- `BulkResult.UpsertedCount` and `UpsertedIDs`.
- Pipeline: `PipelineOptions.AllowDiskUse`, `Collation`, `Hint`, `BatchSize`; stages `UnwindPreserve`, `LookupPipeline`, `Set`, `Unset`, `ReplaceRoot`, `Facet`, `Sample`. `Execute` and `Cursor` run middleware under the new `OpAggregate`.
- Middleware may now modify `OpInfo.Filter` and the new `OpInfo.Pipeline`; the operation reads them after the chain runs. Every find, `UpdateOne`, `UpdateMany`, `DeleteOne`, `DeleteMany`, the `FindOneAnd*` family, and aggregation use the rewritten filter directly; `Update`, `UpdateFields`, `Delete`, and `SaveMany` AND it with their `_id` and version filters, reporting out-of-scope documents as `ErrNotFound`. `Bulk` operations carry their own filters and `Populate` runs no middleware, so neither is scoped. New op types `OpSaveMany`, `OpAggregate`, `OpBulkWrite`.
- Registry: `SchemaOf(model)` resolves by type, `Unregister(model)`, `Schema.Type`, `Schema.PkgPath`, `Schema.QualifiedName()`.
- Custom `_id` types: the ID field is the one tagged `bson:"_id"`, directly or through embedded structs tagged `bson:",inline"`, and may be any type (see Changed for the `Register` checks). ObjectIDs are still generated automatically; other types must be set before `Create` or in `BeforeCreate`, and a zero value is rejected. `Register` rejects a model with no `_id` field.
- `TransactionOptions.Transaction` for driver-level transaction options.
- `goodm discover` reads index key directions and kinds (including the fields of text indexes), sparse, and TTL into field tags, emits struct-literal `Indexes()` entries for indexes with options or custom names and for single-field indexes on paths that are not generated fields, and leaves a TODO comment for partial filters and collations it cannot read back.
- `FieldSchema.GoType()`; `DiscoveredField.IndexKind`, `Sparse`, and `TTL`; `DiscoveredIndex.Sparse`, `ExpireAfterSeconds`, `HasPartialFilter`, `HasCollation`, `Tagged`, `SingleFieldTag()`, and `DerivedName()`.
- `PlanMigration` plans the indexes of models that share a collection as one set, judges drift against the union of their fields, and fails if two models define the same index name differently (partial filters and collations are compared by meaning, not spelling).
- Partial filters are compared canonically everywhere, so a nested `bson.M` in a `PartialFilter` no longer makes `Enforce` report a mismatch at random.
- `goodm discover` expresses at most one index per field as tags; a second index on the same field (such as a hashed index beside a unique one), and any index on a field the embedded `goodm.Model` provides (such as a TTL on `created_at`), becomes an `Indexes()` entry instead of being merged into an invalid tag or lost.
- Partial filters are normalized to a canonical ordered form when the index definition is built, so a filter written with `bson.M` is created and compared deterministically; comparison keeps integers exact, treats the documents inside `$and`/`$or`/`$nor` as filters, and keeps key order inside exact-match subdocuments.
- `goodm discover` with the embedded model no longer emits a `__v` field that would shadow `Model.Version`.

### Fixed
- `min`/`max` were skipped for zero values, so `min=1` accepted `0`, and floats were truncated to integers before comparison, so `max=10` accepted `10.9` and `min=0` accepted `-0.5`. Bounds now apply to every present value and compare numerically; only a nil pointer is treated as absent.
- Middleware could not change the filter: operations closed over their original argument. The soft-delete and tenant-scoping patterns in the docs now work.
- Schemas were keyed by bare struct name, so a second `User` type in another package failed to register and an unregistered type with a registered name silently used the wrong collection. The registry is keyed by `reflect.Type`.
- `UpdateFields` skipped validation of the values it set, allowed immutable fields to change, rejected dotted paths into subdocuments, and mutated the caller's map. Each value is now converted to the field's Go type before validation, allowing only conversions that keep its meaning: numbers to numbers of another width when representable, documents (`bson.M`, `bson.D`, maps, structs of another type) to structs and maps by bson name, arrays to slices and arrays element by element, hex strings to ObjectIDs, and anything into an `interface{}` field; everything else (a bool for an int, a string for a date, a document for an array, an integer a float field cannot hold exactly, NaN) is a `ValidationError`; a `float64` into a `float32` field is rounded. `bson.DateTime` converts to `time.Time`, generic `bson.Binary` to `[]byte`, and any document to a `bson.D` field; struct sources are read field by field so their `time.Time` and `[]byte` values keep their types. The converted value is both what is stored and what is copied onto the model, so a later `Update` cannot undo it; `nil` is stored as null. A path the model cannot reach, such as an index past the end of a slice, is rejected. Keys a subdocument map carries that the schema does not declare are dropped, as `Update` drops them.
- `Migrate` reconstructed index keys by parsing index names and compared by name only; it now shares index definitions and mismatch detection with `Enforce`.
- `Populate` and `BatchPopulate` ignored the referenced model's collection options (read preference and concerns).
- A nested `WithTransaction` started a second session; it now joins the transaction already running in the context's session, detected before the callback runs so an error the callback returns can never be mistaken for nesting. A context carrying a session without an active transaction gets a transaction started on that session.
- The hooks documentation listed the immutable check after `BeforeSave`; it runs before.
- The `FindOne` family compares driver errors with `errors.Is` instead of `==`.

### Changed
- **Breaking:** `GetAll()` returns `[]*Schema` sorted by collection and qualified name instead of `map[string]*Schema`; `PlanMigration` takes `[]*Schema`.
- **Breaking:** `Get(name)` returns `false` when more than one registered type has that name. Use `SchemaOf`.
- **Breaking:** `FieldSchema.Min` and `Max` are `*float64`.
- **Breaking:** `CreateMany` returns a `*BulkError` for write failures instead of a wrapped driver error, and unique violations everywhere are `*DuplicateKeyError` (the driver error remains reachable through `errors.As`).
- **Breaking:** `Pipeline.Execute` and `Pipeline.Cursor` now run middleware.
- **Breaking:** validation of `min`/`max` no longer skips zero values (see Fixed), and the bounds now apply to the length of slices and maps as well as strings, so a nil slice tagged `min=1` fails. Optional bounded fields should be pointers.
- **Breaking:** schema parsing follows the driver's default registry: any exported struct or pointer-to-struct field tagged `bson:",inline"`, embedded or named, is flattened; any other exported embedded struct is a subdocument field named by its tag or lowercased type name; unexported fields and embeds are ignored; an inline map declares no fields. The managed fields are located by their bson names (`created_at`, `updated_at`, `__v`) rather than by Go field name, so a promoted field with the same Go name elsewhere no longer disables timestamps or versioning; a `CreatedAt` field stored under another bson name is no longer managed. `Register` also rejects duplicate bson names inside subdocument types. Validation paths, index names, and `UpdateFields` paths of a non-inline embed now live under its name. A model that embeds `goodm.Model` without the inline tag, that has no field tagged `bson:"_id"` among the fields the driver stores, or that stores two fields under the same bson name is rejected by `Register`.
- **Breaking:** `UpdateFields` drops keys of a subdocument value that the schema does not declare, as `Update` does; previously they were written.
- **Breaking:** `unique`, `index`, `sparse`, and `ttl` tags on fields inside subdocuments were silently ignored before and now create real (dotted-path, multikey) indexes. A `unique` tag inside an array of subdocuments indexes every element, and documents whose array is empty or missing all index as `null` and collide with each other; use `unique,sparse` or a partial filter for such fields, and expect `Enforce` to fail if existing data already violates the new index.
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
