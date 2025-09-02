package goscripter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

func decodeConfigStrict(p string) (Config, error) {
	var cfg Config
	if !fileExists(p) {
		return cfg, os.ErrNotExist
	}
	if _, err := toml.DecodeFile(p, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", p, err)
	}
	return cfg, nil
}

func asStringSlice(x interface{}) []string {
	switch v := x.(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []string:
		return v
	case []interface{}:
		var out []string
		for _, it := range v {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func validateGO111(s string) bool { return s == "" || s == "auto" || s == "on" || s == "off" }

func validateGOPATHList(vals []string) (ok bool, badIdx int, badVal string) {
	for i, v := range vals {
		if v == "." || filepath.IsAbs(v) {
			continue
		}
		return false, i, v
	}
	return true, -1, ""
}

func validateConfig(c Config, path string) []error {
	var errs []error
	if !validateGO111(c.Env.GO111MODULE) {
		errs = append(errs, cfgErr{fmt.Sprintf("%s: [env].GO111MODULE must be one of {auto,on,off}; got %q", path, c.Env.GO111MODULE)})
	}
	if gp := asStringSlice(c.Env.GOPATH); gp != nil {
		if ok, idx, bad := validateGOPATHList(gp); !ok {
			errs = append(errs, cfgErr{fmt.Sprintf("%s: [env].GOPATH[%d] = %q is invalid; use absolute paths or \".\"", path, idx, bad)})
		}
	}
	if gp := asStringSlice(c.EnvAppend.GOPATH); gp != nil {
		if ok, idx, bad := validateGOPATHList(gp); !ok {
			errs = append(errs, cfgErr{fmt.Sprintf("%s: [env_append].GOPATH[%d] = %q is invalid; use absolute paths or \".\"", path, idx, bad)})
		}
	}
	return errs
}

func mergeConfig(globOrdered []Config, local Config, scriptDir string) mergedConfig {
	m := mergedConfig{
		Env: mergedEnv{
			GO111MODULE: defaultGOMODULE,
			GOPATH:      []string{defaultGOPATH},
		},
		CmdYes: map[string]bool{},
	}
	apply := func(c Config) {
		if c.Cache.Root != "" {
			m.Global.Cache.Root = c.Cache.Root
		}
		if c.Env.GO111MODULE != "" {
			m.Env.GO111MODULE = c.Env.GO111MODULE
		}
		if gp := asStringSlice(c.Env.GOPATH); gp != nil {
			m.Env.GOPATH = gp
		}
		if gp := asStringSlice(c.EnvAppend.GOPATH); gp != nil {
			m.Env.GOPATH = append(m.Env.GOPATH, gp...)
		}
		if len(c.Build.Flags) > 0 {
			m.Flags = append(m.Flags, c.Build.Flags...)
		}
		if c.Goscripter.Nodeps != nil {
			m.Nodeps = c.Goscripter.Nodeps
		}
		if c.Cmd != nil {
			for k, prefs := range c.Cmd {
				key := strings.ToLower(k)
				if prefs.AlwaysYes != nil {
					m.CmdYes[key] = *prefs.AlwaysYes
				}
			}
		}
	}
	for _, g := range globOrdered {
		apply(g)
	}
	apply(local)

	for i := range m.Env.GOPATH {
		if m.Env.GOPATH[i] == "." {
			m.Env.GOPATH[i] = scriptDir
		}
	}
	seen := map[string]bool{}
	out := m.Env.GOPATH[:0]
	for _, g := range m.Env.GOPATH {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = TrueDefault()
		out = append(out, g)
	}
	m.Env.GOPATH = out
	return m
}

func loadGlobalConfigs(cwd string, mode loadMode) cfgLoad {
	paths := []string{
		"/etc/goscripter.toml",
		"/usr/local/etc/goscripter.toml",
		filepath.Join(cwd, "goscripter.toml"),
		filepath.Join(homeDir(), ".config", "goscripter", "config.toml"),
	}
	var out cfgLoad
	for _, p := range paths {
		if p == "" || !fileExists(p) {
			continue
		}
		c, err := decodeConfigStrict(p)
		if err != nil {
			if mode == loadStrict {
				out.Errs = append(out.Errs, err)
			} else {
				out.Warns = append(out.Warns, "parse error: "+err.Error())
			}
			continue
		}
		if verrs := validateConfig(c, p); len(verrs) > 0 {
			if mode == loadStrict {
				out.Errs = append(out.Errs, verrs...)
			} else {
				for _, e := range verrs {
					out.Warns = append(out.Warns, e.Error())
				}
			}
			continue
		}
		out.Configs = append(out.Configs, c)
	}
	return out
}

func loadLocalConfig(path string, mode loadMode) (Config, []string, []error) {
	var warns []string
	var errs []error
	if !fileExists(path) {
		return Config{}, warns, errs
	}
	c, err := decodeConfigStrict(path)
	if err != nil {
		if mode == loadStrict {
			errs = append(errs, err)
		} else {
			warns = append(warns, "parse error: "+err.Error())
		}
		return Config{}, warns, errs
	}
	verrs := validateConfig(c, path)
	if len(verrs) > 0 {
		if mode == loadStrict {
			errs = append(errs, verrs...)
			return Config{}, warns, errs
		}
		for _, e := range verrs {
			warns = append(warns, e.Error())
		}
	}
	return c, warns, errs
}
