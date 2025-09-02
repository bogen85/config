package goscripter

import "time"

type CmdPrefs struct {
	AlwaysYes   *bool  `toml:"always_yes,omitempty"`
	AlwaysStrip *bool  `toml:"always_strip,omitempty"`
	Note        string `toml:"__note,omitempty"`
}

type Config struct {
	RootNote string `toml:"__note,omitempty"`

	Cache struct {
		Root string `toml:"root"`
		Note string `toml:"__note,omitempty"`
	} `toml:"cache"`

	Env struct {
		GO111MODULE string      `toml:"GO111MODULE"`
		GOPATH      interface{} `toml:"GOPATH"`
		Note        string      `toml:"__note,omitempty"`
	} `toml:"env"`

	EnvAppend struct {
		GOPATH interface{} `toml:"GOPATH"`
		Note   string      `toml:"__note,omitempty"`
	} `toml:"env_append"`

	Build struct {
		Flags []string `toml:"flags"`
		Note  string   `toml:"__note,omitempty"`
	} `toml:"build"`

	Goscripter struct {
		Nodeps *bool  `toml:"nodeps"`
		Note   string `toml:"__note,omitempty"`
	} `toml:"goscripter"`

	Cmd map[string]CmdPrefs `toml:"cmd"`
}

type Manifest struct {
	SourceMTime    int64    `toml:"source_mtime"`
	Flags          []string `toml:"flags"`
	EnvGO111MODULE string   `toml:"env_go111module"`
	EnvGOPATH      []string `toml:"env_gopath"`
}

type DepsSnapshot struct {
	Meta struct {
		GeneratedAt     string   `toml:"generated_at"`
		GoVersion       string   `toml:"goversion"`
		GOOS            string   `toml:"goos"`
		GOARCH          string   `toml:"goarch"`
		GOROOT          string   `toml:"goroot"`
		GO111MODULE     string   `toml:"go111module"`
		GOPATH          []string `toml:"gopath"`
		Flags           []string `toml:"flags"`
		GoscripterPath  string   `toml:"goscripter_path"`
		GoscripterMTime int64    `toml:"goscripter_mtime"`
		SnapshotFormat  int      `toml:"snapshot_format"`
	} `toml:"meta"`
	Deps []DepEntry   `toml:"dep"`
	Fb   *FallbackRec `toml:"fallback_scan,omitempty"`
}

type DepEntry struct {
	ImportPath string `toml:"import_path"`
	Dir        string `toml:"dir"`
	MaxMTime   int64  `toml:"max_mtime"`
	FileCount  int    `toml:"file_count"`
}

type FallbackRec struct {
	Root      string `toml:"root"`
	MaxMTime  int64  `toml:"max_mtime"`
	FileCount int    `toml:"file_count"`
}

type mergedEnv struct {
	GO111MODULE string
	GOPATH      []string
}
type mergedConfig struct {
	Env      mergedEnv
	Flags    []string
	Global   Config
	Nodeps   *bool
	CmdYes   map[string]bool
	CmdStrip map[string]bool
}

type cfgErr struct{ msg string }

func (e cfgErr) Error() string { return e.msg }

type loadMode int

const (
	loadStrict loadMode = iota
	loadLenient
)

type cfgLoad struct {
	Configs []Config
	Warns   []string
	Errs    []error
}

type cacheDecision struct {
	rebuild   bool
	reasons   []string
	man       Manifest
	binOK     bool
	binMTime  time.Time
	cacheDir  string
	buildCmd  string
	buildEnvM string
	buildEnvP string
}

type listPkg struct {
	ImportPath string   `json:"ImportPath"`
	Dir        string   `json:"Dir"`
	Standard   bool     `json:"Standard"`
	GoFiles    []string `json:"GoFiles"`
	CgoFiles   []string `json:"CgoFiles"`
	CFiles     []string `json:"CFiles"`
	HFiles     []string `json:"HFiles"`
	SFiles     []string `json:"SFiles"`
	SysoFiles  []string `json:"SysoFiles"`
	OtherFiles []string `json:"OtherFiles"`
}
