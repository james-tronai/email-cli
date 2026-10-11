package main

import (
	"os"

	// Mozilla's CA roots, used only where the system has none (the
	// FROM scratch image, minimal containers). A system store wins.
	_ "golang.org/x/crypto/x509roots/fallback"

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
