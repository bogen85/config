package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

/*
Build:
  go build -o /opt/x.org/bin/goscripter
*/

const (
	manifestName     = "script.toml"
	modifiedSrcName  = "main.go"
	cacheBinName     = "prog"
	depsSnapshotName = "deps.toml"

	defaultGOMODULE = "auto"
	defaultGOPATH   = "/usr/share/gocode"
)

type Config struct {
	Cache struct {
		Root string `toml:"root"`
	} `toml:"cache"`

	Env struct {
		GO111MODULE string      `toml:"GO111MODULE"`
		GOPATH      interface{} `toml:"GOPATH"`
	} `toml:"env"`

	EnvAppend struct {
		GOPATH interface{} `toml:"GOPATH"`
	} `toml:"env_append"`

	Build struct {
		Flags []string `toml:"flags"`
	} `toml:"build"`
}

type Manifest struct {
	SourceMTime    int64    `toml:"source_mtime"`
	Flags          []string `toml:"flags"`
	EnvGO111MODULE string   `toml:"env_go111module"`
	EnvGOPATH      []string `toml:"env_gopath"`
}

type DepsSnapshot struct {
	Meta struct {
		GeneratedAt string   `toml:"generated_at"`
		GoVersion   string   `toml:"goversion"`
		GOOS        string   `toml:"goos"`
		GOARCH      string   `toml:"goarch"`
		GOROOT      string   `toml:"goroot"`
		GO111MODULE string   `toml:"go111module"`
		GOPATH      []string `toml:"gopath"`
		Flags       []string `toml:"flags"`
	} `toml:"meta"`
	Deps []DepEntry   `toml:"dep"`
	Fb   *FallbackRec `toml:"fallback_scan,omitempty"`
}

type DepEntry struct {
	ImportPath string `toml:"import_path"`
	Dir        string `toml:"dir"`
	MaxMTime   int64  `toml:"max_mtime"`
	FileCount  int    `toml:"file_count"`
}

type FallbackRec struct {
	Root      string `toml:"root"`
	MaxMTime  int64  `toml:"max_mtime"`
	FileCount int    `toml:"file_count"`
}

func fatalf(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...); os.Exit(1) }
func warnf(format string, a ...any)  { fmt.Fprintf(os.Stderr, "warn: "+format+"\n", a...) }

// --- paths & identity ---

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
	base         string
	homeStyle    bool
	resolvedRoot string
}

func resolveCacheBase(globalMerged Config) cacheBase {
	if globalMerged.Cache.Root == "" {
		xc := xdgCacheHome()
		if xc == "" {
			fatalf("cannot resolve cache home (no $HOME and no XDG_CACHE_HOME)")
		}
		return cacheBase{base: xc, homeStyle: true, resolvedRoot: filepath.Join(xc, "goscripter")}
	}
	base := filepath.Clean(globalMerged.Cache.Root)
	return cacheBase{base: base, homeStyle: false, resolvedRoot: filepath.Join(base, "goscripter", userName())}
}

func cacheDirFor(cb cacheBase, scriptAbs string) string {
	clean := strings.TrimPrefix(filepath.Clean(scriptAbs), string(filepath.Separator))
	return filepath.Join(cb.resolvedRoot, clean) + string(filepath.Separator)
}
func ensureDir(dir string) error        { return os.MkdirAll(dir, 0o755) }
func userCacheRoot(cb cacheBase) string { return cb.resolvedRoot }

// --- shebang handling ---

type shebangInfo struct {
	hasShebang bool
	line       string
	path       string
	argv       []string
}

func readFirstLine(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func parseShebang(script string) (shebangInfo, error) {
	line, err := readFirstLine(script)
	if err != nil {
		return shebangInfo{}, err
	}
	if !strings.HasPrefix(line, "#!") {
		return shebangInfo{hasShebang: false}, nil
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "#!"))
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return shebangInfo{hasShebang: true, line: line}, nil
	}
	return shebangInfo{hasShebang: true, line: line, path: fields[0], argv: fields[1:]}, nil
}

func isEnvGoscripter(sb shebangInfo) bool {
	if !sb.hasShebang {
		return false
	}
	if !strings.HasSuffix(sb.path, "/usr/bin/env") && sb.path != "env" {
		return false
	}
	return len(sb.argv) > 0 && sb.argv[0] == "goscripter"
}
func sameFile(a, b string) bool {
	ap, _ := filepath.EvalSymlinks(a)
	bp, _ := filepath.EvalSymlinks(b)
	if ap == "" {
		ap = a
	}
	if bp == "" {
		bp = b
	}
	return ap == bp
}
func desiredShebangAbs() string { return "#!" + selfAbsPath() + " run" }
func desiredShebangEnvOrAbsForApply(sb shebangInfo) string {
	if isEnvGoscripter(sb) {
		if ep := lookPathGoscripter(); ep != "" && sameFile(ep, selfAbsPath()) {
			return "#!/usr/bin/env goscripter run"
		}
	}
	return desiredShebangAbs()
}

func writeShebangLinePreserveMode(script, newLine string) (changed bool, err error) {
	info, statErr := os.Stat(script)
	if statErr != nil {
		return false, statErr
	}
	origMode := info.Mode().Perm()

	b, err := os.ReadFile(script)
	if err != nil {
		return false, err
	}
	lines := bytes.Split(b, []byte("\n"))
	if len(lines) == 0 {
		lines = [][]byte{{}}
	}

	current := ""
	if bytes.HasPrefix(lines[0], []byte("#!")) {
		current = string(lines[0])
	}
	if current == newLine {
		return false, nil
	}

	if current != "" {
		lines[0] = []byte(newLine)
	} else {
		lines = append([][]byte{[]byte(newLine)}, lines...)
	}
	tmp := script + ".goscripter.shebang.tmp"
	if err := os.WriteFile(tmp, bytes.Join(lines, []byte("\n")), origMode); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, script); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// --- config loading/validation/merging ---

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func decodeConfigStrict(p string) (Config, error) {
	var cfg Config
	if !fileExists(p) {
		return cfg, os.ErrNotExist
	}
	if _, err := toml.DecodeFile(p, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", p, err)
	}
	return cfg, nil
}

func asStringSlice(x interface{}) []string {
	switch v := x.(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []string:
		return v
	case []interface{}:
		var out []string
		for _, it := range v {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func validateGO111(s string) bool { return s == "" || s == "auto" || s == "on" || s == "off" }
func validateGOPATHList(vals []string) (ok bool, badIdx int, badVal string) {
	for i, v := range vals {
		if v == "." || filepath.IsAbs(v) {
			continue
		}
		return false, i, v
	}
	return true, -1, ""
}

type cfgErr struct{ msg string }

func (e cfgErr) Error() string { return e.msg }

func validateConfig(c Config, path string) []error {
	var errs []error
	if !validateGO111(c.Env.GO111MODULE) {
		errs = append(errs, cfgErr{fmt.Sprintf("%s: [env].GO111MODULE must be one of {auto,on,off}; got %q", path, c.Env.GO111MODULE)})
	}
	if gp := asStringSlice(c.Env.GOPATH); gp != nil {
		if ok, idx, bad := validateGOPATHList(gp); !ok {
			errs = append(errs, cfgErr{fmt.Sprintf("%s: [env].GOPATH[%d] = %q is invalid; use absolute paths or \".\"", path, idx, bad)})
		}
	}
	if gp := asStringSlice(c.EnvAppend.GOPATH); gp != nil {
		if ok, idx, bad := validateGOPATHList(gp); !ok {
			errs = append(errs, cfgErr{fmt.Sprintf("%s: [env_append].GOPATH[%d] = %q is invalid; use absolute paths or \".\"", path, idx, bad)})
		}
	}
	return errs
}

type mergedEnv struct {
	GO111MODULE string
	GOPATH      []string
}
type mergedConfig struct {
	env    mergedEnv
	flags  []string
	global Config
}

func mergeConfig(globOrdered []Config, local Config, scriptDir string) mergedConfig {
	m := mergedConfig{
		env: mergedEnv{
			GO111MODULE: defaultGOMODULE,
			GOPATH:      []string{defaultGOPATH},
		},
	}
	apply := func(c Config) {
		if c.Cache.Root != "" {
			m.global.Cache.Root = c.Cache.Root
		}
		if c.Env.GO111MODULE != "" {
			m.env.GO111MODULE = c.Env.GO111MODULE
		}
		if gp := asStringSlice(c.Env.GOPATH); gp != nil {
			m.env.GOPATH = gp
		}
		if gp := asStringSlice(c.EnvAppend.GOPATH); gp != nil {
			m.env.GOPATH = append(m.env.GOPATH, gp...)
		}
		if len(c.Build.Flags) > 0 {
			m.flags = append(m.flags, c.Build.Flags...)
		}
	}
	for _, g := range globOrdered {
		apply(g)
	}
	apply(local)
	for i := range m.env.GOPATH {
		if m.env.GOPATH[i] == "." {
			m.env.GOPATH[i] = scriptDir
		}
	}
	seen := map[string]bool{}
	out := m.env.GOPATH[:0]
	for _, g := range m.env.GOPATH {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	m.env.GOPATH = out
	return m
}

type loadMode int

const (
	loadStrict loadMode = iota
	loadLenient
)

type cfgLoad struct {
	configs []Config
	warns   []string
	errs    []error
}

func loadGlobalConfigs(cwd string, mode loadMode) cfgLoad {
	paths := []string{
		"/etc/goscripter.toml",
		"/usr/local/etc/goscripter.toml",
		filepath.Join(cwd, "goscripter.toml"),
		filepath.Join(homeDir(), ".config", "goscripter", "config.toml"),
	}
	var out cfgLoad
	for _, p := range paths {
		if p == "" || !fileExists(p) {
			continue
		}
		c, err := decodeConfigStrict(p)
		if err != nil {
			if mode == loadStrict {
				out.errs = append(out.errs, err)
			} else {
				out.warns = append(out.warns, "parse error: "+err.Error())
			}
			continue
		}
		if verrs := validateConfig(c, p); len(verrs) > 0 {
			if mode == loadStrict {
				out.errs = append(out.errs, verrs...)
			} else {
				for _, e := range verrs {
					out.warns = append(out.warns, e.Error())
				}
			}
			continue
		}
		out.configs = append(out.configs, c)
	}
	return out
}

func loadLocalConfig(path string, mode loadMode) (Config, []string, []error) {
	//var warns, errsOut []string
	var warns []string
	var errs []error
	if !fileExists(path) {
		return Config{}, warns, errs
	}
	c, err := decodeConfigStrict(path)
	if err != nil {
		if mode == loadStrict {
			errs = append(errs, err)
		} else {
			warns = append(warns, "parse error: "+err.Error())
		}
		return Config{}, warns, errs
	}
	verrs := validateConfig(c, path)
	if len(verrs) > 0 {
		if mode == loadStrict {
			errs = append(errs, verrs...)
			return Config{}, warns, errs
		}
		for _, e := range verrs {
			warns = append(warns, e.Error())
		}
	}
	return c, warns, errs
}

// --- manifest & build ---

func readManifest(p string) (Manifest, error) {
	var m Manifest
	if !fileExists(p) {
		return m, os.ErrNotExist
	}
	_, err := toml.DecodeFile(p, &m)
	return m, err
}
func writeManifest(p string, m Manifest) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	return os.WriteFile(p, buf.Bytes(), 0o644)
}
func mtimeUnix(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.ModTime().Unix()
}
func flagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- toolchain & deps (GOPATH mode only) ---

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

type listPkg struct {
	ImportPath string   `json:"ImportPath"`
	Dir        string   `json:"Dir"`
	Standard   bool     `json:"Standard"`
	GoFiles    []string `json:"GoFiles"`
	CgoFiles   []string `json:"CgoFiles"`
	CFiles     []string `json:"CFiles"`
	HFiles     []string `json:"HFiles"`
	SFiles     []string `json:"SFiles"`
	SysoFiles  []string `json:"SysoFiles"`
	OtherFiles []string `json:"OtherFiles"`
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
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if p.Standard || p.ImportPath == "" || p.Dir == "" {
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
		// fallback only if GOPATH included scriptDir
		for _, gp := range env.GOPATH {
			if filepath.Clean(gp) == filepath.Clean(scriptDir) {
				if fb, fe := fallbackScan(scriptDir); fe == nil {
					snap.Fb = &fb
				}
				break
			}
		}
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(snap); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cacheDir, depsSnapshotName), buf.Bytes(), 0o644)
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
			reasons = append(reasons, fmt.Sprintf("toolchain %s changed: %s -> %s", name, a, b))
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
			return true, append(reasons, fmt.Sprintf("fallback mtime increased: %d -> %d", old.Fb.MaxMTime, cur.Fb.MaxMTime))
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
				oldT := time.Unix(ov.MaxMTime, 0).Format(time.RFC3339)
				newT := time.Unix(nv.MaxMTime, 0).Format(time.RFC3339)
				reasons = append(reasons, fmt.Sprintf("dep changed: %s (%s -> %s)", k, oldT, newT))
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

// --- cache analysis, build, run ---

type cacheDecision struct {
	rebuild   bool
	reasons   []string
	man       Manifest
	binOK     bool
	binMTime  time.Time
	cacheDir  string
	buildCmd  string
	buildEnvM string
	buildEnvP string
}

func analyzeCache(scriptAbs, cacheDir string, flags []string, env mergedEnv) cacheDecision {
	manPath := filepath.Join(cacheDir, manifestName)
	binPath := filepath.Join(cacheDir, cacheBinName)
	m, err := readManifest(manPath)
	dec := cacheDecision{rebuild: false, reasons: []string{}, man: m, cacheDir: cacheDir}
	dec.buildCmd = fmt.Sprintf("go build -C %s -o %s %s", cacheDir, cacheBinName, strings.Join(flags, " "))
	dec.buildEnvM = "GO111MODULE=" + env.GO111MODULE
	dec.buildEnvP = "GOPATH=" + strings.Join(env.GOPATH, string(os.PathListSeparator))

	if fi, e := os.Stat(binPath); e == nil {
		dec.binOK = true
		dec.binMTime = fi.ModTime()
	}

	if err != nil {
		dec.rebuild = true
		dec.reasons = append(dec.reasons, "manifest missing")
	}
	if !dec.binOK {
		dec.rebuild = true
		dec.reasons = append(dec.reasons, "binary missing")
	}

	if err == nil && m.SourceMTime != mtimeUnix(scriptAbs) {
		dec.rebuild = true
		old := time.Unix(m.SourceMTime, 0).Format(time.RFC3339)
		new := time.Unix(mtimeUnix(scriptAbs), 0).Format(time.RFC3339)
		dec.reasons = append(dec.reasons, "source mtime changed: "+old+" -> "+new)
	}

	if err == nil && !flagsEqual(m.Flags, flags) {
		dec.rebuild = true
		dec.reasons = append(dec.reasons, "build flags changed")
	}

	if err == nil {
		if m.EnvGO111MODULE != env.GO111MODULE {
			dec.rebuild = true
			dec.reasons = append(dec.reasons, "GO111MODULE changed: "+m.EnvGO111MODULE+" -> "+env.GO111MODULE)
		}
		if !sliceEqual(m.EnvGOPATH, env.GOPATH) {
			dec.rebuild = true
			dec.reasons = append(dec.reasons, "GOPATH changed")
		}
	} else {
		dec.reasons = append(dec.reasons, "env not recorded (first build)")
	}

	// Deps & toolchain comparison when we have a prior snapshot
	depsPath := filepath.Join(cacheDir, depsSnapshotName)
	if fileExists(depsPath) && fileExists(filepath.Join(cacheDir, modifiedSrcName)) {
		oldSnap, e1 := readDepsSnapshot(depsPath)
		if e1 == nil {
			curSnap := currentDepsSnapshot(cacheDir, env, flags, filepath.Dir(scriptAbs))
			if ch, rs := compareToolchain(oldSnap, curSnap); ch {
				dec.rebuild = true
				dec.reasons = append(dec.reasons, rs...)
			}
			if ch, rs := compareDeps(oldSnap, curSnap); ch {
				dec.rebuild = true
				dec.reasons = append(dec.reasons, rs...)
			}
		}
	}

	return dec
}

func produceModifiedSource(scriptAbs, outPath string) error {
	b, err := os.ReadFile(scriptAbs)
	if err != nil {
		return err
	}
	raw := b
	if bytes.HasPrefix(raw, []byte("#!")) {
		if idx := bytes.IndexByte(raw, '\n'); idx >= 0 {
			raw = raw[idx+1:]
		} else {
			raw = []byte{}
		}
	}
	var out bytes.Buffer
	out.WriteString("// Code generated by goscripter; DO NOT EDIT.\n")
	out.WriteString("//line " + scriptAbs + ":2\n")
	out.Write(raw)
	return os.WriteFile(outPath, out.Bytes(), 0o644)
}
func goBuild(cacheDir string, flags []string, env mergedEnv) error {
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

	args := []string{"build", "-C", cacheDir, "-o", cacheBinName}
	args = append(args, flags...)
	cmd := exec.Command("go", args...)
	cmd.Env = envList
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// refreshCache now also generates deps.toml on cache hits if missing
func refreshCache(op string, scriptAbs string, cb cacheBase, flags []string, env mergedEnv, verbose bool) (cacheDecision, error) {
	cdir := cacheDirFor(cb, scriptAbs)
	if err := ensureDir(cdir); err != nil {
		return cacheDecision{}, err
	}
	dec := analyzeCache(scriptAbs, cdir, flags, env)
	if dec.rebuild {
		if verbose {
			fmt.Printf("%s: rebuild needed:\n", op)
			for _, r := range dec.reasons {
				fmt.Println("  -", r)
			}
			fmt.Printf("%s: build dir: %s\n", op, dec.cacheDir)
			fmt.Printf("%s: build env: %s\n", op, dec.buildEnvM)
			fmt.Printf("%s: build env: %s\n", op, dec.buildEnvP)
			fmt.Printf("%s: build cmd: %s\n", op, dec.buildCmd)
		}
		if err := produceModifiedSource(scriptAbs, filepath.Join(cdir, modifiedSrcName)); err != nil {
			return dec, fmt.Errorf("write modified source: %w", err)
		}
		if err := goBuild(cdir, flags, env); err != nil {
			return dec, fmt.Errorf("build failed: %w", err)
		}
		m := Manifest{
			SourceMTime:    mtimeUnix(scriptAbs),
			Flags:          append([]string{}, flags...),
			EnvGO111MODULE: env.GO111MODULE,
			EnvGOPATH:      append([]string{}, env.GOPATH...),
		}
		if err := writeManifest(filepath.Join(cdir, manifestName), m); err != nil && verbose {
			warnf("write manifest: %v", err)
		}
		if err := writeDepsSnapshot(cdir, env, flags, filepath.Dir(scriptAbs)); err != nil && verbose {
			warnf("write deps: %v", err)
		}
		dec = analyzeCache(scriptAbs, cdir, flags, env)
		if verbose {
			fmt.Printf("%s: cache rebuilt\n", op)
		}
	} else {
		// cache hit: if deps snapshot is missing, generate it now
		depsPath := filepath.Join(cdir, depsSnapshotName)
		if !fileExists(depsPath) {
			if err := writeDepsSnapshot(cdir, env, flags, filepath.Dir(scriptAbs)); err != nil {
				if verbose {
					warnf("%s: deps snapshot missing; generation failed: %v", op, err)
				}
			} else if verbose {
				fmt.Printf("%s: deps snapshot missing; generated now\n", op)
			}
		}
		if verbose {
			fmt.Printf("%s: using cached binary\n", op)
			if dec.binOK {
				fmt.Printf("%s: cached binary mtime: %s\n", op, dec.binMTime.Format(time.RFC3339))
			}
		}
	}
	return dec, nil
}

func runFromCache(scriptAbs string, cb cacheBase, argv []string) int {
	cdir := cacheDirFor(cb, scriptAbs)
	exe := filepath.Join(cdir, cacheBinName)
	cmd := exec.Command(exe, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		warnf("run error: %v", err)
		return 1
	}
	return 0
}

// --- human sizes & removal helpers ---

type rmStats struct {
	files int64
	dirs  int64
	bytes int64
}

func humanBytes(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
		TiB = 1024 * GiB
	)
	switch {
	case n >= TiB:
		return fmt.Sprintf("%.2f TiB", float64(n)/float64(TiB))
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func measureTree(root string, verbose bool) (rmStats, error) {
	var st rmStats
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			st.dirs++
			if verbose {
				fmt.Println("rm  dir ", p)
			}
			return nil
		}
		fi, e := d.Info()
		if e == nil {
			st.bytes += fi.Size()
		}
		st.files++
		if verbose {
			if e == nil {
				fmt.Printf("rm  file %s (%s)\n", p, humanBytes(fi.Size()))
			} else {
				fmt.Printf("rm  file %s\n", p)
			}
		}
		return nil
	})
	return st, err
}

func removeTree(root string) error { return os.RemoveAll(root) }

// --- fmt (fixed): format shebang-free body in cache, then write back atomically ---

func runFormatterOn(path string) error {
	if _, err := exec.LookPath("gofmt"); err == nil {
		cmd := exec.Command("gofmt", "-w", path)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	cmd := exec.Command("go", "fmt", path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func fmtOne(cwd string, gl cfgLoad, script string) error {
	abs, err := filepath.Abs(script)
	if err != nil {
		return err
	}
	// local config (lenient for fmt)
	lc, _, _ := loadLocalConfig(abs+".toml", loadLenient)
	mc := mergeConfig(gl.configs, lc, filepath.Dir(abs))
	cb := resolveCacheBase(mc.global)
	cdir := cacheDirFor(cb, abs)
	if err := ensureDir(cdir); err != nil {
		return err
	}

	origInfo, err := os.Stat(abs)
	if err != nil {
		return err
	}
	origMode := origInfo.Mode().Perm()
	content, err := os.ReadFile(abs)
	if err != nil {
		return err
	}

	// split shebang/body
	body := content
	shebang := ""
	if bytes.HasPrefix(body, []byte("#!")) {
		if idx := bytes.IndexByte(body, '\n'); idx >= 0 {
			shebang = string(body[:idx])
			body = body[idx+1:]
		} else {
			// file is only a shebang line; nothing else to format
			body = []byte{}
		}
	}

	// write temp body in cache dir
	tmpBody := filepath.Join(cdir, ".fmt-body.go")
	if err := os.WriteFile(tmpBody, body, 0o644); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpBody) }()

	// run formatter on temp body
	if err := runFormatterOn(tmpBody); err != nil {
		return fmt.Errorf("fmt: formatter failed for %s: %w", script, err)
	}
	formatted, err := os.ReadFile(tmpBody)
	if err != nil {
		return err
	}

	// reassemble with normalized shebang (absolute goscripter path)
	newShebang := desiredShebangAbs()
	var out bytes.Buffer
	out.WriteString(newShebang)
	out.WriteByte('\n')
	out.Write(formatted)

	// if identical to current file (taking into account existing shebang), skip rewrite
	var cur bytes.Buffer
	if shebang == "" {
		// original had no shebang; simulate none
		cur.Write(content)
	} else {
		// normalize current to compare fairly: replace current shebang with desired
		cur.WriteString(newShebang)
		cur.WriteByte('\n')
		cur.Write(body)
	}
	if bytes.Equal(out.Bytes(), cur.Bytes()) {
		return nil // nothing to change; avoid mtime churn
	}

	// atomic replace
	tmp := abs + ".goscripter.fmt.tmp"
	if err := os.WriteFile(tmp, out.Bytes(), origMode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func cmdFmt(args []string) int {
	// targets = args (if none, use *.go in CWD)
	var targets []string
	if len(args) == 0 {
		files, _ := filepath.Glob("*.go")
		targets = files
	} else {
		targets = args
	}
	if len(targets) == 0 {
		fmt.Println("fmt: no .go files in current directory")
		return 0
	}

	cwd, _ := os.Getwd()
	gl := loadGlobalConfigs(cwd, loadLenient)
	for _, t := range targets {
		if err := fmtOne(cwd, gl, t); err != nil {
			warnf("%v", err)
		}
	}
	return 0
}

// --- ls / rm / apply / run / gc (unchanged behavior except deps-gen-on-hit) ---

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

	fmt.Println("GO111MODULE:  ", mc.env.GO111MODULE)
	fmt.Println("GOPATH:       ", strings.Join(mc.env.GOPATH, string(os.PathListSeparator)))
	if len(mc.flags) > 0 {
		fmt.Println("Build Flags:  ", strings.Join(mc.flags, " "))
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
		dec := analyzeCache(abs, cdir, mc.flags, mc.env)
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

func cmdLs(args []string) int {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	all := fs.Bool("all", false, "list the entire cache tree for the current user")
	verbose := fs.Bool("verbose", false, "show cache paths, env, and rebuild reasoning")
	depsFlag := fs.Bool("deps", false, "dump dependency list for each script")
	_ = fs.Parse(args)
	rest := fs.Args()

	cwd, _ := os.Getwd()
	gl := loadGlobalConfigs(cwd, loadLenient)
	for _, w := range gl.warns {
		warnf("ls: %s", w)
	}
	var merged = mergeConfig(gl.configs, Config{}, cwd)
	cb := resolveCacheBase(merged.global)

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
		mc := mergeConfig(gl.configs, lc, filepath.Dir(abs))
		cb = resolveCacheBase(mc.global)
		printDescForScript(f, cb, mc, *verbose, *depsFlag)
	}
	return 0
}

func cmdRm(args []string) int {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	all := fs.Bool("all", false, "remove the entire cache tree for the current user")
	verbose := fs.Bool("verbose", false, "print each file/dir removed and total space freed")
	_ = fs.Parse(args)
	rest := fs.Args()

	cwd, _ := os.Getwd()
	gl := loadGlobalConfigs(cwd, loadStrict)
	if len(gl.errs) > 0 {
		for _, e := range gl.errs {
			fmt.Fprintln(os.Stderr, e.Error())
		}
		return 2
	}
	cb := resolveCacheBase(mergeConfig(gl.configs, Config{}, cwd).global)

	if *all {
		root := userCacheRoot(cb)
		if !fileExists(root) {
			fmt.Println("rm --all:", root, "does not exist")
			return 0
		}
		st, _ := measureTree(root, *verbose)
		if err := removeTree(root); err != nil {
			fatalf("rm --all: %v", err)
		}
		fmt.Printf("Removed: %s (files: %d, dirs: %d)\n", humanBytes(st.bytes), st.files, st.dirs)
		return 0
	}

	if len(rest) != 1 {
		fatalf("rm: script.go required (or use --all)")
	}
	script := rest[0]
	abs, err := filepath.Abs(script)
	if err != nil {
		fatalf("rm: %v", err)
	}
	lc, lwarns, lerrs := loadLocalConfig(abs+".toml", loadStrict)
	for _, w := range lwarns {
		fmt.Fprintln(os.Stderr, w)
	}
	if len(lerrs) > 0 {
		for _, e := range lerrs {
			fmt.Fprintln(os.Stderr, e.Error())
		}
		return 2
	}
	mc := mergeConfig(gl.configs, lc, filepath.Dir(abs))
	cb = resolveCacheBase(mc.global)

	cdir := cacheDirFor(cb, abs)
	if !fileExists(cdir) {
		fmt.Println("Nothing to remove:", cdir)
		return 0
	}
	st, _ := measureTree(cdir, *verbose)
	if err := removeTree(cdir); err != nil {
		fatalf("rm: %v", err)
	}
	fmt.Printf("Removed: %s (files: %d, dirs: %d)\n", humanBytes(st.bytes), st.files, st.dirs)
	return 0
}

// ensureOwnerExec adds u+x if missing. Logs when --verbose is on.
func ensureOwnerExec(path string, verbose bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	if mode&0o100 == 0 {
		if err := os.Chmod(path, mode|0o100); err != nil {
			return err
		}
		if verbose {
			fmt.Printf("apply: chmod u+x %s\n", path)
		}
	}
	return nil
}

// askConfirm prompts the user with a yes/no question.
// If defaultYes is true, ENTER counts as "yes" and the prompt shows [Y/n].
// Otherwise ENTER counts as "no" and the prompt shows [y/N].
// Returns true for yes, false for no.
func askConfirm(prompt string, defaultYes bool) bool {
	suffix := " [y/N]: "
	if defaultYes {
		suffix = " [Y/n]: "
	}
	fmt.Print(prompt, suffix)

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		// conservative: treat I/O error as "no"
		return false
	}
	resp := strings.ToLower(strings.TrimSpace(line))
	switch resp {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	case "":
		return defaultYes
	default:
		// keep it simple: anything else = no
		return false
	}
}

func cmdApply(y bool, script string, verbose bool) int {
	if script == "" {
		fatalf("apply: script.go required")
	}
	abs, err := filepath.Abs(script)
	if err != nil {
		fatalf("apply: %v", err)
	}

	// Shebang normalize (prompt unless -y)
	sb, err := parseShebang(abs)
	if err != nil {
		fatalf("apply: %v", err)
	}
	want := desiredShebangEnvOrAbsForApply(sb)
	needShebang := !sb.hasShebang || sb.line != want
	if needShebang {
		allowed := y || askConfirm(fmt.Sprintf("Add/normalize shebang on %s?", abs) /*defaultYes=*/, false)
		if !allowed {
			if verbose {
				fmt.Println("apply: shebang unchanged (user declined)")
			}
			fmt.Println("apply: skipped")
			return 0
		}
		changed, err := writeShebangLinePreserveMode(abs, want)
		if err != nil {
			fatalf("apply: shebang: %v", err)
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

	// Ensure owner-executable if missing (prompt unless -y)
	info, err := os.Stat(abs)
	if err != nil {
		fatalf("apply: %v", err)
	}
	needsExec := info.Mode().Perm()&0o100 == 0
	if needsExec {
		allowed := y || askConfirm(fmt.Sprintf("Add owner-exec bit (chmod u+x) on %s?", abs) /*defaultYes=*/, false)
		if allowed {
			if err := ensureOwnerExec(abs, verbose); err != nil {
				fatalf("apply: chmod: %v", err)
			}
		} else if verbose {
			fmt.Println("apply: not executable (user declined chmod)")
		}
	}

	// Load configs (strict), merge, refresh cache (no run)
	cwd, _ := os.Getwd()
	gl := loadGlobalConfigs(cwd, loadStrict)
	if len(gl.errs) > 0 {
		for _, e := range gl.errs {
			fmt.Fprintln(os.Stderr, e.Error())
		}
		return 2
	}
	local, lwarns, lerrs := loadLocalConfig(abs+".toml", loadStrict)
	for _, w := range lwarns {
		fmt.Fprintln(os.Stderr, w)
	}
	if len(lerrs) > 0 {
		for _, e := range lerrs {
			fmt.Fprintln(os.Stderr, e.Error())
		}
		return 2
	}
	mc := mergeConfig(gl.configs, local, filepath.Dir(abs))
	cb := resolveCacheBase(mc.global)

	if _, err := refreshCache("apply", abs, cb, mc.flags, mc.env, verbose); err != nil {
		fatalf("apply: %v", err)
	}
	fmt.Println("apply: did not run (use 'goscripter run' to execute)")
	return 0
}

// parse run args: allow --verbose/-v anywhere before optional "--"
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

func cmdRun(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	verbose, script, pass, ok := parseRunArgs(args)
	if !ok {
		usage()
		return 2
	}
	abs, err := filepath.Abs(script)
	if err != nil {
		fatalf("run: %v", err)
	}

	if verbose {
		if sb, e := parseShebang(abs); e == nil {
			if !sb.hasShebang || (!isEnvGoscripter(sb) && !strings.Contains(sb.path, "goscripter")) {
				fmt.Println("run: warning: script does not have a goscripter shebang (fine when invoking 'goscripter run')")
			}
		}
	}

	cwd, _ := os.Getwd()
	gl := loadGlobalConfigs(cwd, loadStrict)
	if len(gl.errs) > 0 {
		for _, e := range gl.errs {
			fmt.Fprintln(os.Stderr, e.Error())
		}
		return 2
	}
	local, lwarns, lerrs := loadLocalConfig(abs+".toml", loadStrict)
	for _, w := range lwarns {
		fmt.Fprintln(os.Stderr, w)
	}
	if len(lerrs) > 0 {
		for _, e := range lerrs {
			fmt.Fprintln(os.Stderr, e.Error())
		}
		return 2
	}
	mc := mergeConfig(gl.configs, local, filepath.Dir(abs))
	cb := resolveCacheBase(mc.global)

	if _, err := refreshCache("run", abs, cb, mc.flags, mc.env, verbose); err != nil {
		fatalf("run: %v", err)
	}
	if verbose {
		cdir := cacheDirFor(cb, abs)
		fmt.Printf("run: exec %s -- %s\n", filepath.Join(cdir, cacheBinName), strings.Join(pass, " "))
	}
	return runFromCache(abs, cb, pass)
}

func cmdGC(args []string) int {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	staleOnly := fs.Bool("stale-only", true, "remove only cache entries whose source script is missing")
	verbose := fs.Bool("verbose", false, "print each file/dir removed and total space freed")
	_ = fs.Parse(args)

	cwd, _ := os.Getwd()
	gl := loadGlobalConfigs(cwd, loadStrict)
	if len(gl.errs) > 0 {
		for _, e := range gl.errs {
			fmt.Fprintln(os.Stderr, e.Error())
		}
		return 2
	}
	cb := resolveCacheBase(mergeConfig(gl.configs, Config{}, cwd).global)

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

// --- usage ---

func usage() {
	exe := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, `goscripter (%s %s)

Usage:
  %s apply [-y] [--verbose|-v] <script.go>   Add/normalize shebang; ensure u+x; refresh cache (no run)
  %s fmt [script.go]                         Format shebang-free body via cache temp; write back w/ normalized shebang
  %s ls [--all] [--verbose|-v] [--deps] [script.go]   Show cache/config (file, CWD, or entire cache)
  %s rm [--all] [--verbose|-v] [script.go]   Remove cache for script, or whole cache tree for user
  %s gc [--stale-only] [--verbose|-v]        Remove stale cache entries (missing source scripts)
  %s run [--verbose|-v] <script.go> [-- args...]  Build if needed and run (verbose must be before "--")
`, runtime.GOOS, runtime.GOARCH, exe, exe, exe, exe, exe, exe)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "fmt":
		os.Exit(cmdFmt(os.Args[2:]))

	case "ls":
		os.Exit(cmdLs(os.Args[2:]))

	case "rm":
		os.Exit(cmdRm(os.Args[2:]))

	case "apply":
		fs := flag.NewFlagSet("apply", flag.ExitOnError)
		autoYes := fs.Bool("y", false, "assume yes; do not prompt")
		verbose := fs.Bool("verbose", false, "verbose output")
		fs.BoolVar(verbose, "v", false, "verbose output (short)")
		_ = fs.Parse(os.Args[2:])
		args := fs.Args()
		if len(args) != 1 {
			usage()
			os.Exit(2)
		}
		os.Exit(cmdApply(*autoYes, args[0], *verbose))

	case "gc":
		os.Exit(cmdGC(os.Args[2:]))

	case "run":
		os.Exit(cmdRun(os.Args[2:]))

	default:
		usage()
		os.Exit(2)
	}
}
