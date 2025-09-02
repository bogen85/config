package goscripter

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func newBuildFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	verbose := FalseDefault()
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "verbose output")
	fs.BoolVar(&verbose, "v", FalseDefault(), "verbose output (short)")
	fs.Usage = func() { usageBuild(fs) }
	return fs
}

func CmdBuild(args []string) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	verbose := FalseDefault()
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "verbose output")
	fs.BoolVar(&verbose, "v", FalseDefault(), "verbose output (short)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		usageBuild(newBuildFlagSet())
		return 2
	}
	script := rest[0]
	abs, err := filepath.Abs(script)
	if err != nil {
		eprintf("build: %v", err)
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

	if _, err := refreshCache("build", abs, cb, mc.Flags, mc.Env, verbose, FalseDefault() /*skipDeps*/); err != nil {
		eprintf("build: %v", err)
		return 2
	}
	if verbose {
		cdir := cacheDirFor(cb, abs)
		fmt.Printf("build: binary at %s\n", filepath.Join(cdir, cacheBinName))
	}
	return 0
}

func init() {
	Register(&Command{
		Name:    "build",
		Summary: "Build (or reuse) cached binary without running (full deps check)",
		Help:    func() { usageBuild(newBuildFlagSet()) },
		Run:     CmdBuild,
	})
}
