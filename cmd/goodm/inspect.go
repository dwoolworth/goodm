package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dwoolworth/goodm"
	"github.com/spf13/cobra"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var (
	diffFlag       bool
	jsonSchemaFlag bool
	mongoURI       string
	dbName         string
)

var inspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Inspect registered model schemas",
	Long:  "Display all registered model schemas with fields, indexes, and relations. Use --diff to compare against a live MongoDB instance, or --json-schema to print each model's $jsonSchema validator.",
	RunE: func(cmd *cobra.Command, args []string) error {
		schemas := goodm.GetAll()
		if len(schemas) == 0 {
			fmt.Println("No models registered. Import your model packages to register them.")
			return nil
		}

		for _, schema := range schemas {
			if jsonSchemaFlag {
				if err := printJSONSchema(schema); err != nil {
					return err
				}
				continue
			}
			printSchema(schema)

			if diffFlag {
				if err := printDiff(schema); err != nil {
					fmt.Fprintf(os.Stderr, "  Warning: could not diff %s: %v\n", schema.Collection, err)
				}
			}
			fmt.Println()
		}
		return nil
	},
}

func init() {
	inspectCmd.Flags().BoolVar(&diffFlag, "diff", false, "Compare schemas against live MongoDB")
	inspectCmd.Flags().BoolVar(&jsonSchemaFlag, "json-schema", false, "Print each model's $jsonSchema validator as JSON")
	inspectCmd.Flags().StringVar(&mongoURI, "uri", "mongodb://localhost:27017", "MongoDB connection URI")
	inspectCmd.Flags().StringVar(&dbName, "db", "", "MongoDB database name (required with --diff)")
}

// printJSONSchema prints the collection validator derived from the schema,
// as a document ready for collMod or createCollection.
func printJSONSchema(schema *goodm.Schema) error {
	validator := bson.D{{Key: "$jsonSchema", Value: schema.JSONSchema()}}
	out, err := bson.MarshalExtJSONIndent(validator, false, false, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", schema.ModelName, err)
	}
	fmt.Printf("// %s (collection: %s)\n%s\n", schema.ModelName, schema.Collection, out)
	return nil
}

func printSchema(schema *goodm.Schema) {
	fmt.Printf("%s (collection: %s)\n", schema.ModelName, schema.Collection)

	for i, field := range schema.Fields {
		connector := "├──"
		if i == len(schema.Fields)-1 && len(schema.IndexDefinitions()) == 0 {
			connector = "└──"
		}

		attrs := formatFieldAttrs(field)
		refStr := ""
		if field.Ref != "" {
			refStr = fmt.Sprintf(" → %s._id", field.Ref)
		}

		fmt.Printf("  %s %-12s %-14s %s%s\n", connector, field.BSONName, field.Type, attrs, refStr)
	}

	printIndexes(schema)

	// Print relations
	refs := collectRefs(schema)
	if len(refs) > 0 {
		fmt.Println()
		fmt.Println("  Relations:")
		for _, r := range refs {
			fmt.Printf("    → %s (1:1 via %s)\n", r.ref, r.field)
		}
	}

	// Print hooks
	if len(schema.Hooks) > 0 {
		fmt.Println()
		fmt.Println("  Hooks:")
		for _, h := range schema.Hooks {
			fmt.Printf("    ⚡ %s\n", h)
		}
	}
}

func printIndexes(schema *goodm.Schema) {
	defs := schema.IndexDefinitions()
	if len(defs) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("  Indexes:")
	for _, def := range defs {
		fmt.Printf("    ✓ %s%s\n", def.Name, describeIndex(def))
	}
}

func describeIndex(def goodm.IndexDefinition) string {
	var parts []string
	if len(def.Keys) > 1 {
		parts = append(parts, "compound")
	}
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
	return " (" + strings.Join(parts, ", ") + ")"
}

func formatFieldAttrs(f goodm.FieldSchema) string {
	var parts []string
	if f.Unique {
		parts = append(parts, "unique")
	}
	if f.Index {
		parts = append(parts, "indexed")
	}
	if f.Required {
		parts = append(parts, "required")
	}
	if f.Immutable {
		parts = append(parts, "immutable")
	}
	if len(f.Enum) > 0 {
		parts = append(parts, fmt.Sprintf("enum(%s)", strings.Join(f.Enum, "|")))
	}
	if f.Default != "" {
		parts = append(parts, fmt.Sprintf("default: %s", f.Default))
	}
	if f.Sparse {
		parts = append(parts, "sparse")
	}
	if f.TTL != nil {
		parts = append(parts, fmt.Sprintf("ttl: %ds", *f.TTL))
	}
	if f.Min != nil {
		parts = append(parts, fmt.Sprintf("min: %g", *f.Min))
	}
	if f.Max != nil {
		parts = append(parts, fmt.Sprintf("max: %g", *f.Max))
	}
	return strings.Join(parts, ", ")
}

type refInfo struct {
	field string
	ref   string
}

func collectRefs(schema *goodm.Schema) []refInfo {
	var refs []refInfo
	for _, f := range schema.Fields {
		if f.Ref != "" {
			refs = append(refs, refInfo{field: f.BSONName, ref: f.Ref})
		}
	}
	return refs
}

func printDiff(schema *goodm.Schema) error {
	if dbName == "" {
		return fmt.Errorf("--db flag is required with --diff")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := goodm.Connect(ctx, mongoURI, dbName)
	if err != nil {
		return err
	}

	drifts := goodm.DetectDrift(ctx, db, schema, goodm.DefaultDriftSampleSize)
	if len(drifts) == 0 {
		fmt.Println("  Drift: ✓ No drift detected")
	} else {
		fmt.Println("  Drift:")
		for _, d := range drifts {
			fmt.Printf("    ⚠ %s: %s\n", d.Field, d.Message)
		}
	}
	return nil
}
