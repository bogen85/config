package goscripter

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func printDescForScript(script string, cb cacheBase, mc mergedConfig, verbose bool, showDeps bool) {
	abs, err := filepath.Abs(script)
	if err != nil {
		warnf("ls: %s: %v", script, err)
		return
	}
	sb, err := parseShebang(abs)
	if err != nil {
		warnf("ls: %s: %v", script, err)
		return
	}

	fmt.Println("Script:       ", abs)
	if sb.hasShebang {
		fmt.Println("Shebang:      ", sb.line)
	} else {
		fmt.Println("Shebang:       (none)")
	}
	wantAbs := desiredShebangAbs()
	isNorm := sb.hasShebang && (sb.line == wantAbs || (isEnvGoscripter(sb) && sameFile(lookPathGoscripter(), selfAbsPath())))
	fmt.Println("Normalize?:   ", !isNorm)

	fmt.Println("GO111MODULE:  ", mc.Env.GO111MODULE)
	fmt.Println("GOPATH:       ", strings.Join(mc.Env.GOPATH, string(os.PathListSeparator)))
	if len(mc.Flags) > 0 {
		fmt.Println("Build Flags:  ", strings.Join(mc.Flags, " "))
	} else {
		fmt.Println("Build Flags:   (none)")
	}

	cdir := cacheDirFor(cb, abs)
	man := filepath.Join(cdir, manifestName)
	bin := filepath.Join(cdir, cacheBinName)
	mod := filepath.Join(cdir, modifiedSrcName)
	dep := filepath.Join(cdir, depsSnapshotName)

	if m, err := readManifest(man); err == nil {
		fmt.Println("Manifest:      present")
		fmt.Println("  source_mtime:", time.Unix(m.SourceMTime, 0))
		fmt.Println("  flags:       ", strings.Join(m.Flags, " "))
		if m.EnvGO111MODULE != "" || len(m.EnvGOPATH) > 0 {
			fmt.Println("  env:         GO111MODULE=", m.EnvGO111MODULE)
			fmt.Println("               GOPATH     =", strings.Join(m.EnvGOPATH, string(os.PathListSeparator)))
		}
	} else {
		fmt.Println("Manifest:      (missing)")
	}
	if fi, err := os.Stat(bin); err == nil {
		fmt.Printf("Binary:        present (%d bytes)\n", fi.Size())
	} else {
		fmt.Println("Binary:        (missing)")
	}

	// deps snapshot summary
	if s, err := readDepsSnapshot(dep); err == nil {
		if s.Fb != nil {
			fmt.Printf("Deps:          fallback scan (root=%s, files=%d)\n", s.Fb.Root, s.Fb.FileCount)
		} else {
			fmt.Printf("Deps:          %d packages tracked\n", len(s.Deps))
		}
	} else {
		fmt.Println("Deps:          (missing)")
	}

	if verbose {
		fmt.Println("Cache Dir:     ", cdir)
		fmt.Println("Modified Src:  ", mod)
		fmt.Println("Manifest Path: ", man)
		fmt.Println("Binary Path:   ", bin)
		fmt.Println("Deps Path:     ", dep)
		dec := analyzeCache(abs, cdir, mc.Flags, mc.Env)
		if dec.rebuild {
			fmt.Println("Would rebuild: yes")
			for _, r := range dec.reasons {
				fmt.Println("  -", r)
			}
			fmt.Printf("Build cmd:     %s\n", dec.buildCmd)
			fmt.Printf("Build env:     %s\n", dec.buildEnvM)
			fmt.Printf("               %s\n", dec.buildEnvP)
		} else {
			fmt.Println("Would rebuild: no")
			if dec.binOK {
				fmt.Printf("Cached mtime:  %s\n", dec.binMTime.Format(time.RFC3339))
			}
		}
		fmt.Println("This goscripter:", selfAbsPath())
		if ep := lookPathGoscripter(); ep != "" {
			fmt.Println("env goscripter:", ep, "(same? ", sameFile(ep, selfAbsPath()), ")")
		} else {
			fmt.Println("env goscripter: (not found)")
		}
	}

	if showDeps {
		if s, err := readDepsSnapshot(dep); err == nil {
			if s.Fb != nil {
				fmt.Printf("Deps (fallback root=%s, files=%d, max_mtime=%s)\n",
					s.Fb.Root, s.Fb.FileCount, time.Unix(s.Fb.MaxMTime, 0).Format(time.RFC3339))
			} else if len(s.Deps) > 0 {
				fmt.Println("Deps (non-stdlib):")
				for _, d := range s.Deps {
					fmt.Printf("  - %s\n    dir=%s\n    newest=%s files=%d\n",
						d.ImportPath, d.Dir, time.Unix(d.MaxMTime, 0).Format(time.RFC3339), d.FileCount)
				}
			} else {
				fmt.Println("Deps: (empty)")
			}
		} else {
			fmt.Println("Deps: (none)")
		}
	}

	fmt.Println()
}

func listAllCache(cb cacheBase, verbose bool, showDeps bool) {
	root := userCacheRoot(cb)
	var hits []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		switch filepath.Base(p) {
		case manifestName, cacheBinName:
			dir := filepath.Dir(p)
			rel, err := filepath.Rel(root, dir)
			if err != nil || rel == "." || rel == "" {
				return nil
			}
			scriptAbs := string(filepath.Separator) + rel
			hits = append(hits, scriptAbs)
		}
		return nil
	})
	sort.Strings(hits)
	seen := map[string]bool{}
	for _, s := range hits {
		if seen[s] {
			continue
		}
		seen[s] = true
		fmt.Printf("Script:        %s", s)
		if !fileExists(s) {
			fmt.Printf("  (missing on disk)")
		}
		fmt.Println()
		cdir := cacheDirFor(cb, s)
		bin := filepath.Join(cdir, cacheBinName)
		man := filepath.Join(cdir, manifestName)
		dep := filepath.Join(cdir, depsSnapshotName)
		if fi, err := os.Stat(bin); err == nil {
			fmt.Printf("Binary:        present (%d bytes)\n", fi.Size())
		} else {
			fmt.Println("Binary:        (missing)")
		}
		if m, err := readManifest(man); err == nil {
			fmt.Println("  source_mtime:", time.Unix(m.SourceMTime, 0))
			fmt.Println("  flags:       ", strings.Join(m.Flags, " "))
		}
		if fileExists(dep) {
			fmt.Println("Deps:          present")
			if showDeps {
				if sdp, e := readDepsSnapshot(dep); e == nil {
					if sdp.Fb != nil {
						fmt.Printf("  fallback root=%s files=%d newest=%s\n",
							sdp.Fb.Root, sdp.Fb.FileCount, time.Unix(sdp.Fb.MaxMTime, 0).Format(time.RFC3339))
					} else {
						fmt.Printf("  packages=%d\n", len(sdp.Deps))
					}
				}
			}
		}
		if verbose {
			fmt.Println("Cache Dir:     ", cdir)
			fmt.Println("Binary Path:   ", bin)
			fmt.Println("Manifest Path: ", man)
			fmt.Println("Deps Path:     ", dep)
		}
		fmt.Println()
	}
	if len(hits) == 0 {
		fmt.Println("ls --all: no cached scripts for this user")
	}
}

func CmdLs(args []string) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	all := fs.Bool("all", FalseDefault(), "list the entire cache tree for the current user")
	verbose := fs.Bool("verbose", FalseDefault(), "show cache paths, env, and rebuild reasoning")
	depsFlag := fs.Bool("deps", FalseDefault(), "dump dependency list for each script")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()

	cwd, _ := os.Getwd()
	gl := loadGlobalConfigs(cwd, loadLenient)
	for _, w := range gl.Warns {
		warnf("ls: %s", w)
	}
	var merged = mergeConfig(gl.Configs, Config{}, cwd)
	cb := resolveCacheBase(merged.Global)

	if *all {
		listAllCache(cb, *verbose, *depsFlag)
		return 0
	}

	var targets []string
	if len(rest) == 0 {
		files, _ := filepath.Glob("*.go")
		targets = files
	} else {
		targets = rest
	}
	if len(targets) == 0 {
		fmt.Println("ls: no .go files in current directory")
		return 0
	}
	sort.Strings(targets)
	for _, f := range targets {
		abs, _ := filepath.Abs(f)
		lc, lwarns, _ := loadLocalConfig(abs+".toml", loadLenient)
		for _, w := range lwarns {
			warnf("ls: %s", w)
		}
		mc := mergeConfig(gl.Configs, lc, filepath.Dir(abs))
		cb = resolveCacheBase(mc.Global)
		printDescForScript(f, cb, mc, *verbose, *depsFlag)
	}
	return 0
}
