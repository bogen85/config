package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"local/csjrpc"
)

/* ===========================
   Flags / CLI
   =========================== */

var (
	flagRoot     string
	flagName     string
	flagStartDir string
	flagEnvs     envList
)

type envList []string

func (e *envList) String() string { return strings.Join(*e, ",") }
func (e *envList) Set(s string) error {
	if s == "" {
		return errors.New("empty env entry")
	}
	*e = append(*e, s)
	return nil
}

func init() {
	flag.StringVar(&flagRoot, "root", "", "socket root path (REQUIRED)")
	flag.StringVar(&flagName, "name", "", "server socket name (listens on <root>/<name>.sock) (REQUIRED)")
	flag.StringVar(&flagStartDir, "startdir", "", "server: chdir on startup")
	flag.Var(&flagEnvs, "env", "repeatable env (KEY=VAL or KEY) for server base environment (repeat)")
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
	sessions      *sessionTable
	wg            sync.WaitGroup
	root          string
	serverBaseEnv []string // KEY=VAL, built at startup per --env
}

func (s *ServerService) Process(args csjrpc.ProcessArgs, reply *csjrpc.ProcessReply) error {
	s.wg.Add(1)
	defer s.wg.Done()

	key := csjrpc.IdPidKey(args.MachineID, args.PID)
	csjrpc.Infof("Process start: key=%s cmd=%q args=%d startdir=%q", key, args.Command, len(args.Args), args.StartDir)

	// Validate StartDir if provided
	workDir := args.StartDir
	if workDir != "" {
		if !filepath.IsAbs(workDir) {
			wd, _ := os.Getwd()
			workDir = filepath.Join(wd, workDir)
		}
		if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
			reply.ReturnCode = 2
			reply.Error = fmt.Sprintf("invalid startdir: %q", args.StartDir)
			reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
			reply.ExecEndRFC3339 = reply.ExecStartRFC3339
			reply.ElapsedMillis = 0
			csjrpc.Errorf("key=%s startdir invalid: %v", key, args.StartDir)
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

	stdoutSock, stderrSock, stdinSock := csjrpc.DeriveClientSockets(s.root, args.MachineID, args.PID)

	// Wait for client sockets to appear (up to a few seconds)
	if err := waitForSocket(stdoutSock, 5*time.Second); err != nil {
		reply.ReturnCode = 2
		reply.Error = "stdout socket not available: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		csjrpc.Errorf("key=%s stdout socket error: %v", key, err)
		return nil
	}
	if err := waitForSocket(stderrSock, 5*time.Second); err != nil {
		reply.ReturnCode = 2
		reply.Error = "stderr socket not available: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		csjrpc.Errorf("key=%s stderr socket error: %v", key, err)
		return nil
	}
	if err := waitForSocket(stdinSock, 5*time.Second); err != nil {
		reply.ReturnCode = 2
		reply.Error = "stdin socket not available: " + err.Error()
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		csjrpc.Errorf("key=%s stdin socket error: %v", key, err)
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
		csjrpc.Errorf("key=%s stdout dial: %v", key, err)
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
		csjrpc.Errorf("key=%s stderr dial: %v", key, err)
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
		csjrpc.Errorf("key=%s stdin dial: %v", key, err)
		return nil
	}
	defer stdinConn.Close()
	stdinCli := jsonrpc.NewClient(stdinConn)

	// Ping behavior (empty command)
	if strings.TrimSpace(args.Command) == "" {
		_ = stdoutCli.Call("Stdout.WriteLine", csjrpc.Line{Index: 0, Text: "server ping to stdout"}, &struct{}{})
		_ = stderrCli.Call("Stderr.WriteLine", csjrpc.Line{Index: 0, Text: "server ping to stderr"}, &struct{}{})
		reply.ReturnCode = 0
		reply.ExecStartRFC3339 = time.Now().UTC().Format(time.RFC3339Nano)
		reply.ExecEndRFC3339 = reply.ExecStartRFC3339
		reply.ElapsedMillis = 0
		reply.ResolvedCmdLine = "(ping)"
		csjrpc.Infof("key=%s ping handled", key)
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
		csjrpc.Errorf("key=%s resolve command %q failed: %v", key, args.Command, err)
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
	// Signal for goroutines when the child process has exited.
	procDone := make(chan struct{})

	wgIO.Add(3)

	// stdout pump
	go func() {
		defer wgIO.Done()
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		idx := 0
		for sc.Scan() {
			_ = stdoutCli.Call("Stdout.WriteLine", csjrpc.Line{Index: idx, Text: sc.Text()}, &struct{}{})
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
			_ = stderrCli.Call("Stderr.WriteLine", csjrpc.Line{Index: idx, Text: sc.Text()}, &struct{}{})
			idx++
		}
	}()

	// stdin pump (pull from client's stdin service)
	go func() {
		defer wgIO.Done()
		const chunk = 64 * 1024
		for {
			// Fast exit on cancel or when the child process has exited,
			// even if the client hasn't provided EOF on stdin.
			select {
			case <-ctx.Done():
				_ = stdinWriter.Close()
				return
			case <-procDone:
				_ = stdinWriter.Close()
				return
			default:
			}

			var rep csjrpc.StdinReadReply
			err := stdinCli.Call("Stdin.ReadChunk", csjrpc.StdinReadArgs{Max: chunk}, &rep)
			if err != nil {
				// RPC broke (client closed, etc.) -> stop and close child's stdin.
				break
			}
			if rep.Err != "" {
				// Client-side read error -> stop.
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
			// No data and not EOF: avoid a busy loop when client has nothing ready.
			if len(rep.Data) == 0 {
				time.Sleep(10 * time.Millisecond)
			}
		}
		_ = stdinWriter.Close()
	}()

	// Cancellation watcher: on ctx.Done, SIGTERM -> wait a bit -> SIGKILL
	done := make(chan struct{})
	go func(pid int) {
		defer close(done)
		<-ctx.Done()
		csjrpc.Infof("key=%s cancel received; signaling process group", key)
		_ = signalGroup(pid, syscall.SIGTERM)
		// small grace period
		time.Sleep(1 * time.Second)
		_ = signalGroup(pid, syscall.SIGKILL)
	}(cmd.Process.Pid)

	waitErr := cmd.Wait()
	execEnd := time.Now().UTC()
	// Notify I/O goroutines the process is gone; ensure stdin writer is shut.
	close(procDone)
	_ = stdinWriter.Close()

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

	csjrpc.Infof("Process end: key=%s rc=%d stopped=%v by=%s elapsed=%dms", key, rc, reply.Stopped, reply.StoppedBy, reply.ElapsedMillis)
	return nil
}

func (s *ServerService) Cancel(args csjrpc.CancelArgs, reply *csjrpc.CancelReply) error {
	key := csjrpc.IdPidKey(args.MachineID, args.PID)
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
		csjrpc.Infof("Cancel: key=%s acknowledged", key)
		return nil
	}
	reply.OK = false
	csjrpc.Warnf("Cancel: key=%s not found", key)
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

func main() {
	// Flags
	flag.Parse()
	if flagRoot == "" || flagName == "" {
		fmt.Fprintf(os.Stderr, "usage: %s -root <path> -name <name> [-startdir DIR] [--env ...]\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	absRoot, err := filepath.Abs(flagRoot)
	if err != nil {
		csjrpc.Errorf("invalid root: %v", err)
		os.Exit(2)
	}
	if absRoot == "/" {
		csjrpc.Errorf("refusing to use root=/")
		os.Exit(2)
	}

	// Build server base env from flags
	serverBase := make([]string, 0, len(flagEnvs))
	for _, e := range flagEnvs {
		if strings.Contains(e, "=") {
			serverBase = append(serverBase, e)
			continue
		}
		val, ok := os.LookupEnv(e)
		if !ok {
			csjrpc.Errorf("--env %s requested but not present in server environment", e)
			os.Exit(2)
		}
		serverBase = append(serverBase, e+"="+val)
	}
	// Ensure root exists (do not clear it)
	if fi, err := os.Stat(absRoot); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(absRoot, 0o755); err != nil {
				csjrpc.Errorf("mkdir root: %v", err)
				os.Exit(2)
			}
			csjrpc.Infof("server root created: %s", absRoot)
		} else {
			csjrpc.Errorf("stat root: %v", err)
			os.Exit(2)
		}
	} else if !fi.IsDir() {
		csjrpc.Errorf("root is not a directory: %s", absRoot)
		os.Exit(2)
	} else {
		csjrpc.Infof("server root: %s", absRoot)
	}

	// Optional chdir at startup
	if flagStartDir != "" {
		if err := os.Chdir(flagStartDir); err != nil {
			csjrpc.Errorf("server chdir %q: %v", flagStartDir, err)
			os.Exit(2)
		}
		cwd, _ := os.Getwd()
		csjrpc.Infof("server cwd: %s", cwd)
	}

	// Main RPC socket (fail if already exists)
	mainSock := filepath.Join(absRoot, flagName+".sock")
	if _, err := os.Lstat(mainSock); err == nil {
		csjrpc.Errorf("refusing to overwrite existing socket: %s", mainSock)
		os.Exit(2)
	} else if !os.IsNotExist(err) {
		csjrpc.Errorf("stat socket %s: %v", mainSock, err)
		os.Exit(2)
	}
	l, err := net.Listen("unix", mainSock)
	if err != nil {
		csjrpc.Errorf("listen %s: %v", mainSock, err)
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
		csjrpc.Errorf("rpc register: %v", err)
		os.Exit(1)
	}

	// Graceful shutdown
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigc
		csjrpc.Warnf("server signal: %v; canceling all sessions", sig)
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
	csjrpc.Infof("server shutdown complete")
}
