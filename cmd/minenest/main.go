// MineNest: easy Minecraft servers on Windows and Linux. See README.md.
package main

import (
	"os"

	"github.com/OwenCosta/minenest/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:])) }
