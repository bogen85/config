package goscripter

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// FlagSet constructors + usage printer
func newApplyFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	autoYes := FalseDefault()
	verbose := FalseDefault()
	fs.BoolVar(&autoYes, "y", FalseDefault(), "assume yes; do not prompt")
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "verbose output")
	fs.BoolVar(&verbose, "v", FalseDefault(), "verbose output (short)")
	fs.Usage = func() { usageApply(fs) }
	return fs
}

func CmdApply(argv []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	autoYes := FalseDefault()
	verbose := FalseDefault()
	fs.BoolVar(&autoYes, "y", FalseDefault(), "assume yes; do not prompt")
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "verbose output")
	fs.BoolVar(&verbose, "v", FalseDefault(), "verbose output (short)")
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

	// Load configs (strict), merge (for defaults like always_yes), before prompting
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

	// Effective autoYes: CLI -y overrides config; otherwise consult [cmd.apply].always_yes
	effYes := autoYes || mc.CmdYes["apply"]

	// Shebang normalize (prompt unless effYes)
	sb, err := parseShebang(abs)
	if err != nil {
		eprintf("apply: %v", err)
		return 2
	}
	want := desiredShebangEnvOrAbsForApply(sb)
	needShebang := !sb.hasShebang || sb.line != want
	if needShebang {
		allowed := effYes || askConfirm(fmt.Sprintf("Add/normalize shebang on %s?", abs), false)
		if !allowed {
			if verbose {
				fmt.Println("apply: shebang unchanged (user declined)")
			}
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

	// Ensure owner-executable if missing (prompt unless effYes)
	info, err := os.Stat(abs)
	if err != nil {
		eprintf("apply: %v", err)
		return 2
	}
	needsExec := info.Mode().Perm()&0o100 == 0
	if needsExec {
		allowed := effYes || askConfirm(fmt.Sprintf("Add owner-exec bit (chmod u+x) on %s?", abs), false)
		if allowed {
			if err := ensureOwnerExec(abs, verbose); err != nil {
				eprintf("apply: chmod: %v", err)
				return 2
			}
		} else if verbose {
			fmt.Println("apply: not executable (user declined chmod)")
		}
	}

	// Refresh cache (no run)
	if _, err := refreshCache("apply", abs, cb, mc.Flags, mc.Env, verbose, false /*skipDeps*/); err != nil {
		eprintf("apply: %v", err)
		return 2
	}
	fmt.Println("apply: did not run (use 'goscripter run' to execute)")
	return 0
}

// FalseDefault returns false; it's here to make flag.Bool calls a bit cleaner.
func FalseDefault() bool { return false }

// TrueDefault returns true; for future use if needed.
func TrueDefault() bool { return true }
