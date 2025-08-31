package csjrpc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ===== Config =====

const DefaultConfigPath = "./config.json"   // current directory default
const ClientConfigEnv = "CSJRPC_CONFIG"     // client-only env override

type CommonConfig struct {
	Root string `json:"root"`
	Name string `json:"name"`
}

type ServerSection struct {
	StartDir string            `json:"startdir"`
	Env      map[string]string `json:"env"`
}

type ClientSection struct {
	Env     map[string]string `json:"env"`
	ID      string            `json:"id"`
	Summary bool              `json:"summary"`
}

type Config struct {
	Common CommonConfig `json:"common"`
	Server ServerSection `json:"server"`
	Client ClientSection `json:"client"`
}

// LoadConfig reads a JSON config file. If the path is empty, DefaultConfigPath is used.
// Returns (cfg, found, err). found=false when the file does not exist.
func LoadConfig(path string) (Config, bool, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	var cfg Config
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, false, nil
		}
		return cfg, false, err
	}
	if st.IsDir() {
		return cfg, false, fmt.Errorf("config path is a directory: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, false, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, false, err
	}
	return cfg, true, nil
}

// EnvMapToList converts {"K":"V"} to []{"K=V"} (sorted order not guaranteed).
func EnvMapToList(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// ===== Logging (ts + file:line) =====

func logWithCaller(level, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	// skip 2 frames so file:line points to the original caller in server/client.
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		file = "?"
		line = 0
	}
	base := filepath.Base(file)
	fmt.Fprintf(os.Stderr, "%s [%s] %s:%d: %s\n", ts, strings.ToUpper(level), base, line, fmt.Sprintf(msg, args...))
}

func Infof(msg string, args ...any)  { logWithCaller("info", msg, args...) }
func Warnf(msg string, args ...any)  { logWithCaller("warn", msg, args...) }
func Errorf(msg string, args ...any) { logWithCaller("error", msg, args...) }

// ===== Shared RPC payloads =====

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
	ReturnCode       int
	Error            string
	Stopped          bool
	StoppedBy        string // "", "client", "server"
	ExecStartRFC3339 string
	ExecEndRFC3339   string
	ElapsedMillis    int64
	ResolvedCmdLine  string // e.g. "/abs/path arg1 arg2"
}

type CancelArgs struct {
	MachineID string
	PID       int
}
type CancelReply struct {
	OK bool
}

// Stdin pull-RPC types
type StdinReadArgs struct{ Max int }
type StdinReadReply struct {
	Data []byte
	EOF  bool
	Err  string
}

// ===== ProcessReply helpers (error population) =====

// FailAt sets an error reply using a provided start time (UTC recommended).
// It mirrors the common early-error shape used by the server: start=end, zero elapsed,
// return code rc, message msg, and clears ResolvedCmdLine.
func (r *ProcessReply) FailAt(rc int, msg string, start time.Time) {
	r.ReturnCode = rc
	r.Error = msg
	startUTC := start.UTC()
	r.ExecStartRFC3339 = startUTC.Format(time.RFC3339Nano)
	r.ExecEndRFC3339 = startUTC.Format(time.RFC3339Nano)
	r.ElapsedMillis = 0
	r.ResolvedCmdLine = ""
}

// FailNow is a convenience for FailAt with start = time.Now().
func (r *ProcessReply) FailNow(rc int, msg string) {
	now := time.Now().UTC()
	r.FailAt(rc, msg, now)
}

// FailErrAt/FailErrNow are conveniences that accept error values.
func (r *ProcessReply) FailErrAt(rc int, err error, start time.Time) {
	if err == nil {
		r.FailAt(rc, "", start)
		return
	}
	r.FailAt(rc, err.Error(), start)
}
func (r *ProcessReply) FailErrNow(rc int, err error) {
	now := time.Now().UTC()
	r.FailErrAt(rc, err, now)
}

// ===== Identity & paths =====

func SanitizeMachineID(s string) (string, error) {
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

func LoadMachineID(override string) string {
	if override != "" {
		id, err := SanitizeMachineID(override)
		if err != nil {
			Errorf("invalid --id: %v", err)
			os.Exit(2)
		}
		return id
	}
	data, err := os.ReadFile("/etc/machine-id")
	if err == nil {
		id := strings.TrimSpace(string(data))
		id, err = SanitizeMachineID(id)
		if err == nil && id != "" {
			return id
		}
	}
	// fallback random
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	Warnf("machine-id not found; using random id=%s", id)
	return id
}

func IdPidKey(machineID string, pid int) string {
	return machineID + ":" + strconv.Itoa(pid)
}

func DeriveClientSocketDir(root, machineID string, pid int) string {
	return filepath.Join(root, machineID, strconv.Itoa(pid))
}

func DeriveClientSockets(root, machineID string, pid int) (stdoutSock, stderrSock, stdinSock string) {
	dir := DeriveClientSocketDir(root, machineID, pid)
	return filepath.Join(dir, "stdout.sock"),
		filepath.Join(dir, "stderr.sock"),
		filepath.Join(dir, "stdin.sock")
}
