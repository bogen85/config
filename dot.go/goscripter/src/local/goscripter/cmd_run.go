package goscripter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func parseRunArgs(args []string) (verbose bool, script string, pass []string, ok bool) {
	verbose = false
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
			verbose = true
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

func CmdRun(args []string) int {
	if len(args) < 1 {
		eprintf("run: missing arguments")
		return 2
	}
	verbose, script, pass, ok := parseRunArgs(args)
	if !ok {
		eprintf("run: script.go required")
		return 2
	}
	abs, err := filepath.Abs(script)
	if err != nil {
		eprintf("run: %v", err)
		return 2
	}

	// DO NOT modify source here; warn in verbose if non-goscripter shebang
	if verbose {
		if sb, e := parseShebang(abs); e == nil {
			if !sb.hasShebang || (!isEnvGoscripter(sb) && !strings.Contains(sb.path, "goscripter")) {
				fmt.Println("run: warning: script does not have a goscripter shebang (fine when invoking 'goscripter run')")
			}
		}
	}

	// configs (strict)
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

	if _, err := refreshCache("run", abs, cb, mc.Flags, mc.Env, verbose); err != nil {
		eprintf("run: %v", err)
		return 2
	}
	if verbose {
		cdir := cacheDirFor(cb, abs)
		fmt.Printf("run: exec %s -- %s\n", filepath.Join(cdir, cacheBinName), strings.Join(pass, " "))
	}
	return runFromCache(abs, cb, pass)
}
