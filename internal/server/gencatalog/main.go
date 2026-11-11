//go:build !windows

// Command gencatalog writes the guest's provider/model catalog as JSON
// (the snapshot Windows servers embed). Usage: go run ./gencatalog FILE
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/crowdvm/crowdvm/internal/guest/agents"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gencatalog FILE")
		os.Exit(2)
	}
	b, err := json.MarshalIndent(agents.Catalog(), "", "  ")
	if err == nil {
		err = os.WriteFile(os.Args[1], append(b, '\n'), 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
