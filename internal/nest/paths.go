// Package nest is MineNest's engine: servers, downloads, mods, the ways in from the internet,
// and the daemon's API that the CLI, the GUI and XenithOS all drive.
package nest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Version is set at build time (-ldflags "-X github.com/OwenCosta/minenest/internal/nest.Version=1.2.3").
var Version = "dev"

const (
	AppName      = "MineNest"
	UserAgent    = "MineNest/" // + Version, see agent()
	DefaultPort  = 25565
	BedrockPort  = 19132
	APIPort      = 47575 // the daemon, on 127.0.0.1 only
	EulaURL      = "https://aka.ms/MinecraftEULA"
	DefaultCloud = "https://cloud.owencosta.diy"
	RepoSlug     = "OwenCosta/minenest"
)

func agent() string { return UserAgent + Version + " (+https://github.com/" + RepoSlug + ")" }

// Home is where MineNest keeps everything: MINENEST_HOME if set, otherwise
// %LOCALAPPDATA%\MineNest on Windows and $XDG_DATA_HOME/minenest (~/.local/share/minenest) on Linux.
func Home() string {
	if h := os.Getenv("MINENEST_HOME"); h != "" {
		abs, err := filepath.Abs(h)
		if err == nil {
			return abs
		}
		return h
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "MineNest")
		}
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "minenest")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "share", "minenest")
}

func homePath(parts ...string) string {
	return filepath.Join(append([]string{Home()}, parts...)...)
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// readJSON reads path into v; a missing file leaves v as it is and isn't an error.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// writeJSON writes v to path atomically (through path.tmp).
func writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'), mode)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// Windows can't rename over a file another process has open; try the slow way.
		os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			os.Remove(tmp)
			return err
		}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// safeName makes a server's folder name from what the user called it.
func safeName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_', r == '.':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	return s
}

// within reports whether path is inside dir (both cleaned), to refuse "../" from archives.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
