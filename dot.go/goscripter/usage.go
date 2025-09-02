package main

import (
	"fmt"
	"runtime"
)

func printUsage() {
	exe := "goscripter"
	fmt.Printf(`%s (%s %s)

Usage:
  %s <command> [<args>]

Commands:
  help         Show general help or help for a subcommand
  apply        Add/normalize shebang; ensure u+x; refresh cache (no run)
  update       Alias of 'apply'
  fmt          Format shebang-free body via cache temp; write back with normalized shebang
  ls           Show cache/config (file, CWD, or entire cache)
  rm           Remove cache for a script, or whole cache tree for user
  gc           Remove stale cache entries (missing source scripts)
  build        Build (or reuse) cached binary without running (full deps check)
  copy         Build (full deps) then copy cached binary to destination
  install      Alias of 'copy'; also supports --uid/--gid/--mode
  run          Build if needed and run (supports --nodeps)

See 'goscripter help <command>' for detailed flags.
`, exe, runtime.GOOS, runtime.GOARCH, exe)
}
