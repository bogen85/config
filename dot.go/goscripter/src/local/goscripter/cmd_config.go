package goscripter

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

type configParams struct {
	// scopes (choose one for write ops; read ops default to effective if requested)
	scriptPath string
	local      bool
	global     bool
	system     bool
	etc        bool
	filePath   string

	// read controls
	effective bool
	origin    bool
	jsonOut   bool
	strict    bool

	// write controls
	appendVal bool
	removeVal string
	typ       string // string,int,bool,array,toml
	section   string
	create    bool
	backup    bool
}

func newConfigFlagSet(p *configParams) *flag.FlagSet {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)

	// Scopes
	fs.StringVar(&p.scriptPath, "script", "", "use <script.go>.toml for scope")
	fs.BoolVar(&p.local, "local", false, "use ./goscripter.toml in current directory")
	fs.BoolVar(&p.global, "global", false, "use ~/.config/goscripter/config.toml")
	fs.BoolVar(&p.system, "system", false, "use /usr/local/etc/goscripter.toml")
	fs.BoolVar(&p.etc, "etc", false, "use /etc/goscripter.toml")
	fs.StringVar(&p.filePath, "file", "", "use an explicit config file path")

	// Read flags
	fs.BoolVar(&p.effective, "effective", false, "read-only: show merged effective view (honors --script)")
	fs.BoolVar(&p.origin, "origin", false, "when reading, also print which file the value comes from")
	fs.BoolVar(&p.jsonOut, "json", false, "output JSON instead of text/TOML")
	p.strict = true
	fs.BoolVar(&p.strict, "strict", true, "strict parsing/validation on read")
	fs.BoolVar(&p.strict, "no-strict", true, "alias to disable strict parsing (use: --no-strict=false)")

	// Write flags (set/unset)
	fs.BoolVar(&p.appendVal, "append", false, "append to array (for array keys)")
	fs.StringVar(&p.removeVal, "remove", "", "remove a value from array (for array keys)")
	fs.StringVar(&p.typ, "type", "", "value type for set: string,int,bool,array,toml (default: infer)")
	fs.StringVar(&p.section, "section", "", "section helper for cmd.<name>.* keys")
	p.create = true
	fs.BoolVar(&p.create, "create", true, "create config file/dirs if missing")
	fs.BoolVar(&p.create, "no-create", true, "alias to disable create (use: --no-create=false)")
	p.backup = true
	fs.BoolVar(&p.backup, "backup", true, "write <path>.bak before modifying")
	fs.BoolVar(&p.backup, "no-backup", true, "alias to disable backup (use: --no-backup=false)")

	fs.Usage = func() { usageConfig(fs) }
	return fs
}

func CmdConfig(argv []string) int {
	var p configParams
	fs := newConfigFlagSet(&p)
	if help, err := parseWithHelp(fs, argv); help {
		return 0
	} else if err != nil {
		return 2
	}

	args := fs.Args()
	if len(args) < 1 {
		usageConfig(fs)
		return 2
	}
	action := strings.ToLower(args[0])
	args = args[1:]

	// Strict/lenient mode
	mode := loadStrict
	if !p.strict {
		mode = loadLenient
	}

	switch action {
	case "get":
		if len(args) != 1 {
			eprintf("config get: requires exactly 1 key")
			return 2
		}
		key := args[0]
		return configGet(p, mode, key)
	case "set":
		if len(args) < 2 {
			eprintf("config set: requires <key> <value...>")
			return 2
		}
		key := args[0]
		vals := args[1:]
		return configSet(p, mode, key, vals)
	case "unset":
		if len(args) != 1 {
			eprintf("config unset: requires exactly 1 key")
			return 2
		}
		key := args[0]
		return configUnset(p, mode, key)
	case "list":
		return configList(p, mode)
	case "sections":
		return configSections(p, mode)
	case "dump":
		return configDump(p, mode)
	default:
		eprintf("config: unknown action %q", action)
		usageConfig(fs)
		return 2
	}
}

// --------------------------- scope resolution ------------------------------

type cfgSource struct {
	Path string
	C    Config
	Err  error
}

func resolveScopePath(p configParams) (string, error) {
	// write operations need a single, concrete file path
	if p.filePath != "" {
		return p.filePath, nil
	}
	if p.scriptPath != "" {
		abs, err := filepath.Abs(p.scriptPath)
		if err != nil {
			return "", err
		}
		return abs + ".toml", nil
	}
	if p.local {
		cwd, _ := os.Getwd()
		return filepath.Join(cwd, "goscripter.toml"), nil
	}
	if p.global {
		return filepath.Join(homeDir(), ".config", "goscripter", "config.toml"), nil
	}
	if p.system {
		return "/usr/local/etc/goscripter.toml", nil
	}
	if p.etc {
		return "/etc/goscripter.toml", nil
	}
	return "", fmt.Errorf("no scope specified; use one of --file, --script, --local, --global, --system, or --etc")
}

func gatherReadSources(p configParams, mode loadMode) (ordered []cfgSource, scriptDir string) {
	// Order must mirror loadGlobalConfigs + optional script local
	cwd, _ := os.Getwd()

	paths := []string{
		"/etc/goscripter.toml",
		"/usr/local/etc/goscripter.toml",
		filepath.Join(cwd, "goscripter.toml"),
		filepath.Join(homeDir(), ".config", "goscripter", "config.toml"),
	}

	for _, path := range paths {
		if !fileExists(path) {
			continue
		}
		c, err := decodeConfigStrict(path)
		if err != nil && mode == loadLenient {
			ordered = append(ordered, cfgSource{Path: path, C: Config{}, Err: err})
			continue
		}
		if err != nil {
			ordered = append(ordered, cfgSource{Path: path, C: Config{}, Err: err})
			continue
		}
		ordered = append(ordered, cfgSource{Path: path, C: c})
	}

	if p.scriptPath != "" {
		abs, err := filepath.Abs(p.scriptPath)
		if err == nil {
			scriptDir = filepath.Dir(abs)
			lc, _, _ := loadLocalConfig(abs+".toml", mode)
			ordered = append(ordered, cfgSource{Path: abs + ".toml", C: lc})
		}
	}
	return
}

func mergeEffectiveFromSources(sources []cfgSource, scriptDir string) mergedConfig {
	var cfgs []Config
	for _, s := range sources {
		cfgs = append(cfgs, s.C)
	}
	return mergeConfig(cfgs, Config{}, scriptDir)
}

// ------------------------------ actions ------------------------------------

func configGet(p configParams, mode loadMode, key string) int {
	if p.effective {
		// Read-only merged view
		sources, scriptDir := gatherReadSources(p, mode)
		m := mergeEffectiveFromSources(sources, scriptDir)
		val, ok := getKeyFromMerged(m, key, p.section)
		if !ok {
			eprintf("config get: key not found: %s", key)
			return 1
		}
		if p.origin {
			// Walk sources in precedence order to find where it came from
			src := findOriginForKey(sources, key, p.section)
			printValue(val, p.jsonOut, src)
			return 0
		}
		printValue(val, p.jsonOut, "")
		return 0
	}

	// Single file read (scope)
	path, err := resolveScopePath(p)
	if err != nil {
		eprintf("config get: %v", err)
		return 2
	}
	c, _, _ := loadLocalConfig(path, mode)
	val, ok := getKeyFromConfig(c, key, p.section)
	if !ok {
		eprintf("config get: key not found in %s: %s", path, key)
		return 1
	}
	if p.origin {
		printValue(val, p.jsonOut, path)
		return 0
	}
	printValue(val, p.jsonOut, "")
	return 0
}

func configSet(p configParams, mode loadMode, key string, vals []string) int {
	path, err := resolveScopePath(p)
	if err != nil {
		eprintf("config set: %v", err)
		return 2
	}

	// load existing (lenient)
	cfg, _, _ := loadLocalConfig(path, loadLenient)

	// Prepare value based on type flags
	val, err := parseSetValue(p, vals)
	if err != nil {
		eprintf("config set: %v", err)
		return 2
	}

	if err := setKeyInConfig(&cfg, key, p.section, val, p.appendVal, p.removeVal); err != nil {
		eprintf("config set: %v", err)
		return 2
	}

	// validate in strict mode before write
	if verrs := validateConfig(cfg, path); len(verrs) > 0 {
		var b strings.Builder
		for _, e := range verrs {
			b.WriteString(e.Error())
			b.WriteByte('\n')
		}
		eprintf("config set: validation failed:\n%s", strings.TrimSpace(b.String()))
		return 2
	}

	// write back with optional backup
	if err := writeConfigFile(path, cfg, p.create, p.backup); err != nil {
		eprintf("config set: %v", err)
		return 2
	}
	return 0
}

func configUnset(p configParams, mode loadMode, key string) int {
	path, err := resolveScopePath(p)
	if err != nil {
		eprintf("config unset: %v", err)
		return 2
	}
	cfg, _, _ := loadLocalConfig(path, loadLenient)
	if err := unsetKeyInConfig(&cfg, key, p.section); err != nil {
		eprintf("config unset: %v", err)
		return 2
	}
	if verrs := validateConfig(cfg, path); len(verrs) > 0 {
		var b strings.Builder
		for _, e := range verrs {
			b.WriteString(e.Error())
			b.WriteByte('\n')
		}
		eprintf("config unset: validation failed:\n%s", strings.TrimSpace(b.String()))
		return 2
	}
	if err := writeConfigFile(path, cfg, p.create, p.backup); err != nil {
		eprintf("config unset: %v", err)
		return 2
	}
	return 0
}

func configList(p configParams, mode loadMode) int {
	if p.effective {
		sources, scriptDir := gatherReadSources(p, mode)
		m := mergeEffectiveFromSources(sources, scriptDir)
		flat := flattenMerged(m)
		printFlat(flat, p.jsonOut)
		return 0
	}
	path, err := resolveScopePath(p)
	if err != nil {
		eprintf("config list: %v", err)
		return 2
	}
	cfg, _, _ := loadLocalConfig(path, mode)
	flat := flattenConfig(cfg)
	printFlat(flat, p.jsonOut)
	return 0
}

func configSections(p configParams, mode loadMode) int {
	if p.effective {
		sources, scriptDir := gatherReadSources(p, mode)
		m := mergeEffectiveFromSources(sources, scriptDir)
		secs := sectionsMerged(m)
		printStrings(secs, p.jsonOut)
		return 0
	}
	path, err := resolveScopePath(p)
	if err != nil {
		eprintf("config sections: %v", err)
		return 2
	}
	cfg, _, _ := loadLocalConfig(path, mode)
	secs := sectionsConfig(cfg)
	printStrings(secs, p.jsonOut)
	return 0
}

func configDump(p configParams, mode loadMode) int {
	if p.effective {
		sources, scriptDir := gatherReadSources(p, mode)
		m := mergeEffectiveFromSources(sources, scriptDir)
		// Re-materialize an effective Config from merged to dump TOML
		cfg := reifyFromMerged(m)
		return mustEncodeTOML(os.Stdout, cfg)
	}
	path, err := resolveScopePath(p)
	if err != nil {
		eprintf("config dump: %v", err)
		return 2
	}
	cfg, _, _ := loadLocalConfig(path, mode)
	return mustEncodeTOML(os.Stdout, cfg)
}

// --------------------------- key helpers -----------------------------------

func splitKey(key string, section string) (top string, rest []string) {
	key = strings.TrimSpace(key)
	if section != "" {
		// helper: if top is "cmd", allow --section <name> to prefix rest
		if !strings.HasPrefix(key, "cmd.") && key == "always_yes" || key == "always_strip" || key == "__note" {
			key = "cmd." + section + "." + key
		}
	}
	parts := strings.Split(key, ".")
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

func getKeyFromMerged(m mergedConfig, key string, section string) (interface{}, bool) {
	top, rest := splitKey(key, section)
	switch top {
	case "cache":
		if len(rest) == 1 && rest[0] == "root" {
			return m.Global.Cache.Root, m.Global.Cache.Root != ""
		}
	case "env":
		if len(rest) == 1 && rest[0] == "GO111MODULE" {
			return m.Env.GO111MODULE, true
		}
		if len(rest) == 1 && rest[0] == "GOPATH" {
			return append([]string{}, m.Env.GOPATH...), true
		}
	case "env_append":
		// effective view already merged; nothing separate to show
		return nil, false
	case "build":
		if len(rest) == 1 && rest[0] == "flags" {
			return append([]string{}, m.Flags...), true
		}
	case "goscripter":
		if len(rest) == 1 && rest[0] == "nodeps" && m.Nodeps != nil {
			return *m.Nodeps, true
		}
	case "cmd":
		if len(rest) >= 1 {
			cmdName := strings.ToLower(rest[0])
			key2 := ""
			if len(rest) > 1 {
				key2 = strings.ToLower(rest[1])
			}
			switch key2 {
			case "always_yes":
				return m.CmdYes[cmdName], true
			case "always_strip":
				return m.CmdStrip[cmdName], true
			}
		}
	case "__note":
		// not tracked in mergedConfig; only on concrete Config
		return nil, false
	}
	return nil, false
}

func getKeyFromConfig(c Config, key string, section string) (interface{}, bool) {
	top, rest := splitKey(key, section)
	switch top {
	case "__note":
		return c.RootNote, c.RootNote != ""
	case "cache":
		if len(rest) == 1 && rest[0] == "root" {
			return c.Cache.Root, c.Cache.Root != ""
		}
		if len(rest) == 1 && rest[0] == "__note" {
			return c.Cache.Note, c.Cache.Note != ""
		}
	case "env":
		if len(rest) == 1 && rest[0] == "GO111MODULE" {
			return c.Env.GO111MODULE, c.Env.GO111MODULE != ""
		}
		if len(rest) == 1 && rest[0] == "GOPATH" {
			return asStringSlice(c.Env.GOPATH), c.Env.GOPATH != nil
		}
		if len(rest) == 1 && rest[0] == "__note" {
			return c.Env.Note, c.Env.Note != ""
		}
	case "env_append":
		if len(rest) == 1 && rest[0] == "GOPATH" {
			return asStringSlice(c.EnvAppend.GOPATH), c.EnvAppend.GOPATH != nil
		}
		if len(rest) == 1 && rest[0] == "__note" {
			return c.EnvAppend.Note, c.EnvAppend.Note != ""
		}
	case "build":
		if len(rest) == 1 && rest[0] == "flags" {
			return append([]string{}, c.Build.Flags...), len(c.Build.Flags) > 0
		}
		if len(rest) == 1 && rest[0] == "__note" {
			return c.Build.Note, c.Build.Note != ""
		}
	case "goscripter":
		if len(rest) == 1 && rest[0] == "nodeps" {
			if c.Goscripter.Nodeps == nil {
				return false, false
			}
			return *c.Goscripter.Nodeps, true
		}
		if len(rest) == 1 && rest[0] == "__note" {
			return c.Goscripter.Note, c.Goscripter.Note != ""
		}
	case "cmd":
		if len(rest) >= 1 {
			name := strings.ToLower(rest[0])
			cp, ok := c.Cmd[name]
			if !ok {
				return nil, false
			}
			if len(rest) == 1 {
				// whole section?
				return cp, true
			}
			switch strings.ToLower(rest[1]) {
			case "always_yes":
				if cp.AlwaysYes == nil {
					return false, false
				}
				return *cp.AlwaysYes, true
			case "always_strip":
				if cp.AlwaysStrip == nil {
					return false, false
				}
				return *cp.AlwaysStrip, true
			case "__note":
				return cp.Note, cp.Note != ""
			}
		}
	}
	return nil, false
}

func setKeyInConfig(c *Config, key string, section string, val interface{}, appendArr bool, removeVal string) error {
	top, rest := splitKey(key, section)
	switch top {
	case "__note":
		if s, ok := val.(string); ok {
			c.RootNote = s
			return nil
		}
		return fmt.Errorf("__note must be a string")
	case "cache":
		if len(rest) == 1 && rest[0] == "root" {
			if s, ok := val.(string); ok {
				c.Cache.Root = s
				return nil
			}
			return fmt.Errorf("cache.root must be a string")
		}
		if len(rest) == 1 && rest[0] == "__note" {
			if s, ok := val.(string); ok {
				c.Cache.Note = s
				return nil
			}
			return fmt.Errorf("cache.__note must be a string")
		}
	case "env":
		if len(rest) == 1 && rest[0] == "GO111MODULE" {
			if s, ok := val.(string); ok {
				c.Env.GO111MODULE = s
				return nil
			}
			return fmt.Errorf("env.GO111MODULE must be a string")
		}
		if len(rest) == 1 && rest[0] == "GOPATH" {
			switch v := val.(type) {
			case string:
				c.Env.GOPATH = v
			case []string:
				c.Env.GOPATH = v
			default:
				return fmt.Errorf("env.GOPATH must be string or []string")
			}
			return nil
		}
		if len(rest) == 1 && rest[0] == "__note" {
			if s, ok := val.(string); ok {
				c.Env.Note = s
				return nil
			}
			return fmt.Errorf("env.__note must be a string")
		}
	case "env_append":
		if len(rest) == 1 && rest[0] == "GOPATH" {
			switch v := val.(type) {
			case string:
				c.EnvAppend.GOPATH = v
			case []string:
				// append/remove support
				existing := asStringSlice(c.EnvAppend.GOPATH)
				if appendArr {
					c.EnvAppend.GOPATH = append(existing, v...)
				} else if removeVal != "" {
					c.EnvAppend.GOPATH = removeFromSlice(existing, removeVal)
				} else {
					c.EnvAppend.GOPATH = v
				}
			default:
				return fmt.Errorf("env_append.GOPATH must be string or []string")
			}
			return nil
		}
		if len(rest) == 1 && rest[0] == "__note" {
			if s, ok := val.(string); ok {
				c.EnvAppend.Note = s
				return nil
			}
			return fmt.Errorf("env_append.__note must be a string")
		}
	case "build":
		if len(rest) == 1 && rest[0] == "flags" {
			switch v := val.(type) {
			case []string:
				existing := append([]string{}, c.Build.Flags...)
				if appendArr {
					c.Build.Flags = append(existing, v...)
				} else if removeVal != "" {
					c.Build.Flags = removeFromSlice(existing, removeVal)
				} else {
					c.Build.Flags = v
				}
				return nil
			case string:
				// single string
				if strings.TrimSpace(v) == "" {
					c.Build.Flags = nil
					return nil
				}
				c.Build.Flags = []string{v}
				return nil
			default:
				return fmt.Errorf("build.flags must be string or []string")
			}
		}
		if len(rest) == 1 && rest[0] == "__note" {
			if s, ok := val.(string); ok {
				c.Build.Note = s
				return nil
			}
			return fmt.Errorf("build.__note must be a string")
		}
	case "goscripter":
		if len(rest) == 1 && rest[0] == "nodeps" {
			switch v := val.(type) {
			case bool:
				b := v
				c.Goscripter.Nodeps = &b
				return nil
			case string:
				b := parseBoolString(v)
				c.Goscripter.Nodeps = &b
				return nil
			default:
				return fmt.Errorf("goscripter.nodeps must be bool")
			}
		}
		if len(rest) == 1 && rest[0] == "__note" {
			if s, ok := val.(string); ok {
				c.Goscripter.Note = s
				return nil
			}
			return fmt.Errorf("goscripter.__note must be a string")
		}
	case "cmd":
		if len(rest) >= 1 {
			name := strings.ToLower(rest[0])
			cp := c.Cmd[name]
			if cp == (CmdPrefs{}) {
				cp = CmdPrefs{}
				if c.Cmd == nil {
					c.Cmd = map[string]CmdPrefs{}
				}
			}
			if len(rest) == 1 {
				// set whole section via toml? Accept __note string too.
				switch v := val.(type) {
				case string:
					// interpret as __note
					cp.Note = v
				default:
					// ignore; to set both booleans, use explicit keys
				}
				c.Cmd[name] = cp
				return nil
			}
			switch strings.ToLower(rest[1]) {
			case "always_yes":
				switch v := val.(type) {
				case bool:
					b := v
					cp.AlwaysYes = &b
				case string:
					b := parseBoolString(v)
					cp.AlwaysYes = &b
				default:
					return fmt.Errorf("cmd.%s.always_yes must be bool", name)
				}
				c.Cmd[name] = cp
				return nil
			case "always_strip":
				switch v := val.(type) {
				case bool:
					b := v
					cp.AlwaysStrip = &b
				case string:
					b := parseBoolString(v)
					cp.AlwaysStrip = &b
				default:
					return fmt.Errorf("cmd.%s.always_strip must be bool", name)
				}
				c.Cmd[name] = cp
				return nil
			case "__note":
				if s, ok := val.(string); ok {
					cp.Note = s
					c.Cmd[name] = cp
					return nil
				}
				return fmt.Errorf("cmd.%s.__note must be string", name)
			}
		}
	}
	return fmt.Errorf("unknown or unsupported key path: %s", key)
}

func unsetKeyInConfig(c *Config, key string, section string) error {
	top, rest := splitKey(key, section)
	switch top {
	case "__note":
		c.RootNote = ""
		return nil
	case "cache":
		if len(rest) == 1 && rest[0] == "root" {
			c.Cache.Root = ""
			return nil
		}
		if len(rest) == 1 && rest[0] == "__note" {
			c.Cache.Note = ""
			return nil
		}
	case "env":
		if len(rest) == 1 && rest[0] == "GO111MODULE" {
			c.Env.GO111MODULE = ""
			return nil
		}
		if len(rest) == 1 && rest[0] == "GOPATH" {
			c.Env.GOPATH = nil
			return nil
		}
		if len(rest) == 1 && rest[0] == "__note" {
			c.Env.Note = ""
			return nil
		}
	case "env_append":
		if len(rest) == 1 && rest[0] == "GOPATH" {
			c.EnvAppend.GOPATH = nil
			return nil
		}
		if len(rest) == 1 && rest[0] == "__note" {
			c.EnvAppend.Note = ""
			return nil
		}
	case "build":
		if len(rest) == 1 && rest[0] == "flags" {
			c.Build.Flags = nil
			return nil
		}
		if len(rest) == 1 && rest[0] == "__note" {
			c.Build.Note = ""
			return nil
		}
	case "goscripter":
		if len(rest) == 1 && rest[0] == "nodeps" {
			c.Goscripter.Nodeps = nil
			return nil
		}
		if len(rest) == 1 && rest[0] == "__note" {
			c.Goscripter.Note = ""
			return nil
		}
	case "cmd":
		if len(rest) >= 1 {
			name := strings.ToLower(rest[0])
			cp, ok := c.Cmd[name]
			if !ok {
				return nil
			}
			if len(rest) == 1 {
				delete(c.Cmd, name)
				return nil
			}
			switch strings.ToLower(rest[1]) {
			case "always_yes":
				cp.AlwaysYes = nil
			case "always_strip":
				cp.AlwaysStrip = nil
			case "__note":
				cp.Note = ""
			default:
				return fmt.Errorf("unknown cmd.%s subkey: %s", name, rest[1])
			}
			c.Cmd[name] = cp
			return nil
		}
	}
	return fmt.Errorf("unknown or unsupported key path: %s", key)
}

// --------------------------- print helpers ---------------------------------

func printValue(val interface{}, jsonOut bool, origin string) {
	if jsonOut {
		var payload struct {
			Value  interface{} `json:"value"`
			Origin string      `json:"origin,omitempty"`
		}
		payload.Value = val
		if origin != "" {
			payload.Origin = origin
		}
		b, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(b))
		return
	}
	if origin != "" {
		fmt.Printf("%v\t# from %s\n", val, origin)
	} else {
		switch v := val.(type) {
		case []string:
			for _, s := range v {
				fmt.Println(s)
			}
		default:
			fmt.Printf("%v\n", val)
		}
	}
}

func printStrings(ss []string, jsonOut bool) {
	if jsonOut {
		b, _ := json.MarshalIndent(ss, "", "  ")
		fmt.Println(string(b))
		return
	}
	for _, s := range ss {
		fmt.Println(s)
	}
}

func printFlat(m map[string]interface{}, jsonOut bool) {
	if jsonOut {
		b, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(b))
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s = %v\n", k, m[k])
	}
}

func mustEncodeTOML(w io.Writer, c Config) int {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		eprintf("encode: %v", err)
		return 2
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		eprintf("write: %v", err)
		return 2
	}
	return 0
}

// ---------------------- flatten / sections / reify -------------------------

func flattenMerged(m mergedConfig) map[string]interface{} {
	out := map[string]interface{}{}
	if m.Global.Cache.Root != "" {
		out["cache.root"] = m.Global.Cache.Root
	}
	out["env.GO111MODULE"] = m.Env.GO111MODULE
	out["env.GOPATH"] = append([]string{}, m.Env.GOPATH...)
	if len(m.Flags) > 0 {
		out["build.flags"] = append([]string{}, m.Flags...)
	}
	if m.Nodeps != nil {
		out["goscripter.nodeps"] = *m.Nodeps
	}
	for name, v := range m.CmdYes {
		out["cmd."+name+".always_yes"] = v
	}
	for name, v := range m.CmdStrip {
		out["cmd."+name+".always_strip"] = v
	}
	return out
}

func flattenConfig(c Config) map[string]interface{} {
	out := map[string]interface{}{}
	if c.RootNote != "" {
		out["__note"] = c.RootNote
	}
	if c.Cache.Root != "" {
		out["cache.root"] = c.Cache.Root
	}
	if c.Cache.Note != "" {
		out["cache.__note"] = c.Cache.Note
	}
	if c.Env.GO111MODULE != "" {
		out["env.GO111MODULE"] = c.Env.GO111MODULE
	}
	if c.Env.GOPATH != nil {
		out["env.GOPATH"] = asStringSlice(c.Env.GOPATH)
	}
	if c.Env.Note != "" {
		out["env.__note"] = c.Env.Note
	}
	if c.EnvAppend.GOPATH != nil {
		out["env_append.GOPATH"] = asStringSlice(c.EnvAppend.GOPATH)
	}
	if c.EnvAppend.Note != "" {
		out["env_append.__note"] = c.EnvAppend.Note
	}
	if len(c.Build.Flags) > 0 {
		out["build.flags"] = append([]string{}, c.Build.Flags...)
	}
	if c.Build.Note != "" {
		out["build.__note"] = c.Build.Note
	}
	if c.Goscripter.Nodeps != nil {
		out["goscripter.nodeps"] = *c.Goscripter.Nodeps
	}
	if c.Goscripter.Note != "" {
		out["goscripter.__note"] = c.Goscripter.Note
	}
	for name, cp := range c.Cmd {
		if cp.AlwaysYes != nil {
			out["cmd."+name+".always_yes"] = *cp.AlwaysYes
		}
		if cp.AlwaysStrip != nil {
			out["cmd."+name+".always_strip"] = *cp.AlwaysStrip
		}
		if cp.Note != "" {
			out["cmd."+name+".__note"] = cp.Note
		}
	}
	return out
}

func sectionsMerged(m mergedConfig) []string {
	s := map[string]bool{}
	s["cache"] = m.Global.Cache.Root != ""
	s["env"] = true
	if len(m.Flags) > 0 {
		s["build"] = true
	}
	if m.Nodeps != nil {
		s["goscripter"] = true
	}
	if len(m.CmdYes) > 0 || len(m.CmdStrip) > 0 {
		s["cmd"] = true
	}
	var out []string
	for k, v := range s {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func sectionsConfig(c Config) []string {
	var out []string
	if c.RootNote != "" {
		out = append(out, "__note")
	}
	if c.Cache.Root != "" || c.Cache.Note != "" {
		out = append(out, "cache")
	}
	if c.Env.GO111MODULE != "" || c.Env.GOPATH != nil || c.Env.Note != "" {
		out = append(out, "env")
	}
	if c.EnvAppend.GOPATH != nil || c.EnvAppend.Note != "" {
		out = append(out, "env_append")
	}
	if len(c.Build.Flags) > 0 || c.Build.Note != "" {
		out = append(out, "build")
	}
	if c.Goscripter.Nodeps != nil || c.Goscripter.Note != "" {
		out = append(out, "goscripter")
	}
	if len(c.Cmd) > 0 {
		out = append(out, "cmd")
	}
	sort.Strings(out)
	return out
}

func reifyFromMerged(m mergedConfig) Config {
	var c Config
	// cache root (from merged global)
	c.Cache.Root = m.Global.Cache.Root

	// env
	c.Env.GO111MODULE = m.Env.GO111MODULE
	if len(m.Env.GOPATH) == 1 {
		c.Env.GOPATH = m.Env.GOPATH[0]
	} else if len(m.Env.GOPATH) > 1 {
		c.Env.GOPATH = append([]string{}, m.Env.GOPATH...)
	}

	// build
	if len(m.Flags) > 0 {
		c.Build.Flags = append([]string{}, m.Flags...)
	}

	// goscripter
	if m.Nodeps != nil {
		b := *m.Nodeps
		c.Goscripter.Nodeps = &b
	}

	// cmd prefs
	if len(m.CmdYes) > 0 || len(m.CmdStrip) > 0 {
		c.Cmd = map[string]CmdPrefs{}
		for k, v := range m.CmdYes {
			cp := c.Cmd[k]
			cp.AlwaysYes = boolPtr(v)
			c.Cmd[k] = cp
		}
		for k, v := range m.CmdStrip {
			cp := c.Cmd[k]
			cp.AlwaysStrip = boolPtr(v)
			c.Cmd[k] = cp
		}
	}
	return c
}

// --------------------------- origin lookup ---------------------------------

func findOriginForKey(sources []cfgSource, key string, section string) string {
	// Precedence follows the apply order in mergeConfig: later entries override.
	// We iterate forward but keep updating origin when a non-empty value is found.
	origin := ""
	for _, s := range sources {
		if s.Err != nil {
			continue
		}
		if v, ok := getKeyFromConfig(s.C, key, section); ok {
			// empty slices/strings considered a hit too (user explicitly set empty)
			_ = v
			origin = s.Path
		}
	}
	return origin
}

// --------------------------- small utils -----------------------------------

func parseSetValue(p configParams, vals []string) (interface{}, error) {
	typ := strings.ToLower(p.typ)
	if typ == "" {
		// infer: multiple args => array, single parseable => bool/int
		if len(vals) > 1 {
			typ = "array"
		} else {
			if _, err := strconv.ParseInt(vals[0], 10, 64); err == nil {
				typ = "int"
			} else if isBoolString(vals[0]) {
				typ = "bool"
			} else {
				typ = "string"
			}
		}
	}

	switch typ {
	case "string":
		return strings.Join(vals, " "), nil
	case "int":
		i, err := strconv.ParseInt(vals[0], 10, 64)
		if err != nil {
			return nil, err
		}
		return int(i), nil
	case "bool":
		return parseBoolString(vals[0]), nil
	case "array":
		if len(vals) == 1 && strings.Contains(vals[0], ",") {
			parts := strings.Split(vals[0], ",")
			var out []string
			for _, p := range parts {
				s := strings.TrimSpace(p)
				if s != "" {
					out = append(out, s)
				}
			}
			return out, nil
		}
		return append([]string{}, vals...), nil
	case "toml":
		// Accept a TOML snippet for array or complex types
		var v interface{}
		_, err := toml.Decode(vals[0], &v)
		if err != nil {
			return nil, err
		}
		// Try to normalize common shapes:
		switch vv := v.(type) {
		case map[string]interface{}:
			return vv, nil
		case []interface{}:
			var out []string
			for _, it := range vv {
				out = append(out, fmt.Sprint(it))
			}
			return out, nil
		default:
			return v, nil
		}
	default:
		return nil, fmt.Errorf("unknown --type %q", p.typ)
	}
}

func writeConfigFile(path string, cfg Config, create bool, backup bool) error {
	if !fileExists(path) {
		if !create {
			return fmt.Errorf("config file %s does not exist (use --create)", path)
		}
		if err := ensureParent(path); err != nil {
			return err
		}
	}
	if backup && fileExists(path) {
		bak := path + ".bak"
		if err := copyFile(path, bak); err != nil {
			warnf("backup: %v", err)
		}
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func removeFromSlice(ss []string, val string) []string {
	out := ss[:0]
	for _, s := range ss {
		if s != val {
			out = append(out, s)
		}
	}
	return out
}

func parseBoolString(s string) bool {
	return isBoolString(s) && (strings.EqualFold(s, "1") || strings.EqualFold(s, "true") || strings.EqualFold(s, "yes") || strings.EqualFold(s, "on"))
}
func isBoolString(s string) bool {
	switch strings.ToLower(s) {
	case "0", "1", "true", "false", "yes", "no", "on", "off":
		return true
	default:
		return false
	}
}
func boolPtr(b bool) *bool { return &b }

// --------------------------- registration ----------------------------------

func init() {
	Register(&Command{
		Name:    "config",
		Summary: "Read/write goscripter config (global/local/script)",
		Help:    func() { usageConfig(newConfigFlagSet(&configParams{})) },
		Run:     CmdConfig,
	})
}
