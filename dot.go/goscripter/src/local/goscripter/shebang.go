package goscripter

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type shebangInfo struct {
	hasShebang bool
	line       string
	path       string
	argv       []string
}

func readFirstLine(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, os.ErrClosed) && err.Error() != "EOF" {
		// still return the partial line
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func parseShebang(script string) (shebangInfo, error) {
	line, err := readFirstLine(script)
	if err != nil {
		return shebangInfo{}, err
	}
	if !strings.HasPrefix(line, "#!") {
		return shebangInfo{hasShebang: false}, nil
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "#!"))
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return shebangInfo{hasShebang: true, line: line}, nil
	}
	return shebangInfo{
		hasShebang: true,
		line:       line,
		path:       fields[0],
		argv:       fields[1:],
	}, nil
}

func isEnvGoscripter(sb shebangInfo) bool {
	if !sb.hasShebang {
		return false
	}
	if !strings.HasSuffix(sb.path, "/usr/bin/env") && sb.path != "env" {
		return false
	}
	return len(sb.argv) > 0 && sb.argv[0] == "goscripter"
}

func sameFile(a, b string) bool {
	ap, _ := filepath.EvalSymlinks(a)
	bp, _ := filepath.EvalSymlinks(b)
	if ap == "" {
		ap = a
	}
	if bp == "" {
		bp = b
	}
	return ap == bp
}

func desiredShebangAbs() string { return "#!" + selfAbsPath() + " run" }

func desiredShebangEnvOrAbsForApply(sb shebangInfo) string {
	if isEnvGoscripter(sb) {
		if ep := lookPathGoscripter(); ep != "" && sameFile(ep, selfAbsPath()) {
			return "#!/usr/bin/env goscripter run"
		}
	}
	return desiredShebangAbs()
}

func writeShebangLinePreserveMode(script, newLine string) (changed bool, err error) {
	info, statErr := os.Stat(script)
	if statErr != nil {
		return false, statErr
	}
	origMode := info.Mode().Perm()

	b, err := os.ReadFile(script)
	if err != nil {
		return false, err
	}
	lines := bytes.Split(b, []byte("\n"))
	if len(lines) == 0 {
		lines = [][]byte{{}}
	}

	current := ""
	if bytes.HasPrefix(lines[0], []byte("#!")) {
		current = string(lines[0])
	}
	if current == newLine {
		return false, nil
	}

	if current != "" {
		lines[0] = []byte(newLine)
	} else {
		lines = append([][]byte{[]byte(newLine)}, lines...)
	}
	tmp := script + ".goscripter.shebang.tmp"
	if err := os.WriteFile(tmp, bytes.Join(lines, []byte("\n")), origMode); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, script); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}
