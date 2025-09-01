package main

import (
	"fmt"
	"os"
	"runtime"
)

// printUsage is kept in package main, as requested.
func printUsage() {
	exe := os.Args[0]
	fmt.Fprintf(os.Stderr, `goscripter (%s %s)

Usage:
  %s apply [-y] [--verbose|-v] <script.go>   Add/normalize shebang; ensure u+x; refresh cache (no run)
  %s fmt [script.go]                         Format shebang-free body via cache temp; write back w/ normalized shebang
  %s ls [--all] [--verbose|-v] [--deps] [script.go]   Show cache/config (file, CWD, or entire cache)
  %s rm [--all] [--verbose|-v] [script.go]   Remove cache for script, or whole cache tree for user
  %s gc [--stale-only] [--verbose|-v]        Remove stale cache entries (missing source scripts)
  %s run [--verbose|-v] <script.go> [-- args...]  Build if needed and run (verbose must be before "--")
`, runtime.GOOS, runtime.GOARCH, exe, exe, exe, exe, exe, exe)
}
