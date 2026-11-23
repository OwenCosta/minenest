package nest

import (
	"os"
	"sort"
	"strings"
)

// server.properties, read and written in place: lines MineNest doesn't touch stay as they were.

type Properties struct {
	path  string
	lines []string
}

func loadProperties(path string) *Properties {
	p := &Properties{path: path}
	if b, err := os.ReadFile(path); err == nil {
		s := strings.ReplaceAll(string(b), "\r\n", "\n")
		p.lines = strings.Split(strings.TrimRight(s, "\n"), "\n")
		if len(p.lines) == 1 && p.lines[0] == "" {
			p.lines = nil
		}
	}
	return p
}

func propKey(line string) (string, string, bool) {
	t := strings.TrimSpace(line)
	if t == "" || t[0] == '#' || t[0] == '!' {
		return "", "", false
	}
	i := strings.IndexAny(t, "=:")
	if i < 0 {
		return t, "", true
	}
	return strings.TrimSpace(t[:i]), unescapeProp(strings.TrimSpace(t[i+1:])), true
}

func unescapeProp(v string) string {
	if !strings.Contains(v, "\\") {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
			switch v[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(v[i])
			}
			continue
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

func escapeProp(v string) string {
	r := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "=", "\\=", ":", "\\:")
	return r.Replace(v)
}

func (p *Properties) Get(key string) (string, bool) {
	for _, l := range p.lines {
		if k, v, ok := propKey(l); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func (p *Properties) GetDefault(key, def string) string {
	if v, ok := p.Get(key); ok {
		return v
	}
	return def
}

func (p *Properties) Set(key, value string) {
	line := key + "=" + escapeProp(value)
	for i, l := range p.lines {
		if k, _, ok := propKey(l); ok && k == key {
			p.lines[i] = line
			return
		}
	}
	p.lines = append(p.lines, line)
}

func (p *Properties) All() map[string]string {
	out := map[string]string{}
	for _, l := range p.lines {
		if k, v, ok := propKey(l); ok {
			out[k] = v
		}
	}
	return out
}

func (p *Properties) Keys() []string {
	m := p.All()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (p *Properties) Save() error {
	return writeFile(p.path, []byte(strings.Join(p.lines, "\n")+"\n"), 0o644)
}

// PropertyInfo describes the settings the GUI shows with friendly names. Anything else in
// server.properties is still there under "All settings".
type PropertyInfo struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Kind    string   `json:"kind"` // text, number, bool, choice
	Choices []string `json:"choices,omitempty"`
	Default string   `json:"default"`
	Help    string   `json:"help,omitempty"`
}

var CommonProperties = []PropertyInfo{
	{"motd", "Message in the server list", "text", nil, "A Minecraft Server", ""},
	{"gamemode", "Game mode", "choice", []string{"survival", "creative", "adventure", "spectator"}, "survival", ""},
	{"difficulty", "Difficulty", "choice", []string{"peaceful", "easy", "normal", "hard"}, "easy", ""},
	{"max-players", "Most players at once", "number", nil, "20", ""},
	{"pvp", "Players can hurt each other", "bool", nil, "true", ""},
	{"hardcore", "Hardcore", "bool", nil, "false", "One life: players who die become spectators."},
	{"white-list", "Only listed players can join", "bool", nil, "false", "Add players on the Players page."},
	{"online-mode", "Check accounts with Mojang", "bool", nil, "true", "Leave on unless you know why: off lets anyone use any name."},
	{"allow-flight", "Allow flying (needed by some mods)", "bool", nil, "false", ""},
	{"spawn-protection", "Spawn protection radius", "number", nil, "16", "Blocks around spawn only operators can change."},
	{"view-distance", "View distance (chunks)", "number", nil, "10", "Lower it if the server is slow."},
	{"simulation-distance", "Simulation distance (chunks)", "number", nil, "10", ""},
	{"level-seed", "World seed (new worlds)", "text", nil, "", "Used when a new world is made."},
	{"level-type", "World type (new worlds)", "choice", []string{"minecraft:normal", "minecraft:flat", "minecraft:large_biomes", "minecraft:amplified"}, "minecraft:normal", ""},
	{"generate-structures", "Villages and other structures", "bool", nil, "true", ""},
	{"enable-command-block", "Command blocks", "bool", nil, "false", ""},
	{"server-port", "Port", "number", nil, "25565", "What players join at. Change it if another server uses it."},
}

// propertiesFromText reads properties from text that isn't (yet) in a file: a hand-edited
// server.properties on its way to being saved.
func propertiesFromText(text string) *Properties {
	s := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	p := &Properties{}
	p.lines = strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(p.lines) == 1 && p.lines[0] == "" {
		p.lines = nil
	}
	return p
}

// Text is the file's content as Save would write it.
func (p *Properties) Text() string {
	if len(p.lines) == 0 {
		return ""
	}
	return strings.Join(p.lines, "\n") + "\n"
}
