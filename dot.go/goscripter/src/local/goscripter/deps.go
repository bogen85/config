package goscripter

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

func readDepsSnapshot(path string) (DepsSnapshot, error) {
	var s DepsSnapshot
	_, err := toml.DecodeFile(path, &s)
	return s, err
}

func writeDepsSnapshot(cacheDir string, env mergedEnv, flags []string, scriptDir string) error {
	s := currentDepsSnapshot(cacheDir, env, flags, scriptDir)
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(&s); err != nil {
		return err
	}
	out := filepath.Join(cacheDir, depsSnapshotName)
	return os.WriteFile(out, buf.Bytes(), 0o644)
}

func currentDepsSnapshot(cacheDir string, env mergedEnv, flags []string, scriptDir string) DepsSnapshot {
	meta := struct {
		GoVersion string
		GOOS      string
		GOARCH    string
		GOROOT    string
	}{}
	if out, err := runGoJSON(cacheDir, env, []string{"env", "-json"}); err == nil {
		_ = json.Unmarshal(out, &meta)
	}
	var s DepsSnapshot
	s.Meta.GeneratedAt = time.Now().Format(time.RFC3339)
	s.Meta.GoVersion = meta.GoVersion
	s.Meta.GOOS = meta.GOOS
	s.Meta.GOARCH = meta.GOARCH
	s.Meta.GOROOT = meta.GOROOT
	s.Meta.GO111MODULE = env.GO111MODULE
	s.Meta.GOPATH = append([]string{}, env.GOPATH...)
	s.Meta.Flags = append([]string{}, flags...)

	self := selfAbsPath()
	var mt int64
	if fi, err := os.Stat(self); err == nil {
		mt = fi.ModTime().Unix()
	}
	s.Meta.GoscripterPath = self
	s.Meta.GoscripterMTime = mt
	s.Meta.SnapshotFormat = 1

	pkgs := listDeps(cacheDir, env)
	for _, p := range pkgs {
		if p.Standard || p.Dir == "" {
			continue
		}
		max, cnt := maxTimeAndCount(p.Dir)
		s.Deps = append(s.Deps, DepEntry{ImportPath: p.ImportPath, Dir: p.Dir, MaxMTime: max, FileCount: cnt})
	}
	if len(s.Deps) == 0 {
		max, cnt := maxTimeAndCount(scriptDir)
		s.Fb = &FallbackRec{Root: scriptDir, MaxMTime: max, FileCount: cnt}
	}
	return s
}

func runGoJSON(workdir string, env mergedEnv, args []string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(),
		"GO111MODULE="+env.GO111MODULE,
		"GOPATH="+strings.Join(env.GOPATH, string(os.PathListSeparator)),
	)
	return cmd.Output()
}

func listDeps(workdir string, env mergedEnv) []listPkg {
	cmd := exec.Command("go", "list", "-deps", "-json", ".")
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(),
		"GO111MODULE="+env.GO111MODULE,
		"GOPATH="+strings.Join(env.GOPATH, string(os.PathListSeparator)),
	)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var res []listPkg
	for {
		var p listPkg
		if err := dec.Decode(&p); err != nil {
			break
		}
		res = append(res, p)
	}
	return res
}

func maxTimeAndCount(dir string) (int64, int) {
	var max int64
	count := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(p) != ".go" {
			return nil
		}
		count++
		if fi, e := os.Stat(p); e == nil {
			if t := fi.ModTime().Unix(); t > max {
				max = t
			}
		}
		return nil
	})
	return max, count
}

func compareToolchain(old, cur DepsSnapshot) (changed bool, reasons []string) {
	if old.Meta.GoVersion != cur.Meta.GoVersion {
		changed = true
		reasons = append(reasons, "Go version changed")
	}
	if old.Meta.GOOS != cur.Meta.GOOS || old.Meta.GOARCH != cur.Meta.GOARCH {
		changed = true
		reasons = append(reasons, "GOOS/GOARCH changed")
	}
	if old.Meta.GOROOT != cur.Meta.GOROOT {
		changed = true
		reasons = append(reasons, "GOROOT changed")
	}
	if old.Meta.GO111MODULE != cur.Meta.GO111MODULE {
		changed = true
		reasons = append(reasons, "GO111MODULE changed")
	}
	if !sliceEqual(old.Meta.GOPATH, cur.Meta.GOPATH) {
		changed = true
		reasons = append(reasons, "GOPATH changed")
	}
	if !flagsEqual(old.Meta.Flags, cur.Meta.Flags) {
		changed = true
		reasons = append(reasons, "build flags changed")
	}
	if old.Meta.SnapshotFormat != cur.Meta.SnapshotFormat {
		changed = true
		reasons = append(reasons, "deps snapshot format changed")
	}
	if old.Meta.GoscripterPath != cur.Meta.GoscripterPath || old.Meta.GoscripterMTime != cur.Meta.GoscripterMTime {
		changed = TrueDefault()
		reasons = append(reasons, "goscripter binary changed")
	}
	return
}

func compareDeps(old, cur DepsSnapshot) (changed bool, reasons []string) {
	om := map[string]int64{}
	for _, d := range old.Deps {
		om[d.ImportPath] = d.MaxMTime
	}
	cm := map[string]int64{}
	for _, d := range cur.Deps {
		cm[d.ImportPath] = d.MaxMTime
	}
	if len(om) != len(cm) {
		return true, []string{"dependency set changed"}
	}
	for k, ov := range om {
		if cv, ok := cm[k]; !ok || cv != ov {
			return true, []string{"dependency mtime changed or missing: " + k}
		}
	}
	if (old.Fb == nil) != (cur.Fb == nil) {
		return true, []string{"fallback scan presence changed"}
	}
	if old.Fb != nil && cur.Fb != nil {
		if old.Fb.MaxMTime != cur.Fb.MaxMTime || old.Fb.FileCount != cur.Fb.FileCount {
			return true, []string{"fallback scan changed"}
		}
	}
	return false, nil
}
