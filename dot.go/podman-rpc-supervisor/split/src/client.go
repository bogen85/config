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

var (
	flagRoot      string
	flagName      string
	flagStartDir  string
	flagStdinStr  string
	flagStdinFile string
	flagEnvs      envList
	flagID        string
	flagSummary   bool
	flagConfig    string
	flagServer    bool
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

type StdoutService struct{ sink *reorderSink }
type StderrService struct{ sink *reorderSink }

func (s *StdoutService) WriteLine(in csjrpc.Line, _ *struct{}) error { s.sink.write(in); return nil }
func (s *StderrService) WriteLine(in csjrpc.Line, _ *struct{}) error { s.sink.write(in); return nil }

type StdinService struct {
	mu sync.Mutex
	r  io.Reader
}

func (s *StdinService) ReadChunk(args csjrpc.StdinReadArgs, reply *csjrpc.StdinReadReply) error {
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

func serveOnSocket(sockPath, serviceName string, svc any, ready chan<- struct{}) (net.Listener, error) {
	if _, err := os.Lstat(sockPath); err == nil {
		return nil, fmt.Errorf("refusing to overwrite existing socket: %s", sockPath)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat socket %s: %v", sockPath, err)
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
	flag.StringVar(&flagRoot, "root", "", "socket root path (REQUIRED if not provided in config.common.root)")
	flag.StringVar(&flagName, "name", "", "server socket name to connect to (REQUIRED if not provided in config.common.name)")
	flag.StringVar(&flagStartDir, "startdir", "", "per-request working directory")
	flag.StringVar(&flagStdinStr, "stdin", "", "literal stdin data (mutually exclusive with -stdinfile)")
	flag.StringVar(&flagStdinFile, "stdinfile", "", "path to file used as stdin (mutually exclusive with -stdin)")
	flag.Var(&flagEnvs, "env", "repeatable env (KEY=VAL or KEY) overlay for child process (repeat)")
	flag.StringVar(&flagID, "id", "", "machine ID override (else /etc/machine-id; else random; can come from config.client.id)")
	flag.BoolVar(&flagSummary, "summary", false, "emit execution summary via logger (can be enabled by config.client.summary)")
	flag.StringVar(&flagConfig, "config", "", "path to JSON config (optional; default ./config.json or $CSJRPC_CONFIG). If provided and missing, it's an error.")
	flag.BoolVar(&flagServer, "server", false, "admin mode: run a server command instead of executing a process")
	flag.Parse()

	if flagStdinStr != "" && flagStdinFile != "" {
		csjrpc.Errorf("-stdin and -stdinfile are mutually exclusive")
		os.Exit(2)
	}

	cfgPath := csjrpc.DefaultConfigPath
	if envPath := os.Getenv(csjrpc.ClientConfigEnv); envPath != "" {
		cfgPath = envPath
	}
	if flagConfig != "" {
		cfgPath = flagConfig
	}
	cfg, found, err := csjrpc.LoadConfig(cfgPath)
	if err != nil {
		if flagConfig != "" {
			csjrpc.Errorf("load config %q: %v", cfgPath, err)
			os.Exit(2)
		}
		found = false
	}
	if flagConfig != "" && !found {
		csjrpc.Errorf("config file not found: %s", cfgPath)
		os.Exit(2)
	}

	root := cfg.Common.Root
	name := cfg.Common.Name
	if flagRoot != "" {
		root = flagRoot
	}
	if flagName != "" {
		name = flagName
	}
	if root == "" || name == "" {
		fmt.Fprintf(os.Stderr, "usage: %s -root <path> -name <name> [-startdir DIR] [-stdin STR|-stdinfile PATH] [--env ...] [-id ID] [-summary] [-config PATH]\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}

	machineID := flagID
	if machineID == "" {
		if cfg.Client.ID != "" {
			machineID = cfg.Client.ID
		} else {
			machineID = csjrpc.LoadMachineID("")
		}
	} else {
		if _, err := csjrpc.SanitizeMachineID(machineID); err != nil {
			csjrpc.Errorf("invalid --id: %v", err)
			os.Exit(2)
		}
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		csjrpc.Errorf("invalid root: %v", err)
		os.Exit(2)
	}

	clientOverlay := csjrpc.EnvMapToList(cfg.Client.Env)
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

	pid := os.Getpid()
	dir := csjrpc.DeriveClientSocketDir(absRoot, machineID, pid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		csjrpc.Errorf("mkdir %s: %v", dir, err)
		os.Exit(2)
	}
	stdoutSock, stderrSock, stdinSock := csjrpc.DeriveClientSockets(absRoot, machineID, pid)
	for _, s := range []string{stdoutSock, stderrSock, stdinSock} {
		if _, err := os.Lstat(s); err == nil {
			csjrpc.Errorf("refusing to overwrite existing socket: %s", s)
			os.Exit(2)
		} else if !os.IsNotExist(err) {
			csjrpc.Errorf("stat socket %s: %v", s, err)
			os.Exit(2)
		}
	}

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
	stdinL, err := serveOnSocket(stdinSock, "Stdin", &StdinService{r: stdinReader}, stdinReady)
	if err != nil {
		csjrpc.Errorf("serve stdin: %v", err)
		os.Exit(1)
	}
	defer stdinL.Close()
	<-stdinReady

	mainSock := filepath.Join(absRoot, name+".sock")
	conn, err := net.Dial("unix", mainSock)
	if err != nil {
		csjrpc.Errorf("dial server: %v", err)
		os.Exit(1)
	}
	defer conn.Close()
	client := jsonrpc.NewClient(conn)

	// Admin mode
	if flagServer {
		reqStart := time.Now().UTC()
		var arep csjrpc.AdminReply
		cmdline := flag.Args()
		cmd := ""
		var aargs []string
		if len(cmdline) > 0 {
			cmd = cmdline[0]
			aargs = cmdline[1:]
		} else {
			cmd = "ls"
		}
		err = client.Call("ServerService.Admin", csjrpc.AdminArgs{
			MachineID: machineID,
			PID:       pid,
			Command:   cmd,
			Args:      aargs,
		}, &arep)
		reqEnd := time.Now().UTC()
		if err != nil {
			csjrpc.Errorf("rpc error: %v", err)
			os.Exit(1)
		}
		finalSummary := flagSummary || cfg.Client.Summary
		if finalSummary {
			rtt := reqEnd.Sub(reqStart).Milliseconds()
			csjrpc.Infof("admin=%q args=%q rtt_ms=%d rc=%d err=%q", cmd, strings.Join(aargs, " "), rtt, arep.ReturnCode, arep.Error)
		}
		if arep.Error != "" {
			fmt.Fprintln(os.Stderr, arep.Error)
		}
		os.Exit(arep.ReturnCode)
	}

	// Process mode
	localSig := make(chan os.Signal, 1)
	signal.Notify(localSig, syscall.SIGINT, syscall.SIGTERM)
	var cancelOnce sync.Once
	go func() {
		<-localSig
		cancelOnce.Do(func() {
			if c2, err := net.Dial("unix", mainSock); err == nil {
				defer c2.Close()
				cc := jsonrpc.NewClient(c2)
				_ = cc.Call("ServerService.Cancel", csjrpc.CancelArgs{MachineID: machineID, PID: pid}, &csjrpc.CancelReply{})
			}
		})
	}()

	cmdline := flag.Args()
	var command string
	var cmdArgs []string
	if len(cmdline) > 0 {
		command = cmdline[0]
		if len(cmdline) > 1 {
			cmdArgs = cmdline[1:]
		}
	} else {
		command = ""
		cmdArgs = nil
	}

	reqStart := time.Now().UTC()
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

	_ = os.Remove(stdoutSock)
	_ = os.Remove(stderrSock)
	_ = os.Remove(stdinSock)
	_ = os.Remove(dir)

	if err != nil {
		csjrpc.Errorf("rpc error: %v", err)
		os.Exit(1)
	}

	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, resp.Error)
	}

	finalSummary := flagSummary || cfg.Client.Summary
	if finalSummary {
		rtt := reqEnd.Sub(reqStart).Milliseconds()
		overhead := rtt - resp.ElapsedMillis
		if overhead < 0 {
			overhead = 0
		}
		cmdShown := resp.ResolvedCmdLine
		if cmdShown == "" {
			cmdShown = strings.Join(append([]string{command}, cmdArgs...), " ")
			if strings.TrimSpace(command) == "" {
				cmdShown = "(ping)"
			}
		}
		csjrpc.Infof("command=%q client_start=%s client_end=%s rtt_ms=%d server_start=%s server_end=%s exec_ms=%d overhead_ms=%d rc=%d stopped=%v stopped_by=%q",
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

	os.Exit(resp.ReturnCode)
}
