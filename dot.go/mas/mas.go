//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type request struct {
	Message string `json:"message"`
}

type response struct {
	Length int `json:"length"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	// Top-level flags shared by modes
	master := flag.Bool("master", false, "run in master mode")
	agent := flag.Bool("agent", false, "run in agent mode")
	slave := flag.Bool("slave", false, "run in slave mode")

	// Master-specific
	msg := flag.String("message", "", "message for the slave (master mode)")

	// Agent-specific
	uidFlag := flag.Int("uid", -1, "target uid (agent mode)")
	gidFlag := flag.Int("gid", -1, "target gid (agent mode)")

	// Agent & Slave
	reqPath := flag.String("req", "", "request FIFO path")
	respPath := flag.String("resp", "", "response FIFO path")

	flag.Parse()

	switch {
	case *master:
		if err := runMaster(*msg); err != nil {
			log.Fatalf("master error: %v", err)
		}
	case *agent:
		if err := runAgent(*uidFlag, *gidFlag, *reqPath, *respPath); err != nil {
			log.Fatalf("agent error: %v", err)
		}
	case *slave:
		if err := runSlave(*reqPath, *respPath); err != nil {
			log.Fatalf("slave error: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  --master --message <text>\n")
		fmt.Fprintf(os.Stderr, "  --agent  --uid <uid> --gid <gid> --req <fifo> --resp <fifo>\n")
		fmt.Fprintf(os.Stderr, "  --slave  --req <fifo> --resp <fifo>\n")
		os.Exit(2)
	}
}

// ===================== MASTER =====================

func runMaster(message string) error {
	if strings.TrimSpace(message) == "" {
		return errors.New("message is required in --master mode")
	}
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" {
		return errors.New("SUDO_USER is empty; master must be run via sudo with a real sudo user")
	}

	uid, gid, err := lookupUserIDs(sudoUser)
	if err != nil {
		return fmt.Errorf("lookup user %q: %w", sudoUser, err)
	}

	// Per-run private workspace to avoid races/collisions.

	baseDir, err := os.MkdirTemp("/dev/shm", "maslave-")
	if err != nil {
		return fmt.Errorf("mkdtemp: %w", err)
	}
	// Make the workspace traversable by the slave (sudo user).
	if err := os.Chown(baseDir, uid, 0); err != nil {
		_ = os.RemoveAll(baseDir)
		return fmt.Errorf("chown temp dir to user: %w", err)
	}

	// Keep it private to root; we'll chown the FIFOs themselves.
	if err := os.Chmod(baseDir, 0o700); err != nil {
		_ = os.RemoveAll(baseDir)
		return fmt.Errorf("chmod temp dir: %w", err)
	}

	// Ensure cleanup at the end.
	defer func() {
		_ = os.RemoveAll(baseDir)
	}()

	req := filepath.Join(baseDir, "req.fifo")
	resp := filepath.Join(baseDir, "resp.fifo")

	// Create FIFOs (umask may interfere, so chmod afterwards).
	if err := mkfifo(req, 0o460); err != nil {
		return fmt.Errorf("mkfifo(req): %w", err)
	}
	if err := mkfifo(resp, 0o640); err != nil {
		return fmt.Errorf("mkfifo(resp): %w", err)
	}

	// Ownership per spec: owner = sudo user, group = 0 (root)
	if err := os.Chown(req, uid, 0); err != nil {
		return fmt.Errorf("chown req: %w", err)
	}
	if err := os.Chown(resp, uid, 0); err != nil {
		return fmt.Errorf("chown resp: %w", err)
	}

	// Set exact modes (umask-safe).
	if err := os.Chmod(req, 0o460); err != nil {
		return fmt.Errorf("chmod req: %w", err)
	}
	if err := os.Chmod(resp, 0o640); err != nil {
		return fmt.Errorf("chmod resp: %w", err)
	}

	// Launch agent (which will drop to uid/gid and invoke slave).
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	agentCmd := exec.Command(exe,
		"--agent",
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--req", req,
		"--resp", resp,
	)
	agentCmd.Stdout = os.Stdout
	agentCmd.Stderr = os.Stderr

	if err := agentCmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	// --- NEW: async I/O + agent wait with goroutines/channels ---

	writeCh := make(chan error, 1)
	respCh := make(chan response, 1)
	readErrCh := make(chan error, 1)
	agentCh := make(chan error, 1)

	// kick off the blocking tasks
	go func() {
		writeCh <- writeJSONToFIFO(req, request{Message: message})
	}()

	go func() {
		var r response
		if err := readJSONFromFIFO(resp, &r); err != nil {
			readErrCh <- err
			return
		}
		respCh <- r
	}()

	go func() {
		agentCh <- agentCmd.Wait() // reaps the agent; sends its exit status
	}()

	// Stage 1: ensure the request was written before the agent exits
	var agentDone bool
	var agentErr error

	select {
	case err := <-writeCh:
		if err != nil {
			_ = agentCmd.Process.Kill()
			<-agentCh // reap
			return fmt.Errorf("write request: %w", err)
		}
	case agentErr = <-agentCh:
		agentDone = true
		return fmt.Errorf("agent exited early before request was written: %v", agentErr)
	}

	// Stage 2: wait for response, still watching the agent
	var respObj response
	select {
	case respObj = <-respCh:
		// got response
	case err := <-readErrCh:
		_ = agentCmd.Process.Kill()
		if !agentDone {
			<-agentCh
		}
		return fmt.Errorf("read response: %w", err)
	case agentErr = <-agentCh:
		agentDone = true
		return fmt.Errorf("agent exited early before response arrived: %v", agentErr)
	}

	// Final: make sure agent is fully reaped and ok
	if !agentDone {
		agentErr = <-agentCh
	}
	if agentErr != nil {
		return fmt.Errorf("agent exited with error: %v", agentErr)
	}

	fmt.Printf("master: response length = %d\n", respObj.Length)
	return nil
}

// ===================== AGENT =====================

func runAgent(uid, gid int, req, resp string) error {
	if uid < 0 || gid < 0 || req == "" || resp == "" {
		return errors.New("agent requires --uid, --gid, --req, and --resp")
	}

	// Drop privileges: clear supplementary groups, setgid, setuid (in this order).
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups([]): %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid(%d): %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid(%d): %w", uid, err)
	}

	// Exec the slave (new process image running as the target user).
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	slaveCmd := exec.Command(exe, "--slave", "--req", req, "--resp", resp)
	slaveCmd.Stdout = os.Stdout
	slaveCmd.Stderr = os.Stderr

	if err := slaveCmd.Run(); err != nil {
		return fmt.Errorf("slave run: %w", err)
	}
	return nil
}

// ===================== SLAVE =====================

func runSlave(req, resp string) error {
	if req == "" || resp == "" {
		return errors.New("slave requires --req and --resp")
	}

	// Read request (one JSON doc) until EOF.
	var reqObj request
	if err := readJSONFromFIFO(req, &reqObj); err != nil {
		return fmt.Errorf("read request: %w", err)
	}

	// Print the message (as required by your skeleton).
	fmt.Printf("slave: message = %q\n", reqObj.Message)

	// Prepare and write response JSON.
	respObj := response{Length: len(reqObj.Message)}
	if err := writeJSONToFIFO(resp, respObj); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

// ===================== Helpers =====================

func mkfifo(path string, mode os.FileMode) error {
	// Use syscall.Mkfifo; permissions may be affected by umask, so we chmod after.
	if err := syscall.Mkfifo(path, uint32(mode&0o7777)); err != nil {
		return err
	}
	return nil
}

func writeJSONToFIFO(path string, v any) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readJSONFromFIFO(path string, v any) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// Read the entire stream and unmarshal (robust if other side closes to signal end).
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	// Some encoders add a trailing newline; json.Unmarshal handles it.
	return json.Unmarshal(data, v)
}

func lookupUserIDs(name string) (int, int, error) {
	// Try os/user first (preferred when cgo is available).
	if u, err := user.Lookup(name); err == nil {
		uid, err1 := strconv.Atoi(u.Uid)
		gid, err2 := strconv.Atoi(u.Gid)
		if err1 == nil && err2 == nil {
			return uid, gid, nil
		}
	}
	return -1, -1, fmt.Errorf("user %q not found", name)
}
