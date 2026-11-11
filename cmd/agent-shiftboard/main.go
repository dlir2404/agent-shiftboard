// Command agent-shiftboard is the AgentShiftboard CLI.
package main

import (
	"os"

	"github.com/dlir2404/agent-shiftboard/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
