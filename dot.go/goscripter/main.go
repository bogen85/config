package main

import (
	"os"

	gs "local/goscripter"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "apply":
		os.Exit(gs.CmdApply(os.Args[2:]))
	case "fmt":
		os.Exit(gs.CmdFmt(os.Args[2:]))
	case "ls":
		os.Exit(gs.CmdLs(os.Args[2:]))
	case "rm":
		os.Exit(gs.CmdRm(os.Args[2:]))
	case "gc":
		os.Exit(gs.CmdGC(os.Args[2:]))
	case "run":
		os.Exit(gs.CmdRun(os.Args[2:]))
	default:
		printUsage()
		os.Exit(2)
	}
}
