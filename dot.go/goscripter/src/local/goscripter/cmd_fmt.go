package goscripter

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

func newFmtFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("fmt", flag.ContinueOnError)
	fs.Usage = func() { usageFmt(fs) }
	return fs
}

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
	mc := mergeConfig(gl.Configs, lc, filepath.Dir(abs))
	cb := resolveCacheBase(mc.Global)
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
	if len(body) > 2 && body[0] == '#' && body[1] == '!' {
		if idx := indexByte(body, '\n'); idx >= 0 {
			shebang = string(body[:idx])
			body = body[idx+1:]
		} else {
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
		cur.Write(content)
	} else {
		cur.WriteString(newShebang)
		cur.WriteByte('\n')
		cur.Write(body)
	}
	if bytes.Equal(out.Bytes(), cur.Bytes()) {
		return nil
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

func CmdFmt(args []string) int {
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
	sort.Strings(targets)
	for _, t := range targets {
		if err := fmtOne(cwd, gl, t); err != nil {
			warnf("%v", err)
		}
	}
	return 0
}
