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
	"syscall"
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

type CleanupCfg struct {
	Enabled    bool `toml:"enabled"`
	AgeMinutes int  `toml:"age_minutes"`
}

type UICfg struct {
	ErrLinesMax int `toml:"err_lines_max"` // max height for error panel; 0 disables
}

type Config struct {
	// Action on Enter in TUI: "print" | "spawn"
	Action  string   `toml:"action"`
	Command []string `toml:"command"` // argv; supports ${VAR} and "<path>"

	Colors struct {
		Normal          ColorPair `toml:"normal"`
		Highlight       ColorPair `toml:"highlight"`
		Gutter          ColorPair `toml:"gutter"`
		GutterHighlight ColorPair `toml:"gutter_highlight"`
		Status          ColorPair `toml:"status"`     // bottom status bar
		TopStatus       ColorPair `toml:"top_status"` // top bar
		ErrPanel        ColorPair `toml:"err_panel"`  // error panel
	} `toml:"colors"`

	Rules   []Rule     `toml:"rules"`
	Cleanup CleanupCfg `toml:"cleanup"`
	UI      UICfg      `toml:"ui"`
}

func defaultConfig() Config {
	var c Config
	// default behavior: print json on Enter
	c.Action = "print"
	c.Command = []string{"xfce4-terminal", "--window", "--working-directory=${PWD}", "--execute", "less", "<path>"}

	// colors (names or #rrggbb or palette:N)
	c.Colors.Normal = ColorPair{FG: "white", BG: "black"}
	c.Colors.Highlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Gutter = ColorPair{FG: "gray", BG: "black"}
	c.Colors.GutterHighlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Status = ColorPair{FG: "#000000", BG: "#ffff00"}
	c.Colors.TopStatus = ColorPair{FG: "#000000", BG: "#ff00ff"}
	c.Colors.ErrPanel = ColorPair{FG: "#ffffff", BG: "#303030"}

	// default rule: path:line:col (gcc/go-style)
	c.Rules = []Rule{
		{Name: "path:line:col", Regex: `(?:\.\.?/)?[A-Za-z0-9._/\-]+:\d+:\d+`, FG: "black", BG: "green"},
	}

	// cleanup & UI defaults
	c.Cleanup.Enabled = true
	c.Cleanup.AgeMinutes = 60
	c.UI.ErrLinesMax = 5

	return c
}

/* =========================
   Flags
   ========================= */

var (
	flagFile          = flag.String("file", "", "path to UTF-8 text file (one entry per line)")
	flagConfig        = flag.String("config", "", "path to config TOML (use ::default:: for per-user default path)")
	flagOutputDefault = flag.Bool("output-default-config", false, "print or write a default config TOML and exit")
	flagForce         = flag.Bool("force", false, "allow overwriting existing config when outputting defaults")

	flagPrimary       = flag.Bool("primary", false, "use PRIMARY selection via xclip as input (mutually exclusive with --file/--pipe)")
	flagPipe          = flag.Bool("pipe", false, "read from stdin, pass-through to stdout in real time, then optionally open TUI")
	flagOnlyOnMatches = flag.Bool("only-on-matches", false, "if there are no matches, exit without opening the TUI")
	flagJSONMatches   = flag.Bool("json-matches", false, "emit NDJSON for each matching line (pre-TUI/quasi-print)")
	flagJSONDest      = flag.String("json-dest", "stderr", "NDJSON destination: stderr|stdout|/path/to/file")
	flagNoTUI         = flag.Bool("no-tui", false, "when emitting NDJSON, skip TUI and exit")

	flagErrLinesMax = flag.Int("err-lines", 5, "max lines for bottom error panel (0 disables)")
	flagSpawnDryRun = flag.Bool("spawn-dry-run", false, "do not spawn; log argv with <path> substituted to the error panel")

	flagCleanupNow = flag.Bool("cleanup-orphaned", false, "cleanup old temp files at startup (uses config.cleanup.age_minutes)")
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
func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
func pathExists(p string) bool    { _, err := os.Stat(p); return err == nil }
func exeBase() string {
	exe, err := os.Executable()
	if err != nil {
		return "output-tool"
	}
	return filepath.Base(exe)
}
func tempBase() string            { return filepath.Join(os.TempDir(), exeBase()) }
func tempRoot() string            { return filepath.Join(tempBase(), fmt.Sprintf("%d", os.Getpid())) }
func ensureDir(path string) error { return os.MkdirAll(filepath.Dir(path), 0o755) }
func expandArg(s string) string   { return os.Expand(s, func(k string) string { return os.Getenv(k) }) }
func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	need := false
	for _, r := range arg {
		if r <= 0x20 || strings.ContainsRune(`'"$\`+"`*?[]{}<>|&;()!", r) {
			need = true
			break
		}
	}
	if !need {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}
func expandArgs(argv []string, path string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		a = strings.ReplaceAll(a, "<path>", path)
		a = expandArg(a)
		out = append(out, a)
	}
	return out
}

/* =========================
   Color parsing
   ========================= */

func parseColor(s string) tcell.Color {
	s = strings.TrimSpace(s)
	if s == "" {
		return tcell.ColorReset
	}
	if strings.HasPrefix(s, "palette:") {
		if n, err := strconv.Atoi(strings.TrimPrefix(s, "palette:")); err == nil && n >= 0 && n <= 255 {
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
	sc.Buffer(make([]byte, 0, 64*1024), maxCap)
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
	// clear PRIMARY (best effort)
	cmd := exec.Command("xclip", "-selection", "primary", "-in")
	cmd.Stdin = bytes.NewReader(nil)
	_ = cmd.Run()
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

func writeConfigTo(path string, cfg Config) error {
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

/* =========================
   Confirm overwrite dialog
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

	msg := []string{
		"Configuration file already exists:",
		target,
		"",
		"Overwrite it?  [y]es / [n]o (Enter = yes, Esc = no)",
	}

	draw := func() {
		s.Clear()
		W, H := s.Size()
		boxW := 0
		for _, m := range msg {
			if len(m) > boxW {
				boxW = len(m)
			}
		}
		boxW += 4
		boxH := len(msg) + 2
		x0 := max(0, (W-boxW)/2)
		y0 := max(0, (H-boxH)/2)
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
		for i, m := range msg {
			start := x0 + 2
			for j, r := range m {
				s.SetContent(start+j, y0+1+i, r, nil, bg)
			}
		}
		s.Show()
	}

	draw()
	for {
		switch e := s.PollEvent().(type) {
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
   Regex/matching model
   ========================= */

type compiledRule struct {
	re       *regexp.Regexp
	style    tcell.Style
	styleInv tcell.Style
	name     string
}
type matchSpan struct{ start, end, rule int }
type lineInfo struct {
	spans       []matchSpan
	matchesText []string
}

func compileRules(cfg Config) ([]compiledRule, []ColorPair) {
	var out []compiledRule
	var pairs []ColorPair
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
		out = append(out, compiledRule{
			re:       re,
			style:    styleFrom(cp),
			styleInv: invertStyle(cp),
			name:     r.Name,
		})
		pairs = append(pairs, cp)
	}
	return out, pairs
}

func buildLineInfo(line string, rules []compiledRule) lineInfo {
	var li lineInfo
	if len(rules) == 0 || len(line) == 0 {
		return li
	}

	type rawSpan struct{ start, end, rule int }
	var raw []rawSpan
	for ri, cr := range rules {
		for _, p := range cr.re.FindAllStringIndex(line, -1) {
			raw = append(raw, rawSpan{p[0], p[1], ri})
			li.matchesText = append(li.matchesText, line[p[0]:p[1]])
		}
	}
	if len(raw) == 0 {
		return li
	}

	b := []byte(line)
	owner := make([]int, len(b))
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
	for i := 0; i < len(owner); {
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

func precompute(lines []string, rules []compiledRule) preScan {
	ps := preScan{lines: lines, info: make([]lineInfo, len(lines))}
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
   NDJSON helpers
   ========================= */

type nopCloser struct{ io.Writer }

func (n nopCloser) Close() error { return nil }

func openDest(dest string) (io.WriteCloser, error) {
	switch dest {
	case "stderr":
		return nopCloser{os.Stderr}, nil
	case "stdout":
		return nopCloser{os.Stdout}, nil
	default:
		if err := ensureDir(dest); err != nil {
			return nil, err
		}
		return os.Create(dest)
	}
}

/* =========================
   Temp file helpers (argv[0]-scoped)
   ========================= */

func makeTempJSON() (string, *os.File, error) {
	root := tempRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}
	path := filepath.Join(root, fmt.Sprintf("line-%d.json", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		return "", nil, err
	}
	return path, f, nil
}

/* =========================
   Cleanup orphans
   ========================= */

func cleanupOrphans(base string, olderThan time.Duration) (files int, dirs int, err error) {
	if !pathExists(base) {
		return 0, 0, nil
	}

	now := time.Now()

	// delete stale files we created
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !(strings.HasPrefix(name, "line-") && strings.HasSuffix(name, ".json")) &&
			!(strings.HasPrefix(name, "stream-") && strings.HasSuffix(name, ".log")) {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return nil
		}
		if now.Sub(info.ModTime()) >= olderThan {
			if rmErr := os.Remove(path); rmErr == nil {
				files++
			}
		}
		return nil
	})

	// prune empty dirs
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || !d.IsDir() || path == base {
			return nil
		}
		entries, e := os.ReadDir(path)
		if e == nil && len(entries) == 0 {
			if rmErr := os.Remove(path); rmErr == nil {
				dirs++
			}
		}
		return nil
	})
	return
}

/* =========================
   Data types for output
   ========================= */

type SourceInfo struct {
	Kind string `json:"kind"`           // "file" | "primary" | "pipe"
	Path string `json:"path,omitempty"` // for file mode
}
type Output struct {
	Line       string     `json:"line"`
	Matches    []string   `json:"matches"`
	LineNumber int        `json:"line_number"` // 1-based; 0 on cancel
	Source     SourceInfo `json:"source"`
}

/* =========================
   TUI
   ========================= */

func runListUIWithRules(ps preScan, cfg Config, source SourceInfo, rulePairs []ColorPair, errLinesMax int, errPanel *[]string) (string, []string, int, bool, int, int) {
	lines, info, matchLines, totalMatches := ps.lines, ps.info, ps.matchLines, ps.totalMatches
	if len(lines) == 0 {
		return "", nil, 0, false, 0, 0
	}

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

	normal := styleFrom(cfg.Colors.Normal)
	highlight := styleFrom(cfg.Colors.Highlight)
	gutterStyle := styleFrom(cfg.Colors.Gutter)
	gutterHLStyle := styleFrom(cfg.Colors.GutterHighlight)
	statusStyle := styleFrom(cfg.Colors.Status)
	topStyle := styleFrom(cfg.Colors.TopStatus)
	errStyle := styleFrom(cfg.Colors.ErrPanel)

	if errPanel == nil {
		tmp := []string{}
		errPanel = &tmp
	}
	addErr := func(msg string) {
		*errPanel = append(*errPanel, msg) // buffer only; no stderr during TUI
		s.PostEvent(tcell.NewEventInterrupt(nil))
	}
	getErrH := func() int {
		if errLinesMax <= 0 {
			return 0
		}
		n := len(*errPanel)
		if n <= 0 {
			return 0
		}
		if n > errLinesMax {
			return errLinesMax
		}
		return n
	}

	cursor, offset := 0, 0
	lnWidth := digits(len(lines))
	gutterWidth := lnWidth + 2
	contentLeft := gutterWidth

	ensureCursorVisible := func() {
		_, h := s.Size()
		errH := getErrH()
		usable := max(0, h-2-errH) // top + status + err panel
		if cursor < offset {
			offset = cursor
		} else if cursor >= offset+usable {
			offset = cursor - usable + 1
		}
		maxOffset := max(0, len(lines)-usable)
		offset = clamp(offset, 0, maxOffset)
	}
	putStr := func(x, y int, style tcell.Style, str string, wlimit int) int {
		col, w := x, 0
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
		W, _ := s.Size()
		for x := 0; x < W; x++ {
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

	spawnCurrent := func(idx int) {
		if strings.ToLower(cfg.Action) != "spawn" {
			return
		}
		rec := Output{Line: lines[idx], Matches: info[idx].matchesText, LineNumber: idx + 1, Source: source}
		path, f, err := makeTempJSON()
		if err != nil {
			addErr("temp file create failed: " + err.Error())
			return
		}
		enc := json.NewEncoder(f)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(rec); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			addErr("write json failed: " + err.Error())
			return
		}
		_ = f.Close()

		argv := expandArgs(cfg.Command, path)
		if *flagSpawnDryRun {
			parts := make([]string, 0, len(argv))
			for _, a := range argv {
				parts = append(parts, shellQuote(a))
			}
			addErr("spawn (dry-run): " + strings.Join(parts, " "))
			_ = os.Remove(path)
			return
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			addErr("spawn failed: " + err.Error())
			_ = os.Remove(path)
			return
		}
		_ = cmd.Process.Release()
		addErr(fmt.Sprintf("spawned: pid %d  file %s", cmd.Process.Pid, path))
	}

	drawContentLine := func(rowY, lineIdx, width int, isSel bool) {
		line := lines[lineIdx]
		rowStyle, gutStyle := normal, gutterStyle
		if isSel {
			rowStyle, gutStyle = highlight, gutterHLStyle
		}
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

		// content with painted spans
		avail := max(0, width-contentLeft)
		if avail <= 0 {
			return
		}
		li := info[lineIdx]
		spanIdx := 0
		var curSpan *matchSpan
		if len(li.spans) > 0 {
			curSpan = &li.spans[0]
		}
		bytePos, col := 0, contentLeft
		for _, r := range line {
			if col >= width {
				break
			}
			rStart := bytePos
			bytePos += len(string(r))
			for curSpan != nil && rStart >= curSpan.end && spanIdx+1 < len(li.spans) {
				spanIdx++
				curSpan = &li.spans[spanIdx]
			}
			st := rowStyle
			if curSpan != nil && rStart >= curSpan.start && rStart < curSpan.end {
				ri := li.spans[spanIdx].rule
				if ri >= 0 && ri < len(rulePairs) {
					if isSel {
						st = invertStyle(rulePairs[ri])
					} else {
						st = styleFrom(rulePairs[ri])
					}
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
		errH := getErrH()
		usable := max(0, h-2-errH)

		// top bar
		fillRow(0, topStyle)
		topMsg := fmt.Sprintf(" input:%s  action:%s  match-lines:%d  matches:%d  |  n:next  N:prev ",
			source.Kind, strings.ToLower(cfg.Action), len(matchLines), totalMatches)
		putStr(0, 0, topStyle, topMsg, -1)

		// content
		for row := 0; row < usable; row++ {
			i := offset + row
			if i >= len(lines) {
				fillRow(1+row, normal)
				continue
			}
			drawContentLine(1+row, i, w, i == cursor)
		}

		// error panel
		if errH > 0 {
			startY := 1 + usable
			for i := 0; i < errH; i++ {
				fillRow(startY+i, errStyle)
			}
			msgs := *errPanel
			n := len(msgs)
			first := n - errH
			if first < 0 {
				first = 0
			}
			y := startY
			for i := first; i < n; i++ {
				putStr(0, y, errStyle, msgs[i], -1)
				y++
			}
		}

		// bottom status bar
		statusRow := h - 1
		fillRow(statusRow, statusStyle)
		lineNum := clamp(cursor+1, 1, len(lines))
		charCount := utf8.RuneCountInString(lines[cursor])
		actionLabel := map[bool]string{true: "spawn", false: "select"}[strings.ToLower(cfg.Action) == "spawn"]
		status := fmt.Sprintf(" %d/%d  |  chars: %d  |  ↑/↓ PgUp/PgDn Home/End  Enter=%s  q/Esc=quit ",
			lineNum, len(lines), charCount, actionLabel)
		putStr(0, statusRow, statusStyle, status, -1)

		s.Show()
	}

	ensureCursorVisible()
	draw()

	for {
		switch e := s.PollEvent().(type) {
		case *tcell.EventInterrupt:
			ensureCursorVisible()
			draw()
		case *tcell.EventResize:
			s.Sync()
			ensureCursorVisible()
			draw()
		case *tcell.EventKey:
			switch e.Key() {
			case tcell.KeyEnter:
				if strings.ToLower(cfg.Action) == "spawn" {
					spawnCurrent(cursor)
					ensureCursorVisible()
					draw()
					break
				}
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
				usable := max(0, h-2-getErrH())
				cursor = clamp(cursor-usable, 0, len(lines)-1)
				ensureCursorVisible()
				draw()
			case tcell.KeyPgDn:
				_, h := s.Size()
				usable := max(0, h-2-getErrH())
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
   PIPE mode (stdin streaming)
   ========================= */

func runPipeMode(cfg Config, isTTYOut bool, emitNDJSON bool, jsonDest string, onlyOnMatches bool, noTUI bool, errLinesMax int) error {
	cRules, rulePairs := compileRules(cfg)

	// capture stdin to temp file (so user can open later)
	root := tempRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("mk temp dir: %w", err)
	}
	tempFile := filepath.Join(root, fmt.Sprintf("stream-%d.log", time.Now().UnixNano()))
	outf, err := os.Create(tempFile)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer outf.Close()

	in := bufio.NewScanner(os.Stdin)
	const maxCap = 10 * 1024 * 1024
	in.Buffer(make([]byte, 0, 64*1024), maxCap)

	var lines []string
	matchLines := make([]int, 0, 128)
	totalMatches, lineNo := 0, 0

	for in.Scan() {
		raw := in.Text()
		fmt.Fprintln(os.Stdout, raw) // passthrough
		fmt.Fprintln(outf, raw)      // capture
		lines = append(lines, raw)
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

	ps := preScan{
		lines:        lines,
		info:         make([]lineInfo, len(lines)),
		matchLines:   matchLines,
		totalMatches: totalMatches,
	}
	for i, ln := range lines {
		ps.info[i] = buildLineInfo(ln, cRules)
	}

	// non-tty: cat-like; optional NDJSON
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
				rec := Output{Line: ps.lines[i], Matches: ps.info[i].matchesText, LineNumber: i + 1, Source: src}
				if err := enc.Encode(rec); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// TTY decisions
	if onlyOnMatches && totalMatches == 0 {
		return nil
	}
	if emitNDJSON && totalMatches > 0 {
		wc, err := openDest(jsonDest)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(wc)
		enc.SetEscapeHTML(false)
		src := SourceInfo{Kind: "pipe"}
		for _, i := range ps.matchLines {
			rec := Output{Line: ps.lines[i], Matches: ps.info[i].matchesText, LineNumber: i + 1, Source: src}
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

	src := SourceInfo{Kind: "pipe"}
	var panel []string
	lineText, matches, lineNum, ok, _, _ := runListUIWithRules(ps, cfg, src, rulePairs, errLinesMax, &panel)

	// replay error panel after exit
	if len(panel) > 0 {
		fmt.Fprintln(os.Stderr, "--- output-tool messages ---")
		for _, m := range panel {
			fmt.Fprintln(os.Stderr, m)
		}
	}

	if strings.ToLower(cfg.Action) == "print" {
		out := Output{Source: src}
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

	// detect if user explicitly set --err-lines (so CLI can override config and also override defaults when writing)
	errLinesWasSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "err-lines" {
			errLinesWasSet = true
		}
	})

	// handle default-config writing (with overrides!)
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

		// build defaults, then apply CLI overrides
		cfg := defaultConfig()
		if errLinesWasSet {
			cfg.UI.ErrLinesMax = *flagErrLinesMax
		}

		// write it (with overwrite confirmation if file exists and not --force)
		if outPath == "" {
			if err := writeConfigTo("", cfg); err != nil {
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
		if err := writeConfigTo(outPath, cfg); err != nil {
			log.Fatalf("failed to write default config: %v", err)
		}
		if existed {
			fmt.Printf("overwrote config: %s\n", outPath)
		} else {
			fmt.Printf("wrote config: %s\n", outPath)
		}
		return
	}

	// exactly one input mode
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

	// load config (from provided path or ::default:: or built-in)
	cfg := defaultConfig()
	var tryPath string
	if *flagConfig == "" || *flagConfig == "::default::" {
		if dp, err := defaultConfigPath(); err == nil {
			tryPath = dp
		}
	} else {
		tryPath = *flagConfig
	}
	if tryPath != "" && fileExists(tryPath) {
		if _, err := toml.DecodeFile(tryPath, &cfg); err != nil {
			log.Fatalf("failed to load config: %v", err)
		}
	}

	cfg.Action = strings.ToLower(strings.TrimSpace(cfg.Action))
	if cfg.Action != "print" && cfg.Action != "spawn" {
		cfg.Action = "print"
	}
	if cfg.Cleanup.AgeMinutes <= 0 {
		cfg.Cleanup.AgeMinutes = 60
	}
	if cfg.UI.ErrLinesMax <= 0 {
		cfg.UI.ErrLinesMax = 5
	}

	// effective err panel height: CLI overrides config if provided
	effectiveErrLines := cfg.UI.ErrLinesMax
	if errLinesWasSet {
		effectiveErrLines = *flagErrLinesMax
	}

	// auto- or on-demand cleanup
	if cfg.Cleanup.Enabled || *flagCleanupNow {
		files, dirs, _ := cleanupOrphans(tempBase(), time.Duration(cfg.Cleanup.AgeMinutes)*time.Minute)
		if files+dirs > 0 {
			fmt.Fprintf(os.Stderr, "cleanup-orphaned: removed %d files, %d dirs from %s (older than %d minutes)\n",
				files, dirs, tempBase(), cfg.Cleanup.AgeMinutes)
		}
	}

	// tty detection
	isTTYOut := term.IsTerminal(int(os.Stdout.Fd()))

	// PIPE MODE
	if *flagPipe {
		if err := runPipeMode(cfg, isTTYOut, *flagJSONMatches, *flagJSONDest, *flagOnlyOnMatches, *flagNoTUI, effectiveErrLines); err != nil {
			log.Fatalf("pipe mode error: %v", err)
		}
		return
	}

	// PRIMARY / FILE MODE
	var lines []string
	source := SourceInfo{Kind: "file"}

	if *flagPrimary {
		source.Kind = "primary"
		txt, err := readPrimaryWithXclip()
		if err != nil {
			log.Fatalf("%v", err)
		}
		if strings.TrimSpace(txt) == "" {
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

	// compile + pre-scan
	cRules, rulePairs := compileRules(cfg)
	ps := precompute(lines, cRules)

	// open only on matches if requested
	if *flagOnlyOnMatches && ps.totalMatches == 0 {
		return
	}

	// optional NDJSON before TUI
	if *flagJSONMatches && ps.totalMatches > 0 {
		wc, err := openDest(*flagJSONDest)
		if err != nil {
			log.Fatalf("json-dest error: %v", err)
		}
		enc := json.NewEncoder(wc)
		enc.SetEscapeHTML(false)
		for _, i := range ps.matchLines {
			rec := Output{Line: ps.lines[i], Matches: ps.info[i].matchesText, LineNumber: i + 1, Source: source}
			if err := enc.Encode(rec); err != nil {
				_ = wc.Close()
				log.Fatalf("json encode error: %v", err)
			}
		}
		_ = wc.Close()
	}
	if *flagNoTUI {
		return
	}

	// TUI
	var panel []string
	lineText, matches, lineNum, ok, _, _ := runListUIWithRules(ps, cfg, source, rulePairs, effectiveErrLines, &panel)

	// replay panel after exit
	if len(panel) > 0 {
		fmt.Fprintln(os.Stderr, "--- output-tool messages ---")
		for _, m := range panel {
			fmt.Fprintln(os.Stderr, m)
		}
	}

	// print result only in print mode
	if strings.ToLower(cfg.Action) == "print" {
		out := Output{Source: source}
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
