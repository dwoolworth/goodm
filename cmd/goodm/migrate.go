package main

import (
	"context"
	"fmt"
	"time"

	"github.com/dwoolworth/goodm"
	"github.com/spf13/cobra"
)

var (
	migrateURI        string
	migrateDB         string
	migrateDryRun     bool
	migrateDropExtras bool
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate database indexes to match registered schemas",
	Long:  "Compare registered model schemas against the live database and apply index changes.",
	RunE:  runMigrate,
}

func init() {
	migrateCmd.Flags().StringVar(&migrateURI, "uri", "mongodb://localhost:27017", "MongoDB connection URI")
	migrateCmd.Flags().StringVar(&migrateDB, "db", "", "MongoDB database name")
	migrateCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false, "Show planned changes without applying them")
	migrateCmd.Flags().BoolVar(&migrateDropExtras, "drop-extras", false, "Drop indexes not defined in schemas")
	_ = migrateCmd.MarkFlagRequired("db")
}

func runMigrate(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := goodm.Connect(ctx, migrateURI, migrateDB)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	schemas := goodm.GetAll()
	if len(schemas) == 0 {
		fmt.Println("No models registered. Import your model packages to register them.")
		return nil
	}

	plan, err := goodm.PlanMigration(ctx, db, schemas)
	if err != nil {
		return err
	}

	fmt.Printf("Migration Plan for %s\n", migrateDB)
	fmt.Println(repeat("=", len("Migration Plan for ")+len(migrateDB)))
	fmt.Println()

	// Group actions by collection
	collectionActions := make(map[string][]goodm.MigrationAction)
	collectionOrder := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		collectionOrder = append(collectionOrder, schema.Collection)
	}
	for _, action := range plan.Actions {
		collectionActions[action.Collection] = append(collectionActions[action.Collection], action)
	}

	createCount, dropCount, warnCount := 0, 0, 0
	for _, collName := range collectionOrder {
		fmt.Printf("%s:\n", collName)
		c, d, w := displayPlanActions(collectionActions[collName])
		createCount += c
		dropCount += d
		warnCount += w
		fmt.Println()
	}

	fmt.Printf("Summary: %d to create, %d to drop, %d warning(s)\n", createCount, dropCount, warnCount)

	if migrateDryRun {
		fmt.Println("Run without --dry-run to apply.")
		return nil
	}

	// Execute
	opts := goodm.MigrateOptions{
		DryRun:     false,
		DropExtras: migrateDropExtras,
	}
	result, err := goodm.ExecuteMigration(ctx, db, plan, opts)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("Executed: %d, Skipped: %d\n", result.Executed, result.Skipped)

	for _, w := range result.Warnings {
		fmt.Printf("  ⚠ %s\n", w)
	}
	for _, e := range result.Errors {
		fmt.Printf("  ✗ %s\n", e)
	}

	return nil
}

func displayPlanActions(actions []goodm.MigrationAction) (created, dropped, warned int) {
	if len(actions) == 0 {
		fmt.Println("  ✓ No changes needed")
		return 0, 0, 0
	}
	for _, action := range actions {
		switch action.Type {
		case goodm.ActionCreateIndex:
			fmt.Printf("  + %s\n", action.Description)
			created++
		case goodm.ActionDropIndex:
			fmt.Printf("  - %s\n", action.Description)
			dropped++
		case goodm.ActionFieldDrift:
			fmt.Printf("  ⚠ %s\n", action.Description)
			warned++
		}
	}
	return
}

func repeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	result := make([]byte, n*len(s))
	for i := 0; i < n; i++ {
		copy(result[i*len(s):], s)
	}
	return string(result)
}
