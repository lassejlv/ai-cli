// Command ai generates images from the terminal.
package main

import (
	"os"

	"github.com/lassejlv/ai-cli/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
