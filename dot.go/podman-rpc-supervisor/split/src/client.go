package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
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
	flagRoot      string
	flagName      string
	flagStartDir  string
	flagStdinStr  string
	flagStdinFile string
	flagEnvs      envList
	flagID        string
	flagVerbose   bool
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
	flag.StringVar(&flagName, "name", "", "client target name (connects to <root>/<name>.sock) (REQUIRED)")
	flag.StringVar(&flagStartDir, "startdir", "", "client: per-request working directory")
	flag.StringVar(&flagStdinStr, "stdin", "", "client: literal stdin data (mutually exclusive with -stdinfile)")
	flag.StringVar(&flagStdinFile, "stdinfile", "", "client: path to file used as stdin (mutually exclusive with -stdin)")
	flag.Var(&flagEnvs, "env", "repeatable env (KEY=VAL or KEY) for per-request overlay (repeat)")
	flag.StringVar(&flagID, "id", "", "machine ID override (else /etc/machine-id; else random)")
	flag.BoolVar(&flagVerbose, "verbose", false, "print timing/summary (stderr) and still exit with server return code")
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

func (s *reorderSink) write(line csjrpc.Line) {
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

func (s *StdoutService) WriteLine(in csjrpc.Line, _ *struct{}) error { s.sink.write(in); return nil }
func (s *StderrService) WriteLine(in csjrpc.Line, _ *struct{}) error { s.sink.write(in); return nil }

// Stdin: pull-based RPC
type StdinService struct {
	mu      sync.Mutex
	r       io.Reader // from -stdin or -stdinfile; may be nil
	aborted bool      // force EOF immediately (e.g., on client Ctrl-C)
}

// Abort forces subsequent ReadChunk calls to return EOF immediately.
func (s *StdinService) Abort() {
	s.mu.Lock()
	s.aborted = true
	s.mu.Unlock()
}

// nonblockingReadOnce toggles nonblocking on fd, attempts one read, then restores blocking.
// Returns (data, eof, err, wouldBlock).
func nonblockingReadOnce(f *os.File, max int) ([]byte, bool, error, bool) {
	fd := int(f.Fd())

	// Enable non-blocking, then restore to blocking.
	if err := syscall.SetNonblock(fd, true); err != nil {
		return nil, false, err, false
	}
	defer syscall.SetNonblock(fd, false)

	buf := make([]byte, max)
	n, rerr := syscall.Read(fd, buf)
	if n > 0 {
		return buf[:n], false, nil, false
	}
	if rerr == nil && n == 0 {
		// EOF
		return nil, true, nil, false
	}
	if rerr == syscall.EAGAIN || rerr == syscall.EINTR {
		// No data available right now (or interrupted); report wouldBlock.
		return nil, false, nil, true
	}
	if rerr != nil {
		return nil, false, rerr, false
	}
	return nil, false, nil, false
}

func (s *StdinService) ReadChunk(args csjrpc.StdinReadArgs, reply *csjrpc.StdinReadReply) error {
	max := args.Max
	if max <= 0 {
		max = 64 * 1024
	}
	reply.Data = nil
	reply.EOF = false
	reply.Err = ""

	s.mu.Lock()
	r := s.r
	aborted := s.aborted
	s.mu.Unlock()

	// If we've been told to abort, synthesize EOF immediately.
	if aborted || r == nil {
		reply.EOF = true
		return nil
	}

	// If the reader is an *os.File (e.g., /dev/stdin), do a non-blocking single read.
	if f, ok := r.(*os.File); ok {
		data, eof, err, wouldBlock := nonblockingReadOnce(f, max)
		if err != nil {
			reply.Err = err.Error()
			return nil
		}
		if eof {
			reply.EOF = true
			return nil
		}
		if wouldBlock {
			// No data available right now — return immediately (non-blocking poll).
			return nil
		}
		if len(data) > 0 {
			reply.Data = data
			return nil
		}
	}

	// Fallback: blocking read on non-file readers (e.g., bytes.Reader).
	buf := make([]byte, max)
	n, err := r.Read(buf)
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
   Client runner
   =========================== */

func serveOnSocket(sockPath, serviceName string, svc any, ready chan<- struct{}) (net.Listener, error) {
	if _, err := os.Lstat(sockPath); err == nil {
		return nil, fmt.Errorf("refusing to overwrite existing socket: %s", sockPath)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat socket %s: %w", sockPath, err)
	}
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

func main() {
	flag.Parse()
	if flagRoot == "" || flagName == "" {
		fmt.Fprintf(os.Stderr, "usage: %s -root <path> -name <name> [-startdir DIR] [-stdin STR|-stdinfile PATH] [--env ...] [-id ID] [-verbose] -- [COMMAND [ARGS...]]\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	if flagStdinStr != "" && flagStdinFile != "" {
		csjrpc.Errorf("-stdin and -stdinfile are mutually exclusive")
		os.Exit(2)
	}

	machineID := csjrpc.LoadMachineID(flagID)
	pid := os.Getpid()

	absRoot, err := filepath.Abs(flagRoot)
	if err != nil {
		csjrpc.Errorf("invalid root: %v", err)
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
			csjrpc.Errorf("--env %s requested but not present in client environment", e)
			os.Exit(2)
		}
		clientOverlay = append(clientOverlay, e+"="+val)
	}

	// Prepare callback sockets dir
	dir := csjrpc.DeriveClientSocketDir(absRoot, machineID, pid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		csjrpc.Errorf("mkdir %s: %v", dir, err)
		os.Exit(2)
	}
	stdoutSock, stderrSock, stdinSock := csjrpc.DeriveClientSockets(absRoot, machineID, pid)

	// Start stdout/stderr services
	stdoutReady := make(chan struct{})
	stdoutL, err := serveOnSocket(stdoutSock, "Stdout", &StdoutService{sink: newReorderSink(os.Stdout)}, stdoutReady)
	if err != nil {
		csjrpc.Errorf("serve stdout: %v", err)
		os.Exit(1)
	}
	defer stdoutL.Close()
	<-stdoutReady

	stderrReady := make(chan struct{})
	stderrL, err := serveOnSocket(stderrSock, "Stderr", &StderrService{sink: newReorderSink(os.Stderr)}, stderrReady)
	if err != nil {
		csjrpc.Errorf("serve stderr: %v", err)
		os.Exit(1)
	}
	defer stderrL.Close()
	<-stderrReady

	// Prepare stdin service
	var stdinReader io.Reader
	if flagStdinFile != "" {
		f, err := os.Open(flagStdinFile)
		if err != nil {
			csjrpc.Errorf("open stdinfile: %v", err)
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
	stdinSvc := &StdinService{r: stdinReader}
	stdinL, err := serveOnSocket(stdinSock, "Stdin", stdinSvc, stdinReady)
	if err != nil {
		csjrpc.Errorf("serve stdin: %v", err)
		os.Exit(1)
	}
	defer stdinL.Close()
	<-stdinReady

	// Connect to server
	mainSock := filepath.Join(absRoot, flagName+".sock")
	conn, err := net.Dial("unix", mainSock)
	if err != nil {
		csjrpc.Errorf("dial server: %v", err)
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
			// Ensure stdin immediately reports EOF to the server so the pump doesn't wait for a newline.
			stdinSvc.Abort()

			if c2, err := net.Dial("unix", mainSock); err == nil {
				defer c2.Close()
				cc := jsonrpc.NewClient(c2)
				_ = cc.Call("ServerService.Cancel", csjrpc.CancelArgs{MachineID: machineID, PID: pid}, &csjrpc.CancelReply{})
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
	var resp csjrpc.ProcessReply
	err = client.Call("ServerService.Process", csjrpc.ProcessArgs{
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
		csjrpc.Errorf("rpc error: %v", err)
		// Unknown server failure → mimic rc=1
		os.Exit(1)
	}

	// If server provided an error message (setup/exec failure), show it on stderr
	if resp.Error != "" {
		csjrpc.Errorf("%s", resp.Error)
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
		csjrpc.Infof(
			"command=%q client_start=%s client_end=%s rtt_ms=%d server_start=%s server_end=%s exec_ms=%d overhead_ms=%d rc=%d stopped=%v stopped_by=%q",
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
