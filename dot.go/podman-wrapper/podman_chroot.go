// podman-chroot.go
// A Go port of your Python podman-chroot helper.
// Usage examples are the same as your Python version.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"
)

func die(msg string, code int) {
	fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	os.Exit(code)
}

func cmdPath(name string) string {
	exepath, err := exec.LookPath(name)
	if err != nil {
		die(fmt.Sprintf("missing required command: %s\n%v\n", name, err), 1)
	}
	return exepath
}

func ensureDirHost(p string, mode os.FileMode) {
	// If it exists but isn't a dir, bail early.
	if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
		die(fmt.Sprintf("%s exists but is not a directory", p), 1)
	}
	// Try as current user first.
	if err := os.MkdirAll(p, mode); err == nil {
		_ = os.Chmod(p, mode)
		return
	}
	// Retry via sudo if not root.
	if syscall.Geteuid() != 0 {

		cmd := exec.Command(cmdPath("sudo"), cmdPath("mkdir"), "-p", p)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			die(fmt.Sprintf("unable to create %s (even with sudo): %v", p, err), 1)
		}
		_ = exec.Command(cmdPath("sudo"), cmdPath("chmod"), "0755", p).Run() // host-side normalize
		return
	}
	die(fmt.Sprintf("unable to create %s: check path/permissions", p), 1)
}

func expandUser(p string) (string, error) {
	if p == "" || p[0] != '~' {
		return p, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	home := u.HomeDir
	switch {
	case p == "~":
		return home, nil
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:]), nil
	default:
		// ~otheruser not supported for simplicity (like many tools)
		return "", fmt.Errorf("cannot expand %q (only ~ or ~/... supported)", p)
	}
}

func abspath(p string) string {
	ep, err := expandUser(p)
	if err != nil {
		die(err.Error(), 1)
	}
	ap, err := filepath.Abs(ep)
	if err != nil {
		die(err.Error(), 1)
	}
	// Resolve symlinks in the host path of the rootfs dir itself.
	// (Matches Python's Path(...).resolve())
	rp, err := filepath.EvalSymlinks(ap)
	if err == nil {
		return rp
	}
	return ap
}

// resolve a path that lives INSIDE the rootfs, interpreting absolute link targets
// relative to rootfs (not host '/'). Returns the HOST path to the final target.
func resolveInRootfs(rootfs string, insidePath string, maxSymlinks int) (string, error) {
	p := insidePath
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for i := 0; i < maxSymlinks; i++ {
		hostP := filepath.Join(rootfs, strings.TrimPrefix(p, "/"))
		fi, err := os.Lstat(hostP)
		if err != nil {
			// final path doesn't exist in rootfs
			return hostP, nil
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return hostP, nil // resolved to non-link
		}
		// Readlink and interpret with POSIX semantics
		target, err := os.Readlink(hostP)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(target, "/") {
			// absolute -> from root of rootfs
			p = path.Clean(target)
		} else {
			// relative -> from containing dir (POSIX)
			p = path.Clean(path.Join(path.Dir(p), target))
		}
	}
	return "", fmt.Errorf("too many levels of symbolic links while resolving %q", insidePath)
}

func isExecutableInRootfs(rootfs string, insidePath string) bool {
	target, err := resolveInRootfs(rootfs, insidePath, 40)
	if err != nil {
		return false
	}
	st, err := os.Stat(target) // follow final link
	if err != nil {
		return false
	}
	mode := st.Mode()
	// regular file + any exec bit set (user/group/other)
	return mode.IsRegular() && (mode.Perm()&0o111) != 0
}

func pickInitInRootfs(rootfs string) []string {
	candidates := []string{
		"/usr/lib/systemd/systemd",
		"/lib/systemd/systemd",
		"/sbin/openrc-init",
		"/sbin/init",   // may symlink to busybox
		"/bin/busybox", // needs arg "init"
	}
	for _, c := range candidates {
		if isExecutableInRootfs(rootfs, c) {
			if strings.HasSuffix(c, "/busybox") {
				return []string{c, "init"}
			}
			return []string{c}
		}
	}
	return nil
}

func defaultShell(rootfs string) []string {
	if _, err := os.Stat(filepath.Join(rootfs, "bin", "bash")); err == nil {
		return []string{"/bin/bash"}
	}
	return []string{"/bin/sh"}
}

func main() {
	var (
		nameFlag    string
		overlayFlag bool
		initFlag    bool
		runHostFlag bool
		showCmdFlag bool
		//netFlag     string
	)

	flag.StringVar(&nameFlag, "name", "", "Container name (default: derived from ROOTFS)")
	flag.BoolVar(&showCmdFlag, "showcmd", false, "Show podman command before executing it")
	flag.BoolVar(&overlayFlag, "overlay", false, "Use writable overlay (:O) on ROOTFS")
	flag.BoolVar(&runHostFlag, "runhost", false, "bind host / to container /run/host")
	flag.BoolVar(&initFlag, "init", false, "Run a full init inside the container")

	// Define flags (support both --net and --network)
	//flag.StringVar(&netFlag, "net", "host", "Network mode (default: host)")
	//flag.StringVar(&netFlag, "network", "host", "Network mode (default: host)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] ROOTFS [command...]\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		die("missing ROOTFS", 2)
	}
	rootfs := abspath(args[0])
	if fi, err := os.Stat(rootfs); err != nil || !fi.IsDir() {
		die(fmt.Sprintf("ROOTFS does not exist: %s", rootfs), 2)
	}

	// Remaining args are the command
	var providedCmd []string
	if len(args) > 1 {
		providedCmd = append([]string{}, args[1:]...)
	}

	var cmdSlice []string
	if initFlag {
		cmdSlice = pickInitInRootfs(rootfs)
		if len(cmdSlice) == 0 {
			fmt.Fprintf(os.Stderr, "warning: no init found inside %s; falling back to /bin/sh\n", rootfs)
			if len(providedCmd) > 0 {
				cmdSlice = providedCmd
			} else {
				cmdSlice = defaultShell(rootfs)
			}
		} else {
			if len(providedCmd) > 0 {
				die("Do not provide command with --init", 2)
			}
		}
	} else {
		cmdSlice = append([]string{}, providedCmd...)
		if len(cmdSlice) > 0 && cmdSlice[0] == "--" {
			cmdSlice = cmdSlice[1:]
		}
		if len(cmdSlice) == 0 {
			cmdSlice = defaultShell(rootfs)
		}
	}

	// Container name
	name := nameFlag
	if name == "" {
		base := filepath.Base(rootfs)
		base = strings.ReplaceAll(base, " ", "-")
		name = "chroot-" + base
	}

	// Overlay
	rootfsArg := rootfs
	if overlayFlag {
		rootfsArg += ":O"
	}

	// TTY only if stdout is a terminal
	var ttyFlag []string
	if term.IsTerminal(int(os.Stdout.Fd())) {
		ttyFlag = []string{"-t"}
	}

	termEnv := os.Getenv("TERM")
	if termEnv == "" {
		termEnv = "xterm-256color"
	}
	langEnv := os.Getenv("LANG")
	if langEnv == "" {
		langEnv = "C.UTF-8"
	}

	// Base podman command
	var pargs []string
	if initFlag {
		cmdPath("machinectl")
		// wrap podman run in machinectl shell root@
		pargs = []string{
			cmdPath("machinectl"), "shell", "root@", "/usr/bin/podman", "run", "--rm", "-i",
		}
	} else {
		// direct podman run
		pargs = []string{
			cmdPath("podman"), "run", "--rm", "-i",
		}
	}

	pargs = append(pargs, ttyFlag...)
	pargs = append(pargs,
		"--name", name, "--replace",
		"--privileged",
		"--cgroupns=host",
		"--systemd=always",
		"--hostname", name,
		// "--network", netFlag,
		"--security-opt", "label=disable",
		// cgroups + runtime tmpfs
		"--volume", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
		"--tmpfs", "/run",
		"--tmpfs", "/run/lock",
		"--tmpfs", "/tmp",
		// env pass-through
		"-e", "TERM="+termEnv,
		"-e", "LANG="+langEnv,
	)

	if runHostFlag {
		pargs = append(pargs,
			"--volume", "/:/run/host:ro,rslave",
			"--volume", "/mnt:/run/host/mnt:rw,rslave",
			"--volume", "/home:/run/host/home:rw,rslave",
		)
	}

	// Bind the rootfs’ own journal dir over /var/log/journal so logs live with the rootfs
	journalHost := filepath.Join(rootfs, "var", "log", "journal")
	// host-side creation; in-container service will set 2755 root:systemd-journal
	ensureDirHost(journalHost, 0o755)
	pargs = append(pargs, "-v", journalHost+":/var/log/journal:Z")
	if showCmdFlag {
	    fmt.Fprintf(os.Stderr, "journald bind: %s -> /var/log/journal\n", journalHost)
	}

	// LAST option must be --rootfs
	pargs = append(pargs, "--rootfs", rootfsArg)
	pargs = append(pargs, cmdSlice...)

	// Elevate if needed
	if syscall.Geteuid() != 0 {
		pargs = append([]string{cmdPath("sudo"), "-E"}, pargs...)
	}

	if showCmdFlag {
		fmt.Fprintf(os.Stderr, "pargs:\n%v\n\n", pargs)
	}

	// Exec
	cmd := exec.Command(pargs[0], pargs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Process finished but with error code
			if status, ok := ee.Sys().(syscall.WaitStatus); ok {
				exitCode = status.ExitStatus()
			} else {
				exitCode = 1
			}
		} else {
			fmt.Fprintf(os.Stderr, "error executing: %v\n", err)
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}
