package goscripter

import (
	"flag"
	"fmt"
)

func usageApply(fs *flag.FlagSet) {
	fmt.Println("Usage: goscripter apply [-y] [--verbose|-v] [--init-config[=minimal|full]] <script.go>")
	fmt.Println("Add/normalize shebang; optionally set u+x; refresh cache (no run).")
	fmt.Println("Default prompts can be disabled via config: [cmd.apply] always_yes = true")
	fs.PrintDefaults()
}
func usageFmt(fs *flag.FlagSet) {
	fmt.Println("Usage: goscripter fmt [script.go ...]")
	fmt.Println("Format shebang-free body via cache temp; write back with normalized shebang.")
	fs.PrintDefaults()
}
func usageLs(fs *flag.FlagSet) {
	fmt.Println("Usage: goscripter ls [--all] [--verbose|-v] [--deps] [script.go ...]")
	fmt.Println("Show cache/config for scripts in CWD (default), explicit files, or entire cache with --all.")
	fs.PrintDefaults()
}
func usageRm(fs *flag.FlagSet) {
	fmt.Println("Usage: goscripter rm [--all] [--verbose|-v] [script.go]")
	fmt.Println("Remove cache for a script, or whole cache tree for user (--all).")
	fs.PrintDefaults()
}
func usageGc(fs *flag.FlagSet) {
	fmt.Println("Usage: goscripter gc [--stale-only] [--verbose|-v]")
	fmt.Println("Remove cache entries; default removes only stale (source missing).")
	fs.PrintDefaults()
}
func usageBuild(fs *flag.FlagSet) {
	fmt.Println("Usage: goscripter build [--verbose|-v] <script.go>")
	fmt.Println("Build (or reuse) cached binary without running. Always performs full deps/toolchain checks (ignores any nodeps).")
	fs.PrintDefaults()
}
func usageCopy(fs *flag.FlagSet) {
	fmt.Println("Usage: goscripter copy [--verbose|-v] [--force|-f] [--mkdirs] [--uid N] [--gid N] [--mode 0755] [--strip] <script.go> [--] <dest>")
	fmt.Println("Build (full deps) then copy cached binary to destination path; optional ownership, file mode, and strip.")
	fs.PrintDefaults()
}
func usageRun(fs *flag.FlagSet) {
	fmt.Println("Usage: goscripter run [--verbose|-v] [--nodeps|-n] <script.go> [-- args...]")
	fmt.Println("Build if needed and run. --nodeps skips dependency/toolchain checks & snapshot.")
	fs.PrintDefaults()
}
func usageConfig(fs *flag.FlagSet) {
	fmt.Println("Usage: goscripter config [SCOPE] ACTION [ARGS] [FLAGS]")
	fmt.Println("\nScopes (one of): --script <file.go> | --local | --global | --system | --etc | --file <path>")
	fmt.Println("Actions: get <key> | set <key> <value> | unset <key> | list | sections | dump")
	fmt.Println("Common flags: --effective (read-only), --origin, --json, --strict/--no-strict")
	fmt.Println("Set/unset flags: --append (arrays), --remove <value> (arrays), --type {string,int,bool,array,toml}, --section, --create/--no-create, --backup/--no-backup")
	fs.PrintDefaults()
}
