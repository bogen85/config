package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

/* ===========================
   Config types
   =========================== */

// Dur unmarshals either a JSON string like "10s" or a number of nanoseconds.
type Dur struct{ time.Duration }

func (d *Dur) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		d.Duration = 0
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			d.Duration = 0
			return nil
		}
		dd, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		d.Duration = dd
		return nil
	default:
		// accept integer or float nanoseconds
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		if i, err := n.Int64(); err == nil {
			d.Duration = time.Duration(i)
			return nil
		}
		f, err := n.Float64()
		if err != nil {
			return err
		}
		d.Duration = time.Duration(f)
		return nil
	}
}

type ServiceCfg struct {
	Path    string   `json:"path"`              // executable (absolute or PATH-searchable)
	Args    []string `json:"args,omitempty"`    // arguments
	Restart bool     `json:"restart,omitempty"` // restart on exit
	Dir     string   `json:"dir,omitempty"`     // optional working directory
	Grace   Dur      `json:"grace,omitempty"`   // per-service grace (e.g., "10s"), overrides global -grace
}

type Config map[string]ServiceCfg

/* ===========================
   Logging (ts + file:line)
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
   Helpers
   =========================== */

func lookPathOrAbs(path string, env []string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(path) || strings.Contains(path, "/") {
		return path, nil
	}
	// Search PATH from env (fallback to process PATH).
	var PATH string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			PATH = strings.TrimPrefix(kv, "PATH=")
			break
		}
	}
	if PATH == "" {
		PATH = os.Getenv("PATH")
	}
	res, err := exec.LookPath(path)
	if err != nil {
		return "", fmt.Errorf("lookpath %q: %w", path, err)
	}
	return res, nil
}

func signalGroup(pid int, sig syscall.Signal) error {
	// negative PID targets the process group
	return syscall.Kill(-pid, sig)
}

/* ===========================
   Runner
   =========================== */

type runner struct {
	name string
	cfg  ServiceCfg

	mu   sync.Mutex
	cmd  *exec.Cmd
}

// startLoop launches/restarts a service until context is done.
// On shutdown: SIGTERM -> wait up to grace -> SIGKILL if still running.
func (r *runner) startLoop(ctx context.Context, wg *sync.WaitGroup, defaultGrace time.Duration) {
	defer wg.Done()

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	env := os.Environ()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		path, err := lookPathOrAbs(r.cfg.Path, env)
		if err != nil {
			errorf(r.name, "resolve path: %v", err)
			if !r.cfg.Restart {
				return
			}
			// backoff then retry unless shutting down
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}
		}

		cmd := exec.Command(path, r.cfg.Args...)
		if r.cfg.Dir != "" {
			cmd.Dir = r.cfg.Dir
		}
		// Inherit stdio so container logs capture child output directly
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = nil

		// New process group so we can signal the whole tree
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		if err := cmd.Start(); err != nil {
			errorf(r.name, "start failed: %v", err)
			if !r.cfg.Restart {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}
		}

		r.mu.Lock()
		r.cmd = cmd
		r.mu.Unlock()

		info(r.name, "started pid=%d path=%q args=%q dir=%q", cmd.Process.Pid, path, strings.Join(r.cfg.Args, " "), r.cfg.Dir)

		// Channel closed when process exits (after Wait returns).
		exited := make(chan struct{})

		// Grace to apply on shutdown (per-service overrides default).
		grace := r.cfg.Grace.Duration
		if grace <= 0 {
			grace = defaultGrace
		}

		// Shutdown watcher: on ctx.Done, send SIGTERM, then only SIGKILL
		// if the process is still running after 'grace'.
		go func(pid int) {
			<-ctx.Done()
			warn(r.name, "shutdown: sending SIGTERM to pgid=%d", pid)
			_ = signalGroup(pid, syscall.SIGTERM)

			select {
			case <-exited:
				// Process exited within grace; no SIGKILL necessary
				info(r.name, "shutdown: process exited after SIGTERM")
			case <-time.After(grace):
				warn(r.name, "shutdown: grace %s elapsed; sending SIGKILL to pgid=%d", grace, pid)
				_ = signalGroup(pid, syscall.SIGKILL)
				// Waiter will observe exit and close 'exited'
			}
		}(cmd.Process.Pid)

		// Wait for process to exit (normal or due to signals)
		err = cmd.Wait()
		close(exited)

		exitCode := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				if st, ok := ee.Sys().(syscall.WaitStatus); ok {
					if st.Signaled() {
						exitCode = 128 + int(st.Signal())
					} else {
						exitCode = st.ExitStatus()
					}
				} else {
					exitCode = 1
				}
			} else {
				exitCode = 1
			}
		}
		info(r.name, "exited rc=%d err=%v", exitCode, err)

		// Clear cmd
		r.mu.Lock()
		r.cmd = nil
		r.mu.Unlock()

		// Restart policy
		if !r.cfg.Restart {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = time.Second // reset after a successful cycle
		}
	}
}

/* ===========================
   Main / wiring
   =========================== */

func main() {
	var cfgPath string
	var defaultGrace time.Duration
	flag.StringVar(&cfgPath, "config", "/etc/services.json", "path to JSON config (map of service name -> options)")
	flag.DurationVar(&defaultGrace, "grace", 3*time.Second, "default grace before SIGKILL on shutdown (overridden by per-service 'grace')")
	flag.Parse()

	f, err := os.Open(cfgPath)
	if err != nil {
		errorf("", "open config: %v", err)
		os.Exit(2)
	}
	defer f.Close()

	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		errorf("", "parse config: %v", err)
		os.Exit(2)
	}
	if len(cfg) == 0 {
		errorf("", "empty config")
		os.Exit(2)
	}
	for name, sc := range cfg {
		if strings.TrimSpace(name) == "" {
			errorf("", "blank service name")
			os.Exit(2)
		}
		if strings.TrimSpace(sc.Path) == "" {
			errorf(name, "missing path")
			os.Exit(2)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGINT/SIGTERM
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup
	wg.Add(len(cfg))
	for name, sc := range cfg {
		r := &runner{name: name, cfg: sc}
		go r.startLoop(ctx, &wg, defaultGrace)
	}

	// Exit when: a) we get a signal, or b) all non-restarting services have exited
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case sig := <-sigc:
		warn("", "received signal: %v; shutting down", sig)
		cancel()
		<-done
	case <-done:
		info("", "all services exited; shutting down")
	}

	info("", "supervisor exiting")
}
