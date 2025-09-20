// output-tool.go
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/gdamore/tcell/v2"
	"golang.org/x/term"
)

/* =========================
   Config types & defaults
   ========================= */

type ColorPair struct {
	FG string `toml:"fg"`
	BG string `toml:"bg"`
}

type Rule struct {
	Name  string `toml:"name"`
	Regex string `toml:"regex"`
	FG    string `toml:"fg"`
	BG    string `toml:"bg"`
}

type Config struct {
	// Global action: "print" (default) or "spawn" (next pass wires FIFO spawning on Enter)
	Action  string   `toml:"action"`
	Command []string `toml:"command"`

	Colors struct {
		Normal          ColorPair `toml:"normal"`
		Highlight       ColorPair `toml:"highlight"`
		Gutter          ColorPair `toml:"gutter"`
		GutterHighlight ColorPair `toml:"gutter_highlight"`
		Status          ColorPair `toml:"status"`     // bottom bar
		TopStatus       ColorPair `toml:"top_status"` // top bar
	} `toml:"colors"`
	Rules []Rule `toml:"rules"`
}

func defaultConfig() Config {
	var c Config

	// default action: print (spawn is supported by config, we’ll wire the FIFOed run next pass)
	c.Action = "print"
	c.Command = []string{"xfce4-terminal", "--window", "--working-directory=${PWD}", "--execute", "less", "<path>"}

	// Colors: keep named for main UI, use hex for bars to avoid theme remap
	c.Colors.Normal = ColorPair{FG: "white", BG: "black"}
	c.Colors.Highlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Gutter = ColorPair{FG: "gray", BG: "black"}
	c.Colors.GutterHighlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Status = ColorPair{FG: "#000000", BG: "#ffff00"}    // bottom
	c.Colors.TopStatus = ColorPair{FG: "#000000", BG: "#ff00ff"} // top

	// Default rule: POSIX-ish path:line:col
	c.Rules = []Rule{
		{
			Name:  "path:line:col",
			Regex: `(?:\.\.?/)?[A-Za-z0-9._/\-]+:\d+:\d+`,
			FG:    "black",
			BG:    "green",
		},
	}
	return c
}

/* =========================
   Flags (new)
   ========================= */

var (
	flagFile          = flag.String("file", "", "path to a UTF-8 text file (one entry per line)")
	flagConfig        = flag.String("config", "", "path to a config TOML (use ::default:: for per-user default path)")
	flagOutputDefault = flag.Bool("output-default-config", false, "print or write a default config TOML and exit")
	flagForce         = flag.Bool("force", false, "allow overwriting existing config when outputting defaults")
	flagPrimary       = flag.Bool("primary", false, "use PRIMARY selection via xclip as input (mutually exclusive with --file/--pipe)")
	flagPipe          = flag.Bool("pipe", false, "read from stdin, pass-through to stdout in real time, then optionally open TUI")
	flagOnlyOnMatches = flag.Bool("only-on-matches", false, "if there are no matches, exit without opening the TUI")
	flagJSONMatches   = flag.Bool("json-matches", false, "emit NDJSON records for each matching line (pre-TUI/quasi-print)")
	flagJSONDest      = flag.String("json-dest", "stderr", "destination for NDJSON: stderr|stdout|/path/to/file")
	flagNoTUI         = flag.Bool("no-tui", false, "when emitting NDJSON, skip opening the TUI and exit")
)

/* =========================
   Small helpers
   ========================= */

func digits(n int) int {
	if n <= 0 {
		return 1
	}
	d := 0
	for n > 0 {
		d++
		n /= 10
	}
	return d
}
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

/* =========================
   Color parsing
   ========================= */

type ColorPairStyle struct {
	N  tcell.Style
	HL tcell.Style
}

func parseColor(s string) tcell.Color {
	s = strings.TrimSpace(s)
	if s == "" {
		return tcell.ColorReset
	}
	if strings.HasPrefix(s, "palette:") {
		nStr := strings.TrimPrefix(s, "palette:")
		if n, err := strconv.Atoi(nStr); err == nil && n >= 0 && n <= 255 {
			return tcell.PaletteColor(n)
		}
	}
	return tcell.GetColor(s) // supports #rrggbb and HTML/X11 names
}

func styleFrom(p ColorPair) tcell.Style {
	return tcell.StyleDefault.Foreground(parseColor(p.FG)).Background(parseColor(p.BG))
}

func invertStyle(p ColorPair) tcell.Style {
	return tcell.StyleDefault.Foreground(parseColor(p.BG)).Background(parseColor(p.FG))
}

/* =========================
   I/O helpers
   ========================= */

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	const maxCap = 10 * 1024 * 1024
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, maxCap)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

func readPrimaryWithXclip() (string, error) {
	_, err := exec.LookPath("xclip")
	if err != nil {
		return "", fmt.Errorf("xclip not found on PATH (install xclip or provide --file)")
	}
	out, err := exec.Command("xclip", "-o", "-selection", "primary").Output()
	if err != nil {
		return "", fmt.Errorf("reading PRIMARY via xclip failed: %w", err)
	}
	// Clear PRIMARY
	cmd := exec.Command("xclip", "-selection", "primary", "-in")
	cmd.Stdin = bytes.NewReader(nil)
	_ = cmd.Run() // best effort
	return string(out), nil
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	base := filepath.Base(exe)
	dir := filepath.Join(home, ".local", "share", "user-dev-tooling", base)
	return filepath.Join(dir, "config.toml"), nil
}

func ensureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

func writeDefaultConfigTo(path string) error {
	cfg := defaultConfig()
	if path == "" {
		return toml.NewEncoder(os.Stdout).Encode(cfg)
	}
	if err := ensureDir(path); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

func loadConfigMaybe(path string) (Config, error) {
	cfg := defaultConfig()
	var tryPath string
	if path == "" || path == "::default::" {
		if dp, err := defaultConfigPath(); err == nil {
			tryPath = dp
		}
	} else {
		tryPath = path
	}
	if tryPath != "" && fileExists(tryPath) {
		if _, err := toml.DecodeFile(tryPath, &cfg); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

/* =========================
   Config overwrite dialog
   ========================= */

func confirmOverwriteDialog(target string) bool {
	s, err := tcell.NewScreen()
	if err != nil {
		return false
	}
	if err = s.Init(); err != nil {
		return false
	}
	defer s.Fini()

	def := tcell.StyleDefault
	bg := def.Background(tcell.ColorReset)

	msgLines := []string{
		"Configuration file already exists:",
		target,
		"",
		"Overwrite it?  [y]es / [n]o (Enter = yes, Esc = no)",
	}

	w, h := s.Size()
	boxW := 0
	for _, m := range msgLines {
		if len(m) > boxW {
			boxW = len(m)
		}
	}
	boxW += 4
	boxH := len(msgLines) + 2
	x0 := max(0, (w-boxW)/2)
	y0 := max(0, (h-boxH)/2)

	draw := func() {
		s.Clear()
		w, h = s.Size()
		x0 = max(0, (w-boxW)/2)
		y0 = max(0, (h-boxH)/2)
		for y := 0; y < boxH; y++ {
			for x := 0; x < boxW; x++ {
				ch := ' '
				if y == 0 || y == boxH-1 {
					ch = '─'
				}
				if x == 0 || x == boxW-1 {
					ch = '│'
				}
				if (x == 0 || x == boxW-1) && (y == 0 || y == boxH-1) {
					ch = '┼'
				}
				s.SetContent(x0+x, y0+y, ch, nil, bg)
			}
		}
		for i, m := range msgLines {
			start := x0 + 2
			for j, r := range m {
				s.SetContent(start+j, y0+1+i, r, nil, bg)
			}
		}
		s.Show()
	}

	draw()
	for {
		ev := s.PollEvent()
		switch e := ev.(type) {
		case *tcell.EventResize:
			s.Sync()
			draw()
		case *tcell.EventKey:
			switch e.Key() {
			case tcell.KeyEnter:
				return true
			case tcell.KeyEscape:
				return false
			default:
				switch e.Rune() {
				case 'y', 'Y':
					return true
				case 'n', 'N':
					return false
				}
			}
		}
	}
}

/* =========================
   Regex matching model
   ========================= */

type compiledRule struct {
	re       *regexp.Regexp
	style    tcell.Style
	styleInv tcell.Style // for selected row (inverted)
	name     string
}

type matchSpan struct {
	start int // byte offsets
	end   int
	rule  int // index into compiledRules
}

type lineInfo struct {
	spans       []matchSpan // merged, non-overlapping paint spans
	matchesText []string    // all raw match texts (includes overlaps)
}

func compileRules(cfg Config) []compiledRule {
	var cr []compiledRule
	for i, r := range cfg.Rules {
		if strings.TrimSpace(r.Regex) == "" {
			fmt.Fprintf(os.Stderr, "warning: rule %d has empty regex; skipping\n", i)
			continue
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: rule %d regex compile failed: %v; skipping\n", i, err)
			continue
		}
		cp := ColorPair{FG: r.FG, BG: r.BG}
		cr = append(cr, compiledRule{
			re:       re,
			style:    styleFrom(cp),
			styleInv: invertStyle(cp),
			name:     r.Name,
		})
	}
	return cr
}

func buildLineInfo(line string, rules []compiledRule) lineInfo {
	var li lineInfo
	if len(rules) == 0 || len(line) == 0 {
		return li
	}

	type rawSpan struct {
		start, end, rule int
	}
	var raw []rawSpan
	for ri, cr := range rules {
		idxs := cr.re.FindAllStringIndex(line, -1)
		for _, p := range idxs {
			raw = append(raw, rawSpan{start: p[0], end: p[1], rule: ri})
			li.matchesText = append(li.matchesText, line[p[0]:p[1]])
		}
	}
	if len(raw) == 0 {
		return li
	}

	// Per-byte owner table; last rule wins
	b := []byte(line)
	owner := make([]int, len(b)) // -1 = none
	for i := range owner {
		owner[i] = -1
	}
	for _, rs := range raw {
		if rs.start < 0 {
			rs.start = 0
		}
		if rs.end > len(b) {
			rs.end = len(b)
		}
		if rs.start >= rs.end {
			continue
		}
		for i := rs.start; i < rs.end; i++ {
			owner[i] = rs.rule
		}
	}
	// Compress owner map into spans
	i := 0
	for i < len(owner) {
		if owner[i] == -1 {
			i++
			continue
		}
		rule := owner[i]
		j := i + 1
		for j < len(owner) && owner[j] == rule {
			j++
		}
		li.spans = append(li.spans, matchSpan{start: i, end: j, rule: rule})
		i = j
	}
	return li
}

type preScan struct {
	lines        []string
	info         []lineInfo
	matchLines   []int
	totalMatches int
}

/* =========================
   Pre-scan utility
   ========================= */

func precompute(lines []string, rules []compiledRule) preScan {
	ps := preScan{
		lines: lines,
		info:  make([]lineInfo, len(lines)),
	}
	for i, ln := range lines {
		li := buildLineInfo(ln, rules)
		ps.info[i] = li
		if len(li.spans) > 0 {
			ps.matchLines = append(ps.matchLines, i)
		}
		ps.totalMatches += len(li.matchesText)
	}
	return ps
}

/* =========================
   UI with rules (print mode on Enter)
   ========================= */

type SourceInfo struct {
	Kind string `json:"kind"`           // "file" | "primary" | "pipe"
	Path string `json:"path,omitempty"` // only when kind == "file"
}

type Output struct {
	Line       string     `json:"line"`
	Matches    []string   `json:"matches"`
	LineNumber int        `json:"line_number"` // 1-based; 0 on cancel
	Source     SourceInfo `json:"source"`
}

func runListUIWithRules(ps preScan, cfg Config, source SourceInfo) (string, []string, int, bool, int, int) {
	lines := ps.lines
	info := ps.info
	matchLines := ps.matchLines
	totalMatches := ps.totalMatches

	if len(lines) == 0 {
		return "", nil, 0, false, 0, 0
	}

	// Screen
	s, err := tcell.NewScreen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tcell.NewScreen:", err)
		return "", nil, 0, false, len(matchLines), totalMatches
	}
	if err = s.Init(); err != nil {
		fmt.Fprintln(os.Stderr, "screen.Init:", err)
		return "", nil, 0, false, len(matchLines), totalMatches
	}
	defer s.Fini()

	// Styles
	normal := styleFrom(cfg.Colors.Normal)
	highlight := styleFrom(cfg.Colors.Highlight)
	gutterStyle := styleFrom(cfg.Colors.Gutter)
	gutterHLStyle := styleFrom(cfg.Colors.GutterHighlight)
	statusStyle := styleFrom(cfg.Colors.Status)
	topStyle := styleFrom(cfg.Colors.TopStatus)

	// Navigation
	cursor := 0
	offset := 0
	lnWidth := digits(len(lines))
	gutterWidth := lnWidth + 2
	contentLeft := gutterWidth

	ensureCursorVisible := func() {
		_, h := s.Size()
		usable := max(0, h-2) // top+bottom bars
		if cursor < offset {
			offset = cursor
		} else if cursor >= offset+usable {
			offset = cursor - usable + 1
		}
		maxOffset := max(0, len(lines)-usable)
		offset = clamp(offset, 0, maxOffset)
	}
	putStr := func(x, y int, style tcell.Style, str string, wlimit int) int {
		col := x
		w := 0
		for _, r := range str {
			if wlimit >= 0 && w >= wlimit {
				break
			}
			s.SetContent(col, y, r, nil, style)
			col++
			w++
		}
		return col - x
	}
	fillRow := func(y int, style tcell.Style) {
		w, _ := s.Size()
		for x := 0; x < w; x++ {
			s.SetContent(x, y, ' ', nil, style)
		}
	}
	nextMatch := func() bool {
		if len(matchLines) == 0 {
			return false
		}
		for _, ml := range matchLines {
			if ml > cursor {
				cursor = ml
				return true
			}
		}
		cursor = matchLines[0]
		return true
	}
	prevMatch := func() bool {
		if len(matchLines) == 0 {
			return false
		}
		for i := len(matchLines) - 1; i >= 0; i-- {
			if matchLines[i] < cursor {
				cursor = matchLines[i]
				return true
			}
		}
		cursor = matchLines[len(matchLines)-1]
		return true
	}

	drawContentLine := func(rowY, lineIdx, width int, isSel bool) {
		line := lines[lineIdx]
		rowStyle := normal
		gutStyle := gutterStyle
		if isSel {
			rowStyle = highlight
			gutStyle = gutterHLStyle
		}
		// background
		fillRow(rowY, rowStyle)

		// gutter
		numStr := strconv.Itoa(lineIdx + 1)
		pad := lnWidth - len(numStr)
		x := 0
		for j := 0; j < pad; j++ {
			s.SetContent(x, rowY, ' ', nil, gutStyle)
			x++
		}
		x += putStr(x, rowY, gutStyle, numStr, -1)
		x += putStr(x, rowY, gutStyle, ": ", -1)

		// content area
		avail := max(0, width-contentLeft)
		if avail <= 0 {
			return
		}

		// Render matches: track byte positions
		li := info[lineIdx]
		spanIdx := 0
		var curSpan *matchSpan
		if len(li.spans) > 0 {
			curSpan = &li.spans[0]
		}
		bytePos := 0
		col := contentLeft
		for _, r := range line {
			if col >= width {
				break
			}
			rStart := bytePos
			bytePos += len(string(r)) // rune -> bytes in UTF-8

			for curSpan != nil && rStart >= curSpan.end && spanIdx+1 < len(li.spans) {
				spanIdx++
				curSpan = &li.spans[spanIdx]
			}
			st := rowStyle
			if curSpan != nil && rStart >= curSpan.start && rStart < curSpan.end {
				rule := cfg.Rules[li.spans[spanIdx].rule] // used only to choose style; safe
				_ = rule
				cr := info[lineIdx].spans[spanIdx].rule
				// derive style from compiled rules (we didn’t keep them here; rely on rowStyle override below)
				// For simplicity, reusing precomputed styles via cfg again:
				// (We stored only spans with rule index; visual style computed earlier in buildLineInfo)
				// Here, just choose: selected → invert style; else → use compiled style.
				// We’ll reconstruct styles quickly:
				stPair := ColorPair{FG: cfg.Rules[cr].FG, BG: cfg.Rules[cr].BG}
				if isSel {
					st = invertStyle(stPair)
				} else {
					st = styleFrom(stPair)
				}
			}
			s.SetContent(col, rowY, r, nil, st)
			col++
		}
	}

	draw := func() {
		s.Clear()
		w, h := s.Size()
		if h <= 0 {
			s.Show()
			return
		}
		usable := max(0, h-2)

		// Top bar
		fillRow(0, topStyle)
		topMsg := fmt.Sprintf(" input:%s  action:%s  match-lines:%d  matches:%d  |  n:next-match  N:prev-match ",
			source.Kind, strings.ToLower(cfg.Action), len(matchLines), totalMatches)
		putStr(0, 0, topStyle, topMsg, -1)

		// Content
		for row := 0; row < usable; row++ {
			i := offset + row
			if i >= len(lines) {
				fillRow(1+row, normal)
				continue
			}
			drawContentLine(1+row, i, w, i == cursor)
		}

		// Bottom bar
		statusRow := h - 1
		fillRow(statusRow, statusStyle)
		lineNum := cursor + 1
		charCount := utf8.RuneCountInString(lines[cursor])
		btm := fmt.Sprintf(" %d/%d  |  chars: %d  |  ↑/↓ PgUp/PgDn Home/End  Enter=select  q/Esc=quit ",
			lineNum, len(lines), charCount)
		putStr(0, statusRow, statusStyle, btm, -1)

		s.Show()
	}

	ensureCursorVisible()
	draw()

	for {
		ev := s.PollEvent()
		switch e := ev.(type) {
		case *tcell.EventResize:
			s.Sync()
			ensureCursorVisible()
			draw()
		case *tcell.EventKey:
			switch e.Key() {
			case tcell.KeyEnter:
				// For now: print-mode behavior (spawn mode will be wired next pass)
				return lines[cursor], info[cursor].matchesText, cursor + 1, true, len(matchLines), totalMatches
			case tcell.KeyEscape:
				return "", nil, 0, false, len(matchLines), totalMatches
			case tcell.KeyUp:
				if cursor > 0 {
					cursor--
					ensureCursorVisible()
					draw()
				}
			case tcell.KeyDown:
				if cursor < len(lines)-1 {
					cursor++
					ensureCursorVisible()
					draw()
				}
			case tcell.KeyPgUp:
				_, h := s.Size()
				usable := max(0, h-2)
				cursor = clamp(cursor-usable, 0, len(lines)-1)
				ensureCursorVisible()
				draw()
			case tcell.KeyPgDn:
				_, h := s.Size()
				usable := max(0, h-2)
				cursor = clamp(cursor+usable, 0, len(lines)-1)
				ensureCursorVisible()
				draw()
			case tcell.KeyHome:
				cursor = 0
				ensureCursorVisible()
				draw()
			case tcell.KeyEnd:
				cursor = len(lines) - 1
				ensureCursorVisible()
				draw()
			default:
				switch e.Rune() {
				case 'q':
					return "", nil, 0, false, len(matchLines), totalMatches
				case 'k':
					if cursor > 0 {
						cursor--
						ensureCursorVisible()
						draw()
					}
				case 'j':
					if cursor < len(lines)-1 {
						cursor++
						ensureCursorVisible()
						draw()
					}
				case 'n':
					if nextMatch() {
						ensureCursorVisible()
						draw()
					}
				case 'N':
					if prevMatch() {
						ensureCursorVisible()
						draw()
					}
				}
			}
		}
	}
}

/* =========================
   NDJSON helpers
   ========================= */

func openDest(dest string) (io.WriteCloser, error) {
	switch dest {
	case "stderr":
		return nopCloser{os.Stderr}, nil
	case "stdout":
		return nopCloser{os.Stdout}, nil
	default:
		// path
		if err := ensureDir(dest); err != nil {
			return nil, err
		}
		return os.Create(dest)
	}
}

type nopCloser struct{ io.Writer }

func (n nopCloser) Close() error { return nil }

/* =========================
   PIPE mode (stdin streaming)
   ========================= */

func runPipeMode(cfg Config, isTTYOut bool, emitNDJSON bool, jsonDest string, onlyOnMatches bool, noTUI bool) error {
	// Compile rules once
	cRules := compileRules(cfg)

	// Temp dir for this run
	root := filepath.Join(os.TempDir(), "output-tool", fmt.Sprintf("%d", os.Getpid()))
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("mk temp dir: %w", err)
	}
	tempFile := filepath.Join(root, fmt.Sprintf("stream-%d.log", time.Now().UnixNano()))
	outf, err := os.Create(tempFile)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer outf.Close()

	// Stream stdin → stdout and file; match on the fly
	in := bufio.NewScanner(os.Stdin)
	const maxCap = 10 * 1024 * 1024
	buf := make([]byte, 0, 64*1024)
	in.Buffer(buf, maxCap)

	var lines []string
	matchLines := make([]int, 0, 128)
	totalMatches := 0

	lineNo := 0
	for in.Scan() {
		raw := in.Text()

		// passthrough
		fmt.Fprintln(os.Stdout, raw)

		// store
		fmt.Fprintln(outf, raw)
		lines = append(lines, raw)

		// match this line
		li := buildLineInfo(raw, cRules)
		if len(li.spans) > 0 {
			matchLines = append(matchLines, lineNo)
			totalMatches += len(li.matchesText)
		}

		lineNo++
	}
	if err := in.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	_ = outf.Sync()

	// Build pre-scan for TUI / NDJSON (re-use of what we computed)
	ps := preScan{
		lines:        lines,
		info:         make([]lineInfo, len(lines)),
		matchLines:   matchLines,
		totalMatches: totalMatches,
	}
	// We need full info (matchesText) for NDJSON; rebuild using compiled rules
	for i, ln := range lines {
		ps.info[i] = buildLineInfo(ln, cRules)
	}

	// Non-TTY: stay “cat-like”; optionally dump NDJSON
	if !isTTYOut {
		if emitNDJSON && totalMatches > 0 {
			wc, err := openDest(jsonDest)
			if err != nil {
				return err
			}
			defer wc.Close()
			enc := json.NewEncoder(wc)
			enc.SetEscapeHTML(false)
			src := SourceInfo{Kind: "pipe"}
			for _, i := range ps.matchLines {
				rec := Output{
					Line:       ps.lines[i],
					Matches:    ps.info[i].matchesText,
					LineNumber: i + 1,
					Source:     src,
				}
				if err := enc.Encode(rec); err != nil {
					return err
				}
			}
		}
		// Done
		return nil
	}

	// TTY: decide whether to open TUI
	if onlyOnMatches && totalMatches == 0 {
		// nothing to show; you already saw the stream
		return nil
	}

	// If requested, dump NDJSON before TUI
	if emitNDJSON && totalMatches > 0 {
		wc, err := openDest(jsonDest)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(wc)
		enc.SetEscapeHTML(false)
		src := SourceInfo{Kind: "pipe"}
		for _, i := range ps.matchLines {
			rec := Output{
				Line:       ps.lines[i],
				Matches:    ps.info[i].matchesText,
				LineNumber: i + 1,
				Source:     src,
			}
			if err := enc.Encode(rec); err != nil {
				_ = wc.Close()
				return err
			}
		}
		_ = wc.Close()
	}

	if noTUI {
		return nil
	}

	// Open TUI over captured content
	src := SourceInfo{Kind: "pipe"}
	lineText, matches, lineNum, ok, _, _ := runListUIWithRules(ps, cfg, src)

	// Print selection (JSON) only if in print mode
	if cfg.Action == "print" {
		var out Output
		out.Source = src
		if ok {
			out.Line = strings.TrimRightFunc(lineText, func(r rune) bool { return r == '\r' })
			out.Matches = matches
			out.LineNumber = lineNum
		} else {
			out.Line = ""
			out.Matches = []string{}
			out.LineNumber = 0
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(out)
	}
	return nil
}

/* =========================
   Main
   ========================= */

func main() {
	flag.Parse()

	// Handle default config output path creation / overwrite prompts
	if *flagOutputDefault {
		if *flagFile != "" || *flagPipe || *flagPrimary {
			fmt.Fprintln(os.Stderr, "error: --file/--pipe/--primary cannot be used with --output-default-config")
			os.Exit(2)
		}
		var outPath string
		if *flagConfig == "::default::" {
			dp, err := defaultConfigPath()
			if err != nil {
				log.Fatalf("cannot resolve default config path: %v", err)
			}
			outPath = dp
		} else {
			outPath = *flagConfig // empty => stdout
		}

		if outPath == "" {
			if err := writeDefaultConfigTo(""); err != nil {
				log.Fatalf("failed to write default config: %v", err)
			}
			return
		}

		existed := fileExists(outPath)
		if existed && !*flagForce {
			if !confirmOverwriteDialog(outPath) {
				fmt.Fprintf(os.Stderr, "cancelled: did not overwrite %s\n", outPath)
				return
			}
		}
		if err := writeDefaultConfigTo(outPath); err != nil {
			log.Fatalf("failed to write default config: %v", err)
		}
		if existed {
			fmt.Printf("overwrote config: %s\n", outPath)
		} else {
			fmt.Printf("wrote config: %s\n", outPath)
		}
		return
	}

	// Enforce mutual exclusivity: exactly one of --pipe, --file, --primary
	modeCount := 0
	if *flagPipe {
		modeCount++
	}
	if *flagFile != "" {
		modeCount++
	}
	if *flagPrimary {
		modeCount++
	}
	if modeCount != 1 {
		fmt.Fprintln(os.Stderr, "error: specify exactly one of --pipe, --file, or --primary")
		os.Exit(2)
	}

	// Load config
	cfg, err := loadConfigMaybe(*flagConfig)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	// normalize action
	cfg.Action = strings.ToLower(strings.TrimSpace(cfg.Action))
	if cfg.Action != "print" && cfg.Action != "spawn" {
		cfg.Action = "print"
	}

	// TTY detection
	isTTYOut := term.IsTerminal(int(os.Stdout.Fd()))

	// PIPE MODE
	if *flagPipe {
		if err := runPipeMode(cfg, isTTYOut, *flagJSONMatches, *flagJSONDest, *flagOnlyOnMatches, *flagNoTUI); err != nil {
			log.Fatalf("pipe mode error: %v", err)
		}
		return
	}

	// PRIMARY or FILE MODE
	var lines []string
	source := SourceInfo{Kind: "file"}

	if *flagPrimary {
		source.Kind = "primary"
		txt, err := readPrimaryWithXclip()
		if err != nil {
			log.Fatalf("%v", err)
		}
		if strings.TrimSpace(txt) == "" {
			// nothing to show; behave like empty selection
			if cfg.Action == "print" {
				out := Output{Line: "", Matches: []string{}, LineNumber: 0, Source: source}
				enc := json.NewEncoder(os.Stdout)
				enc.SetEscapeHTML(false)
				_ = enc.Encode(out)
			}
			return
		}
		lines = splitLines(txt)
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	} else {
		// file
		var err error
		lines, err = readLines(*flagFile)
		if err != nil {
			log.Fatalf("failed to read file: %v", err)
		}
		source.Path = *flagFile
		if len(lines) == 0 {
			if cfg.Action == "print" {
				out := Output{Line: "", Matches: []string{}, LineNumber: 0, Source: source}
				enc := json.NewEncoder(os.Stdout)
				enc.SetEscapeHTML(false)
				_ = enc.Encode(out)
			}
			return
		}
	}

	// Compile & pre-scan
	cRules := compileRules(cfg)
	ps := precompute(lines, cRules)

	// If only-on-matches and none → exit
	if *flagOnlyOnMatches && ps.totalMatches == 0 {
		// If json-matches requested, there’s nothing to emit, so just exit
		return
	}

	// If requested, emit NDJSON of matches prior to TUI (even in TTY)
	if *flagJSONMatches && ps.totalMatches > 0 {
		wc, err := openDest(*flagJSONDest)
		if err != nil {
			log.Fatalf("json-dest error: %v", err)
		}
		enc := json.NewEncoder(wc)
		enc.SetEscapeHTML(false)
		for _, i := range ps.matchLines {
			rec := Output{
				Line:       ps.lines[i],
				Matches:    ps.info[i].matchesText,
				LineNumber: i + 1,
				Source:     source,
			}
			if err := enc.Encode(rec); err != nil {
				_ = wc.Close()
				log.Fatalf("json encode error: %v", err)
			}
		}
		_ = wc.Close()
	}

	// If no-TUI requested, stop here
	if *flagNoTUI {
		return
	}

	// Open TUI
	lineText, matches, lineNum, ok, _, _ := runListUIWithRules(ps, cfg, source)

	// Print selection JSON only in print mode
	if cfg.Action == "print" {
		var out Output
		out.Source = source
		if ok {
			out.Line = strings.TrimRightFunc(lineText, func(r rune) bool { return r == '\r' })
			out.Matches = matches
			out.LineNumber = lineNum
		} else {
			out.Line = ""
			out.Matches = []string{}
			out.LineNumber = 0
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(out)
	}
}
