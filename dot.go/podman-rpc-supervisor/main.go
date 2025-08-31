package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
//	"math"
	mrand "math/rand"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

/* ===========================
   Flags / CLI
   =========================== */

type envList []string

func (e *envList) String() string { return strings.Join(*e, ",") }
func (e *envList) Set(s string) error {
	if s == "" {
		return errors.New("empty env entry")
	}
	*e = append(*e, s)
	return nil
}

var (
	flagRoot     string
	flagMode     string
	flagName     string
	flagStartDir string
	flagStdinStr string
	flagStdinFile string
	flagEnvs     envList
	flagID       string
	flagVerbose  bool
)

func init() {
	// NOTE: Go's flag package accepts "-flag" by default; many shells also pass "--flag" fine.
	flag.StringVar(&flagRoot, "root", "", "socket root path (REQUIRED)")
	flag.StringVar(&flagMode, "mode", "", "mode: server or client (REQUIRED)")
	flag.StringVar(&flagName, "name", "", "server socket name (listens on <root>/<name>.sock) or client target name (connects to it) (REQUIRED)")
	flag.StringVar(&flagStartDir, "startdir", "", "server: chdir on startup; client: per-request working directory")
	flag.StringVar(&flagStdinStr, "stdin", "", "client: literal stdin data (mutually exclusive with -stdinfile)")
	flag.StringVar(&flagStdinFile, "stdinfile", "", "client: path to file used as stdin (mutually exclusive with -stdin)")
	flag.Var(&flagEnvs, "env", "repeatable env (KEY=VAL or KEY). Server: base env; Client: per-request overlay. (repeat)")
	flag.StringVar(&flagID, "id", "", "machine ID override (else /etc/machine-id; else random)")
	flag.BoolVar(&flagVerbose, "verbose", false, "client: print timing/summary (still exits with server return code)")
}

/* ===========================
   Logging (ts + file:line)
   =========================== */

func logWithCaller(level, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	_, file, line, ok := runtime.Caller(2) // 2: caller of infof/warnf/errorf
	if !ok {
		file = "?"
		line = 0
	}
	base := filepath.Base(file)
	fmt.Fprintf(os.Stderr, "%s [%s] %s:%d: %s\n", ts, strings.ToUpper(level), base, line, fmt.Sprintf(msg, args...))
}
func infof(msg string, args ...any)  { logWithCaller("info", msg, args...) }
func warnf(msg string, args ...any)  { logWithCaller("warn", msg, args...) }
func errorf(msg string, args ...any) { logWithCaller("error", msg, args...) }

/* ===========================
   Shared types (RPC payloads)
   =========================== */

type Line struct {
	Index int
	Text  string
}

type ProcessArgs struct {
	MachineID string
	PID       int
	StartDir  string
	Command   string
	Args      []string
	Env       []string // KEY=VAL pairs from client overlay
}

type ProcessReply struct {
	ReturnCode        int
	Error             string
	Stopped           bool
	StoppedBy         string // "", "client", "server"
	ExecStartRFC3339  string
	ExecEndRFC3339    string
	ElapsedMillis     int64
	ResolvedCmdLine   string // e.g. "/abs/path arg1 arg2"
}

type CancelArgs struct {
	MachineID string
	PID       int
}
type CancelReply struct {
	OK bool
}

/* ===========================
   Identity & paths
   =========================== */

func sanitizeMachineID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty machine-id")
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-' {
			continue
		}
		return "", fmt.Errorf("invalid machine-id char: %q", r)
	}
	return s, nil
}

func loadMachineID(override string) string {
	if override != "" {
		id, err := sanitizeMachineID(override)
		if err != nil {
			errorf("invalid --id: %v", err)
			os.Exit(2)
		}
		return id
	}
	data, err := os.ReadFile("/etc/machine-id")
	if err == nil {
		id := strings.TrimSpace(string(data))
		id, err = sanitizeMachineID(id)
		if err == nil && id != "" {
			return id
		}
	}
	// fallback random
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	warnf("machine-id not found; using random id=%s", id)
	return id
}

func idPidKey(machineID string, pid int) string {
	return machineID + ":" + strconv.Itoa(pid)
}

func deriveClientSocketDir(root, machineID string, pid int) string {
	return filepath.Join(root, machineID, strconv.Itoa(pid))
}

func deriveClientSockets(root, machineID string, pid int) (stdoutSock, stderrSock, stdinSock string) {
	dir := deriveClientSocketDir(root, machineID, pid)
	return filepath.Join(dir, "stdout.sock"),
		filepath.Join(dir, "stderr.sock"),
		filepath.Join(dir, "stdin.sock")
}

/* ===========================
   Reorder sinks (client printing)
   =========================== */

type reorderSink struct {
	mu     sync.Mutex
	next   int
	buffer map[int]string
	out    *os.File
}

func newReorderSink(out *os.File) *reorderSink {
	return &reorderSink{
		buffer: make(map[int]string),
		out:    out,
	}
}

func (s *reorderSink) write(line Line) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buffer[line.Index] = line.Text
	for {
		txt, ok := s.buffer[s.next]
		if !ok {
			break
		}
		fmt.Fprintln(s.out, txt)
		delete(s.buffer, s.next)
		s.next++
	}
}

/* ===========================
   Client services
   =========================== */

type StdoutService struct{ sink *reorderSink }
type StderrService struct{ sink *reorderSink }

func (s *StdoutService) WriteLine(in Line, _ *struct{}) error { s.sink.write(in); return nil }
func (s *StderrService) WriteLine(in Line, _ *struct{}) error { s.sink.write(in); return nil }

// Stdin: pull-based RPC
type StdinReadArgs struct {
	Max int
}
type StdinReadReply struct {
	Data []byte
	EOF  bool
	Err  string
}

type StdinService struct {
	mu   sync.Mutex
	r    io.Reader // from -stdin or -stdinfile; may be nil
}

func (s *StdinService) ReadChunk(args StdinReadArgs, reply *StdinReadReply) error {
	if args.Max <= 0 {
		args.Max = 64 * 1024
	}
	reply.Data = nil
	reply.EOF = false
	reply.Err = ""
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.r == nil {
		reply.EOF = true
		return nil
	}
	buf := make([]byte, args.Max)
	n, err := s.r.Read(buf)
	if n > 0 {
		reply.Data = buf[:n]
	}
	if errors.Is(err, io.EOF) {
		reply.EOF = true
		return nil
	}
	if err != nil {
		reply.Err = err.Error()
	}
	return nil
}

/* ===========================
   Server session management
   =========================== */

type session struct {
	mu        sync.Mutex
	machineID string
	pid       int
	cancel    context.CancelFunc
	stoppedBy string // "", "client", "server"
	proc      *os.Process
}

type sessionTable struct {
	mu sync.Mutex
	m  map[string]*session
}

func newSessionTable() *sessionTable { return &sessionTable{m: make(map[string]*session)} }

func (t *sessionTable) add(key string, s *session) {
	t.mu.Lock()
	t.m[key] = s
	t.mu.Unlock()
}
func (t *sessionTable) delete(key string) {
	t.mu.Lock()
	delete(t.m, key)
	t.mu.Unlock()
}
func (t *sessionTable) get(key string) (*session, bool) {
	t.mu.Lock()
	s, ok := t.m[key]
	t.mu.Unlock()
	return s, ok
}
func (t *sessionTable) cancelAll(by string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.m {
		s.mu.Lock()
		s.stoppedBy = by
		if s.cancel != nil {
			s.cancel()
		}
		// best-effort signal; actual Wait happens in Process
		if s.proc != nil {
			_ = signalGroup(s.proc.Pid, syscall.SIGTERM)
		}
		s.mu.Unlock()
	}
}

/* ===========================
   Server RPC service
   =========================== */

type ServerService struct {
	sessions     *sessionTable
	wg           sync.WaitGroup
	root         string
	serverBaseEnv []string // KEY=VAL, built at startup per --env
}

func (s *ServerService) Process(args ProcessArgs, reply *ProcessReply) error {
	s.wg.Add(1)
	defer s.wg.Done()

	key := idPidKey(args.MachineID, args.PID)
	infof("Process start: key=%s cmd=%q args=%d startdir=%q", key, args.Command, len(args.Args), args.StartDir)

	// Validate StartDir if provided
	workDir := args.StartDir
	if workDir != "" {
		if !filepath.IsAbs(workDir) {
			// normalize relative to server's current dir
			wd, _ := os.Getwd()
			workDir = filepath.Join(wd, workDir)
		}
		if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
			reply.ReturnCode = 2
			reply.Error = fmt.Sprintf("invalid startdir: %q", args.StartDir)
			reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
			reply.ExecEndRFC3339 = reply.ExecStartRFC3339
			reply.ElapsedMillis = 0
			errorf("key=%s startdir invalid: %v", key, args.StartDir)
			return nil
		}
	}

	// Prepare context & session
	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{machineID: args.MachineID, pid: args.PID, cancel: cancel}
	s.sessions.add(key, sess)
	defer func() {
		cancel()
		s.sessions.delete(key)
	}()

	stdoutSock, stderrSock, stdinSock := deriveClientSockets(s.root, args.MachineID, args.PID)

	// Wait for client sockets to appear (up to a few seconds)
	if err := waitForSocket(stdoutSock, 5*time.Second); err != nil {
		reply.ReturnCode = 2
		reply.Error = "stdout socket not available: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		errorf("key=%s stdout socket error: %v", key, err)
		return nil
	}
	if err := waitForSocket(stderrSock, 5*time.Second); err != nil {
		reply.ReturnCode = 2
		reply.Error = "stderr socket not available: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		errorf("key=%s stderr socket error: %v", key, err)
		return nil
	}
	if err := waitForSocket(stdinSock, 5*time.Second); err != nil {
		reply.ReturnCode = 2
		reply.Error = "stdin socket not available: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		errorf("key=%s stdin socket error: %v", key, err)
		return nil
	}

	// Connect to client services
	stdoutConn, err := net.Dial("unix", stdoutSock)
	if err != nil {
		reply.ReturnCode = 2
		reply.Error = "connect stdout service: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		errorf("key=%s stdout dial: %v", key, err)
		return nil
	}
	defer stdoutConn.Close()
	stdoutCli := jsonrpc.NewClient(stdoutConn)

	stderrConn, err := net.Dial("unix", stderrSock)
	if err != nil {
		reply.ReturnCode = 2
		reply.Error = "connect stderr service: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		errorf("key=%s stderr dial: %v", key, err)
		return nil
	}
	defer stderrConn.Close()
	stderrCli := jsonrpc.NewClient(stderrConn)

	stdinConn, err := net.Dial("unix", stdinSock)
	if err != nil {
		reply.ReturnCode = 2
		reply.Error = "connect stdin service: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		errorf("key=%s stdin dial: %v", key, err)
		return nil
	}
	defer stdinConn.Close()
	stdinCli := jsonrpc.NewClient(stdinConn)

	// Ping behavior (empty command)
	if strings.TrimSpace(args.Command) == "" {
		_ = stdoutCli.Call("Stdout.WriteLine", Line{Index: 0, Text: "server ping to stdout"}, &struct{}{})
		_ = stderrCli.Call("Stderr.WriteLine", Line{Index: 0, Text: "server ping to stderr"}, &struct{}{})
		reply.ReturnCode = 0
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		reply.ResolvedCmdLine = "(ping)"
		infof("key=%s ping handled", key)
		return nil
	}

	// Build env for child: server base -> client overlay
	finalEnv := mergeEnv(os.Environ(), s.serverBaseEnv, args.Env)

	// Resolve command path
	resolvedPath, rc, err := resolveCommandPath(args.Command, workDirOrCwd(workDir), finalEnv)
	if err != nil {
		reply.ReturnCode = rc
		reply.Error = err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		reply.ResolvedCmdLine = ""
		errorf("key=%s resolve command %q failed: %v", key, args.Command, err)
		return nil
	}

	// Prepare child process
	cmd := exec.Command(resolvedPath, args.Args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = finalEnv
	// Make a new process group so we can signal the whole tree
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		reply.ReturnCode = 2
		reply.Error = "stdout pipe: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		return nil
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		reply.ReturnCode = 2
		reply.Error = "stderr pipe: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		return nil
	}

	// We'll provide stdin via a pipe and pull from client's stdin service
	stdinWriter, err := cmd.StdinPipe()
	if err != nil {
		reply.ReturnCode = 2
		reply.Error = "stdin pipe: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		return nil
	}

	// Time stamps (server-side)
	execStart := time.Now().UTC()

	if err := cmd.Start(); err != nil {
		reply.ReturnCode = 127
		reply.Error = "exec start: " + err.Error()
		reply.ExecStartRFC3339 = execStart.Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ElapsedMillis = 0
		return nil
	}
	reply.ResolvedCmdLine = strings.Join(append([]string{resolvedPath}, args.Args...), " ")

	// Register process to the session
	sess.mu.Lock()
	sess.proc = cmd.Process
	sess.mu.Unlock()

	var wgIO sync.WaitGroup
	wgIO.Add(3)

	// stdout pump
	go func() {
		defer wgIO.Done()
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		idx := 0
		for sc.Scan() {
			_ = stdoutCli.Call("Stdout.WriteLine", Line{Index: idx, Text: sc.Text()}, &struct{}{})
			idx++
		}
		// ignore sc.Err(); if the process dies, pipes close
	}()

	// stderr pump
	go func() {
		defer wgIO.Done()
		sc := bufio.NewScanner(stderrPipe)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		idx := 0
		for sc.Scan() {
			_ = stderrCli.Call("Stderr.WriteLine", Line{Index: idx, Text: sc.Text()}, &struct{}{})
			idx++
		}
	}()

	// stdin pump (pull from client's stdin service)
	go func() {
		defer wgIO.Done()
		const chunk = 64 * 1024
		for {
			var rep StdinReadReply
			err := stdinCli.Call("Stdin.ReadChunk", StdinReadArgs{Max: chunk}, &rep)
			if err != nil {
				// If canceled, we'll be closing stdin anyway
				break
			}
			if rep.Err != "" {
				// Treat as EOF after reporting
				break
			}
			if len(rep.Data) > 0 {
				if _, werr := stdinWriter.Write(rep.Data); werr != nil {
					break
				}
			}
			if rep.EOF {
				break
			}
		}
		_ = stdinWriter.Close()
	}()

	// Cancellation watcher: on ctx.Done, SIGTERM -> wait a bit -> SIGKILL
	done := make(chan struct{})
	go func(pid int) {
		defer close(done)
		<-ctx.Done()
		infof("key=%s cancel received; signaling process group", key)
		_ = signalGroup(pid, syscall.SIGTERM)
		// small grace period
		time.Sleep(1 * time.Second)
		_ = signalGroup(pid, syscall.SIGKILL)
	}(cmd.Process.Pid)

	waitErr := cmd.Wait()
	execEnd := time.Now().UTC()

	// Ensure I/O pumps finish
	wgIO.Wait()

	// Determine return code & stopped state
	rc = exitCodeFromWaitErr(waitErr)
	reply.ReturnCode = rc
	reply.Stopped = false
	reply.StoppedBy = ""
	sess.mu.Lock()
	if sess.stoppedBy != "" {
		reply.Stopped = true
		reply.StoppedBy = sess.stoppedBy
	}
	sess.mu.Unlock()

	reply.ExecStartRFC3339 = execStart.Format(time.RFC3339Nano)
	reply.ExecEndRFC3339 = execEnd.Format(time.RFC3339Nano)
	reply.ElapsedMillis = execEnd.Sub(execStart).Milliseconds()

	infof("Process end: key=%s rc=%d stopped=%v by=%s elapsed=%dms", key, rc, reply.Stopped, reply.StoppedBy, reply.ElapsedMillis)
	return nil
}

func (s *ServerService) Cancel(args CancelArgs, reply *CancelReply) error {
	key := idPidKey(args.MachineID, args.PID)
	if sess, ok := s.sessions.get(key); ok {
		sess.mu.Lock()
		sess.stoppedBy = "client"
		if sess.cancel != nil {
			sess.cancel()
		}
		if sess.proc != nil {
			_ = signalGroup(sess.proc.Pid, syscall.SIGTERM)
		}
		sess.mu.Unlock()
		reply.OK = true
		infof("Cancel: key=%s acknowledged", key)
		return nil
	}
	reply.OK = false
	warnf("Cancel: key=%s not found", key)
	return nil
}

/* ===========================
   Utilities (server)
   =========================== */

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		fi, err := os.Lstat(path)
		if err == nil && (fi.Mode()&os.ModeSocket) != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("timed out waiting for socket %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func signalGroup(pid int, sig syscall.Signal) error {
	// negative pid => signal process group
	return syscall.Kill(-pid, sig)
}

// mergeEnv merges osEnv -> serverBase -> clientOverlay (client wins on conflicts).
func mergeEnv(osEnv []string, serverBase []string, clientOverlay []string) []string {
	m := make(map[string]string, len(osEnv)+len(serverBase)+len(clientOverlay))
	for _, kv := range osEnv {
		if k, v, ok := splitEnvKV(kv); ok {
			m[k] = v
		}
	}
	for _, kv := range serverBase {
		if k, v, ok := splitEnvKV(kv); ok {
			m[k] = v
		}
	}
	for _, kv := range clientOverlay {
		if k, v, ok := splitEnvKV(kv); ok {
			m[k] = v
		}
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func splitEnvKV(kv string) (string, string, bool) {
	i := strings.IndexByte(kv, '=')
	if i <= 0 {
		return "", "", false
	}
	return kv[:i], kv[i+1:], true
}

func defaultPATH() string {
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

func workDirOrCwd(workDir string) string {
	if workDir != "" {
		return workDir
	}
	wd, _ := os.Getwd()
	return wd
}

func resolveCommandPath(cmdStr, workDir string, env []string) (string, int, error) {
	// Build PATH map from env
	var PATH string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			PATH = strings.TrimPrefix(kv, "PATH=")
			break
		}
	}
	if PATH == "" {
		PATH = defaultPATH()
	}

	var candidate string
	if filepath.IsAbs(cmdStr) {
		candidate = filepath.Clean(cmdStr)
	} else if strings.ContainsRune(cmdStr, '/') {
		candidate = filepath.Clean(filepath.Join(workDir, cmdStr))
	} else {
		// search PATH
		for _, dir := range filepath.SplitList(PATH) {
			dirToUse := dir
			if dirToUse == "" {
				dirToUse = workDir
			} else if !filepath.IsAbs(dirToUse) {
				dirToUse = filepath.Join(workDir, dirToUse)
			}
			try := filepath.Join(dirToUse, cmdStr)
			if isExecutableFile(try) {
				candidate = try
				break
			}
		}
		if candidate == "" {
			return "", 127, fmt.Errorf("command %q not found in PATH", cmdStr)
		}
	}

	// Resolve symlinks
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// If not found after resolving, treat as not found
		return "", 127, fmt.Errorf("resolve symlinks: %v", err)
	}
	// Must be absolute
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Clean(filepath.Join(workDir, resolved))
	}

	// Validate
	if !exists(resolved) {
		return "", 127, fmt.Errorf("no such file: %s", resolved)
	}
	if isDir(resolved) {
		return "", 126, fmt.Errorf("is a directory: %s", resolved)
	}
	if !isExecutableFile(resolved) {
		return "", 126, fmt.Errorf("not executable: %s", resolved)
	}
	return resolved, 0, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	if fi.IsDir() {
		return false
	}
	mode := fi.Mode().Perm()
	return (mode & 0111) != 0
}

func exitCodeFromWaitErr(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// On Unix, extract wait status
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
	}
	// Fallback (treat as generic failure)
	return 1
}

/* ===========================
   Server runner
   =========================== */

func runServer() {
	if flagRoot == "" || flagMode != "server" || flagName == "" {
		fmt.Fprintf(os.Stderr, "usage: %s -root <path> -mode server -name <name> [-startdir DIR] [--env ...]\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	absRoot, err := filepath.Abs(flagRoot)
	if err != nil {
		errorf("invalid root: %v", err)
		os.Exit(2)
	}
	// Safety: disallow root "/"
	if absRoot == "/" {
		errorf("refusing to use root=/")
		os.Exit(2)
	}

	// Build server base env from flags (validate "--env NAME" exists in server env)
	serverBase := make([]string, 0, len(flagEnvs))
	for _, e := range flagEnvs {
		if strings.Contains(e, "=") {
			serverBase = append(serverBase, e)
			continue
		}
		// bare NAME: must exist
		val, ok := os.LookupEnv(e)
		if !ok {
			errorf("--env %s requested but not present in server environment", e)
			os.Exit(2)
		}
		serverBase = append(serverBase, e+"="+val)
	}
	// Reset/prepare root
	_ = os.RemoveAll(absRoot)
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		errorf("mkdir root: %v", err)
		os.Exit(2)
	}
	infof("server root prepared: %s", absRoot)

	// Optional chdir at startup
	if flagStartDir != "" {
		if err := os.Chdir(flagStartDir); err != nil {
			errorf("server chdir %q: %v", flagStartDir, err)
			os.Exit(2)
		}
		cwd, _ := os.Getwd()
		infof("server cwd: %s", cwd)
	}

	// Main RPC socket
	mainSock := filepath.Join(absRoot, flagName+".sock")
	_ = os.Remove(mainSock)
	l, err := net.Listen("unix", mainSock)
	if err != nil {
		errorf("listen %s: %v", mainSock, err)
		os.Exit(1)
	}
	fmt.Println("Server listening on", mainSock)

	sessions := newSessionTable()
	svc := &ServerService{
		sessions:      sessions,
		root:          absRoot,
		serverBaseEnv: serverBase,
	}
	if err := rpc.Register(svc); err != nil {
		errorf("rpc register: %v", err)
		os.Exit(1)
	}

	// Graceful shutdown
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigc
		warnf("server signal: %v; canceling all sessions", sig)
		sessions.cancelAll("server")
		_ = l.Close()
	}()

	// Accept loop
	for {
		conn, err := l.Accept()
		if err != nil {
			break // listener closed
		}
		go rpc.ServeCodec(jsonrpc.NewServerCodec(conn))
	}

	// Wait for in-flight Process calls
	svc.wg.Wait()
	_ = os.Remove(mainSock)
	infof("server shutdown complete")
}

/* ===========================
   Client runner
   =========================== */

func runClient() {
	if flagRoot == "" || flagMode != "client" || flagName == "" {
		fmt.Fprintf(os.Stderr, "usage: %s -root <path> -mode client -name <name> [-startdir DIR] [-stdin STR|-stdinfile PATH] [--env ...] [-id ID] [-verbose] -- [COMMAND [ARGS...]]\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	if flagStdinStr != "" && flagStdinFile != "" {
		errorf("-stdin and -stdinfile are mutually exclusive")
		os.Exit(2)
	}

	machineID := loadMachineID(flagID)
	pid := os.Getpid()

	absRoot, err := filepath.Abs(flagRoot)
	if err != nil {
		errorf("invalid root: %v", err)
		os.Exit(2)
	}
	// Build per-request env from flags (validate "--env NAME" exists in client env)
	clientOverlay := make([]string, 0, len(flagEnvs))
	for _, e := range flagEnvs {
		if strings.Contains(e, "=") {
			clientOverlay = append(clientOverlay, e)
			continue
		}
		val, ok := os.LookupEnv(e)
		if !ok {
			errorf("--env %s requested but not present in client environment", e)
			os.Exit(2)
		}
		clientOverlay = append(clientOverlay, e+"="+val)
	}

	// Prepare callback sockets dir
	dir := deriveClientSocketDir(absRoot, machineID, pid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		errorf("mkdir %s: %v", dir, err)
		os.Exit(2)
	}
	stdoutSock, stderrSock, stdinSock := deriveClientSockets(absRoot, machineID, pid)
	_ = os.Remove(stdoutSock)
	_ = os.Remove(stderrSock)
	_ = os.Remove(stdinSock)

	// Start stdout/stderr services
	stdoutReady := make(chan struct{})
	stdoutL, err := serveOnSocket(stdoutSock, "Stdout", &StdoutService{sink: newReorderSink(os.Stdout)}, stdoutReady)
	if err != nil {
		errorf("serve stdout: %v", err)
		os.Exit(1)
	}
	defer stdoutL.Close()
	<-stdoutReady

	stderrReady := make(chan struct{})
	stderrL, err := serveOnSocket(stderrSock, "Stderr", &StderrService{sink: newReorderSink(os.Stderr)}, stderrReady)
	if err != nil {
		errorf("serve stderr: %v", err)
		os.Exit(1)
	}
	defer stderrL.Close()
	<-stderrReady

	// Prepare stdin service
	var stdinReader io.Reader
	if flagStdinFile != "" {
		f, err := os.Open(flagStdinFile)
		if err != nil {
			errorf("open stdinfile: %v", err)
			os.Exit(2)
		}
		defer f.Close()
		stdinReader = f
	} else if flagStdinStr != "" {
		stdinReader = bytes.NewReader([]byte(flagStdinStr))
	} else {
		stdinReader = nil
	}
	stdinReady := make(chan struct{})
	stdinL, err := serveOnSocket(stdinSock, "Stdin", &StdinService{r: stdinReader}, stdinReady)
	if err != nil {
		errorf("serve stdin: %v", err)
		os.Exit(1)
	}
	defer stdinL.Close()
	<-stdinReady

	// Connect to server
	mainSock := filepath.Join(absRoot, flagName+".sock")
	conn, err := net.Dial("unix", mainSock)
	if err != nil {
		errorf("dial server: %v", err)
		os.Exit(1)
	}
	defer conn.Close()
	client := jsonrpc.NewClient(conn)

	// Ctrl-C handling: send Cancel and then wait for Process to return
	localSig := make(chan os.Signal, 1)
	signal.Notify(localSig, syscall.SIGINT, syscall.SIGTERM)
	var cancelOnce sync.Once
	go func() {
		<-localSig
		cancelOnce.Do(func() {
			if c2, err := net.Dial("unix", mainSock); err == nil {
				defer c2.Close()
				cc := jsonrpc.NewClient(c2)
				_ = cc.Call("ServerService.Cancel", CancelArgs{MachineID: machineID, PID: pid}, &CancelReply{})
			}
		})
	}()

	// Command & args (positional after flags)
	cmdline := flag.Args()
	var command string
	var cmdArgs []string
	if len(cmdline) > 0 {
		command = cmdline[0]
		if len(cmdline) > 1 {
			cmdArgs = cmdline[1:]
		}
	} else {
		// empty => ping
		command = ""
		cmdArgs = nil
	}

	// Client timing
	reqStart := time.Now().UTC()

	// RPC call
	var resp ProcessReply
	err = client.Call("ServerService.Process", ProcessArgs{
		MachineID: machineID,
		PID:       pid,
		StartDir:  flagStartDir,
		Command:   command,
		Args:      cmdArgs,
		Env:       clientOverlay,
	}, &resp)

	reqEnd := time.Now().UTC()

	// Cleanup sockets dir
	_ = os.Remove(stdoutSock)
	_ = os.Remove(stderrSock)
	_ = os.Remove(stdinSock)
	_ = os.Remove(dir)

	if err != nil {
		errorf("rpc error: %v", err)
		// Unknown server failure → mimic rc=1
		os.Exit(1)
	}

	// If server provided an error message (setup/exec failure), show it on stderr
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, resp.Error)
	}

	// Verbose summary
	if flagVerbose {
		rtt := reqEnd.Sub(reqStart).Milliseconds()
		overhead := rtt - resp.ElapsedMillis
		if overhead < 0 {
			overhead = 0
		}
		cmdShown := resp.ResolvedCmdLine
		if cmdShown == "" {
			// For ping or errors, reconstruct reasonable view
			cmdShown = strings.Join(append([]string{command}, cmdArgs...), " ")
			if strings.TrimSpace(command) == "" {
				cmdShown = "(ping)"
			}
		}
		fmt.Printf(
			"command=%q client_start=%s client_end=%s rtt_ms=%d server_start=%s server_end=%s exec_ms=%d overhead_ms=%d rc=%d stopped=%v stopped_by=%q\n",
			cmdShown,
			reqStart.Format(time.RFC3339Nano),
			reqEnd.Format(time.RFC3339Nano),
			rtt,
			resp.ExecStartRFC3339,
			resp.ExecEndRFC3339,
			resp.ElapsedMillis,
			overhead,
			resp.ReturnCode,
			resp.Stopped,
			resp.StoppedBy,
		)
	}

	// Exit with server-provided return code
	os.Exit(resp.ReturnCode)
}

/* ===========================
   Socket server helper (client side)
   =========================== */

func serveOnSocket(sockPath, serviceName string, svc any, ready chan<- struct{}) (net.Listener, error) {
	_ = os.Remove(sockPath)
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	if err := rpc.RegisterName(serviceName, svc); err != nil {
		l.Close()
		return nil, err
	}
	go func() {
		close(ready)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go rpc.ServeCodec(jsonrpc.NewServerCodec(conn))
		}
	}()
	return l, nil
}

/* ===========================
   main
   =========================== */

func main() {
	flag.Parse()

	// Randomize math/rand once (used minimally)
	mrand.Seed(time.Now().UnixNano())

	switch flagMode {
	case "server":
		runServer()
	case "client":
		runClient()
	default:
		fmt.Fprintf(os.Stderr, "usage: %s -root <path> -mode server|client -name <name> [other flags]\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
}
