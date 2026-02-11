package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dwoolworth/goodm"
	"github.com/spf13/cobra"
)

var (
	discoverURI        string
	discoverDB         string
	discoverCollection string
	discoverOutput     string
	discoverPackage    string
	discoverSampleSize int
)

var discoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Discover collections from an existing MongoDB and generate Go models",
	Long:  "Connect to a MongoDB database, sample documents, read indexes, and generate Go model source files.",
	RunE:  runDiscover,
}

func init() {
	discoverCmd.Flags().StringVar(&discoverURI, "uri", "mongodb://localhost:27017", "MongoDB connection URI")
	discoverCmd.Flags().StringVar(&discoverDB, "db", "", "MongoDB database name")
	discoverCmd.Flags().StringVar(&discoverCollection, "collection", "", "Specific collection to discover (empty = all)")
	discoverCmd.Flags().StringVar(&discoverOutput, "output", "./models", "Output directory for generated files")
	discoverCmd.Flags().StringVar(&discoverPackage, "package", "models", "Go package name for generated files")
	discoverCmd.Flags().IntVar(&discoverSampleSize, "sample-size", 500, "Number of documents to sample per collection")
	_ = discoverCmd.MarkFlagRequired("db")
}

func runDiscover(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := goodm.Connect(ctx, discoverURI, discoverDB)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	opts := goodm.DiscoverOptions{
		SampleSize: discoverSampleSize,
	}
	if discoverCollection != "" {
		opts.Collections = []string{discoverCollection}
	}

	fmt.Printf("Discovering database: %s\n\n", discoverDB)

	collections, err := goodm.Discover(ctx, db, opts)
	if err != nil {
		return err
	}

	if len(collections) == 0 {
		fmt.Println("No collections found.")
		return nil
	}

	// Ensure output directory exists
	if err := os.MkdirAll(discoverOutput, 0o755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	genOpts := goodm.GenerateOptions{
		PackageName: discoverPackage,
		OutputDir:   discoverOutput,
		EmbedModel:  true,
	}

	for _, coll := range collections {
		fmt.Printf("  %s (%d documents, %d fields, %d indexes)\n",
			coll.Name, coll.DocCount, len(coll.Fields), len(coll.Indexes))

		src, err := goodm.GenerateModel(coll, genOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "    Warning: failed to generate %s: %v\n", coll.Name, err)
			continue
		}

		filename := filepath.Join(discoverOutput, coll.Name+".go")
		if err := os.WriteFile(filename, src, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "    Warning: failed to write %s: %v\n", filename, err)
			continue
		}
		fmt.Printf("    → %s\n", filename)
	}

	fmt.Printf("\nGenerated %d model files in %s/\n", len(collections), discoverOutput)
	return nil
}
