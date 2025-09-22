// viewer/status.go (new helper, or inline where you format the top bar)
package viewer

import (
	"fmt"
	"local/capture"
)

type Counters struct {
	MatchLines   int
	MatchesTotal int
}

func buildTopStatus(meta *capture.Meta, counters Counters, shortcuts string) string {
	// Always show capture mode
	base := fmt.Sprintf("input:%s", meta.Source.Mode)
	// For exec, show exit code
	if meta.Source.Mode == "exec" {
		base += fmt.Sprintf("  exit:%d", meta.ExitCode)
	}
	// Existing counters (match-lines/matches) + your navigation hints
	base += fmt.Sprintf("  match-lines:%d  matches:%d", counters.MatchLines, counters.MatchesTotal)
	if shortcuts != "" {
		base += "  |  " + shortcuts
	}
	return base
}
