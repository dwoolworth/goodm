package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dwoolworth/goodm"
	"github.com/spf13/cobra"
)

var (
	diffFlag  bool
	mongoURI  string
	dbName    string
)

var inspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Inspect registered model schemas",
	Long:  "Display all registered model schemas with fields, indexes, and relations. Use --diff to compare against a live MongoDB instance.",
	RunE: func(cmd *cobra.Command, args []string) error {
		schemas := goodm.GetAll()
		if len(schemas) == 0 {
			fmt.Println("No models registered. Import your model packages to register them.")
			return nil
		}

		for _, schema := range schemas {
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
	inspectCmd.Flags().StringVar(&mongoURI, "uri", "mongodb://localhost:27017", "MongoDB connection URI")
	inspectCmd.Flags().StringVar(&dbName, "db", "", "MongoDB database name (required with --diff)")
}

func printSchema(schema *goodm.Schema) {
	fmt.Printf("%s (collection: %s)\n", schema.ModelName, schema.Collection)

	for i, field := range schema.Fields {
		connector := "├──"
		if i == len(schema.Fields)-1 && len(schema.CompoundIndexes) == 0 {
			connector = "└──"
		}

		attrs := formatFieldAttrs(field)
		refStr := ""
		if field.Ref != "" {
			refStr = fmt.Sprintf(" → %s._id", field.Ref)
		}

		fmt.Printf("  %s %-12s %-14s %s%s\n", connector, field.BSONName, field.Type, attrs, refStr)
	}

	// Print indexes
	if len(schema.CompoundIndexes) > 0 || hasIndexedFields(schema) {
		fmt.Println()
		fmt.Println("  Indexes:")
		for _, field := range schema.Fields {
			if field.Unique {
				fmt.Printf("    ✓ %s_1 (unique)\n", field.BSONName)
			} else if field.Index {
				fmt.Printf("    ✓ %s_1\n", field.BSONName)
			}
		}
		for _, ci := range schema.CompoundIndexes {
			name := compoundName(ci)
			label := "(compound)"
			if ci.Unique {
				label = "(compound, unique)"
			}
			fmt.Printf("    ✓ %s %s\n", name, label)
		}
	}

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
	if f.Min != nil {
		parts = append(parts, fmt.Sprintf("min: %d", *f.Min))
	}
	if f.Max != nil {
		parts = append(parts, fmt.Sprintf("max: %d", *f.Max))
	}
	return strings.Join(parts, ", ")
}

func hasIndexedFields(schema *goodm.Schema) bool {
	for _, f := range schema.Fields {
		if f.Unique || f.Index {
			return true
		}
	}
	return false
}

func compoundName(ci goodm.CompoundIndex) string {
	parts := make([]string, 0, len(ci.Fields)*2)
	for _, f := range ci.Fields {
		parts = append(parts, f, "1")
	}
	return strings.Join(parts, "_")
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

	drifts := goodm.DetectDrift(ctx, db, schema)
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
