package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// version is set at build time via -ldflags "-X main.version=x.y.z".
// Falls back to "dev" when built without ldflags.
var version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the goodm version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("goodm v%s\n", version)
	},
}
