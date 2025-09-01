package goscripter

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func CmdGC(args []string) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	staleOnly := fs.Bool("stale-only", TrueDefault(), "remove only cache entries whose source script is missing")
	verbose := fs.Bool("verbose", FalseDefault(), "print each file/dir removed and total space freed")
	if err := fs.Parse(args); err != nil {
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
	cb := resolveCacheBase(mergeConfig(gl.Configs, Config{}, cwd).Global)

	root := userCacheRoot(cb)
	if !fileExists(root) {
		fmt.Println("gc:", root, "does not exist")
		return 0
	}

	var total rmStats
	var toRemove []string

	seenLeaf := map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := filepath.Base(p)
		if base != manifestName && base != cacheBinName {
			return nil
		}
		leaf := filepath.Dir(p)
		if seenLeaf[leaf] {
			return nil
		}
		seenLeaf[leaf] = true
		rel, err := filepath.Rel(root, leaf)
		if err != nil || rel == "." || rel == "" {
			return nil
		}
		scriptAbs := string(filepath.Separator) + rel
		if *staleOnly && fileExists(scriptAbs) {
			return nil
		}
		toRemove = append(toRemove, leaf)
		return nil
	})

	if len(toRemove) == 0 {
		fmt.Println("gc: nothing to remove")
		return 0
	}

	for _, leaf := range toRemove {
		st, _ := measureTree(leaf, *verbose)
		if err := removeTree(leaf); err != nil {
			warnf("gc: remove %s: %v", leaf, err)
			continue
		}
		total.files += st.files
		total.dirs += st.dirs
		total.bytes += st.bytes
	}
	fmt.Printf("gc: removed %s (files: %d, dirs: %d)\n", humanBytes(total.bytes), total.files, total.dirs)
	return 0
}
