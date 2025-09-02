package goscripter

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func eprintf(format string, args ...interface{}) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
func warnf(format string, args ...interface{})   { fmt.Fprintf(os.Stderr, "warn: "+format+"\n", args...) }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
func ensureDir(p string) error { return os.MkdirAll(p, 0o755) }

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/"
}

type cacheBase struct{ Root string }

func resolveCacheBase(cfg Config) cacheBase {
	root := filepath.Join(homeDir(), ".cache", "goscripter")
	if cfg.Cache.Root != "" {
		user := os.Getenv("USER")
		if user == "" {
			user = "user"
		}
		root = filepath.Join(cfg.Cache.Root, "goscripter", user)
	}
	return cacheBase{Root: root}
}

func userCacheRoot(cb cacheBase) string { return cb.Root }

func cacheDirFor(cb cacheBase, scriptAbs string) string {
	s := filepath.Clean(scriptAbs)
	if filepath.IsAbs(s) {
		s = s[1:]
	}
	return filepath.Join(userCacheRoot(cb), s)
}

// shebang parsing -------------------------------------------------------------
type shebangInfo struct {
	hasShebang bool
	line       string
	path       string
	args       []string
}

func parseShebang(abs string) (shebangInfo, error) {
	f, err := os.Open(abs)
	if err != nil {
		return shebangInfo{}, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	b, _, err := r.ReadLine()
	if err != nil {
		return shebangInfo{hasShebang: false}, nil
	}
	line := string(b)
	if !strings.HasPrefix(line, "#!") {
		return shebangInfo{hasShebang: false}, nil
	}
	rest := strings.TrimSpace(line[2:])
	parts := strings.Fields(rest)
	if len(parts) == 0 {
		return shebangInfo{hasShebang: true, line: line}, nil
	}
	return shebangInfo{hasShebang: true, line: line, path: parts[0], args: parts[1:]}, nil
}

func isEnvGoscripter(sb shebangInfo) bool {
	if !sb.hasShebang {
		return false
	}
	if filepath.Base(sb.path) != "env" {
		return false
	}
	return len(sb.args) > 0 && sb.args[0] == "goscripter"
}

func selfAbsPath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	return p
}

func lookPathGoscripter() string {
	if p, err := exec.LookPath("goscripter"); err == nil {
		if rp, err := filepath.EvalSymlinks(p); err == nil {
			return rp
		}
		return p
	}
	return ""
}

func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func desiredShebangAbs() string {
	self := selfAbsPath()
	if self == "" {
		if runtime.GOOS == "linux" {
			self = "/opt/x.org/bin/goscripter"
		} else {
			self = "/usr/local/bin/goscripter"
		}
	}
	return "#!" + self + " run"
}

func desiredShebangEnvOrAbsForApply(sb shebangInfo) string {
	if isEnvGoscripter(sb) {
		envPath := lookPathGoscripter()
		if envPath != "" && sameFile(envPath, selfAbsPath()) {
			return "#!/usr/bin/env goscripter run"
		}
	}
	return desiredShebangAbs()
}

func writeShebangLinePreserveMode(abs string, she string) (changed bool, err error) {
	b, err := os.ReadFile(abs)
	if err != nil {
		return false, err
	}
	mode := os.FileMode(0o644)
	if fi, err2 := os.Stat(abs); err2 == nil {
		mode = fi.Mode().Perm()
	}
	body := b
	if len(b) > 2 && b[0] == '#' && b[1] == '!' {
		if idx := indexByte(b, '\n'); idx >= 0 {
			body = b[idx+1:]
		} else {
			body = []byte{}
		}
	}
	out := []byte(she + "\n")
	out = append(out, body...)
	if len(out) == len(b) && string(out) == string(b) {
		return false, nil
	}
	tmp := abs + ".goscripter.tmp"
	if err := os.WriteFile(tmp, out, mode); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func ensureOwnerExec(abs string, verbose bool) error {
	fi, err := os.Stat(abs)
	if err != nil {
		return err
	}
	mode := fi.Mode().Perm()
	if mode&0o100 != 0 {
		return nil
	}
	n := mode | 0o100
	if err := os.Chmod(abs, n); err != nil {
		return err
	}
	if verbose {
		fmt.Println("chmod u+x:", abs)
	}
	return nil
}

func askConfirm(q string, defYes bool) bool {
	fmt.Printf("%s [%s/%s]: ", q, tern(defYes, "Y", "y"), tern(defYes, "n", "N"))
	in := bufio.NewScanner(os.Stdin)
	if !in.Scan() {
		return defYes
	}
	t := strings.TrimSpace(strings.ToLower(in.Text()))
	if t == "" {
		return defYes
	}
	return t == "y" || t == "yes"
}

func tern[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

func ensureParent(p string) error {
	return os.MkdirAll(filepath.Dir(p), 0o755)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

type rmStats struct {
	files, dirs int
	bytes       int64
}

func measureTree(root string, verbose bool) (rmStats, error) {
	var st rmStats
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if fi, e := os.Stat(p); e == nil {
				st.files++
				st.bytes += fi.Size()
				if verbose {
					fmt.Println("rm:", p)
				}
			}
		}
		return nil
	})
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			st.dirs++
		}
		return nil
	})
	return st, nil
}

func removeTree(root string) error {
	return os.RemoveAll(root)
}
