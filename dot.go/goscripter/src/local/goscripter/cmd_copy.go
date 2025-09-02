package goscripter

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func newCopyFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("copy", flag.ContinueOnError)
	verbose := FalseDefault()
	force := FalseDefault()
	mkdirs := FalseDefault()
	uid := -1
	gid := -1
	mode := ""
	strip := FalseDefault()
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "verbose output")
	fs.BoolVar(&verbose, "v", FalseDefault(), "verbose output (short)")
	fs.BoolVar(&force, "force", FalseDefault(), "overwrite existing destination file")
	fs.BoolVar(&force, "f", FalseDefault(), "overwrite existing destination file (short)")
	fs.BoolVar(&mkdirs, "mkdirs", FalseDefault(), "create destination parent directories if missing")
	fs.IntVar(&uid, "uid", -1, "set destination file owner UID (requires privileges)")
	fs.IntVar(&gid, "gid", -1, "set destination file group GID (requires privileges)")
	fs.StringVar(&mode, "mode", "", "set destination file mode (octal, e.g. 0755)")
	fs.BoolVar(&strip, "strip", FalseDefault(), "strip the destination binary after copy")
	fs.Usage = func() { usageCopy(fs) }
	return fs
}

func CmdCopy(args []string) int {
	fs := flag.NewFlagSet("copy", flag.ContinueOnError)
	verbose := FalseDefault()
	force := FalseDefault()
	mkdirs := FalseDefault()
	uid := -1
	gid := -1
	mode := ""
	strip := FalseDefault()
	fs.BoolVar(&verbose, "verbose", FalseDefault(), "verbose output")
	fs.BoolVar(&verbose, "v", FalseDefault(), "verbose output (short)")
	fs.BoolVar(&force, "force", FalseDefault(), "overwrite existing destination file")
	fs.BoolVar(&force, "f", FalseDefault(), "overwrite existing destination file (short)")
	fs.BoolVar(&mkdirs, "mkdirs", FalseDefault(), "create destination parent directories if missing")
	fs.IntVar(&uid, "uid", -1, "set destination file owner UID (requires privileges)")
	fs.IntVar(&gid, "gid", -1, "set destination file group GID (requires privileges)")
	fs.StringVar(&mode, "mode", "", "set destination file mode (octal, e.g. 0755)")
	fs.BoolVar(&strip, "strip", FalseDefault(), "strip the destination binary after copy")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 2 {
		usageCopy(newCopyFlagSet())
		return 2
	}

	var script string
	var dest string
	dashdash := -1
	for i, a := range rest {
		if a == "--" {
			dashdash = i
			break
		}
	}
	if dashdash >= 0 {
		if dashdash+2 != len(rest) {
			usageCopy(newCopyFlagSet())
			return 2
		}
		script = rest[0]
		dest = rest[dashdash+1]
	} else {
		if len(rest) != 2 {
			usageCopy(newCopyFlagSet())
			return 2
		}
		script = rest[0]
		dest = rest[1]
	}

	abs, err := filepath.Abs(script)
	if err != nil {
		eprintf("copy: %v", err)
		return 2
	}
	dest = filepath.Clean(dest)

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

	if _, err := refreshCache("copy", abs, cb, mc.Flags, mc.Env, verbose, FalseDefault() /*skipDeps*/); err != nil {
		eprintf("copy: %v", err)
		return 2
	}

	cdir := cacheDirFor(cb, abs)
	srcBin := filepath.Join(cdir, cacheBinName)

	fi, err := os.Stat(dest)
	toPath := dest
	if err == nil && fi.IsDir() {
		base := filepath.Base(abs)
		name := strings.TrimSuffix(base, filepath.Ext(base))
		toPath = filepath.Join(dest, name)
	} else {
		if perr := ensureParentDir(toPath, mkdirs); perr != nil {
			eprintf("copy: %v", perr)
			return 2
		}
	}

	if _, e := os.Stat(toPath); e == nil && !force {
		eprintf("copy: destination exists (use --force to overwrite): %s", toPath)
		return 2
	}

	if err := copyFileWithMode(srcBin, toPath); err != nil {
		eprintf("copy: %v", err)
		return 2
	}

	if mode != "" {
		if mode[0] != '0' {
			mode = "0" + mode
		}
		val, perr := strconv.ParseUint(mode, 0, 32)
		if perr != nil {
			eprintf("copy: invalid --mode %q (must be octal like 0755)", mode)
			return 2
		}
		if err := os.Chmod(toPath, os.FileMode(val)); err != nil {
			eprintf("copy: chmod %s: %v", toPath, err)
			return 2
		}
	}

	if uid >= 0 || gid >= 0 {
		if uid < 0 {
			uid = 0
		}
		if gid < 0 {
			gid = 0
		}
		if err := os.Chown(toPath, uid, gid); err != nil {
			eprintf("copy: chown %s to uid=%d gid=%d failed: %v", toPath, uid, gid, err)
			return 2
		}
	}

	// Decide if we should strip
	doStrip := strip || mc.CmdStrip["copy"]
	if doStrip {
		if _, err := exec.LookPath("strip"); err == nil {
			cmd := exec.Command("strip", toPath)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if e := cmd.Run(); e != nil {
				warnf("copy: strip failed: %v", e)
			}
			if verbose {
				fmt.Println("copy: stripped", toPath)
			}
		} else if verbose {
			warnf("copy: 'strip' tool not found; skipping strip")
		}
	}

	if verbose {
		fmt.Printf("copy: %s -> %s\n", srcBin, toPath)
	}
	return 0
}

func ensureParentDir(path string, mkdirs bool) error {
	parent := filepath.Dir(path)
	if parent == "" || parent == "." {
		return nil
	}
	if _, err := os.Stat(parent); err == nil {
		return nil
	}
	if mkdirs {
		return os.MkdirAll(parent, 0o755)
	}
	return fmt.Errorf("destination parent dir does not exist: %s (use --mkdirs)", parent)
}

func copyFileWithMode(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	si, err := in.Stat()
	if err != nil {
		return err
	}
	mode := si.Mode().Perm()

	tmp := dst + ".goscripter.copy.tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	cerr := out.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func init() {
	Register(&Command{
		Name:    "copy",
		Aliases: []string{"install"},
		Summary: "Build (full deps) then copy cached binary to destination",
		Help:    func() { usageCopy(newCopyFlagSet()) },
		Run:     CmdCopy,
	})
}
