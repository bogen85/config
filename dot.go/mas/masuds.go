//go:build linux

package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

/*
Modes:
  --master --message <text>
  --agent  --uid <uid> --gid <gid> --sock <@abstract> --token <hex16bytes>
  --slave  --sock <@abstract>       --token <hex16bytes>

Flow:
  master (root; requires SUDO_USER) -> starts abstract UDS listener
  -> runs agent -> agent drops privs -> exec slave (user)
  -> slave dials UDS, sends handshake {token,hmac,build_id}
  -> master verifies SO_PEERCRED uid + HMAC(token|sock|pid)
  -> master sends {token,message}; slave verifies token, prints message
  -> slave replies {token,length}; master verifies token, prints length
*/

// Build-time injection (override with -ldflags "-X 'main.buildSecretHex=...'" "-X 'main.buildID=...'")
var buildSecretHex string // 32 hex chars (16 bytes) recommended
var buildID string        // optional informational tag

// Parsed secret (process-global)
var buildSecret []byte

// Message types
type hello struct {
	Token   string `json:"token"`
	HMACHex string `json:"hmac"`
	BuildID string `json:"build_id,omitempty"`
}

type request struct {
	Token   string `json:"token"`
	Message string `json:"message"`
}

type response struct {
	Token  string `json:"token"`
	Length int    `json:"length"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	// Modes
	flagMaster := flag.Bool("master", false, "run in master mode")
	flagAgent := flag.Bool("agent", false, "run in agent mode")
	flagSlave := flag.Bool("slave", false, "run in slave mode")

	// Master
	flagMessage := flag.String("message", "", "message text (master)")

	// Agent
	flagUID := flag.Int("uid", -1, "target uid (agent)")
	flagGID := flag.Int("gid", -1, "target gid (agent)")

	// Agent & Slave
	flagSock := flag.String("sock", "", "abstract unix socket name starting with @ (agent/slave)")
	flagToken := flag.String("token", "", "session token hex (agent/slave)")

	flag.Parse()

	// Parse/prepare build secret
	if err := initBuildSecret(); err != nil {
		log.Fatalf("build secret init error: %v", err)
	}

	switch {
	case *flagMaster:
		if err := runMaster(*flagMessage); err != nil {
			log.Fatalf("master error: %v", err)
		}
	case *flagAgent:
		if err := runAgent(*flagUID, *flagGID, *flagSock, *flagToken); err != nil {
			log.Fatalf("agent error: %v", err)
		}
	case *flagSlave:
		if err := runSlave(*flagSock, *flagToken); err != nil {
			log.Fatalf("slave error: %v", err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  --master --message <text>\n")
	fmt.Fprintf(os.Stderr, "  --agent  --uid <uid> --gid <gid> --sock <@name> --token <hex>\n")
	fmt.Fprintf(os.Stderr, "  --slave  --sock <@name> --token <hex>\n")
}

// -------------------------- MASTER --------------------------

func runMaster(message string) error {
	if strings.TrimSpace(message) == "" {
		return errors.New("message is required in --master mode")
	}
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" {
		return errors.New("SUDO_USER is empty; master must be run via sudo")
	}

	u, err := user.Lookup(sudoUser)
	if err != nil {
		return fmt.Errorf("user.Lookup(%q): %w", sudoUser, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("parse uid: %w", err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("parse gid: %w", err)
	}

	// Per-run token (16 bytes, hex) and abstract socket name.
	tokenHex, err := randHex(16)
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	sockName := fmt.Sprintf("@masuds-%d-%s", os.Getpid(), mustRandSuffix(8))

	// Create abstract UDS listener.
	laddr := &net.UnixAddr{Name: sockName, Net: "unix"} // '@' => abstract namespace on Linux
	ln, err := net.ListenUnix("unix", laddr)
	if err != nil {
		return fmt.Errorf("listen(%s): %w", sockName, err)
	}
	defer ln.Close()

	// Launch agent, which will drop to uid/gid and exec slave with sock+token.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	agentCmd := exec.Command(exe,
		"--agent",
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--sock", sockName,
		"--token", tokenHex,
	)
	agentCmd.Stdout = os.Stdout
	agentCmd.Stderr = os.Stderr

	if err := agentCmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	// Watch agent while we handle the handshake + exchange so we don't hang if it dies early.
	agentCh := make(chan error, 1)
	go func() { agentCh <- agentCmd.Wait() }()

	respCh := make(chan response, 1)
	errCh := make(chan error, 1)

	go func() {
		r, e := masterServeOnce(ln, uid, tokenHex, message, sockName)
		if e != nil {
			errCh <- e
			return
		}
		respCh <- r
	}()

	select {
	case err := <-agentCh:
		_ = ln.Close()
		return fmt.Errorf("agent exited early: %v", err)
	case e := <-errCh:
		_ = agentCmd.Process.Kill()
		<-agentCh // reap
		_ = ln.Close()
		return e
	case r := <-respCh:
		// Done; ensure agent is reaped (should already be).
		select {
		case err := <-agentCh:
			if err != nil {
				return fmt.Errorf("agent exited with error: %v", err)
			}
		default:
			<-agentCh
		}
		fmt.Printf("master: response length = %d\n", r.Length)
		return nil
	}
}

// Accept exactly one connection, verify peer creds + HMAC, then do request/response JSON.
func masterServeOnce(ln *net.UnixListener, expectUID int, tokenHex string, message string, sockName string) (response, error) {
	var zero response

	if err := ln.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return zero, fmt.Errorf("set accept deadline: %w", err)
	}
	conn, err := ln.AcceptUnix()
	if err != nil {
		return zero, fmt.Errorf("accept: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	// SO_PEERCRED (uid/pid)
	peerUID, peerPID, err := getPeerCreds(conn)
	if err != nil {
		return zero, fmt.Errorf("SO_PEERCRED: %w", err)
	}
	if int(peerUID) != expectUID {
		return zero, fmt.Errorf("peer uid mismatch: got %d want %d", peerUID, expectUID)
	}

	// Read hello
	var hi hello
	if err := json.NewDecoder(conn).Decode(&hi); err != nil {
		return zero, fmt.Errorf("decode hello: %w", err)
	}
	if hi.Token != tokenHex {
		return zero, fmt.Errorf("session token mismatch in hello")
	}
	// Verify HMAC(buildSecret, token|sock|peerPID)
	exp := computeHMAC(buildSecret, hi.Token, sockName, peerPID)
	got, err := hex.DecodeString(hi.HMACHex)
	if err != nil {
		return zero, fmt.Errorf("bad hmac hex: %w", err)
	}
	if !hmac.Equal(exp, got) {
		return zero, fmt.Errorf("invalid handshake HMAC")
	}

	// Send request
	req := request{Token: tokenHex, Message: message}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		return zero, fmt.Errorf("encode request: %w", err)
	}

	// Read response
	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return zero, fmt.Errorf("decode response: %w", err)
	}
	if resp.Token != tokenHex {
		return zero, fmt.Errorf("response token mismatch")
	}

	return resp, nil
}

// --------------------------- AGENT ---------------------------

func runAgent(uid, gid int, sock, token string) error {
	if uid < 0 || gid < 0 || sock == "" || token == "" {
		return errors.New("agent requires --uid, --gid, --sock, --token")
	}

	// Drop privileges: clear supplementary groups, setgid, setuid (in that order)
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups([]): %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid(%d): %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid(%d): %w", uid, err)
	}

	// Exec slave as the target user
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	cmd := exec.Command(exe, "--slave", "--sock", sock, "--token", token)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --------------------------- SLAVE ---------------------------

func runSlave(sock, token string) error {
	if sock == "" || token == "" {
		return errors.New("slave requires --sock and --token")
	}

	// Dial abstract UDS
	raddr := &net.UnixAddr{Name: sock, Net: "unix"}
	conn, err := net.DialUnix("unix", nil, raddr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", sock, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	// Handshake: send {token, hmac, build_id}
	pid := os.Getpid()
	h := hello{
		Token:   token,
		HMACHex: hex.EncodeToString(computeHMAC(buildSecret, token, sock, pid)),
		BuildID: buildID,
	}
	if err := json.NewEncoder(conn).Encode(&h); err != nil {
		return fmt.Errorf("encode hello: %w", err)
	}

	// Read request
	var req request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if req.Token != token {
		return fmt.Errorf("request token mismatch")
	}

	// Print the message (as per your skeleton)
	fmt.Printf("slave: message = %q\n", req.Message)

	// Respond with the length + echo token
	resp := response{Token: token, Length: len(req.Message)}
	if err := json.NewEncoder(conn).Encode(&resp); err != nil {
		return fmt.Errorf("encode response: %w", err)
	}

	return nil
}

// --------------------------- Helpers ---------------------------

func initBuildSecret() error {
	// If unset, use a fixed small default so both sides match in this single-binary demo.
	// In real deployments, always override with a random secret via -ldflags.
	if strings.TrimSpace(buildSecretHex) == "" {
		buildSecretHex = "00112233445566778899aabbccddeeff" // 16 bytes
	}
	b, err := hex.DecodeString(buildSecretHex)
	if err != nil {
		return fmt.Errorf("decode buildSecretHex: %w", err)
	}
	if len(b) == 0 {
		return errors.New("empty build secret after decode")
	}
	buildSecret = b
	return nil
}

func randHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func mustRandSuffix(nBytes int) string {
	s, err := randHex(nBytes)
	if err != nil {
		return "rnd"
	}
	return s
}

func computeHMAC(secret []byte, token string, sock string, pid int) []byte {
	mac := hmac.New(sha256.New, secret)
	ioWriteString(mac, token)
	ioWriteString(mac, "|")
	ioWriteString(mac, sock)
	ioWriteString(mac, "|")
	ioWriteString(mac, strconv.Itoa(pid))
	return mac.Sum(nil)
}

func ioWriteString(h interface{ Write([]byte) (int, error) }, s string) {
	_, _ = h.Write([]byte(s))
}

// getPeerCreds returns (uid, pid) of the connected peer using SO_PEERCRED.
func getPeerCreds(conn *net.UnixConn) (uid uint32, pid int, err error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var (
		ucred *unix.Ucred
		cErr  error
	)
	if err := raw.Control(func(fd uintptr) {
		uc, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e != nil {
			cErr = e
			return
		}
		ucred = uc
	}); err != nil {
		return 0, 0, err
	}
	if cErr != nil {
		return 0, 0, cErr
	}
	return ucred.Uid, int(ucred.Pid), nil
}
