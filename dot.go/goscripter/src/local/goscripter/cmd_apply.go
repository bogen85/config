package goscripter

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func newApplyFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	autoYes := FalseDefault()
	verbose := FalseDefault()
	initCfg := ""
	fs.BoolVar(&autoYes, "y", FalseDefault(), "assume yes; do not prompt")
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "verbose output")
	fs.BoolVar(&verbose, "v", FalseDefault(), "verbose output (short)")
	fs.StringVar(&initCfg, "init-config", "", "create <script.go>.toml if missing; optional value: minimal|full")
	fs.Usage = func() { usageApply(fs) }
	return fs
}

func writeScriptConfigSkeleton(path string, style string) error {
	if style == "" {
		style = "minimal"
	}
	var body string
	switch style {
	case "minimal":
		body = `__note = "Script-local settings for this Go script."

[env]
GO111MODULE = "auto"
#GOPATH = "/usr/share/gocode"

[env_append]
GOPATH = "."
__note = "Add the script's directory to GOPATH at runtime."

[build]
#flags = ["-trimpath","-ldflags=-s -w"]

[goscripter]
nodeps = false

[cmd.apply]
always_yes = false
__note = "Set to true to skip confirm prompts for apply."

[cmd.copy]
always_strip = false
__note = "Set to true to strip binaries on copy by default."
`
	case "full":
		body = `__note = "Script-local settings for this Go script (full template)."

[cache]
#root = "/custom/cache/root"
__note = "Override cache root; default is ~/.cache/goscripter"

[env]
GO111MODULE = "auto"
#GOPATH = "/usr/share/gocode"
__note = "Explicit env overrides (absolute paths or '.')"

[env_append]
GOPATH = "."
__note = "GOPATH entries appended to env.GOPATH ('.' expands to script dir)."

[build]
#flags = ["-trimpath","-ldflags=-s -w"]
__note = "Default go build flags appended for this script."

[goscripter]
nodeps = false
__note = "If true, 'run' skips deps/toolchain checks (can still 'build' for full checks)."

[cmd.apply]
always_yes = false
__note = "Skip prompts in 'apply' when true."

[cmd.copy]
always_strip = false
__note = "Strip binaries on copy by default when true."
`
	default:
		return fmt.Errorf("unknown template %q (use minimal|full)", style)
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func CmdApply(argv []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	autoYes := FalseDefault()
	verbose := FalseDefault()
	initCfg := ""
	fs.BoolVar(&autoYes, "y", FalseDefault(), "assume yes; do not prompt")
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "verbose output")
	fs.BoolVar(&verbose, "v", FalseDefault(), "verbose output (short)")
	fs.StringVar(&initCfg, "init-config", "", "create <script.go>.toml if missing; optional value: minimal|full")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	args := fs.Args()
	if len(args) != 1 {
		usageApply(newApplyFlagSet())
		return 2
	}
	script := args[0]

	abs, err := filepath.Abs(script)
	if err != nil {
		eprintf("apply: %v", err)
		return 2
	}

	cwd, _ := os.Getwd()
	gl := loadGlobalConfigs(cwd, loadStrict)
	if len(gl.Errs) > 0 {
		for _, e := range gl.Errs {
			eprintf(e.Error())
		}
		return 2
	}
	local, lwarns, lerrs := loadLocalConfig(abs+".toml", loadStrict)
	for _, w := range lwarns {
		eprintf(w)
	}
	if len(lerrs) > 0 {
		for _, e := range lerrs {
			eprintf(e.Error())
		}
		return 2
	}
	mc := mergeConfig(gl.Configs, local, filepath.Dir(abs))
	cb := resolveCacheBase(mc.Global)

	effYes := autoYes || mc.CmdYes["apply"]

	// optional: init-config skeleton creation if missing
	confPath := abs + ".toml"
	if initCfg != "" && !fileExists(confPath) {
		if effYes || askConfirm(fmt.Sprintf("Create %s from %s template?", confPath, initCfg), TrueDefault()) {
			if err := writeScriptConfigSkeleton(confPath, initCfg); err != nil {
				eprintf("apply: init-config: %v", err)
				return 2
			}
			if verbose {
				fmt.Println("apply: created", confPath)
			}
			// reload local to honor any immediate settings
			local, lwarns, lerrs = loadLocalConfig(abs+".toml", loadStrict)
			for _, w := range lwarns {
				eprintf(w)
			}
			if len(lerrs) > 0 {
				for _, e := range lerrs {
					eprintf(e.Error())
				}
			}
			mc = mergeConfig(gl.Configs, local, filepath.Dir(abs))
			cb = resolveCacheBase(mc.Global)
		} else if verbose {
			fmt.Println("apply: init-config skipped")
		}
	}

	sb, err := parseShebang(abs)
	if err != nil {
		eprintf("apply: %v", err)
		return 2
	}
	want := desiredShebangEnvOrAbsForApply(sb)
	needShebang := !sb.hasShebang || sb.line != want
	if needShebang {
		allowed := effYes || askConfirm(fmt.Sprintf("Add/normalize shebang on %s?", abs), FalseDefault())
		if !allowed {
			fmt.Println("apply: skipped")
			return 0
		}
		changed, err := writeShebangLinePreserveMode(abs, want)
		if err != nil {
			eprintf("apply: shebang: %v", err)
			return 2
		}
		if verbose {
			if changed {
				fmt.Println("apply: shebang updated")
			} else {
				fmt.Println("apply: shebang already correct")
			}
		}
	} else if verbose {
		fmt.Println("apply: shebang already correct")
	}

	info, err := os.Stat(abs)
	if err != nil {
		eprintf("apply: %v", err)
		return 2
	}
	needsExec := info.Mode().Perm()&0o100 == 0
	if needsExec {
		allowed := effYes || askConfirm(fmt.Sprintf("Add owner-exec bit (chmod u+x) on %s?", abs), FalseDefault())
		if allowed {
			if err := ensureOwnerExec(abs, verbose); err != nil {
				eprintf("apply: chmod: %v", err)
				return 2
			}
		} else if verbose {
			fmt.Println("apply: not executable (user declined chmod)")
		}
	}

	if _, err := refreshCache("apply", abs, cb, mc.Flags, mc.Env, verbose, FalseDefault() /*skipDeps*/); err != nil {
		eprintf("apply: %v", err)
		return 2
	}
	fmt.Println("apply: did not run (use 'goscripter run' to execute)")
	return 0
}

func FalseDefault() bool { return false }
func TrueDefault() bool  { return true }

func init() {
	Register(&Command{
		Name:    "apply",
		Aliases: []string{"update"},
		Summary: "Add/normalize shebang; ensure u+x; refresh cache (no run)",
		Help:    func() { usageApply(newApplyFlagSet()) },
		Run:     CmdApply,
	})
}
