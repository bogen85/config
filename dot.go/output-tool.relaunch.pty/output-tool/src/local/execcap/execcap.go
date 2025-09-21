package execcap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"local/capture"
	"local/rules"
)

type Options struct {
	OnlyViewMatches bool   // write only matches into capture
	MatchStderr     string // "none" | "line"  (mirror matches to process' stderr)
}

type Result struct {
	CapturePath  string
	Meta         capture.Meta
	AnyMatch     bool
	LinesTotal   int
	MatchLines   int
	MatchesTotal int
}

// Run runs cmdArgs[0] with cmdArgs[1:] and captures both stdout and stderr.
//
// It streams both to os.Stdout in real time (so the invoking tool sees output),
// mirrors matching lines to os.Stderr if opts.MatchStderr == "line",
// and writes a JSONL capture (with Rec.Stream set to "out" or "err").
//
// It returns when the process exits and the capture is fully written.
func Run(cmdArgs []string, rs []rules.Rule, opts Options) (*Result, error) {
	if len(cmdArgs) == 0 {
		return nil, errors.New("execcap: empty cmd")
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("execcap: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("execcap: stderr pipe: %w", err)
	}

	// Create capture writer
	wr, err := capture.NewTempWriter("ot-exec-")
	if err != nil {
		return nil, fmt.Errorf("execcap: temp writer: %w", err)
	}
	enc := json.NewEncoder(wr.Writer())

	if err := cmd.Start(); err != nil {
		_ = wr.Close()
		return nil, fmt.Errorf("execcap: start: %w", err)
	}

	var (
		linesTotal   int64
		matchLines   int64
		matchesTotal int64
		anyMatch     int32
	)

	// Readers
	type streamInfo struct {
		name string
		r    io.Reader
	}
	streams := []streamInfo{
		{"out", stdout},
		{"err", stderr},
	}

	var wg sync.WaitGroup
	writeLine := func(n int64, sname, line string, matched bool) {
		rec := capture.Rec{
			N: int(n), Text: line, M: matched, Stream: sname,
		}
		if !opts.OnlyViewMatches || matched {
			_ = enc.Encode(&rec)
		}
	}

	for _, st := range streams {
		st := st
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := bufio.NewReaderSize(st.r, 64*1024)

			// IMPORTANT: write to the same-origin stream
			var out *bufio.Writer
			switch st.name {
			case "out":
				out = bufio.NewWriterSize(os.Stdout, 64*1024)
			case "err":
				out = bufio.NewWriterSize(os.Stderr, 64*1024)
			default:
				out = bufio.NewWriterSize(os.Stdout, 64*1024)
			}
			defer out.Flush()

			// Separate writer used only for optional mirroring of stdout matches
			errw := bufio.NewWriterSize(os.Stderr, 64*1024)
			defer errw.Flush()

			for {
				line, rerr := in.ReadString('\n')
				if errors.Is(rerr, io.EOF) && len(line) == 0 {
					break
				}
				line = strings.TrimRight(line, "\r\n")
				n := atomic.AddInt64(&linesTotal, 1)

				// Stream to the SAME fd as the child’s origin
				out.WriteString(line)
				out.WriteByte('\n')

				matched, count := rules.AnyMatch(rs, line)
				if matched {
					atomic.StoreInt32(&anyMatch, 1)
					atomic.AddInt64(&matchLines, 1)
					atomic.AddInt64(&matchesTotal, int64(count))

					// Mirror to stderr ONLY when the origin was stdout (avoid double printing)
					if opts.MatchStderr == "line" && st.name == "out" {
						fmt.Fprintf(errw, "%d: %s\n", n, line)
					}
				}

				writeLine(n, st.name, line, matched)

				if rerr != nil {
					if errors.Is(rerr, io.EOF) {
						break
					}
					break
				}
			}
		}()
	}

	wg.Wait()
	_ = wr.Close()
	_ = cmd.Wait()

	res := &Result{
		CapturePath:  wr.Path(),
		AnyMatch:     atomic.LoadInt32(&anyMatch) == 1,
		LinesTotal:   int(linesTotal),
		MatchLines:   int(matchLines),
		MatchesTotal: int(matchesTotal),
		Meta: capture.Meta{
			Version:        1,
			CapturePath:    wr.Path(),
			Filtered:       opts.OnlyViewMatches,
			LineFormat:     "jsonl",
			LinesTotal:     int(linesTotal),
			MatchLines:     int(matchLines),
			MatchesTotal:   int(matchesTotal),
			CreatedUnixSec: time.Now().Unix(),
			Temp:           false, // viewer inline won't auto-delete
			OwnerPID:       os.Getpid(),
			Source: capture.Source{
				Mode: "exec",
				Arg:  strings.Join(cmdArgs, " "),
			},
		},
	}
	return res, nil
}
