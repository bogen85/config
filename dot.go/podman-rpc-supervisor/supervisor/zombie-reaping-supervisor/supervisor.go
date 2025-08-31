// supervisor.go (libc/cgo reaper with age logging, Linux-only)
// Uses libc waitid(WEXITED|WNOWAIT|WNOHANG) to peek PID, reads /proc/<pid>/comm
// and /proc/<pid>/stat (starttime) to compute age, then reaps with waitpid().
// No x/sys dependency.
//
// Build (requires CGO and BurntSushi/toml in GOPATH):
//
//	CGO_ENABLED=1 GO111MODULE=auto go build -trimpath -ldflags="-s -w" -o supervisor supervisor.go
//
// Run:
//
//	./supervisor --config /etc/services.toml
package main

/*
#cgo linux LDFLAGS: -lc
#include <sys/types.h>
#include <sys/wait.h>
#include <signal.h>
#include <errno.h>
#include <unistd.h>

// Prototypes so cgo can resolve these symbols.
int waitid_peek(pid_t *out_pid);
int waitpid_reap(pid_t pid, int *status);
void decode_status(int st, int *out_code, int *out_signaled, int *out_sig);
long clk_tck(void);

// Definitions.
int waitid_peek(pid_t *out_pid) {
    siginfo_t si;
    int r = waitid(P_ALL, 0, &si, WEXITED | WNOHANG | WNOWAIT);
    if (r < 0) return -errno;
    *out_pid = si.si_pid; // 0 when nothing ready
    return 0;
}

int waitpid_reap(pid_t pid, int *status) {
    pid_t r = waitpid(pid, status, 0);
    if (r < 0) return -errno;
    return 0;
}

void decode_status(int st, int *out_code, int *out_signaled, int *out_sig) {
    if (WIFEXITED(st)) {
        *out_code = WEXITSTATUS(st);
        *out_signaled = 0;
        *out_sig = 0;
        return;
    }
    if (WIFSIGNALED(st)) {
        int s = WTERMSIG(st);
        *out_code = 128 + s;
        *out_signaled = 1;
        *out_sig = s;
        return;
    }
    *out_code = 1;
    *out_signaled = 0;
    *out_sig = 0;
}

long clk_tck(void) { return sysconf(_SC_CLK_TCK); }
*/
import "C"

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

/* ===========================
   Config (TOML)
   =========================== */

type Dur struct{ time.Duration }

func (d *Dur) UnmarshalTOML(v interface{}) error {
	if v == nil {
		d.Duration = 0
		return nil
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			d.Duration = 0
			return nil
		}
		dd, err := time.ParseDuration(x)
		if err != nil {
			return err
		}
		d.Duration = dd
		return nil
	case int64:
		d.Duration = time.Duration(x)
		return nil
	case float64:
		d.Duration = time.Duration(x)
		return nil
	default:
		return fmt.Errorf("unsupported duration type %T", v)
	}
}

type ServiceCfg struct {
	Path      string   `toml:"path"`
	Args      []string `toml:"args"`
	Restart   bool     `toml:"restart"`
	Dir       string   `toml:"dir"`
	Grace     Dur      `toml:"grace"`
	StopOrder int      `toml:"stop_order"`
}

type SupervisorCfg struct {
	Grace     Dur  `toml:"grace"`
	Subreaper bool `toml:"subreaper"`
	DrainTick Dur  `toml:"drain_tick"`
}

type RootCfg struct {
	Supervisor SupervisorCfg         `toml:"supervisor"`
	Services   map[string]ServiceCfg `toml:"services"`
}

/* ===========================
   Logging
   =========================== */

func logf(level, svc, msg string, a ...any) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		file, line = "?", 0
	}
	base := filepath.Base(file)
	prefix := fmt.Sprintf("%s [%s] %s:%d", ts, strings.ToUpper(level), base, line)
	if svc != "" {
		prefix += " [" + svc + "]"
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", prefix, fmt.Sprintf(msg, a...))
}
func info(svc, m string, a ...any)   { logf("info", svc, m, a...) }
func warn(svc, m string, a ...any)   { logf("warn", svc, m, a...) }
func errorf(svc, m string, a ...any) { logf("error", svc, m, a...) }

/* ===========================
   Registry for centralized reaping
   =========================== */

type exitMsg struct {
	pid    int
	code   int            // normalized: exit status or 128+signal
	cause  string         // "exited" or "signaled"
	signal syscall.Signal // signal for signaled exits
	comm   string         // short name from /proc/<pid>/comm (best-effort)
	age    time.Duration  // best-effort since process start
}

var (
	repMu     sync.Mutex
	reg       = make(map[int]*runner) // pid -> runner
	preReaped = make(map[int]exitMsg) // pid -> exit (handles rare race: exit-before-register)
)

func registerPid(pid int, r *runner) (pre *exitMsg) {
	repMu.Lock()
	defer repMu.Unlock()
	if msg, ok := preReaped[pid]; ok {
		delete(preReaped, pid)
		return &msg
	}
	reg[pid] = r
	return nil
}

func deliverOrStash(pid int, msg exitMsg) (delivered bool) {
	repMu.Lock()
	r := reg[pid]
	if r != nil {
		delete(reg, pid)
	}
	repMu.Unlock()
	if r != nil {
		select {
		case r.exitCh <- msg:
		default:
		}
		return true
	}
	// Not one of ours (or race before registration) -> stash & log
	repMu.Lock()
	preReaped[pid] = msg
	repMu.Unlock()
	if msg.comm != "" {
		warn("orphan", "reaped pid=%d comm=%q cause=%s rc=%d sig=%d age=%s", pid, msg.comm, msg.cause, msg.code, msg.signal, msg.age)
	} else {
		warn("orphan", "reaped pid=%d cause=%s rc=%d sig=%d age=%s", pid, msg.cause, msg.code, msg.signal, msg.age)
	}
	return false
}

/* ===========================
   Helpers
   =========================== */

func lookPathOrAbs(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(path) || strings.Contains(path, "/") {
		return path, nil
	}
	p, err := exec.LookPath(path)
	if err != nil {
		return "", fmt.Errorf("lookpath %q: %w", path, err)
	}
	return p, nil
}

func signalGroup(pid int, sig syscall.Signal) error { return syscall.Kill(-pid, sig) }

func readProcComm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ---- Age computation (from /proc/stat btime + /proc/<pid>/stat starttime) ----
var (
	bootOnce    sync.Once
	bootUnixSec int64
	clkTck      int64
)

func initBoot() {
	clkTck = int64(C.clk_tck())
	if clkTck <= 0 {
		clkTck = 100 // conservative fallback
	}
	data, err := os.ReadFile("/proc/stat")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "btime ") {
				f := strings.Fields(line)
				if len(f) == 2 {
					if v, err2 := strconv.ParseInt(f[1], 10, 64); err2 == nil {
						bootUnixSec = v
						return
					}
				}
			}
		}
	}
	bootUnixSec = time.Now().Unix() // fallback if /proc/stat not readable
}

func procAge(pid int) time.Duration {
	bootOnce.Do(initBoot)
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// comm can contain spaces in parentheses; find last ')' then split rest.
	s := string(b)
	close := strings.LastIndexByte(s, ')')
	if close == -1 || close+2 >= len(s) {
		return 0
	}
	rest := strings.Fields(s[close+2:])
	if len(rest) < 20 {
		return 0
	}
	// Field 22 (index 21 overall) is starttime; in 'rest' it's at index 19.
	startTicks, err := strconv.ParseInt(rest[19], 10, 64)
	if err != nil || startTicks <= 0 || clkTck <= 0 {
		return 0
	}
	startUnix := bootUnixSec + (startTicks / clkTck)
	ageSec := time.Now().Unix() - startUnix
	if ageSec < 0 {
		return 0
	}
	return time.Duration(ageSec) * time.Second
}

/* ===========================
   Global reaper (libc peek-before-reap)
   =========================== */

func reaper(ctx context.Context, sigchld <-chan os.Signal, drainTick time.Duration) {
	if drainTick <= 0 {
		drainTick = time.Second
	}
	ticker := time.NewTicker(drainTick)
	defer ticker.Stop()

	drain := func() {
		for {
			var cpid C.pid_t
			if rc := C.waitid_peek(&cpid); rc < 0 {
				// No children or error; stop draining
				break
			}
			pid := int(cpid)
			if pid == 0 { // nothing ready
				break
			}

			// Peek time: read /proc for comm and age
			comm := readProcComm(pid)
			age := procAge(pid)

			// Now reap the specific pid
			var st C.int
			if rc := C.waitpid_reap(C.pid_t(pid), &st); rc < 0 {
				_ = deliverOrStash(pid, exitMsg{pid: pid, code: 1, cause: "unknown", signal: 0, comm: comm, age: age})
				continue
			}
			var code, signaled, sig C.int
			C.decode_status(st, &code, &signaled, &sig)
			msg := exitMsg{
				pid:    pid,
				code:   int(code),
				cause:  map[bool]string{true: "signaled", false: "exited"}[signaled != 0],
				signal: syscall.Signal(sig),
				comm:   comm,
				age:    age,
			}
			_ = deliverOrStash(pid, msg)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-sigchld:
			drain()
		case <-ticker.C:
			drain()
		}
	}
}

/* ===========================
   Runner
   =========================== */

type runner struct {
	name string
	cfg  ServiceCfg

	pid    atomic.Int32 // leader pid (pgid)
	exitCh chan exitMsg

	lastExit int // for oneshot aggregation
}

const (
	healthyUptime = 10 * time.Second
	maxBackoff    = 30 * time.Second
)

func (r *runner) startLoop(ctx context.Context, defaultGrace time.Duration, wgDone func()) {
	defer wgDone()
	backoff := time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		path, err := lookPathOrAbs(r.cfg.Path)
		if err != nil {
			errorf(r.name, "resolve path: %v", err)
			if !r.cfg.Restart {
				r.lastExit = 1
				return
			}
			if !sleepBackoff(ctx, &backoff) {
				return
			}
			continue
		}

		cmd := exec.Command(path, r.cfg.Args...)
		if r.cfg.Dir != "" {
			cmd.Dir = r.cfg.Dir
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		startAt := time.Now()
		if err := cmd.Start(); err != nil {
			errorf(r.name, "start failed: %v", err)
			if !r.cfg.Restart {
				r.lastExit = 1
				return
			}
			if !sleepBackoff(ctx, &backoff) {
				return
			}
			continue
		}

		pid := cmd.Process.Pid
		_ = cmd.Process.Release() // no Wait(); reaper owns waits
		r.pid.Store(int32(pid))

		// Register pid -> runner; handle rare pre-reap race
		if pre := registerPid(pid, r); pre != nil {
			info(r.name, "(race) pid=%d exited early rc=%d", pid, pre.code)
			r.lastExit = pre.code
			if !r.cfg.Restart || ctx.Err() != nil {
				return
			}
			uptime := time.Since(startAt)
			if uptime < healthyUptime {
				if !sleepBackoff(ctx, &backoff) {
					return
				}
			} else {
				backoff = time.Second
			}
			continue
		}

		info(r.name, "started pid=%d path=%q args=%q dir=%q", pid, path, strings.Join(r.cfg.Args, " "), r.cfg.Dir)

		// Wait for exit notification from reaper or shutdown
		select {
		case msg := <-r.exitCh:
			r.lastExit = msg.code
			info(r.name, "exited rc=%d", msg.code)
			if ctx.Err() != nil || !r.cfg.Restart {
				return
			}
			uptime := time.Since(startAt)
			if uptime < healthyUptime {
				if !sleepBackoff(ctx, &backoff) {
					return
				}
			} else {
				backoff = time.Second
			}
		case <-ctx.Done():
			return
		}
	}
}

func (r *runner) pgid() int { return int(r.pid.Load()) }

func (r *runner) alive() bool {
	pid := r.pgid()
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	return true
}

func (r *runner) stop(grace time.Duration, escalateNow func() bool) {
	pid := r.pgid()
	if pid <= 0 {
		return
	}
	warn(r.name, "shutdown: sending SIGTERM to pgid=%d", pid)
	_ = signalGroup(pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !r.alive() {
			info(r.name, "shutdown: process exited after SIGTERM")
			return
		}
		if escalateNow != nil && escalateNow() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r.alive() {
		warn(r.name, "shutdown: grace %s elapsed; sending SIGKILL to pgid=%d", grace, pid)
		_ = signalGroup(pid, syscall.SIGKILL)
	}
}

func sleepBackoff(ctx context.Context, backoff *time.Duration) bool {
	b := *backoff
	if b > maxBackoff {
		b = maxBackoff
	}
	t := time.NewTimer(b)
	select {
	case <-ctx.Done():
		t.Stop()
		return false
	case <-t.C:
	}
	if *backoff < maxBackoff {
		*backoff *= 2
		if *backoff > maxBackoff {
			*backoff = maxBackoff
		}
	}
	return true
}

/* ===========================
   Main
   =========================== */

const PR_SET_CHILD_SUBREAPER = 36

func setSubreaper() error {
	_, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, uintptr(PR_SET_CHILD_SUBREAPER), 1, 0, 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func main() {
	var cfgPath string
	var defaultGrace time.Duration
	flag.StringVar(&cfgPath, "config", "/etc/services.toml", "path to TOML config")
	flag.DurationVar(&defaultGrace, "grace", 3*time.Second, "default shutdown grace (overridden by [supervisor].grace)")
	flag.Parse()

	var root RootCfg
	if _, err := toml.DecodeFile(cfgPath, &root); err != nil {
		errorf("", "parse config: %v", err)
		os.Exit(2)
	}
	if len(root.Services) == 0 {
		errorf("", "empty [services]")
		os.Exit(2)
	}
	if root.Supervisor.Grace.Duration > 0 {
		defaultGrace = root.Supervisor.Grace.Duration
	}

	// Optional subreaper
	if root.Supervisor.Subreaper {
		if err := setSubreaper(); err != nil {
			warn("", "prctl(PR_SET_CHILD_SUBREAPER) failed: %v", err)
		} else {
			info("", "subreaper enabled via config")
		}
	}

	// Reaper drain ticker interval
	drainTick := time.Second
	if root.Supervisor.DrainTick.Duration > 0 {
		drainTick = root.Supervisor.DrainTick.Duration
	}

	// Build runners
	runners := make([]*runner, 0, len(root.Services))
	hasDaemons := false
	for name, sc := range root.Services {
		if strings.TrimSpace(name) == "" {
			errorf("", "blank service name")
			os.Exit(2)
		}
		if strings.TrimSpace(sc.Path) == "" {
			errorf(name, "missing path")
			os.Exit(2)
		}
		if sc.Restart {
			hasDaemons = true
		}
		r := &runner{name: name, cfg: sc, exitCh: make(chan exitMsg, 1)}
		runners = append(runners, r)
	}

	// Contexts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signals (stop) & SIGCHLD
	sigTerm := make(chan os.Signal, 2)
	signal.Notify(sigTerm, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	var sigCount int32

	sigChld := make(chan os.Signal, 1)
	signal.Notify(sigChld, syscall.SIGCHLD)
	go reaper(ctx, sigChld, drainTick)

	// Start runners
	var wgAll sync.WaitGroup
	wgAll.Add(len(runners))
	for _, r := range runners {
		go r.startLoop(ctx, defaultGrace, wgAll.Done)
	}

	// Order for shutdown
	sorted := append([]*runner(nil), runners...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].cfg.StopOrder < sorted[j].cfg.StopOrder })

	if !hasDaemons {
		// oneshot-only: wait for all to exit
		wgAll.Wait()
		exitStatus := 0
		for _, r := range runners {
			if !r.cfg.Restart && r.lastExit != 0 {
				exitStatus = 1
			}
		}
		info("", "all oneshot services exited; shutting down")
		os.Exit(exitStatus)
	}

	info("", "daemon mode: waiting for signal")
	<-sigTerm
	warn("", "received stop signal; initiating shutdown")
	atomic.AddInt32(&sigCount, 1)
	cancel() // prevent restarts

	// second-signal escalation watcher
	go func() {
		if _, ok := <-sigTerm; ok {
			warn("", "received second signal; escalating")
			atomic.AddInt32(&sigCount, 1)
		}
	}()
	escalateNow := func() bool { return atomic.LoadInt32(&sigCount) >= 2 }

	// Stop in declared order
	for _, r := range sorted {
		grace := r.cfg.Grace.Duration
		if grace <= 0 {
			grace = defaultGrace
		}
		r.stop(grace, escalateNow)
	}

	wgAll.Wait()
	info("", "supervisor exiting")
}
