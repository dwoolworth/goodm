# Changelog

All notable changes to this project will be documented in this file.

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
