// Command openetl is the CLI of the openetl migration progress platform.
package main

import (
	"os"

	"github.com/life-is-blue/openetl-cli/internal/cli"
)

// version is injected at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	app := &cli.App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Version: version}
	os.Exit(app.Run(os.Args[1:]))
}
