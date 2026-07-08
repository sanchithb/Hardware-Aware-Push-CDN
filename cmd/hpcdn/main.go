// Command hpcdn is the single binary for the hardware-aware push CDN:
// controller, edge and origin roles plus the cluster admin CLI.
package main

import (
	"os"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
