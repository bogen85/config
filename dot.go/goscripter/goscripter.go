package main

import (
	"bufio"
	"bytes"
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

Defaults:
  • Cache: $XDG_CACHE_HOME/goscripter or ~/.cache/goscripter
           layout (home style): ~/.cache/goscripter/<abs/path/to/script.go>/
  • If overridden via [cache].root, layout becomes:
           /<root>/goscripter/$USER/<abs/path/to/script.go>/

Global config search order (later overrides earlier):
  1) /etc/goscripter.toml
  2) /usr/local/etc/goscripter.toml
  3) ./goscripter.toml
  4) ~/.config/goscripter/config.toml

Local (per-script):
  <script.go>.toml   # highest precedence

TOML schema:
  [cache]
  root = "/fast/disk"        # optional

  [env]                      # overrides
  GO111MODULE = "auto"       # must be one of: auto, on, off
  GOPATH = ["/usr/share/gocode", "."]  # string or list; "." expands to script dir

  [env_append]               # appended after overrides
  GOPATH = [".", "/opt/local/go"]

  [build]
  flags = ["-tags=...", "-ldflags=-s -w"]

Shebang policy for `apply`:
  - If current shebang is a different absolute path (not env), rewrite to this binary's absolute path.
  - If current shebang is "#!/usr/bin/env goscripter run":
      keep it ONLY if exec.LookPath("goscripter") resolves to this same binary.
      otherwise rewrite to this binary's absolute path.

Strict config handling:
  • If any consulted config *exists* but fails to parse:
      - run/apply/rm/gc: fail with an error naming the file
      - ls: warn and continue
  • If GOPATH contains invalid entries (only "." or absolute paths allowed) or GO111MODULE ∉ {auto,on,off}:
      - run/apply/rm/gc: fail with an error naming the file/key/index
      - ls: warn and continue

New output labeling:
  • apply --verbose prints only "apply:" lines and ends with "apply: did not run"
  • run   --verbose prints only "run:"   lines
*/

const (
	manifestName    = "script.toml"
	modifiedSrcName = "main.go"
	cacheBinName    = "prog"

	defaultGOMODULE = "auto"
	defaultGOPATH   = "/usr/share/gocode"
)

type Config struct {
	Cache struct {
		Root string `toml:"root"`
	} `toml:"cache"`

	Env struct {
		GO111MODULE string      `toml:"GO111MODULE"`
		GOPATH      interface{} `toml:"GOPATH"` // string or []string
	} `toml:"env"`

	EnvAppend struct {
		GOPATH interface{} `toml:"GOPATH"` // string or []string
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

// cache base resolution
type cacheBase struct {
	base         string // directory that contains "goscripter" subdir
	homeStyle    bool   // true => ~/.cache style (no $USER component)
	resolvedRoot string // base + "/goscripter" or base+"/goscripter/$USER"
}

func resolveCacheBase(globalMerged Config) cacheBase {
	if globalMerged.Cache.Root == "" {
		xc := xdgCacheHome()
		if xc == "" {
			fatalf("cannot resolve cache home (no $HOME and no XDG_CACHE_HOME)")
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
func ensureDir(dir string) error { return os.MkdirAll(dir, 0o755) }

// user cache root for --all
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
	return shebangInfo{
		hasShebang: true,
		line:       line,
		path:       fields[0],
		argv:       fields[1:],
	}, nil
}

func isEnvGoscripter(sb shebangInfo) bool {
	if !sb.hasShebang {
		return false
	}
	if !strings.HasSuffix(sb.path, "/usr/bin/env") && sb.path != "env" {
		return false
	}
	if len(sb.argv) == 0 {
		return false
	}
	return sb.argv[0] == "goscripter"
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

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// decode a config file; return parse error wrapped with path if it exists
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

func validateGO111(s string) bool {
	switch s {
	case "", "auto", "on", "off":
		return true
	default:
		return false
	}
}

func validateGOPATHList(vals []string) (ok bool, badIdx int, badVal string) {
	for i, v := range vals {
		if v == "." {
			continue
		}
		if filepath.IsAbs(v) {
			continue
		}
		return false, i, v
	}
	return true, -1, ""
}

type cfgErr struct{ msg string }

func (e cfgErr) Error() string { return e.msg }

// validate a single Config, returning errors (with file path context provided by caller)
func validateConfig(c Config, path string) []error {
	var errs []error
	// env.GO111MODULE
	if !validateGO111(c.Env.GO111MODULE) {
		errs = append(errs, cfgErr{fmt.Sprintf("%s: [env].GO111MODULE must be one of {auto,on,off}; got %q", path, c.Env.GO111MODULE)})
	}
	// env.GOPATH
	if gp := asStringSlice(c.Env.GOPATH); gp != nil {
		if ok, idx, bad := validateGOPATHList(gp); !ok {
			errs = append(errs, cfgErr{fmt.Sprintf("%s: [env].GOPATH[%d] = %q is invalid; use absolute paths or \".\"", path, idx, bad)})
		}
	}
	// env_append.GOPATH
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
	global Config // includes cache.root
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

	// expand "." to script dir
	for i := range m.env.GOPATH {
		if m.env.GOPATH[i] == "." {
			m.env.GOPATH[i] = scriptDir
		}
	}
	// de-dup preserve order
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
	loadStrict  loadMode = iota // fatal on parse/validation errors
	loadLenient                 // collect parse/validation warnings
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
				continue
			}
			out.warns = append(out.warns, "parse error: "+err.Error())
			continue
		}
		// validate this file
		if verrs := validateConfig(c, p); len(verrs) > 0 {
			if mode == loadStrict {
				for _, e := range verrs {
					out.errs = append(out.errs, e)
				}
				continue
			}
			for _, e := range verrs {
				out.warns = append(out.warns, e.Error())
			}
		}
		out.configs = append(out.configs, c)
	}
	return out
}

func loadLocalConfig(path string, mode loadMode) (Config, []string, []error) {
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

	// Binary status
	if fi, e := os.Stat(binPath); e == nil {
		dec.binOK = true
		dec.binMTime = fi.ModTime()
	}

	// Manifest/binary checks
	if err != nil {
		dec.rebuild = true
		dec.reasons = append(dec.reasons, "manifest missing")
	}
	if !dec.binOK {
		dec.rebuild = true
		dec.reasons = append(dec.reasons, "binary missing")
	}

	// Source mtime
	if err == nil && m.SourceMTime != mtimeUnix(scriptAbs) {
		dec.rebuild = true
		old := time.Unix(m.SourceMTime, 0).Format(time.RFC3339)
		new := time.Unix(mtimeUnix(scriptAbs), 0).Format(time.RFC3339)
		dec.reasons = append(dec.reasons, "source mtime changed: "+old+" -> "+new)
	}

	// Flags
	if err == nil && !flagsEqual(m.Flags, flags) {
		dec.rebuild = true
		dec.reasons = append(dec.reasons, "build flags changed")
	}

	// Env
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

// refresh/build with labeled verbose output
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
		dec = analyzeCache(scriptAbs, cdir, flags, env)
		if verbose {
			fmt.Printf("%s: cache rebuilt\n", op)
		}
	} else if verbose {
		fmt.Printf("%s: using cached binary\n", op)
		if dec.binOK {
			fmt.Printf("%s: cached binary mtime: %s\n", op, dec.binMTime.Format(time.RFC3339))
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

// --- commands ---

func cmdFmt(targets []string) int {
	if len(targets) == 0 {
		files, _ := filepath.Glob("*.go")
		targets = files
	}
	ok := 0
	for _, f := range targets {
		if changed, err := writeShebangLinePreserveMode(f, desiredShebangAbs()); err != nil {
			warnf("fmt: shebang: %s: %v", f, err)
		} else if changed {
			// no chmod; fmt preserves mode
		}
		cmd := exec.Command("go", "fmt", f)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			warnf("fmt: go fmt failed: %s", f)
			continue
		}
		ok++
	}
	if ok == 0 && len(targets) == 0 {
		fmt.Println("fmt: no .go files in current directory")
	}
	return 0
}

func printDescForScript(script string, cb cacheBase, mc mergedConfig, verbose bool) {
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

	if verbose {
		fmt.Println("Cache Dir:     ", cdir)
		fmt.Println("Modified Src:  ", mod)
		fmt.Println("Manifest Path: ", man)
		fmt.Println("Binary Path:   ", bin)
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
	fmt.Println()
}

func listAllCache(cb cacheBase, verbose bool) {
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
		if fi, err := os.Stat(bin); err == nil {
			fmt.Printf("Binary:        present (%d bytes)\n", fi.Size())
		} else {
			fmt.Println("Binary:        (missing)")
		}
		if m, err := readManifest(man); err == nil {
			fmt.Println("  source_mtime:", time.Unix(m.SourceMTime, 0))
			fmt.Println("  flags:       ", strings.Join(m.Flags, " "))
		}
		if verbose {
			fmt.Println("Cache Dir:     ", cdir)
			fmt.Println("Binary Path:   ", bin)
			fmt.Println("Manifest Path: ", man)
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
		listAllCache(cb, *verbose)
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
		printDescForScript(f, cb, mc, *verbose)
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

func cmdApply(y bool, script string, verbose bool) int {
	if script == "" {
		fatalf("apply: script.go required")
	}
	abs, err := filepath.Abs(script)
	if err != nil {
		fatalf("apply: %v", err)
	}

	sb, err := parseShebang(abs)
	if err != nil {
		fatalf("apply: %v", err)
	}
	want := desiredShebangEnvOrAbsForApply(sb)
	needChange := !sb.hasShebang || sb.line != want
	if needChange {
		if !y {
			fmt.Printf("Add/normalize shebang to %s? [y/N]: ", abs)
			var resp string
			fmt.Scanln(&resp)
			if strings.ToLower(strings.TrimSpace(resp)) != "y" {
				fmt.Println("apply: skipped")
				return 0
			}
		}
		changed, err := writeShebangLinePreserveMode(abs, want)
		if err != nil {
			fatalf("apply: shebang: %v", err)
		}
		if changed {
			// ensure u+x
			info, e := os.Stat(abs)
			if e == nil {
				mode := info.Mode().Perm()
				newMode := mode | 0o100
				if newMode != mode {
					if e2 := os.Chmod(abs, newMode); e2 != nil {
						fatalf("apply: chmod: %v", e2)
					}
					if verbose {
						fmt.Printf("apply: chmod u+x %s\n", abs)
					}
				}
			}
			if verbose {
				fmt.Printf("apply: shebang updated\n")
			}
		} else if verbose {
			fmt.Printf("apply: shebang already correct\n")
		}
	} else if verbose {
		fmt.Printf("apply: shebang already correct\n")
	}

	// configs
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

	// refresh cache (no run)
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

	// DO NOT modify source here; just warn in verbose if non-goscripter shebang
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

	// build (if needed) & run
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
  %s fmt [script.go]                         Normalize shebang to absolute path + go fmt (file or all *.go)
  %s ls [--all] [--verbose|-v] [script.go]   Show cache/config (file, all *.go in CWD, or entire cache)
  %s rm [--all] [--verbose|-v] [script.go]   Remove cache for script, or whole cache tree for user
  %s gc [--stale-only] [--verbose|-v]        Remove stale cache entries (missing source scripts)
  %s run [--verbose|-v] <script.go> [-- args...]  Build if needed and run (verbose must be before "--")

Cache:
  Default home style: $XDG_CACHE_HOME/goscripter or ~/.cache/goscripter
  Override via TOML [cache].root -> /<root>/goscripter/$USER/...

Global config search order:
  /etc/goscripter.toml
  /usr/local/etc/goscripter.toml
  ./goscripter.toml
  ~/.config/goscripter/config.toml
Local per-script:
  <script.go>.toml
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
