package main

import (
	"log/slog"
	"os"

	"github.com/iu2tzo/ipsc2mmdvm/cmd"
)

// https://goreleaser.com/cookbooks/using-main.version/
//
//nolint:gochecknoglobals
var (
	version = "1.0.0"
	commit  = "IU2TZO master"
)

func main() {
	rootCmd := cmd.NewCommand(version, commit)

	if err := rootCmd.Execute(); err != nil {
		slog.Error("Encountered an error.", "error", err.Error())
		os.Exit(1)
	}
}
