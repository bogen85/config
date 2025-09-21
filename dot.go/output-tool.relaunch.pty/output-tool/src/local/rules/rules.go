package rules

import "regexp"

type Rule struct {
	ID          string
	Regex       *regexp.Regexp
	FileGroup   int // 1-based capture group index for file path (0 = none)
	LineGroup   int // 1-based capture group index for line number
	ColumnGroup int // 1-based capture group index for column number
}

func Default() []Rule {
	rx := regexp.MustCompile(`(?:\.?\.?\/)?([A-Za-z0-9._\/\-]+):(\d+):(\d+)`)
	return []Rule{
		{ID: "path:line:col", Regex: rx, FileGroup: 1, LineGroup: 2, ColumnGroup: 3},
	}
}

func AnyMatch(rs []Rule, line string) (bool, int) {
	total := 0
	for _, r := range rs {
		locs := r.Regex.FindAllStringIndex(line, -1)
		if len(locs) > 0 {
			total += len(locs)
		}
	}
	return total > 0, total
}

// AllSpans returns merged byte spans [start,end) for highlighting
func AllSpans(rs []Rule, s string) [][2]int {
	spans := make([][2]int, 0, 2)
	for _, r := range rs {
		locs := r.Regex.FindAllStringIndex(s, -1)
		for _, se := range locs {
			spans = append(spans, [2]int{se[0], se[1]})
		}
	}
	if len(spans) <= 1 {
		return spans
	}
	return coalesce(spans)
}

func coalesce(spans [][2]int) [][2]int {
	// insertion sort by start
	for i := 1; i < len(spans); i++ {
		j := i
		for j > 0 && spans[j-1][0] > spans[j][0] {
			spans[j-1], spans[j] = spans[j], spans[j-1]
			j--
		}
	}
	out := make([][2]int, 0, len(spans))
	cur := spans[0]
	for i := 1; i < len(spans); i++ {
		s := spans[i]
		if s[0] <= cur[1] { // overlap/touch
			if s[1] > cur[1] {
				cur[1] = s[1]
			}
		} else {
			out = append(out, cur)
			cur = s
		}
	}
	out = append(out, cur)
	return out
}

// ExtractPathLineCol returns first occurrence
func ExtractPathLineCol(rs []Rule, line string) (file string, lineNo, col int, ok bool) {
	for _, r := range rs {
		idxs := r.Regex.FindStringSubmatchIndex(line)
		if idxs == nil {
			continue
		}
		get := func(g int) (string, bool) {
			if g <= 0 {
				return "", false
			}
			i := 2 * g
			if i+1 >= len(idxs) || idxs[i] < 0 || idxs[i+1] < 0 {
				return "", false
			}
			return line[idxs[i]:idxs[i+1]], true
		}
		if file, ok = get(r.FileGroup); !ok {
			continue
		}
		if s, ok2 := get(r.LineGroup); ok2 {
			lineNo, _ = atoiSafe(s)
		}
		if s, ok2 := get(r.ColumnGroup); ok2 {
			col, _ = atoiSafe(s)
		}
		return file, lineNo, col, true
	}
	return "", 0, 0, false
}

func atoiSafe(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errAtoi{}
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

type errAtoi struct{}

func (errAtoi) Error() string { return "not a number" }
