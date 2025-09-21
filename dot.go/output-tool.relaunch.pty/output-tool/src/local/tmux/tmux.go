package tmux

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// InTmux returns true if we're inside a tmux client.
func InTmux() bool {
	if _, ok := os.LookupEnv("TMUX"); !ok {
		return false
	}
	_, ok := os.LookupEnv("TMUX_PANE")
	return ok
}

func version() (major, minor int, err error) {
	out, err := exec.Command("tmux", "-V").Output() // e.g. "tmux 3.3a"
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(out))
	if len(parts) < 2 {
		return 0, 0, errors.New("unexpected tmux -V output")
	}
	ver := parts[1] // "3.3a" or "3.2"
	// strip trailing letter
	for len(ver) > 0 {
		last := ver[len(ver)-1]
		if last >= '0' && last <= '9' {
			break
		}
		ver = ver[:len(ver)-1]
	}
	segs := strings.SplitN(ver, ".", 3)
	if len(segs) < 2 {
		return 0, 0, errors.New("unexpected tmux version format")
	}
	maj, err := strconv.Atoi(segs[0])
	if err != nil {
		return 0, 0, err
	}
	min, err := strconv.Atoi(segs[1])
	if err != nil {
		return 0, 0, err
	}
	return maj, min, nil
}

// SupportsPopups reports tmux >= 3.2.
func SupportsPopups() bool {
	maj, min, err := version()
	if err != nil {
		return false
	}
	return maj > 3 || (maj == 3 && min >= 2)
}
