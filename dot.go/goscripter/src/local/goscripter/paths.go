package goscripter

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func selfAbsPath() string {
	p, err := os.Executable()
	if err != nil {
		return "goscripter"
	}
	ap, err := filepath.EvalSymlinks(p)
	if err == nil {
		return ap
	}
	return p
}
func lookPathGoscripter() string {
	p, err := exec.LookPath("goscripter")
	if err != nil {
		return ""
	}
	ap, err := filepath.EvalSymlinks(p)
	if err == nil {
		return ap
	}
	return p
}
func userName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "unknown"
}
func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
func xdgCacheHome() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	h := homeDir()
	if h == "" {
		return ""
	}
	return filepath.Join(h, ".cache")
}

type cacheBase struct {
	base         string // directory that contains "goscripter" subdir
	homeStyle    bool   // true => ~/.cache style (no $USER component)
	resolvedRoot string // base + "/goscripter" or base+"/goscripter/$USER"
}

func resolveCacheBase(globalMerged Config) cacheBase {
	if globalMerged.Cache.Root == "" {
		xc := xdgCacheHome()
		if xc == "" {
			eprintf("cannot resolve cache home (no $HOME and no XDG_CACHE_HOME)")
			xc = "/tmp"
		}
		return cacheBase{
			base:         xc,
			homeStyle:    true,
			resolvedRoot: filepath.Join(xc, "goscripter"),
		}
	}
	base := filepath.Clean(globalMerged.Cache.Root)
	return cacheBase{
		base:         base,
		homeStyle:    false,
		resolvedRoot: filepath.Join(base, "goscripter", userName()),
	}
}

// cacheDir = <resolvedRoot>/<abs/path/to/script.go>/
func cacheDirFor(cb cacheBase, scriptAbs string) string {
	clean := strings.TrimPrefix(filepath.Clean(scriptAbs), string(filepath.Separator))
	return filepath.Join(cb.resolvedRoot, clean) + string(filepath.Separator)
}
func userCacheRoot(cb cacheBase) string { return cb.resolvedRoot }
