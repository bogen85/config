package goscripter

import "sort"

type Command struct {
	Name    string
	Aliases []string
	Summary string
	Help    func()
	Run     func([]string) int
}

var (
	cmdReg  = map[string]*Command{}
	aliasTo = map[string]string{}
)

func Register(c *Command) {
	if c == nil || c.Name == "" {
		return
	}
	cmdReg[c.Name] = c
	for _, a := range c.Aliases {
		aliasTo[a] = c.Name
	}
}

func Resolve(name string) *Command {
	if c, ok := cmdReg[name]; ok {
		return c
	}
	if real, ok := aliasTo[name]; ok {
		return cmdReg[real]
	}
	return nil
}

func CommandList() []*Command {
	out := make([]*Command, 0, len(cmdReg))
	for _, c := range cmdReg {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
