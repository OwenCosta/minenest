package nest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Finding the servers people already have on this computer, and taking one as a zip.

type FoundServer struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Flavor   string `json:"flavor"`
	Version  string `json:"version"`
	HasWorld bool   `json:"has_world"`
	Modified int64  `json:"modified"`
	Imported bool   `json:"imported"` // MineNest already has a copy from here
}

// looksLikeServer: server.properties, or eula.txt beside a jar, or a world with a jar.
func looksLikeServer(dir string) bool {
	if exists(filepath.Join(dir, "server.properties")) {
		return true
	}
	jars, _ := filepath.Glob(filepath.Join(dir, "*.jar"))
	return len(jars) > 0 && (exists(filepath.Join(dir, "eula.txt")) || exists(filepath.Join(dir, "world", "level.dat")))
}

var skipDirs = map[string]bool{"node_modules": true, ".git": true, "appdata": true, "library": true, "$recycle.bin": true,
	"windows": true, "program files": true, "program files (x86)": true, ".cache": true, ".local": true, ".npm": true,
	".cargo": true, ".rustup": true, "go": true, ".gradle": true, ".m2": true, "steamapps": true, "proc": true, "sys": true,
	"libraries": true, "plugins": true, "mods": true, "world": true, "logs": true, "crash-reports": true, "versions": true}

// FindServers looks through the usual places (home, Desktop, Documents, Downloads, and on Linux
// /srv and /opt) a few folders deep, for at most a few seconds.
func (m *Manager) FindServers(ctx context.Context) []FoundServer {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	home, _ := os.UserHomeDir()
	roots := []string{home}
	for _, d := range []string{"Desktop", "Documents", "Downloads", "OneDrive", "OneDrive/Desktop", "OneDrive/Documents", "Games", "minecraft", "servers"} {
		roots = append(roots, filepath.Join(home, filepath.FromSlash(d)))
	}
	if runtime.GOOS == "windows" {
		for _, drive := range []string{"C:\\", "D:\\", "E:\\"} {
			for _, d := range []string{"Minecraft", "MinecraftServer", "Servers", "Games"} {
				roots = append(roots, filepath.Join(drive, d))
			}
		}
	} else {
		roots = append(roots, "/srv", "/opt", "/var/lib/minecraft", "/home/minecraft")
	}
	// HostMC's server, on XenithOS (MINENEST_HOSTMC points at its folder).
	if hm := os.Getenv("MINENEST_HOSTMC"); hm != "" {
		roots = append(roots, hm)
	}
	ours := Home()
	imported := map[string]bool{}
	for _, s := range m.List() {
		if c := s.Config(); c.ImportedFrom != "" {
			imported[strings.ToLower(c.ImportedFrom)] = true
		}
	}
	seen := map[string]bool{}
	var out []FoundServer
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if ctx.Err() != nil || len(out) >= 50 {
			return
		}
		key := strings.ToLower(filepath.Clean(dir))
		if seen[key] || within(ours, dir) || key == strings.ToLower(filepath.Clean(ours)) {
			return
		}
		seen[key] = true
		if looksLikeServer(dir) {
			fl, ver := detectServer(dir)
			st, _ := os.Stat(dir)
			f := FoundServer{Path: dir, Name: filepath.Base(dir), Flavor: fl, Version: ver, Imported: imported[key]}
			if filepath.Base(dir) == "server" {
				f.Name = filepath.Base(filepath.Dir(dir))
			}
			if st != nil {
				f.Modified = st.ModTime().Unix()
			}
			f.HasWorld = len(globAny(dir, "*/level.dat")) > 0
			out = append(out, f)
			return
		}
		if depth == 0 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			n := strings.ToLower(e.Name())
			if !e.IsDir() || skipDirs[n] || (strings.HasPrefix(n, ".") && n != ".minecraft") {
				continue
			}
			walk(filepath.Join(dir, e.Name()), depth-1)
		}
	}
	for i, r := range roots {
		depth := 4
		if i == 0 {
			depth = 2 // the home folder itself: only near the top
		}
		walk(r, depth)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified > out[j].Modified })
	if out == nil {
		out = []FoundServer{}
	}
	return out
}

// ImportZip takes a server packed as a zip, unpacks it, and brings it in like a folder.
func (m *Manager) ImportZip(r io.Reader, o ImportOptions) (*Server, *Job, error) {
	b := make([]byte, 6)
	rand.Read(b)
	dir := homePath("import", hex.EncodeToString(b))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	zipPath := dir + ".zip"
	f, err := os.Create(zipPath)
	if err != nil {
		return nil, nil, err
	}
	_, err = io.Copy(f, io.LimitReader(r, 20<<30))
	f.Close()
	defer os.Remove(zipPath)
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	if err := unzip(zipPath, dir, 0); err != nil {
		os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("that zip couldn't be opened: %v", err)
	}
	// The server may be at the top of the zip or in a folder inside it.
	src := dir
	if !looksLikeServer(src) {
		found := ""
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && found == "" && looksLikeServer(p) {
				found = p
				return filepath.SkipAll
			}
			return nil
		})
		if found == "" {
			os.RemoveAll(dir)
			return nil, nil, fmt.Errorf("there's no Minecraft server in that zip (no server.properties)")
		}
		src = found
	}
	if o.Name == "" {
		o.Name = strings.TrimSuffix(o.ZipName, filepath.Ext(o.ZipName))
		if o.Name == "" || src != dir {
			o.Name = filepath.Base(src)
		}
	}
	o.Path = src
	o.cleanup = dir
	s, j, err := m.Import(o)
	if err != nil {
		os.RemoveAll(dir)
	}
	return s, j, err
}
