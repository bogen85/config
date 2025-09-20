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

type ColorPair struct{ FG, BG string }

type Rule struct {
	Name        string `toml:"name"`
	Regex       string `toml:"regex"`
	FileGroup   int    `toml:"file_group"`
	LineGroup   int    `toml:"line_group"`
	ColumnGroup int    `toml:"column_group"`
	FG          string `toml:"fg"`
	BG          string `toml:"bg"`
}

type CleanupCfg struct {
	Enabled    bool `toml:"enabled"`
	AgeMinutes int  `toml:"age_minutes"`
}

type UICfg struct {
	ErrLinesMax int `toml:"err_lines_max"`
}

type Editors struct {
	File        []string `toml:"file"`
	FileLine    []string `toml:"file_line"`
	FileLineCol []string `toml:"file_line_col"`
	Fallback    []string `toml:"fallback"`
	Pretty      bool     `toml:"pretty"`
	UseTabs     bool     `toml:"use_tabs"`
	Spaces      int      `toml:"spaces"`
}

type Config struct {
	Action          string `toml:"action"`
	Pipe            bool   `toml:"pipe"`
	File            string `toml:"file"`
	Primary         bool   `toml:"primary"`
	OnlyOnMatches   bool   `toml:"only_on_matches"`
	OnlyViewMatches bool   `toml:"only_view_matches"`
	JSONMatches     bool   `toml:"json_matches"`
	JSONDest        string `toml:"json_dest"`
	NoTUI           bool   `toml:"no_tui"`

	Colors struct {
		Normal          ColorPair `toml:"normal"`
		Highlight       ColorPair `toml:"highlight"`
		Gutter          ColorPair `toml:"gutter"`
		GutterHighlight ColorPair `toml:"gutter_highlight"`
		Status          ColorPair `toml:"status"`
		TopStatus       ColorPair `toml:"top_status"`
		ErrPanel        ColorPair `toml:"err_panel"`
		CursorGutter    ColorPair `toml:"cursor_gutter"`
	} `toml:"colors"`

	Rules   []Rule     `toml:"rules"`
	Cleanup CleanupCfg `toml:"cleanup"`
	UI      UICfg      `toml:"ui"`
	Editors Editors    `toml:"editors"`

	SplitOnCollision bool `toml:"split_on_collision"`
}

func defaultConfig() Config {
	var c Config
	c.Action = "edit"
	c.Pipe = true
	c.File = ""
	c.Primary = false
	c.OnlyOnMatches = false
	c.OnlyViewMatches = false
	c.JSONMatches = false
	c.JSONDest = "stderr"
	c.NoTUI = false
	c.SplitOnCollision = true

	c.Colors.Normal = ColorPair{FG: "white", BG: "black"}
	c.Colors.Highlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Gutter = ColorPair{FG: "gray", BG: "black"}
	c.Colors.GutterHighlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Status = ColorPair{FG: "#000000", BG: "#ffff00"}
	c.Colors.TopStatus = ColorPair{FG: "#000000", BG: "#00ff00"}
	c.Colors.ErrPanel = ColorPair{FG: "#ffffff", BG: "#303030"}
	c.Colors.CursorGutter = ColorPair{FG: "#ffffff", BG: "#005f87"}

	c.Rules = []Rule{{
		Name:        "path:line:col",
		Regex:       `(?:\.\.?/)?([A-Za-z0-9._/\-]+):(\d+):(\d+)`,
		FileGroup:   1,
		LineGroup:   2,
		ColumnGroup: 3,
		FG:          "black",
		BG:          "green",
	}}

	c.Cleanup.Enabled = true
	c.Cleanup.AgeMinutes = 2
	c.UI.ErrLinesMax = 5

	c.Editors.File = []string{"cudatext", "${__FILE__}"}
	c.Editors.FileLine = []string{"cudatext", "${__FILE__}@${__LINE__}"}
	c.Editors.FileLineCol = []string{"cudatext", "${__FILE__}@${__LINE__}@${__COLUMN__}"}
	c.Editors.Fallback = []string{"cudatext", "${__JSON__}"}
	c.Editors.Pretty = true
	c.Editors.UseTabs = true
	c.Editors.Spaces = 4
	return c
}

var (
	flagFile      = flag.String("file", "", "path to UTF-8 text file")
	flagConfig    = flag.String("config", "", "path to config TOML (use ::default:: for per-user default path)")
	flagOutputNew = flag.Bool("output-new-config", false, "write a NEW config TOML (do not read existing configs) and exit")
	flagForce     = flag.Bool("force", false, "allow overwriting existing config when writing")

	flagAction        = flag.String("action", "edit", `action on Enter: "edit" or "print"`)
	flagPrimary       = flag.Bool("primary", false, "use PRIMARY selection via xclip as input")
	flagPipe          = flag.Bool("pipe", false, "read from stdin and passthrough; optionally open TUI")
	flagOnlyOnMatches = flag.Bool("only-on-matches", false, "if no matches, exit without opening TUI")
	flagOnlyViewMatch = flag.Bool("only-view-matches", false, "show only lines with matches in the TUI")
	flagJSONMatches   = flag.Bool("json-matches", false, "emit NDJSON for each matching line (pre-TUI/quasi-print)")
	flagJSONDest      = flag.String("json-dest", "stderr", "NDJSON destination: stderr|stdout|/path/to/file")
	flagNoTUI         = flag.Bool("no-tui", false, "when emitting NDJSON, skip TUI and exit")
	flagErrLinesMax   = flag.Int("err-lines", 5, "max lines for bottom error panel (0 disables)")
	flagCleanupNow    = flag.Bool("cleanup-orphaned", false, "cleanup old temp files at startup")
	flagSplitOnCol    = flag.Bool("split-on-collision", false, "duplicate view only when different-rule matches overlap")
)

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
func cwdConfigPath() string       { return filepath.Join(".", exeBase()+"-config.toml") }

func expandWithEnv(s string, env map[string]string) string {
	return os.Expand(s, func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return os.Getenv(k)
	})
}
func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	need := false
	for _, r := range arg {
		if r <= 0x20 || strings.ContainsRune(`'"$\\`+"`*?[]{}<>|&;()!", r) {
			need = true
			break
		}
	}
	if !need {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}
func expandArgsWithEnv(argv []string, extra map[string]string, path string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		a = strings.ReplaceAll(a, "<path>", path)
		a = expandWithEnv(a, extra)
		out = append(out, a)
	}
	return out
}

func encodeJSONToFile(v any, f *os.File, pretty, useTabs bool, spaces int) error {
	var b []byte
	var err error
	if pretty {
		indent := "    "
		if useTabs {
			indent = "\t"
		} else if spaces > 0 {
			indent = strings.Repeat(" ", spaces)
		}
		b, err = json.MarshalIndent(v, "", indent)
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func sanitize(s string) string {
	var b []rune
	for _, r := range s {
		if r == '\t' || r >= 0x20 {
			b = append(b, r)
		}
	}
	return string(b)
}

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
	return tcell.GetColor(s)
}
func styleFrom(p ColorPair) tcell.Style {
	return tcell.StyleDefault.Foreground(parseColor(p.FG)).Background(parseColor(p.BG))
}
func invertStyle(p ColorPair) tcell.Style {
	return tcell.StyleDefault.Foreground(parseColor(p.BG)).Background(parseColor(p.FG))
}

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
		lines = append(lines, sanitize(sc.Text()))
	}
	return lines, sc.Err()
}

func readPrimaryWithXclip() (string, error) {
	_, err := exec.LookPath("xclip")
	if err != nil {
		return "", fmt.Errorf("xclip not found on PATH")
	}
	out, err := exec.Command("xclip", "-o", "-selection", "primary").Output()
	if err != nil {
		return "", fmt.Errorf("reading PRIMARY via xclip failed: %w", err)
	}
	cmd := exec.Command("xclip", "-selection", "primary", "-in")
	cmd.Stdin = bytes.NewReader(nil)
	_ = cmd.Run()
	return string(out), nil
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, ln := range raw {
		out = append(out, sanitize(ln))
	}
	return out
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

type compiledRule struct {
	re          *regexp.Regexp
	style       tcell.Style
	styleInv    tcell.Style
	name        string
	fileGroup   int
	lineGroup   int
	columnGroup int
}
type matchSpan struct{ start, end, rule int }
type matchDetail struct {
	start, end int
	rule       int
	text       string
	hasF       bool
	file       string
	hasL       bool
	line       int
	hasC       bool
	column     int
}
type lineInfo struct {
	spans   []matchSpan
	matches []matchDetail
}

func compileRules(cfg Config) ([]compiledRule, []ColorPair) {
	var out []compiledRule
	var pairs []ColorPair
	for i, r := range cfg.Rules {
		if strings.TrimSpace(r.Regex) == "" {
			fmt.Fprintf(os.Stderr, "warning: rule %d empty regex; skipping\n", i)
			continue
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: rule %d regex compile failed: %v; skipping\n", i, err)
			continue
		}
		cp := ColorPair{FG: r.FG, BG: r.BG}
		out = append(out, compiledRule{
			re: re, style: styleFrom(cp), styleInv: invertStyle(cp), name: r.Name,
			fileGroup: r.FileGroup, lineGroup: r.LineGroup, columnGroup: r.ColumnGroup,
		})
		pairs = append(pairs, cp)
	}
	return out, pairs
}

func atoiSafe(s string) (int, bool) { n, err := strconv.Atoi(s); return n, err == nil }

func buildLineInfo(line string, rules []compiledRule) lineInfo {
	var li lineInfo
	if len(rules) == 0 || len(line) == 0 {
		return li
	}
	type rawSpan struct{ start, end, rule int }
	var raw []rawSpan
	for ri, cr := range rules {
		all := cr.re.FindAllStringSubmatchIndex(line, -1)
		for _, idx := range all {
			if len(idx) < 2 {
				continue
			}
			fullStart, fullEnd := idx[0], idx[1]
			raw = append(raw, rawSpan{start: fullStart, end: fullEnd, rule: ri})
			md := matchDetail{start: fullStart, end: fullEnd, rule: ri, text: line[fullStart:fullEnd]}
			if cr.fileGroup > 0 {
				gi := 2 * cr.fileGroup
				if gi+1 < len(idx) && idx[gi] >= 0 && idx[gi+1] >= 0 {
					md.file = line[idx[gi]:idx[gi+1]]
					if md.file != "" {
						md.hasF = true
					}
				}
			}
			if cr.lineGroup > 0 {
				gi := 2 * cr.lineGroup
				if gi+1 < len(idx) && idx[gi] >= 0 && idx[gi+1] >= 0 {
					if n, ok := atoiSafe(line[idx[gi]:idx[gi+1]]); ok {
						md.line, md.hasL = n, true
					}
				}
			}
			if cr.columnGroup > 0 {
				gi := 2 * cr.columnGroup
				if gi+1 < len(idx) && idx[gi] >= 0 && idx[gi+1] >= 0 {
					if n, ok := atoiSafe(line[idx[gi]:idx[gi+1]]); ok {
						md.column, md.hasC = n, true
					}
				}
			}
			li.matches = append(li.matches, md)
		}
	}
	if len(raw) > 0 {
		b := []byte(line)
		owner := make([]int, len(b))
		for i := range owner {
			owner[i] = -1
		}
		for _, rs := range raw {
			s := rs.start
			if s < 0 {
				s = 0
			}
			e := rs.end
			if e > len(b) {
				e = len(b)
			}
			if s >= e {
				continue
			}
			for i := s; i < e; i++ {
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
	}
	return li
}

type preScan struct {
	lines        []string
	info         []lineInfo
	matchLines   []int
	totalMatches int
	origIdx      []int
}

func precompute(lines []string, rules []compiledRule) preScan {
	ps := preScan{lines: lines, info: make([]lineInfo, len(lines)), origIdx: make([]int, len(lines))}
	for i, ln := range lines {
		li := buildLineInfo(ln, rules)
		ps.info[i] = li
		if len(li.matches) > 0 {
			ps.matchLines = append(ps.matchLines, i)
		}
		ps.totalMatches += len(li.matches)
		ps.origIdx[i] = i
	}
	return ps
}

func filterToMatches(ps preScan) preScan {
	if len(ps.matchLines) == 0 {
		return preScan{lines: []string{}, info: []lineInfo{}, matchLines: []int{}, totalMatches: 0, origIdx: []int{}}
	}
	var lines []string
	var info []lineInfo
	var origIdx []int
	for _, i := range ps.matchLines {
		lines = append(lines, ps.lines[i])
		info = append(info, ps.info[i])
		origIdx = append(origIdx, ps.origIdx[i])
	}
	newMatchLines := make([]int, len(lines))
	for i := range newMatchLines {
		newMatchLines[i] = i
	}
	return preScan{lines: lines, info: info, matchLines: newMatchLines, totalMatches: ps.totalMatches, origIdx: origIdx}
}

// --- split_on_collision helpers ---
func overlaps(aStart, aEnd, bStart, bEnd int) bool { return aStart < bEnd && bStart < aEnd }

func ruleSetWithCollisions(mds []matchDetail) map[int]bool {
	collide := map[int]bool{}
	for i := 0; i < len(mds); i++ {
		for j := i + 1; j < len(mds); j++ {
			if mds[i].rule != mds[j].rule && overlaps(mds[i].start, mds[i].end, mds[j].start, mds[j].end) {
				collide[mds[i].rule] = true
				collide[mds[j].rule] = true
			}
		}
	}
	return collide
}

func nonCollidingMatches(mds []matchDetail) map[int]bool {
	ok := map[int]bool{}
	for i := range mds {
		ok[i] = true
	}
	for i := 0; i < len(mds); i++ {
		for j := 0; j < len(mds); j++ {
			if i == j {
				continue
			}
			if mds[i].rule != mds[j].rule && overlaps(mds[i].start, mds[i].end, mds[j].start, mds[j].end) {
				ok[i] = false
				break
			}
		}
	}
	return ok
}

func buildViewMatchesForRule(all []matchDetail, rule int) []matchDetail {
	nonCol := nonCollidingMatches(all)
	var ruleSpans [][2]int
	for _, md := range all {
		if md.rule == rule {
			ruleSpans = append(ruleSpans, [2]int{md.start, md.end})
		}
	}
	noOverlapWithRule := func(md matchDetail) bool {
		for _, sp := range ruleSpans {
			if overlaps(md.start, md.end, sp[0], sp[1]) {
				return false
			}
		}
		return true
	}
	var out []matchDetail
	for i, md := range all {
		if md.rule == rule {
			out = append(out, md)
			continue
		}
		if nonCol[i] {
			out = append(out, md)
			continue
		}
		if noOverlapWithRule(md) {
			out = append(out, md)
		}
	}
	return out
}
func buildSpansFromMatches(mds []matchDetail, line string) []matchSpan {
	if len(mds) == 0 {
		return nil
	}
	b := []byte(line)
	owner := make([]int, len(b))
	for i := range owner {
		owner[i] = -1
	}
	for _, md := range mds {
		s := md.start
		if s < 0 {
			s = 0
		}
		e := md.end
		if e > len(b) {
			e = len(b)
		}
		for i := s; i < e; i++ {
			owner[i] = md.rule
		}
	}
	var spans []matchSpan
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
		spans = append(spans, matchSpan{start: i, end: j, rule: rule})
		i = j
	}
	return spans
}
func precomputeSplitOnCollision(lines []string, rules []compiledRule) preScan {
	var ps preScan
	for i, ln := range lines {
		all := buildLineInfo(ln, rules)
		if len(all.matches) == 0 {
			continue
		}
		colRules := ruleSetWithCollisions(all.matches)
		if len(colRules) == 0 {
			ps.lines = append(ps.lines, ln)
			ps.info = append(ps.info, all)
			ps.matchLines = append(ps.matchLines, len(ps.lines)-1)
			ps.totalMatches += len(all.matches)
			ps.origIdx = append(ps.origIdx, i)
			continue
		}
		for rr := range colRules {
			viewMatches := buildViewMatchesForRule(all.matches, rr)
			var li lineInfo
			li.matches = viewMatches
			li.spans = buildSpansFromMatches(viewMatches, ln)
			ps.lines = append(ps.lines, ln)
			ps.info = append(ps.info, li)
			ps.matchLines = append(ps.matchLines, len(ps.lines)-1)
			ps.totalMatches += len(li.matches)
			ps.origIdx = append(ps.origIdx, i)
		}
	}
	return ps
}

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

func cleanupOrphans(base string, olderThan time.Duration) (files, dirs int, err error) {
	if !pathExists(base) {
		return 0, 0, nil
	}
	now := time.Now()
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
			if os.Remove(path) == nil {
				files++
			}
		}
		return nil
	})
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || !d.IsDir() || path == base {
			return nil
		}
		entries, e := os.ReadDir(path)
		if e == nil && len(entries) == 0 {
			if os.Remove(path) == nil {
				dirs++
			}
		}
		return nil
	})
	return
}

type SourceInfo struct {
	Kind, Path string `json:"kind","path,omitempty"`
}
type Output struct {
	Line       string     `json:"line"`
	Matches    []string   `json:"matches"`
	LineNumber int        `json:"line_number"`
	Source     SourceInfo `json:"source"`
}

func encodeLineJSONForEditor(line string, matches []string, lineNum int, source SourceInfo, editors Editors) (string, error) {
	path, f, err := makeTempJSON()
	if err != nil {
		return "", err
	}
	rec := Output{Line: line, Matches: matches, LineNumber: lineNum, Source: source}
	if err := encodeJSONToFile(rec, f, editors.Pretty, editors.UseTabs, editors.Spaces); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	_ = f.Close()
	return path, nil
}

func envToList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func launchEditorForMatch(cfg Config, md matchDetail, source SourceInfo, line string, matches []string, lineNum int, addErr func(string)) {
	env := map[string]string{}
	if md.hasF {
		env["__FILE__"] = md.file
	}
	if md.hasL {
		env["__LINE__"] = strconv.Itoa(md.line)
	}
	if md.hasC {
		env["__COLUMN__"] = strconv.Itoa(md.column)
	}
	var argv []string
	switch {
	case md.hasF && md.hasL && md.hasC && len(cfg.Editors.FileLineCol) > 0:
		argv = cfg.Editors.FileLineCol
	case md.hasF && md.hasL && len(cfg.Editors.FileLine) > 0:
		argv = cfg.Editors.FileLine
	case md.hasF && len(cfg.Editors.File) > 0:
		argv = cfg.Editors.File
	default:
		jsonPath, err := encodeLineJSONForEditor(line, matches, lineNum, source, cfg.Editors)
		if err != nil {
			addErr("edit: temp json create failed: " + err.Error())
			return
		}
		env["__JSON__"] = jsonPath
		argv = cfg.Editors.Fallback
		if len(argv) == 0 {
			addErr("edit: no fallback editor defined")
			return
		}
	}
	final := expandArgsWithEnv(argv, env, "")
	if len(final) == 0 {
		addErr("edit: empty editor argv")
		return
	}
	cmd := exec.Command(final[0], final[1:]...)
	cmd.Env = append(os.Environ(), envToList(env)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		addErr("edit: spawn failed: " + err.Error())
		return
	}
	_ = cmd.Process.Release()
	shown := make([]string, len(final))
	for i, a := range final {
		shown[i] = shellQuote(a)
	}
	msg := "edit: exec: " + strings.Join(shown, " ")
	if jp, ok := env["__JSON__"]; ok && jp != "" {
		msg += "  (json: " + filepath.Base(jp) + ")"
	}
	addErr(msg)
}

func runListUIWithRules(ps preScan, cfg Config, source SourceInfo, rulePairs []ColorPair, errLinesMax int, errPanel *[]string) (string, []string, int, bool, int, int) {
	lines, info, matchLines, totalMatches, origIdx := ps.lines, ps.info, ps.matchLines, ps.totalMatches, ps.origIdx
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
	cursorGutterStyle := styleFrom(cfg.Colors.CursorGutter)
	statusStyle := styleFrom(cfg.Colors.Status)
	topStyle := styleFrom(cfg.Colors.TopStatus)
	errStyle := styleFrom(cfg.Colors.ErrPanel)

	if errPanel == nil {
		tmp := []string{}
		errPanel = &tmp
	}
	addErr := func(msg string) {
		*errPanel = append(*errPanel, msg)
		if errLinesMax > 0 && len(*errPanel) > errLinesMax*4 {
			keep := errLinesMax * 2
			if keep < errLinesMax {
				keep = errLinesMax
			}
			*errPanel = append([]string(nil), (*errPanel)[len(*errPanel)-keep:]...)
		}
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
	ellipsis := func(str string, maxw int) string {
		if maxw <= 0 {
			return ""
		}
		w := 0
		var b strings.Builder
		for _, r := range str {
			if w+1 >= maxw {
				b.WriteRune('…')
				return b.String()
			}
			b.WriteRune(r)
			w++
		}
		for w < maxw {
			b.WriteByte(' ')
			w++
		}
		return b.String()
	}
	rangeHint := func(n int) string {
		if n <= 1 {
			return ""
		}
		if n >= 10 {
			return "[1-9,0]=choose-match"
		}
		return "[1-" + strconv.Itoa(n) + "]=choose-match"
	}

	cursor, offset := 0, 0
	lnWidth := digits(len(origIdx))
	gutterWidth := lnWidth + 2
	contentLeft := gutterWidth

	ensureCursorVisible := func() {
		_, h := s.Size()
		errH := getErrH()
		usable := max(0, h-2-errH)
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

	editJSONForLine := func(idx int) {
		orig := origIdx[idx] + 1
		mt := []string{}
		for _, m := range info[idx].matches {
			mt = append(mt, m.text)
		}
		jsonPath, err := encodeLineJSONForEditor(lines[idx], mt, orig, source, cfg.Editors)
		if err != nil {
			addErr("edit-json: " + err.Error())
			return
		}
		env := map[string]string{"__JSON__": jsonPath}
		argv := cfg.Editors.Fallback
		if len(argv) == 0 {
			addErr("edit-json: no fallback editor defined")
			return
		}
		final := expandArgsWithEnv(argv, env, "")
		if len(final) == 0 {
			addErr("edit-json: empty argv")
			return
		}
		cmd := exec.Command(final[0], final[1:]...)
		cmd.Env = append(os.Environ(), envToList(env)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			addErr("edit-json: spawn failed: " + err.Error())
			return
		}
		_ = cmd.Process.Release()
		argvShown := make([]string, len(final))
		for i, a := range final {
			argvShown[i] = shellQuote(a)
		}
		addErr("edit-json: exec: " + strings.Join(argvShown, " ") + "  (json: " + filepath.Base(jsonPath) + ")")
	}

	editCurrent := func(idx int) {
		matches := info[idx].matches
		switch len(matches) {
		case 0:
			editJSONForLine(idx)
		case 1:
			orig := origIdx[idx] + 1
			mt := []string{}
			for _, m := range info[idx].matches {
				mt = append(mt, m.text)
			}
			launchEditorForMatch(cfg, matches[0], source, lines[idx], mt, orig, addErr)
		default:
			addErr("edit: multiple matches on line — press " + rangeHint(len(matches)) + " or j to edit JSON")
		}
	}

	drawContentLine := func(rowY, lineIdx, width int, isSel bool) {
		line := lines[lineIdx]
		rowStyle, gutStyle := normal, gutterStyle
		if isSel {
			rowStyle, gutStyle = highlight, gutterHLStyle
			// override gutter on selected row if cursor_gutter provided
			if cfg.Colors.CursorGutter.FG != "" || cfg.Colors.CursorGutter.BG != "" {
				gutStyle = cursorGutterStyle
			}
		}
		fillRow(rowY, rowStyle)

		// gutter
		numStr := strconv.Itoa(origIdx[lineIdx] + 1)
		pad := lnWidth - len(numStr)
		x := 0
		for j := 0; j < pad; j++ {
			s.SetContent(x, rowY, ' ', nil, gutStyle)
			x++
		}
		x += putStr(x, rowY, gutStyle, numStr, -1)
		x += putStr(x, rowY, gutStyle, ": ", -1)

		// content with spans
		avail := max(0, width-contentLeft)
		if avail <= 0 {
			for xx := x; xx < width; xx++ {
				s.SetContent(xx, rowY, ' ', nil, rowStyle)
			}
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
			bytePos += utf8.RuneLen(r)
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
		for xx := col; xx < width; xx++ {
			s.SetContent(xx, rowY, ' ', nil, rowStyle)
		}
	}

	draw := func() {
		s.Sync()
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
		label := "match-lines"
		if cfg.SplitOnCollision {
			label = "match-views"
		}
		topMsg := fmt.Sprintf(" input:%s  action:%s  %s:%d  matches:%d  |  n:next  N:prev ", source.Kind, strings.ToLower(cfg.Action), label, len(matchLines), totalMatches)
		putStr(0, 0, topStyle, ellipsis(topMsg, w), -1)

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
				line := ellipsis(msgs[i], w)
				putStr(0, y, errStyle, line, -1)
				y++
			}
		}

		// bottom status
		statusRow := h - 1
		fillRow(statusRow, statusStyle)
		origNum := origIdx[cursor] + 1
		charCount := utf8.RuneCountInString(lines[cursor])
		base := "Enter=" + strings.ToLower(cfg.Action)
		help := "  e/E=edit  " + rangeHint(len(info[cursor].matches)) + "  j/J=edit-json  q/Esc=quit"
		status := fmt.Sprintf(" %d/%d (orig #%d) | chars: %d | ↑/↓ PgUp/PgDn Home/End  %s %s ", cursor+1, len(lines), origNum, charCount, base, help)
		putStr(0, statusRow, statusStyle, ellipsis(status, w), -1)
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
				switch strings.ToLower(cfg.Action) {
				case "edit":
					editCurrent(cursor)
					ensureCursorVisible()
					draw()
				default:
					mt := []string{}
					for _, m := range info[cursor].matches {
						mt = append(mt, m.text)
					}
					return lines[cursor], mt, origIdx[cursor] + 1, true, len(matchLines), totalMatches
				}
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
					editJSONForLine(cursor)
					ensureCursorVisible()
					draw()
				case 'J':
					editJSONForLine(cursor)
					ensureCursorVisible()
					draw()
				case 'e', 'E':
					editCurrent(cursor)
					ensureCursorVisible()
					draw()
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
				case '1', '2', '3', '4', '5', '6', '7', '8', '9', '0':
					ms := info[cursor].matches
					if len(ms) > 1 {
						pick := -1
						r := e.Rune()
						if r >= '1' && r <= '9' {
							pick = int(r - '1')
						} else if r == '0' {
							pick = 9
						}
						if pick >= 0 && pick < len(ms) {
							orig := origIdx[cursor] + 1
							mt := []string{}
							for _, m := range ms {
								mt = append(mt, m.text)
							}
							launchEditorForMatch(cfg, ms[pick], source, lines[cursor], mt, orig, func(s string) { (*errPanel) = append(*errPanel, s); _ = tcell.NewEventInterrupt(nil) })
							ensureCursorVisible()
							draw()
						}
					}
				}
			}
		}
	}
}

func resolveInputModes(cfg *Config, explicit map[string]bool) (chosen, note string) {
	expFile := explicit != nil && explicit["file"] && strings.TrimSpace(cfg.File) != ""
	expPrimary := explicit != nil && explicit["primary"] && cfg.Primary
	expPipe := explicit != nil && explicit["pipe"] && cfg.Pipe
	if expFile || expPrimary || expPipe {
		if expFile {
			cfg.Pipe = false
			cfg.Primary = false
			chosen = "file"
		} else if expPrimary {
			cfg.Pipe = false
			cfg.File = ""
			chosen = "primary"
		} else {
			cfg.File = ""
			cfg.Primary = false
			chosen = "pipe"
		}
		count := 0
		if expFile {
			count++
		}
		if expPrimary {
			count++
		}
		if expPipe {
			count++
		}
		if count > 1 {
			note = "multiple input modes specified on CLI; resolved by priority (file > primary > pipe)"
		}
		return
	}
	count := 0
	if cfg.Pipe {
		count++
	}
	if strings.TrimSpace(cfg.File) != "" {
		count++
	}
	if cfg.Primary {
		count++
	}
	if count <= 1 {
		if cfg.Pipe {
			chosen = "pipe"
		} else if strings.TrimSpace(cfg.File) != "" {
			chosen = "file"
		} else if cfg.Primary {
			chosen = "primary"
		} else {
			chosen = ""
		}
		return
	}
	if strings.TrimSpace(cfg.File) != "" {
		cfg.Pipe = false
		cfg.Primary = false
		chosen = "file"
		note = "multiple input modes in config; resolved by priority (file > primary > pipe)"
		return
	}
	if cfg.Primary {
		cfg.Pipe = false
		cfg.File = ""
		chosen = "primary"
		note = "multiple input modes in config; resolved by priority (file > primary > pipe)"
		return
	}
	cfg.File = ""
	cfg.Primary = false
	chosen = "pipe"
	note = "multiple input modes in config; resolved by priority (file > primary > pipe)"
	return
}

func runPipeMode(cfg Config, isTTYOut, emitNDJSON bool, jsonDest string, onlyOnMatches, onlyView, noTUI bool, errLinesMax int) error {
	cRules, rulePairs := compileRules(cfg)

	// capture stdin
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
	for in.Scan() {
		raw := sanitize(in.Text())
		fmt.Fprintln(os.Stdout, raw)
		fmt.Fprintln(outf, raw)
		lines = append(lines, raw)
	}
	if err := in.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	_ = outf.Sync()

	var ps preScan
	if cfg.SplitOnCollision {
		ps = precomputeSplitOnCollision(lines, cRules)
	} else {
		ps = precompute(lines, cRules)
	}

	// non-tty
	if !isTTYOut {
		if emitNDJSON && ps.totalMatches > 0 {
			wc, err := openDest(jsonDest)
			if err != nil {
				return err
			}
			defer wc.Close()
			enc := json.NewEncoder(wc)
			enc.SetEscapeHTML(false)
			src := SourceInfo{Kind: "pipe"}
			for _, i := range ps.matchLines {
				mt := []string{}
				for _, m := range ps.info[i].matches {
					mt = append(mt, m.text)
				}
				rec := Output{Line: ps.lines[i], Matches: mt, LineNumber: ps.origIdx[i] + 1, Source: src}
				if err := enc.Encode(rec); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if onlyView {
		ps = filterToMatches(ps)
	}
	if onlyOnMatches && ps.totalMatches == 0 {
		return nil
	}

	if emitNDJSON && ps.totalMatches > 0 {
		wc, err := openDest(jsonDest)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(wc)
		enc.SetEscapeHTML(false)
		src := SourceInfo{Kind: "pipe"}
		for _, i := range ps.matchLines {
			mt := []string{}
			for _, m := range ps.info[i].matches {
				mt = append(mt, m.text)
			}
			rec := Output{Line: ps.lines[i], Matches: mt, LineNumber: ps.origIdx[i] + 1, Source: src}
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
	lineText, matchesText, lineNum, ok, _, _ := runListUIWithRules(ps, cfg, src, rulePairs, errLinesMax, &panel)

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
			out.Matches = matchesText
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

func main() {
	flag.Parse()
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if *flagOutputNew {
		var outPath string
		if *flagConfig == "::default::" {
			dp, err := defaultConfigPath()
			if err != nil {
				log.Fatalf("cannot resolve default config path: %v", err)
			}
			outPath = dp
		} else {
			outPath = *flagConfig
		}
		cfg := defaultConfig()
		if set["err-lines"] {
			cfg.UI.ErrLinesMax = *flagErrLinesMax
		}
		if set["action"] {
			cfg.Action = strings.ToLower(strings.TrimSpace(*flagAction))
		}
		if set["pipe"] {
			cfg.Pipe = *flagPipe
		}
		if set["file"] {
			cfg.File = *flagFile
		}
		if set["primary"] {
			cfg.Primary = *flagPrimary
		}
		if set["only-on-matches"] {
			cfg.OnlyOnMatches = *flagOnlyOnMatches
		}
		if set["only-view-matches"] {
			cfg.OnlyViewMatches = *flagOnlyViewMatch
		}
		if set["json-matches"] {
			cfg.JSONMatches = *flagJSONMatches
		}
		if set["json-dest"] {
			cfg.JSONDest = *flagJSONDest
		}
		if set["no-tui"] {
			cfg.NoTUI = *flagNoTUI
		}
		if set["split-on-collision"] {
			cfg.SplitOnCollision = *flagSplitOnCol
		}

		_, note := resolveInputModes(&cfg, set)
		if note != "" {
			fmt.Fprintln(os.Stderr, note)
		}

		if outPath == "" {
			if err := writeConfigTo("", cfg); err != nil {
				log.Fatalf("failed to write new config: %v", err)
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
			log.Fatalf("failed to write new config: %v", err)
		}
		if existed {
			fmt.Printf("overwrote config: %s\n", outPath)
		} else {
			fmt.Printf("wrote config: %s\n", outPath)
		}
		return
	}

	// load config with precedence
	cfg := defaultConfig()
	var pathToLoad string
	switch {
	case *flagConfig != "" && *flagConfig != "::default::":
		pathToLoad = *flagConfig
	case *flagConfig == "" || *flagConfig == "::default::":
		if fileExists(cwdConfigPath()) {
			pathToLoad = cwdConfigPath()
			break
		}
		if dp, err := defaultConfigPath(); err == nil && fileExists(dp) {
			pathToLoad = dp
		}
	}
	if pathToLoad != "" {
		if _, err := toml.DecodeFile(pathToLoad, &cfg); err != nil {
			log.Fatalf("failed to load config: %v", err)
		}
	}

	// CLI overrides
	if set["action"] {
		cfg.Action = strings.ToLower(strings.TrimSpace(*flagAction))
	}
	if set["err-lines"] {
		cfg.UI.ErrLinesMax = *flagErrLinesMax
	}
	if set["pipe"] {
		cfg.Pipe = *flagPipe
	}
	if set["file"] {
		cfg.File = *flagFile
	}
	if set["primary"] {
		cfg.Primary = *flagPrimary
	}
	if set["only-on-matches"] {
		cfg.OnlyOnMatches = *flagOnlyOnMatches
	}
	if set["only-view-matches"] {
		cfg.OnlyViewMatches = *flagOnlyViewMatch
	}
	if set["json-matches"] {
		cfg.JSONMatches = *flagJSONMatches
	}
	if set["json-dest"] {
		cfg.JSONDest = *flagJSONDest
	}
	if set["no-tui"] {
		cfg.NoTUI = *flagNoTUI
	}
	if set["split-on-collision"] {
		cfg.SplitOnCollision = *flagSplitOnCol
	}

	// resolve + validate
	_, note := resolveInputModes(&cfg, set)
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	cfg.Action = strings.ToLower(strings.TrimSpace(cfg.Action))
	if cfg.Action != "print" && cfg.Action != "edit" {
		cfg.Action = "edit"
	}
	if cfg.Cleanup.AgeMinutes <= 0 {
		cfg.Cleanup.AgeMinutes = 60
	}
	if cfg.UI.ErrLinesMax <= 0 {
		cfg.UI.ErrLinesMax = 5
	}
	if cfg.Editors.Spaces <= 0 {
		cfg.Editors.Spaces = 4
	}
	if strings.TrimSpace(cfg.JSONDest) == "" {
		cfg.JSONDest = "stderr"
	}
	if !cfg.Pipe && strings.TrimSpace(cfg.File) == "" && !cfg.Primary {
		fmt.Fprintln(os.Stderr, "error: no input selected; set one of: pipe=true, file=..., or primary=true")
		os.Exit(2)
	}

	// cleanup
	if cfg.Cleanup.Enabled || *flagCleanupNow {
		files, dirs, _ := cleanupOrphans(tempBase(), time.Duration(cfg.Cleanup.AgeMinutes)*time.Minute)
		if files+dirs > 0 {
			fmt.Fprintf(os.Stderr, "cleanup-orphaned: removed %d files, %d dirs from %s (older than %d minutes)\n", files, dirs, tempBase(), cfg.Cleanup.AgeMinutes)
		}
	}

	isTTYOut := term.IsTerminal(int(os.Stdout.Fd()))
	if cfg.Pipe {
		if err := runPipeMode(cfg, isTTYOut, cfg.JSONMatches, cfg.JSONDest, cfg.OnlyOnMatches, cfg.OnlyViewMatches, cfg.NoTUI, cfg.UI.ErrLinesMax); err != nil {
			log.Fatalf("pipe mode error: %v", err)
		}
		return
	}

	// Primary/File path
	var lines []string
	source := SourceInfo{Kind: "file"}
	if cfg.Primary {
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
		lines, err = readLines(cfg.File)
		if err != nil {
			log.Fatalf("failed to read file: %v", err)
		}
		source.Path = cfg.File
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

	cRules, rulePairs := compileRules(cfg)
	var ps preScan
	if cfg.SplitOnCollision {
		ps = precomputeSplitOnCollision(lines, cRules)
	} else {
		ps = precompute(lines, cRules)
	}
	if cfg.OnlyViewMatches {
		ps = filterToMatches(ps)
	}
	if cfg.OnlyOnMatches && ps.totalMatches == 0 {
		return
	}

	if cfg.JSONMatches && ps.totalMatches > 0 {
		wc, err := openDest(cfg.JSONDest)
		if err != nil {
			log.Fatalf("json-dest error: %v", err)
		}
		enc := json.NewEncoder(wc)
		enc.SetEscapeHTML(false)
		for i := range ps.lines {
			if len(ps.info[i].matches) == 0 {
				continue
			}
			mt := []string{}
			for _, m := range ps.info[i].matches {
				mt = append(mt, m.text)
			}
			rec := Output{Line: ps.lines[i], Matches: mt, LineNumber: ps.origIdx[i] + 1, Source: source}
			if err := enc.Encode(rec); err != nil {
				_ = wc.Close()
				log.Fatalf("json encode error: %v", err)
			}
		}
		_ = wc.Close()
	}
	if cfg.NoTUI {
		return
	}

	var panel []string
	lineText, matchesText, lineNum, ok, _, _ := runListUIWithRules(ps, cfg, source, rulePairs, cfg.UI.ErrLinesMax, &panel)
	if len(panel) > 0 {
		fmt.Fprintln(os.Stderr, "--- output-tool messages ---")
		for _, m := range panel {
			fmt.Fprintln(os.Stderr, m)
		}
	}
	if strings.ToLower(cfg.Action) == "print" {
		out := Output{Source: source}
		if ok {
			out.Line = strings.TrimRightFunc(lineText, func(r rune) bool { return r == '\r' })
			out.Matches = matchesText
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
