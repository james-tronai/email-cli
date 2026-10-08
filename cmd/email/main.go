package main

import (
	"os"

	"github.com/james-tronai/email-cli/internal/cli"
)

// Set by goreleaser / -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(cli.Execute(cli.BuildInfo{Version: version, Commit: commit, Date: date}, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
