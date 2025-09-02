package goscripter

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func newRunFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	verbose := FalseDefault()
	nodeps := FalseDefault()
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "verbose output")
	fs.BoolVar(&verbose, "v", FalseDefault(), "verbose output (short)")
	fs.BoolVar(&nodeps, "nodeps", FalseDefault(), "skip dependency/toolchain checks & snapshot")
	fs.BoolVar(&nodeps, "n", FalseDefault(), "skip dependency/toolchain checks & snapshot (short)")
	fs.Usage = func() { usageRun(fs) }
	return fs
}

func parseRunArgs(args []string) (verbose bool, nodeps bool, script string, pass []string, ok bool) {
	verbose = false
	nodeps = false
	pass = []string{}
	dashdash := -1
	for i, a := range args {
		if a == "--" {
			dashdash = i
			break
		}
	}
	pre := args
	var post []string
	if dashdash >= 0 {
		pre = args[:dashdash]
		post = args[dashdash+1:]
	}
	for _, a := range pre {
		if a == "--verbose" || a == "-v" {
			verbose = TrueDefault()
			continue
		}
		if a == "--nodeps" || a == "-n" {
			nodeps = TrueDefault()
			continue
		}
		if strings.HasPrefix(a, "-") && script == "" {
			continue
		}
		if script == "" {
			script = a
		} else {
			if dashdash < 0 {
				pass = append(pass, a)
			}
		}
	}
	if dashdash >= 0 {
		pass = append(pass, post...)
	}
	ok = script != ""
	return
}

func truthyEnv(name string) bool {
	v := os.Getenv(name)
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func CmdRun(args []string) int {
	if len(args) < 1 {
		eprintf("run: missing arguments")
		return 2
	}
	verbose, nodeps, script, pass, ok := parseRunArgs(args)
	if !ok {
		eprintf("run: script.go required")
		return 2
	}
	abs, err := filepath.Abs(script)
	if err != nil {
		eprintf("run: %v", err)
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

	if !nodeps {
		if truthyEnv("GOSCRIPTER_NODEP") || truthyEnv("GOSCRIPTER_NODEPS") {
			nodeps = TrueDefault()
		} else if mc.Nodeps != nil && *mc.Nodeps {
			nodeps = TrueDefault()
		}
	}

	if _, err := refreshCache("run", abs, cb, mc.Flags, mc.Env, verbose, nodeps); err != nil {
		eprintf("run: %v", err)
		return 2
	}
	if verbose {
		cdir := cacheDirFor(cb, abs)
		if nodeps {
			fmt.Printf("run: (nodeps) exec %s -- %s\n", filepath.Join(cdir, cacheBinName), strings.Join(pass, " "))
		} else {
			fmt.Printf("run: exec %s -- %s\n", filepath.Join(cdir, cacheBinName), strings.Join(pass, " "))
		}
	}
	return runFromCache(abs, cb, pass)
}

func init() {
	Register(&Command{
		Name:    "run",
		Summary: "Build if needed and run (supports --nodeps)",
		Help:    func() { usageRun(newRunFlagSet()) },
		Run:     CmdRun,
	})
}
