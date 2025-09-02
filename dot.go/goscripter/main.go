#!/home/dwight/bin/goscripter run
package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"

	gs "local/goscripter"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	sub := os.Args[1]

	// help dispatch
	if sub == "help" {
		if len(os.Args) == 2 {
			printUsage()
			os.Exit(0)
		}
		want := os.Args[2]
		if cmd := gs.Resolve(want); cmd != nil && cmd.Help != nil {
			cmd.Help()
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Unknown command %q\n", want)
		os.Exit(2)
	}

	// command dispatch
	if cmd := gs.Resolve(sub); cmd != nil && cmd.Run != nil {
		os.Exit(cmd.Run(os.Args[2:]))
	}

	printUsage()
	os.Exit(2)
}

func printUsage() {
	exe := "goscripter"
	fmt.Printf(`%s (%s %s)

Usage:
  %s <command> [<args>]

Commands:
`, exe, runtime.GOOS, runtime.GOARCH, exe)

	for _, c := range gs.CommandList() {
		fmt.Printf("  %-12s %s\n", c.Name, c.Summary)
		if len(c.Aliases) > 0 {
			aliases := append([]string(nil), c.Aliases...)
			sort.Strings(aliases)
			for _, a := range aliases {
				fmt.Printf("    %-10s (alias of %s)\n", a, c.Name)
			}
		}
	}
	fmt.Printf("\nSee '%s help <command>' for detailed flags.\n", exe)
}
