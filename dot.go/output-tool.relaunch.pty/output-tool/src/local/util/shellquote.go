package util

import (
	"bytes"
	"strings"
)

func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$&;|*?<>`()[]{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func SplitLauncher(s string) []string {
	var out []string
	var cur bytes.Buffer
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == 0 {
			if c == '\'' || c == '"' {
				quote = c
				continue
			}
			if c == ' ' || c == '\t' {
				if cur.Len() > 0 {
					out = append(out, cur.String())
					cur.Reset()
				}
				continue
			}
			cur.WriteByte(c)
		} else {
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
