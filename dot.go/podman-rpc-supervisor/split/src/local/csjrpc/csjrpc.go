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
)

// ===== Logging (ts + file:line) =====

func logWithCaller(level, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	// skip 3 frames so file:line points to the original caller in server/client.
	_, file, line, ok := runtime.Caller(3)
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
