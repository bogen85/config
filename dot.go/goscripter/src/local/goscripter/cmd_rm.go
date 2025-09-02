package goscripter

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func newRmFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	all := FalseDefault()
	verbose := FalseDefault()
	fs.BoolVar(&all, "all", FalseDefault(), "remove the entire cache tree for the current user")
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "print each file/dir removed and total space freed")
	fs.Usage = func() { usageRm(fs) }
	return fs
}

func CmdRm(args []string) int {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	all := FalseDefault()
	verbose := FalseDefault()
	fs.BoolVar(&all, "all", FalseDefault(), "remove the entire cache tree for the current user")
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "print each file/dir removed and total space freed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()

	cwd, _ := os.Getwd()
	gl := loadGlobalConfigs(cwd, loadStrict)
	if len(gl.Errs) > 0 {
		for _, e := range gl.Errs {
			eprintf(e.Error())
		}
		return 2
	}
	cb := resolveCacheBase(mergeConfig(gl.Configs, Config{}, cwd).Global)

	if all {
		root := userCacheRoot(cb)
		if !fileExists(root) {
			fmt.Println("rm --all:", root, "does not exist")
			return 0
		}
		st, _ := measureTree(root, verbose)
		if err := removeTree(root); err != nil {
			eprintf("rm --all: %v", err)
			return 2
		}
		fmt.Printf("Removed: %s (files: %d, dirs: %d)\n", humanBytes(st.bytes), st.files, st.dirs)
		return 0
	}

	if len(rest) != 1 {
		usageRm(newRmFlagSet())
		return 2
	}
	script := rest[0]
	abs, err := filepath.Abs(script)
	if err != nil {
		eprintf("rm: %v", err)
		return 2
	}
	lc, lwarns, lerrs := loadLocalConfig(abs+".toml", loadStrict)
	for _, w := range lwarns {
		eprintf(w)
	}
	if len(lerrs) > 0 {
		for _, e := range lerrs {
			eprintf(e.Error())
		}
		return 2
	}
	mc := mergeConfig(gl.Configs, lc, filepath.Dir(abs))
	cb = resolveCacheBase(mc.Global)

	cdir := cacheDirFor(cb, abs)
	if !fileExists(cdir) {
		fmt.Println("Nothing to remove:", cdir)
		return 0
	}
	st, _ := measureTree(cdir, verbose)
	if err := removeTree(cdir); err != nil {
		eprintf("rm: %v", err)
		return 2
	}
	fmt.Printf("Removed: %s (files: %d, dirs: %d)\n", humanBytes(st.bytes), st.files, st.dirs)
	return 0
}
