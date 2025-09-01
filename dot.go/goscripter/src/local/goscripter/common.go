package goscripter

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "warn: "+format+"\n", a...)
}

func eprintf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func ensureDir(dir string) error { return os.MkdirAll(dir, 0o755) }

// ensureOwnerExec adds u+x if missing. Logs when verbose is on.
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

// askConfirm prompts the user; defaultYes decides ENTER behavior.
func askConfirm(prompt string, defaultYes bool) bool {
	suffix := " [y/N]: "
	if defaultYes {
		suffix = " [Y/n]: "
	}
	fmt.Print(prompt, suffix)
	var resp string
	_, err := fmt.Scanln(&resp)
	if err != nil {
		// treat EOF/empty as default
		return defaultYes
	}
	switch resp {
	case "y", "Y", "yes", "YES":
		return true
	case "n", "N", "no", "NO":
		return false
	default:
		return defaultYes
	}
}

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
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
