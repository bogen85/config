package capture

type Source struct {
	Mode string `json:"mode"` // pipe|file|exec
	Arg  string `json:"arg"`
}

type Meta struct {
	Version        int      `json:"version"`
	Source         Source   `json:"source"`
	CapturePath    string   `json:"capture_path"`
	Filtered       bool     `json:"filtered"`
	LineFormat     string   `json:"line_format"`
	LinesTotal     int      `json:"lines_total"`
	MatchLines     int      `json:"match_lines"`
	MatchesTotal   int      `json:"matches_total"`
	Rules          []string `json:"rules"`
	CreatedUnixSec int64    `json:"created_unix"`
	Temp           bool     `json:"temp"`
	OwnerPID       int      `json:"owner_pid"`
	ExitCode       int      `json:"exit_code,omitempty"` // only set for exec
}
