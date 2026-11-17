// minenest-gui is MineNest for the Start menu on Windows: built without a console window, it
// opens MineNest's window (or, with "daemon", runs the background service quietly).
package main

import (
	"os"

	"github.com/OwenCosta/minenest/internal/cli"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"gui"}
	}
	os.Exit(cli.RunQuiet(args))
}
