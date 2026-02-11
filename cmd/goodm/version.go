package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

const version = "0.1.0"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the goodm version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("goodm v%s\n", version)
	},
}
