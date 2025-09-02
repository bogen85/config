package goscripter

import "time"

type CmdPrefs struct {
	AlwaysYes *bool `toml:"always_yes"`
}

type Config struct {
	Cache struct {
		Root string `toml:"root"`
	} `toml:"cache"`

	Env struct {
		GO111MODULE string      `toml:"GO111MODULE"`
		GOPATH      interface{} `toml:"GOPATH"` // string or []string
	} `toml:"env"`

	EnvAppend struct {
		GOPATH interface{} `toml:"GOPATH"` // string or []string
	} `toml:"env_append"`

	Build struct {
		Flags []string `toml:"flags"`
	} `toml:"build"`

	Goscripter struct {
		Nodeps *bool `toml:"nodeps"`
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
		GeneratedAt string   `toml:"generated_at"`
		GoVersion   string   `toml:"goversion"`
		GOOS        string   `toml:"goos"`
		GOARCH      string   `toml:"goarch"`
		GOROOT      string   `toml:"goroot"`
		GO111MODULE string   `toml:"go111module"`
		GOPATH      []string `toml:"gopath"`
		Flags       []string `toml:"flags"`
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
	Env    mergedEnv
	Flags  []string
	Global Config          // includes cache.root
	Nodeps *bool           // optional preference from config
	CmdYes map[string]bool // per-command assume-yes
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

// cache decision
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

// go list -deps: minimal struct we need
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
