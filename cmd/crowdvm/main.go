// Command crowdvm is the CrowdVM CLI: persistent Ubuntu sandboxes on the
// spare compute of your LAN, with a Boat-compatible command set.
package main

import (
	"os"

	"github.com/crowdvm/crowdvm/internal/cli"
	"github.com/crowdvm/crowdvm/internal/client"
)

// version is set at build time (-X main.version=...).
var version = "dev"

func main() {
	client.Version = version
	os.Exit(cli.Main(os.Args[1:]))
}
