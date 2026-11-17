package nest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Bringing an existing server into MineNest: its folder is copied (world, plugins, mods, settings
// and all), and the same kind of server software is installed fresh beside them, so MineNest knows
// how to run it. The original folder is left as it was.

type ImportOptions struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Flavor  string `json:"flavor"`  // "" to work it out
	Version string `json:"version"` // "" to work it out
	RAM     int    `json:"ram_mb"`
	ZipName string `json:"-"`

	cleanup string // a folder to remove afterwards (an unpacked zip)
}

var mcInHistory = regexp.MustCompile(`\(MC: ([0-9.]+)\)`)

// detectServer works out what kind of server a folder holds, and its Minecraft version.
func detectServer(dir string) (flavor, version string) {
	// HostMC (XenithOS) says it plainly.
	if b, err := os.ReadFile(filepath.Join(dir, "installed.txt")); err == nil {
		kv := parseKV(string(b))
		return kv["type"], kv["version"]
	}
	// MineNest's own, if someone points it at one.
	var cfg Config
	if readJSON(filepath.Join(dir, "..", "nest.json"), &cfg) == nil && cfg.Installed != nil {
		return cfg.Installed.Flavor, cfg.Installed.Minecraft
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "libraries", "net", "neoforged", "neoforge", "*")); len(m) > 0 {
		v := filepath.Base(m[len(m)-1])
		return "neoforge", neoMinecraft(v)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "libraries", "net", "minecraftforge", "forge", "*")); len(m) > 0 {
		return "forge", strings.SplitN(filepath.Base(m[len(m)-1]), "-", 2)[0]
	}
	if exists(filepath.Join(dir, ".fabric")) || exists(filepath.Join(dir, "fabric-server-launch.jar")) || exists(filepath.Join(dir, "fabric-server-launcher.properties")) {
		flavor = "fabric"
	}
	// Paper and Purpur keep a note of their version.
	if b, err := os.ReadFile(filepath.Join(dir, "version_history.json")); err == nil {
		var h struct{ CurrentVersion string }
		json.Unmarshal(b, &h)
		if m := mcInHistory.FindStringSubmatch(h.CurrentVersion); m != nil {
			version = m[1]
		}
		if flavor == "" {
			flavor = "paper"
			if strings.Contains(strings.ToLower(h.CurrentVersion), "purpur") {
				flavor = "purpur"
			}
		}
	}
	if flavor == "" {
		jars, _ := filepath.Glob(filepath.Join(dir, "*.jar"))
		for _, j := range jars {
			n := strings.ToLower(filepath.Base(j))
			switch {
			case strings.HasPrefix(n, "purpur"):
				flavor = "purpur"
			case strings.HasPrefix(n, "paper"):
				flavor = "paper"
			case strings.Contains(n, "fabric"):
				flavor = "fabric"
			}
		}
		if flavor == "" && exists(filepath.Join(dir, "plugins")) {
			flavor = "paper"
		}
		if flavor == "" {
			flavor = "vanilla"
		}
	}
	if version == "" {
		// Minecraft's own server keeps its version in versions/<id>/.
		if m, _ := filepath.Glob(filepath.Join(dir, "versions", "*")); len(m) > 0 {
			for _, p := range m {
				if st, err := os.Stat(p); err == nil && st.IsDir() && isRelease(filepath.Base(p)) {
					version = filepath.Base(p)
				}
			}
		}
	}
	return flavor, version
}

// Import copies a server folder into MineNest and installs its software, in a job.
func (m *Manager) Import(o ImportOptions) (*Server, *Job, error) {
	src, err := filepath.Abs(o.Path)
	if err != nil {
		return nil, nil, err
	}
	if !exists(filepath.Join(src, "server.properties")) && len(globAny(src, "world*")) == 0 {
		return nil, nil, fmt.Errorf("%s doesn't look like a Minecraft server's folder (no server.properties or world)", src)
	}
	flavor, version := detectServer(src)
	if o.Flavor != "" {
		flavor = o.Flavor
	}
	if o.Version != "" {
		version = o.Version
	}
	if _, ok := flavorByID(flavor); !ok {
		return nil, nil, fmt.Errorf("MineNest doesn't know a server type called %q", flavor)
	}
	if version == "" {
		return nil, nil, fmt.Errorf("MineNest couldn't tell which Minecraft version it runs: say it with -v (like -v 1.21.4)")
	}
	if o.Name == "" {
		o.Name = filepath.Base(src)
	}
	s, _, err := m.newServer(o.Name, flavor, version, o.RAM)
	if err != nil {
		return nil, nil, err
	}
	s.setBusy("the import", false)
	job := m.Jobs.start("Bringing in "+o.Name, s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		if o.cleanup != "" {
			defer os.RemoveAll(o.cleanup)
		} else {
			s.update(func(c *Config) { c.ImportedFrom = src })
		}
		if err := copyTree(ctx, src, s.Root(), j); err != nil {
			return nil, err
		}
		// Old launch files from elsewhere would only confuse; MineNest runs it.
		for _, n := range []string{"installed.txt"} {
			os.Remove(filepath.Join(s.Root(), n))
		}
		// Another MineNest server may already use its port: give it a free one.
		port := s.Port()
		for _, o := range m.List() {
			if o != s && o.Port() == port {
				p := s.props()
				free := m.freePort()
				p.Set("server-port", fmt.Sprint(free))
				p.Save()
				s.Note("Its port %d is used by %s here, so it's on port %d now.", port, o.Config().Name, free)
				break
			}
		}
		inst, err := installFlavor(ctx, s.Root(), flavor, version, "", j)
		if err != nil {
			return nil, err
		}
		s.update(func(c *Config) { c.Installed = inst })
		if s.eulaAgreed() {
			s.update(func(c *Config) { c.Eula = "kept from " + src })
		}
		if o.cleanup != "" {
			s.Note("Brought in from %s as %s %s.", o.ZipName, flavorName(flavor), version)
		} else {
			s.Note("Brought in from %s as %s %s. The original folder is left as it was.", src, flavorName(flavor), version)
		}
		return nil, nil
	})
	return s, job, nil
}

func globAny(dir, pattern string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	return m
}

// newServer makes the folder and nest.json of a new server (Create and Import).
func (m *Manager) newServer(name, flavor, version string, ram int) (*Server, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "My Server"
	}
	if len(name) > 40 {
		name = name[:40]
	}
	id := safeName(name)
	if id == "" {
		id = "server"
	}
	m.mu.Lock()
	for _, s := range m.servers {
		if strings.EqualFold(s.cfg.Name, name) {
			m.mu.Unlock()
			return nil, "", fmt.Errorf("there's already a server called %s", name)
		}
	}
	base := id
	for n := 2; m.servers[id] != nil || exists(homePath("servers", id)); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	m.mu.Unlock()
	if ram <= 0 {
		ram = suggestedRAM(flavor, false)
	}
	s := &Server{ID: id, Dir: homePath("servers", id), m: m, state: Stopped}
	s.cfg = Config{Name: name, Flavor: flavor, Want: version, RAM: ram, AutoRestart: true, RealIPs: true, Created: time.Now()}
	if err := os.MkdirAll(s.Root(), 0o755); err != nil {
		return nil, "", err
	}
	if err := s.update(func(c *Config) {}); err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	m.servers[id] = s
	m.mu.Unlock()
	return s, id, nil
}

func copyTree(ctx context.Context, src, dst string, j *Job) error {
	var total, done int64
	filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, e := d.Info(); e == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 || d.Name() == "session.lock" {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		info, _ := d.Info()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm()|0o600)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, in)
		out.Close()
		done += n
		j.Progress("Copying the server's files", done, total)
		return err
	})
}
