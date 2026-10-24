package goodm

import (
	"context"
	"fmt"
	"reflect"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// MigrateOptions controls migration behavior.
type MigrateOptions struct {
	DryRun     bool
	DropExtras bool // drop indexes not in schema
}

// ActionType describes the kind of migration action.
type ActionType int

const (
	ActionCreateIndex  ActionType = iota
	ActionDropIndex               // index exists in the database but not in any schema
	ActionFieldDrift              // field in DB not in schema
	ActionRebuildIndex            // index exists under the schema's name but its options differ
)

// MigrationAction describes a single change to apply.
type MigrationAction struct {
	Type        ActionType
	Collection  string
	Description string
	IndexName   string

	// Definition is the index the schema expects, for create and rebuild actions.
	Definition *IndexDefinition
}

// MigrationPlan holds all planned actions.
type MigrationPlan struct {
	Actions []MigrationAction
}

// MigrationResult reports what happened during execution.
type MigrationResult struct {
	Executed int
	Skipped  int
	Warnings []string
	Errors   []error
}

// PlanMigration compares schemas against the live database and builds a
// migration plan. Indexes are compared by name and by options, so an index
// that exists but no longer matches its schema (different keys, uniqueness,
// sparseness, TTL, partial filter, or collation) is planned as a rebuild.
func PlanMigration(ctx context.Context, db *mongo.Database, schemas []*Schema) (MigrationPlan, error) {
	var plan MigrationPlan

	// Several models may share a collection; their indexes are planned as one
	// set so neither marks the other's indexes as extras.
	var order []string
	groups := map[string][]*Schema{}
	for _, schema := range schemas {
		if _, seen := groups[schema.Collection]; !seen {
			order = append(order, schema.Collection)
		}
		groups[schema.Collection] = append(groups[schema.Collection], schema)
	}

	for _, collection := range order {
		group := groups[collection]
		coll := db.Collection(collection)

		existing, err := ListExistingIndexes(ctx, coll)
		if err != nil {
			return plan, fmt.Errorf("migration: failed to list indexes on %s: %w", collection, err)
		}
		delete(existing, "_id_")

		expected := make(map[string]bool)
		declared := map[string]indexOrigin{}
		for _, schema := range group {
			for _, def := range schema.IndexDefinitions() {
				def := def
				if prev, seen := declared[def.Name]; seen {
					if !indexDefinitionsEqual(prev.def, def) {
						return plan, fmt.Errorf("migration: index %s on %s is defined differently by %s and %s", def.Name, collection, prev.schema.QualifiedName(), schema.QualifiedName())
					}
					continue
				}
				declared[def.Name] = indexOrigin{schema: schema, def: def}
				expected[def.Name] = true
				spec, ok := existing[def.Name]
				if !ok {
					plan.Actions = append(plan.Actions, MigrationAction{
						Type:        ActionCreateIndex,
						Collection:  collection,
						Description: fmt.Sprintf("Create index: %s%s", def.Name, describeIndexOptions(def)),
						IndexName:   def.Name,
						Definition:  &def,
					})
					continue
				}
				if mismatch := describeIndexMismatch(spec, def); mismatch != "" {
					plan.Actions = append(plan.Actions, MigrationAction{
						Type:        ActionRebuildIndex,
						Collection:  collection,
						Description: fmt.Sprintf("Rebuild index: %s (%s)", def.Name, mismatch),
						IndexName:   def.Name,
						Definition:  &def,
					})
				}
			}
		}

		for name := range existing {
			if !expected[name] {
				plan.Actions = append(plan.Actions, MigrationAction{
					Type:        ActionDropIndex,
					Collection:  collection,
					Description: fmt.Sprintf("Drop index: %s (not in schema)", name),
					IndexName:   name,
				})
			}
		}

		// Drift is judged against every field any model of the collection declares.
		known := map[string]bool{}
		for _, schema := range group {
			for f := range knownFields(schema) {
				known[f] = true
			}
		}
		for _, d := range detectDriftIn(ctx, db, collection, known, DefaultDriftSampleSize) {
			plan.Actions = append(plan.Actions, MigrationAction{
				Type:        ActionFieldDrift,
				Collection:  collection,
				Description: fmt.Sprintf("Extra field: %s", d.Field),
			})
		}
	}

	return plan, nil
}

// indexOrigin records which schema first declared an index name.
type indexOrigin struct {
	schema *Schema
	def    IndexDefinition
}

// indexDefinitionsEqual reports whether two definitions describe the same index.
func indexDefinitionsEqual(a, b IndexDefinition) bool {
	if !indexKeysEqual(a.Keys, b.Keys) || a.Unique != b.Unique || a.Sparse != b.Sparse || !ttlEqual(a.ExpireAfterSeconds, b.ExpireAfterSeconds) {
		return false
	}
	pa, errA := toBsonMapOrNil(a.PartialFilter)
	pb, errB := toBsonMapOrNil(b.PartialFilter)
	if errA != nil || errB != nil || !reflect.DeepEqual(pa, pb) {
		return false
	}
	return reflect.DeepEqual(a.Collation, b.Collation)
}

func toBsonMapOrNil(v interface{}) (bson.M, error) {
	if v == nil {
		return nil, nil
	}
	return toBsonMap(v)
}

// describeIndexOptions renders the notable options of a definition for plan output.
func describeIndexOptions(def IndexDefinition) string {
	var parts []string
	if def.Unique {
		parts = append(parts, "unique")
	}
	if def.Sparse {
		parts = append(parts, "sparse")
	}
	if def.ExpireAfterSeconds != nil {
		parts = append(parts, fmt.Sprintf("ttl %ds", *def.ExpireAfterSeconds))
	}
	if def.PartialFilter != nil {
		parts = append(parts, "partial")
	}
	if def.Collation != nil {
		parts = append(parts, "collation "+def.Collation.Locale)
	}
	if len(parts) == 0 {
		return ""
	}
	out := " ("
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out + ")"
}

// ExecuteMigration applies the planned actions to the database. Rebuilds use
// the same path as Enforce with IndexMismatchRebuild, so a non-unique index
// that must become unique is converted in place where the server supports it.
func ExecuteMigration(ctx context.Context, db *mongo.Database, plan MigrationPlan, opts MigrateOptions) (MigrationResult, error) {
	var result MigrationResult
	probe := &prepareUniqueProbe{db: db}

	for _, action := range plan.Actions {
		coll := db.Collection(action.Collection)

		switch action.Type {
		case ActionCreateIndex:
			if action.Definition == nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s: no index definition", action.Description))
				continue
			}
			if err := createIndex(ctx, coll, action.Collection, *action.Definition); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s: %w", action.Description, err))
			} else {
				result.Executed++
			}

		case ActionRebuildIndex:
			if action.Definition == nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s: no index definition", action.Description))
				continue
			}
			existing, err := ListExistingIndexes(ctx, coll)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s: %w", action.Description, err))
				continue
			}
			if err := enforceIndex(ctx, coll, action.Collection, existing, *action.Definition, IndexMismatchRebuild, probe); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s: %w", action.Description, err))
			} else {
				result.Executed++
			}

		case ActionDropIndex:
			if !opts.DropExtras {
				result.Skipped++
				result.Warnings = append(result.Warnings, fmt.Sprintf("Skipped drop: %s on %s (use --drop-extras to drop)", action.IndexName, action.Collection))
				continue
			}
			if err := coll.Indexes().DropOne(ctx, action.IndexName); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s: %w", action.Description, err))
			} else {
				result.Executed++
			}

		case ActionFieldDrift:
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: %s", action.Collection, action.Description))
		}
	}

	return result, nil
}

// Migrate is a convenience function that plans and executes a migration for
// all registered schemas.
func Migrate(ctx context.Context, db *mongo.Database, opts MigrateOptions) (MigrationResult, error) {
	plan, err := PlanMigration(ctx, db, GetAll())
	if err != nil {
		return MigrationResult{}, err
	}

	if opts.DryRun {
		return MigrationResult{
			Skipped:  len(plan.Actions),
			Warnings: []string{"Dry run — no changes applied"},
		}, nil
	}

	return ExecuteMigration(ctx, db, plan, opts)
}
