package goodm

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MigrateOptions controls migration behavior.
type MigrateOptions struct {
	DryRun     bool
	DropExtras bool // drop indexes not in schema
}

// ActionType describes the kind of migration action.
type ActionType int

const (
	ActionCreateIndex ActionType = iota
	ActionDropIndex
	ActionFieldDrift // field in DB not in schema
)

// MigrationAction describes a single change to apply.
type MigrationAction struct {
	Type        ActionType
	Collection  string
	Description string
	IndexName   string
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

// PlanMigration compares registered schemas against the live database and builds a migration plan.
func PlanMigration(ctx context.Context, db *mongo.Database, schemas map[string]*Schema) (MigrationPlan, error) {
	var plan MigrationPlan

	for _, schema := range schemas {
		coll := db.Collection(schema.Collection)

		// Build expected index set
		expected := buildExpectedIndexes(schema)

		// Read actual indexes
		existing, err := ListExistingIndexes(ctx, coll)
		if err != nil {
			return plan, fmt.Errorf("migration: failed to list indexes on %s: %w", schema.Collection, err)
		}

		// Filter out _id_ system index
		delete(existing, "_id_")
		delete(expected, "_id_")

		// expected - actual = indexes to create
		for name := range expected {
			if !existing[name] {
				plan.Actions = append(plan.Actions, MigrationAction{
					Type:        ActionCreateIndex,
					Collection:  schema.Collection,
					Description: fmt.Sprintf("Create index: %s", name),
					IndexName:   name,
				})
			}
		}

		// actual - expected = indexes to drop
		for name := range existing {
			if !expected[name] {
				plan.Actions = append(plan.Actions, MigrationAction{
					Type:        ActionDropIndex,
					Collection:  schema.Collection,
					Description: fmt.Sprintf("Drop index: %s (not in schema)", name),
					IndexName:   name,
				})
			}
		}

		// Detect field drift
		drifts := DetectDrift(ctx, db, schema, DefaultDriftSampleSize)
		for _, d := range drifts {
			plan.Actions = append(plan.Actions, MigrationAction{
				Type:        ActionFieldDrift,
				Collection:  schema.Collection,
				Description: fmt.Sprintf("Extra field: %s", d.Field),
			})
		}
	}

	return plan, nil
}

// ExecuteMigration applies the planned actions to the database.
func ExecuteMigration(ctx context.Context, db *mongo.Database, plan MigrationPlan, opts MigrateOptions) (MigrationResult, error) {
	var result MigrationResult

	for _, action := range plan.Actions {
		coll := db.Collection(action.Collection)

		switch action.Type {
		case ActionCreateIndex:
			model := buildIndexModel(action.IndexName)
			if _, err := coll.Indexes().CreateOne(ctx, model); err != nil {
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

// Migrate is a convenience function that plans and executes a migration.
func Migrate(ctx context.Context, db *mongo.Database, opts MigrateOptions) (MigrationResult, error) {
	schemas := GetAll()

	plan, err := PlanMigration(ctx, db, schemas)
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

// buildExpectedIndexes constructs the set of index names a schema expects to exist.
func buildExpectedIndexes(schema *Schema) map[string]bool {
	expected := make(map[string]bool)

	// Single-field indexes from tags
	for _, field := range schema.Fields {
		if field.Unique || field.Index {
			expected[field.BSONName+"_1"] = true
		}
	}

	// Compound indexes
	for _, ci := range schema.CompoundIndexes {
		name := compoundIndexName(ci)
		expected[name] = true
	}

	return expected
}

// buildIndexModel reconstructs a mongo.IndexModel from an index name like "field_1" or "a_1_b_1".
func buildIndexModel(indexName string) mongo.IndexModel {
	parts := strings.Split(indexName, "_")
	keys := bson.D{}

	// Parse pairs: field name, direction. Names can contain underscores,
	// so we look for "1" or "-1" as direction markers.
	i := 0
	for i < len(parts) {
		// Collect field name parts until we hit a direction
		var nameParts []string
		for i < len(parts) {
			if parts[i] == "1" || parts[i] == "-1" {
				break
			}
			nameParts = append(nameParts, parts[i])
			i++
		}
		fieldName := strings.Join(nameParts, "_")
		direction := 1
		if i < len(parts) {
			if parts[i] == "-1" {
				direction = -1
			}
			i++ // consume direction
		}
		if fieldName != "" {
			keys = append(keys, bson.E{Key: fieldName, Value: direction})
		}
	}

	model := mongo.IndexModel{Keys: keys}

	// Check if the original index name suggests uniqueness
	// (we can't determine this from the name alone, so we check the schema)
	// The caller may need to set unique separately if needed.
	// For now, check if this looks like a unique field from the registry.
	schemas := GetAll()
	for _, schema := range schemas {
		for _, field := range schema.Fields {
			if field.Unique && indexName == field.BSONName+"_1" {
				model.Options = options.Index().SetUnique(true)
				return model
			}
		}
		for _, ci := range schema.CompoundIndexes {
			if ci.Unique && compoundIndexName(ci) == indexName {
				model.Options = options.Index().SetUnique(true)
				return model
			}
		}
	}

	return model
}
