package csjrpc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const DefaultConfigPath = "./config.toml"
const ClientConfigEnv = "CSJRPC_CONFIG"

type CommonConfig struct {
	Root string `toml:"root"`
	Name string `toml:"name"`
}

type ServerSection struct {
	StartDir string            `toml:"startdir"`
	Env      map[string]string `toml:"env"`
}

type ClientSection struct {
	Env     map[string]string `toml:"env"`
	ID      string            `toml:"id"`
	Summary bool              `toml:"summary"`
}

type Config struct {
	Common CommonConfig  `toml:"common"`
	Server ServerSection `toml:"server"`
	Client ClientSection `toml:"client"`
}

// LoadConfig loads configuration from TOML file at path.
//   - If path == "" uses DefaultConfigPath
//   - If file does not exist: (zero, false, nil)
//   - If file exists but bad TOML: (zero, true, err)
//   - On success: (cfg, true, nil)
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
	if _, err := toml.Decode(string(b), &cfg); err != nil {
		return cfg, true, err
	}
	return cfg, true, nil
}

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

func logWithCaller(level, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		file = "?"
		line = 0
	}
	base := filepath.Base(file)
	fmt.Fprintf(os.Stderr, "%s [%s] %s:%d: %s\n",
		ts, strings.ToUpper(level), base, line, fmt.Sprintf(msg, args...))
}

func Infof(msg string, args ...any)  { logWithCaller("info", msg, args...) }
func Warnf(msg string, args ...any)  { logWithCaller("warn", msg, args...) }
func Errorf(msg string, args ...any) { logWithCaller("error", msg, args...) }

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
	Env       []string
}

type ProcessReply struct {
	ReturnCode       int
	Error            string
	Stopped          bool
	StoppedBy        string
	ExecStartRFC3339 string
	ExecEndRFC3339   string
	ElapsedMillis    int64
	ResolvedCmdLine  string
}

type CancelArgs struct {
	MachineID string
	PID       int
}
type CancelReply struct {
	OK bool
}

type StdinReadArgs struct{ Max int }
type StdinReadReply struct {
	Data []byte
	EOF  bool
	Err  string
}

// Admin RPC payloads
type AdminArgs struct {
	MachineID string
	PID       int
	Command   string
	Args      []string
}
type AdminReply struct {
	ReturnCode int
	Error      string
}

// ProcessReply helpers
func (r *ProcessReply) FailAt(rc int, msg string, start time.Time) {
	r.ReturnCode = rc
	r.Error = msg
	startUTC := start.UTC()
	r.ExecStartRFC3339 = startUTC.Format(time.RFC3339Nano)
	r.ExecEndRFC3339 = startUTC.Format(time.RFC3339Nano)
	r.ElapsedMillis = 0
	r.ResolvedCmdLine = ""
}
func (r *ProcessReply) FailNow(rc int, msg string) {
	now := time.Now().UTC()
	r.FailAt(rc, msg, now)
}
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

func SanitizeMachineID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty machine-id")
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'f') ||
			(r >= 'A' && r <= 'F') ||
			r == '-' {
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
