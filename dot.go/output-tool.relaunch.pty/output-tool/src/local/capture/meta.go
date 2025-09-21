package capture

type Meta struct {
	Version int `json:"version"`
	Source  struct {
		Mode string `json:"mode"` // pipe|file
		Arg  string `json:"arg"`
	} `json:"source"`

	CapturePath    string   `json:"capture_path"`
	Filtered       bool     `json:"filtered"`
	LineFormat     string   `json:"line_format"`
	LinesTotal     int      `json:"lines_total"`
	MatchLines     int      `json:"match_lines"`
	MatchesTotal   int      `json:"matches_total"`
	Rules          []string `json:"rules"`
	CreatedUnixSec int64    `json:"created_unix"`

	Temp     bool `json:"temp"`
	OwnerPID int  `json:"owner_pid"`
}
