package viewer

import (
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/gdamore/tcell/v2"
	"local/capture"
	"local/rules"
	"local/util"
)

type Options struct {
	Title         string
	GutterWidth   int
	ShowTopBar    bool
	ShowBottomBar bool
	Mouse         bool
	NoAlt         bool
}

type Hooks struct {
	OnActivate func(lineText string)
}

type rec struct {
	N    int
	Text string
	M    bool
}

func RunFromFile(capturePath string, meta *capture.Meta, rs []rules.Rule, opts Options, hooks Hooks) error {
	f, err := os.Open(capturePath)
	if err != nil {
		return err
	}
	defer f.Close()
	return runFromReader(f, meta, rs, opts, hooks)
}

func runFromReader(r io.Reader, meta *capture.Meta, rs []rules.Rule, opts Options, hooks Hooks) error {

	rows, err := capture.ReadAllFromReader(r)
	if err != nil {
		return err
	}
	recs := make([]rec, 0, len(rows))
	for _, x := range rows {
		recs = append(recs, rec{N: x.N, Text: x.Text, M: x.M})
	}

	screen, err := tcell.NewScreen()
	if err != nil {
		return err
	}
	if err := screen.Init(); err != nil {
		return err
	}
	defer screen.Fini()

	if opts.Mouse {
		screen.EnableMouse()
	} else {
		screen.DisableMouse()
	}

	normalStyle := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlack)
	matchStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorGreen)
	cursorStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorYellow)
	cursorMatchStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorBlue)
	gutterStyle := tcell.StyleDefault.Foreground(tcell.ColorGray).Background(tcell.ColorBlack)
	gutterCursor := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlue)
	topStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorGreen)
	botStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorYellow)

	gw := opts.GutterWidth
	if gw < 3 {
		gw = 3
	}

	cur := 0
	top := 0

	var lastClickLine = -1
	var lastClickTime = int64(0)
	doubleClickMaxMs := int64(300)

	for {
		w, h := screen.Size()
		bodyTop := 0
		bodyBottom := h
		if opts.ShowTopBar {
			bodyTop = 1
		}
		if opts.ShowBottomBar {
			bodyBottom -= 1
		}
		rowsVis := bodyBottom - bodyTop
		if rowsVis < 1 {
			rowsVis = 1
		}

		if cur < 0 {
			cur = 0
		}
		if cur >= len(recs) {
			cur = len(recs) - 1
		}
		if cur < 0 {
			cur = 0
		}
		if cur < top {
			top = cur
		}
		if cur >= top+rowsVis {
			top = cur - rowsVis + 1
		}
		if top < 0 {
			top = 0
		}
		if top > int(math.Max(0, float64(len(recs)-rowsVis))) {
			top = int(math.Max(0, float64(len(recs)-rowsVis)))
		}

		screen.Clear()

		if opts.ShowTopBar {
			// Build a richer status including capture mode and (for exec) exit code
			mode := ""
			exit := ""
			if meta != nil {
				if meta.Source.Mode != "" {
					mode = fmt.Sprintf("input:%s  ", meta.Source.Mode)
				}
				if meta.Source.Mode == "exec" {
					exit = fmt.Sprintf("exit:%d  ", meta.ExitCode)
				}
			}
			ml := 0
			mt := 0
			if meta != nil {
				ml, mt = meta.MatchLines, meta.MatchesTotal
			}
			s := fmt.Sprintf(" %s | %s%slines:%d  pos:%d/%d  match-lines:%d  matches:%d  (mouse:%v) ",
				opts.Title, mode, exit, len(recs), cur+1, len(recs), ml, mt, opts.Mouse)

			drawLine(screen, 0, 0, w, s, topStyle)
		}

		for row := 0; row < rowsVis; row++ {
			idx := top + row
			if idx >= len(recs) {
				break
			}
			y := bodyTop + row
			rc := recs[idx]

			ln := fmt.Sprintf("%*d", gw-2, rc.N)
			gs := gutterStyle
			if idx == cur {
				gs = gutterCursor
			}
			drawText(screen, 0, y, ln, gs)
			drawText(screen, gw-2, y, ": ", gs)

			spans := rules.AllSpans(rs, rc.Text)
			mapIdx := util.ByteToRuneIndexMap(rc.Text)

			runeSpans := make([][2]int, 0, len(spans))
			for _, se := range spans {
				startRune := util.ByteIndexToRuneIndex(mapIdx, se[0])
				endRune := util.ByteToRuneIndexMap(rc.Text) // not used; left for clarity
				_ = endRune
				endRuneIdx := util.ByteIndexToRuneIndex(mapIdx, se[1])
				if endRuneIdx < startRune {
					endRuneIdx = startRune
				}
				runeSpans = append(runeSpans, [2]int{startRune, endRuneIdx})
			}

			rx := gw
			runeIdx := 0
			for _, r := range rc.Text {
				if rx >= w {
					break
				}
				st := normalStyle
				if idx == cur {
					st = cursorStyle
				}
				if insideAnySpan(runeIdx, runeSpans) {
					if idx == cur {
						st = cursorMatchStyle
					} else {
						st = matchStyle
					}
				}
				screen.SetContent(rx, y, r, nil, st)
				rx++
				runeIdx++
			}
			for ; rx < w; rx++ {
				screen.SetContent(rx, y, ' ', nil, normalStyle)
			}
		}

		if opts.ShowBottomBar {
			status := " ↑/↓ PgUp/PgDn Home/End  Enter=edit  M=toggle-mouse  q/Esc=quit "
			drawLine(screen, 0, h-1, w, status, botStyle)
		}

		screen.Show()

		ev := screen.PollEvent()
		switch e := ev.(type) {
		case *tcell.EventResize:
			screen.Sync()
		case *tcell.EventMouse:
			if !opts.Mouse {
				break
			}
			_, y := e.Position()
			btn := e.Buttons()
			bodyTop := 0
			if opts.ShowTopBar {
				bodyTop = 1
			}
			if y < bodyTop {
				break
			}
			idx := top + (y - bodyTop)
			if idx < 0 || idx >= len(recs) {
				break
			}
			if btn&tcell.Button1 != 0 {
				cur = idx
				// double click?
				now := time.Now().UnixNano() / 1e6
				if lastClickLine == cur && now-lastClickTime <= doubleClickMaxMs {
					if hooks.OnActivate != nil {
						hooks.OnActivate(recs[cur].Text)
					}
				}
				lastClickLine = cur
				lastClickTime = now
			}
		case *tcell.EventKey:
			switch e.Key() {
			case tcell.KeyEsc:
				return nil
			case tcell.KeyEnter:
				if cur >= 0 && cur < len(recs) {
					if hooks.OnActivate != nil {
						hooks.OnActivate(recs[cur].Text)
					}
				}
			case tcell.KeyRune:
				switch e.Rune() {
				case 'q', 'Q':
					return nil
				case 'M', 'm':
					opts.Mouse = !opts.Mouse
					if opts.Mouse {
						screen.EnableMouse()
					} else {
						screen.DisableMouse()
					}
				}
			case tcell.KeyUp:
				cur--
			case tcell.KeyDown:
				cur++
			case tcell.KeyHome:
				cur = 0
			case tcell.KeyEnd:
				cur = len(recs) - 1
			case tcell.KeyPgUp:
				_, h := screen.Size()
				bodyTop := 0
				bodyBottom := h
				if opts.ShowTopBar {
					bodyTop = 1
				}
				if opts.ShowBottomBar {
					bodyBottom -= 1
				}
				rowsVis := bodyBottom - bodyTop
				cur -= rowsVis
			case tcell.KeyPgDn:
				_, h := screen.Size()
				bodyTop := 0
				bodyBottom := h
				if opts.ShowTopBar {
					bodyTop = 1
				}
				if opts.ShowBottomBar {
					bodyBottom -= 1
				}
				rowsVis := bodyBottom - bodyTop
				cur += rowsVis
			}
			if cur < 0 {
				cur = 0
			}
			if cur >= len(recs) {
				cur = len(recs) - 1
			}
		}
	}
}

func insideAnySpan(pos int, spans [][2]int) bool {
	for _, s := range spans {
		if pos >= s[0] && pos < s[1] {
			return true
		}
	}
	return false
}

func drawText(s tcell.Screen, x, y int, text string, st tcell.Style) {
	w, _ := s.Size()
	if y < 0 || x >= w {
		return
	}
	rx := x
	for _, r := range text {
		if rx >= w {
			break
		}
		s.SetContent(rx, y, r, nil, st)
		rx++
	}
}

func drawLine(s tcell.Screen, x, y, w int, text string, st tcell.Style) {
	for i := 0; i < w; i++ {
		s.SetContent(x+i, y, ' ', nil, st)
	}
	drawText(s, x, y, truncateTo(text, w), st)
}

func truncateTo(s string, max int) string {
	if max <= 0 {
		return ""
	}
	out := make([]rune, 0, max)
	for _, r := range s {
		if len(out) >= max {
			break
		}
		out = append(out, r)
	}
	return string(out)
}
