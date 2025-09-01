package goscripter

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type goEnvInfo struct {
	GOVERSION string `json:"GOVERSION"`
	GOOS      string `json:"GOOS"`
	GOARCH    string `json:"GOARCH"`
	GOROOT    string `json:"GOROOT"`
}

func getGoEnv(env mergedEnv) (goEnvInfo, error) {
	var info goEnvInfo
	cmd := exec.Command("go", "env", "-json")
	envList := os.Environ()
	set := func(k, v string) {
		found := false
		for i := range envList {
			if strings.HasPrefix(envList[i], k+"=") {
				envList[i] = k + "=" + v
				found = true
				break
			}
		}
		if !found {
			envList = append(envList, k+"="+v)
		}
	}
	set("GO111MODULE", env.GO111MODULE)
	set("GOPATH", strings.Join(env.GOPATH, string(os.PathListSeparator)))
	cmd.Env = envList
	out, err := cmd.Output()
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return info, err
	}
	return info, nil
}

func gatherFilesFor(pkg listPkg) []string {
	var out []string
	add := func(xs []string) {
		for _, f := range xs {
			if f == "" {
				continue
			}
			out = append(out, filepath.Join(pkg.Dir, f))
		}
	}
	add(pkg.GoFiles)
	add(pkg.CgoFiles)
	add(pkg.CFiles)
	add(pkg.HFiles)
	add(pkg.SFiles)
	add(pkg.SysoFiles)
	add(pkg.OtherFiles)
	return out
}

func goListDeps(cacheDir string, env mergedEnv) ([]DepEntry, error) {
	cmd := exec.Command("go", "list", "-deps", "-json", ".")
	cmd.Dir = cacheDir
	envList := os.Environ()
	set := func(k, v string) {
		found := false
		for i := range envList {
			if strings.HasPrefix(envList[i], k+"=") {
				envList[i] = k + "=" + v
				found = true
				break
			}
		}
		if !found {
			envList = append(envList, k+"="+v)
		}
	}
	set("GO111MODULE", env.GO111MODULE)
	set("GOPATH", strings.Join(env.GOPATH, string(os.PathListSeparator)))
	cmd.Env = envList

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { _ = cmd.Wait() }()

	dec := json.NewDecoder(stdout)
	var deps []DepEntry
	seen := map[string]bool{}
	for {
		var p listPkg
		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, os.ErrClosed) || err.Error() == "EOF" {
				break
			}
			return nil, err
		}
		if p.Standard {
			continue
		}
		if p.ImportPath == "" || p.Dir == "" {
			continue
		}
		if seen[p.ImportPath] {
			continue
		}
		seen[p.ImportPath] = true

		files := gatherFilesFor(p)
		var max int64
		count := 0
		for _, fp := range files {
			if st, e := os.Stat(fp); e == nil {
				count++
				mt := st.ModTime().Unix()
				if mt > max {
					max = mt
				}
			}
		}
		deps = append(deps, DepEntry{
			ImportPath: p.ImportPath,
			Dir:        resolveSymlinkDir(p.Dir),
			MaxMTime:   max,
			FileCount:  count,
		})
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].ImportPath < deps[j].ImportPath })
	return deps, nil
}

func resolveSymlinkDir(d string) string {
	if r, err := filepath.EvalSymlinks(d); err == nil {
		return r
	}
	return d
}

func fallbackScan(scriptDir string) (FallbackRec, error) {
	root := filepath.Join(scriptDir, "src")
	var fb FallbackRec
	fb.Root = root
	var max int64
	count := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !(strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".c") || strings.HasSuffix(p, ".h") || strings.HasSuffix(p, ".s")) {
			return nil
		}
		if st, e := os.Stat(p); e == nil {
			count++
			mt := st.ModTime().Unix()
			if mt > max {
				max = mt
			}
		}
		return nil
	})
	fb.MaxMTime = max
	fb.FileCount = count
	return fb, nil
}

func writeDepsSnapshot(cacheDir string, env mergedEnv, flags []string, scriptDir string) error {
	info, _ := getGoEnv(env)

	var snap DepsSnapshot
	snap.Meta.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	snap.Meta.GoVersion = info.GOVERSION
	snap.Meta.GOOS = info.GOOS
	snap.Meta.GOARCH = info.GOARCH
	snap.Meta.GOROOT = info.GOROOT
	snap.Meta.GO111MODULE = env.GO111MODULE
	snap.Meta.GOPATH = append([]string{}, env.GOPATH...)
	snap.Meta.Flags = append([]string{}, flags...)

	if deps, e := goListDeps(cacheDir, env); e == nil && len(deps) > 0 {
		snap.Deps = deps
	} else {
		for _, gp := range env.GOPATH {
			if filepath.Clean(gp) == filepath.Clean(scriptDir) {
				if fb, fe := fallbackScan(scriptDir); fe == nil {
					snap.Fb = &fb
				}
				break
			}
		}
	}

	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(snap); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cacheDir, depsSnapshotName), []byte(buf.String()), 0o644)
}

func readDepsSnapshot(p string) (DepsSnapshot, error) {
	var s DepsSnapshot
	if !fileExists(p) {
		return s, os.ErrNotExist
	}
	_, err := toml.DecodeFile(p, &s)
	return s, err
}

func compareToolchain(old DepsSnapshot, cur DepsSnapshot) (changed bool, reasons []string) {
	check := func(name, a, b string) {
		if a != b {
			changed = true
			reasons = append(reasons, "toolchain "+name+" changed: "+a+" -> "+b)
		}
	}
	check("GOVERSION", old.Meta.GoVersion, cur.Meta.GoVersion)
	check("GOOS", old.Meta.GOOS, cur.Meta.GOOS)
	check("GOARCH", old.Meta.GOARCH, cur.Meta.GOARCH)
	check("GOROOT", old.Meta.GOROOT, cur.Meta.GOROOT)
	return
}

func depsMap(xs []DepEntry) map[string]DepEntry {
	m := make(map[string]DepEntry, len(xs))
	for _, d := range xs {
		m[d.ImportPath] = d
	}
	return m
}

func compareDeps(old DepsSnapshot, cur DepsSnapshot) (changed bool, reasons []string) {
	if old.Fb != nil || cur.Fb != nil {
		if (old.Fb == nil) != (cur.Fb == nil) {
			return true, append(reasons, "deps discovery mode changed (fallback vs go list)")
		}
		if old.Fb.Root != cur.Fb.Root {
			return true, append(reasons, "fallback root changed")
		}
		if cur.Fb.MaxMTime > old.Fb.MaxMTime {
			return true, append(reasons, "fallback mtime increased")
		}
		return false, reasons
	}

	oldm := depsMap(old.Deps)
	curm := depsMap(cur.Deps)

	for k := range curm {
		if _, ok := oldm[k]; !ok {
			changed = true
			reasons = append(reasons, "dep set changed: +"+k)
		}
	}
	for k := range oldm {
		if _, ok := curm[k]; !ok {
			changed = true
			reasons = append(reasons, "dep set changed: -"+k)
		}
	}
	for k, nv := range curm {
		if ov, ok := oldm[k]; ok {
			if nv.MaxMTime > ov.MaxMTime {
				changed = true
				reasons = append(reasons, "dep changed: "+k)
			}
		}
	}
	return
}

func currentDepsSnapshot(cacheDir string, env mergedEnv, flags []string, scriptDir string) DepsSnapshot {
	info, _ := getGoEnv(env)
	var cur DepsSnapshot
	cur.Meta.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	cur.Meta.GoVersion = info.GOVERSION
	cur.Meta.GOOS = info.GOOS
	cur.Meta.GOARCH = info.GOARCH
	cur.Meta.GOROOT = info.GOROOT
	cur.Meta.GO111MODULE = env.GO111MODULE
	cur.Meta.GOPATH = append([]string{}, env.GOPATH...)
	cur.Meta.Flags = append([]string{}, flags...)

	if deps, e := goListDeps(cacheDir, env); e == nil && len(deps) > 0 {
		cur.Deps = deps
	} else {
		for _, gp := range env.GOPATH {
			if filepath.Clean(gp) == filepath.Clean(scriptDir) {
				if fb, fe := fallbackScan(scriptDir); fe == nil {
					cur.Fb = &fb
				}
				break
			}
		}
	}
	return cur
}
