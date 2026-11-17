package nest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Updating MineNest itself: the newest release on GitHub, checked against its SHA256SUMS, put in
// place of this program (and minenest-gui.exe beside it on Windows), then MineNest starts again,
// with the servers that were running started again too. The window's Update button, and
// `minenest update`, do it.

type UpdateInfo struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Newer   bool   `json:"newer"`
	URL     string `json:"url"`
	Error   string `json:"error,omitempty"`
}

var (
	updMu    sync.Mutex
	updCache UpdateInfo
	updAt    time.Time
)

// newerVersion: a is a later release than b ("0.2.0" > "0.1.9"; anything beats "dev").
func newerVersion(a, b string) bool {
	clean := func(v string) string { return strings.SplitN(strings.TrimPrefix(v, "v"), "-", 2)[0] }
	ca, cb := clean(a), clean(b)
	if !isRelease(cb) {
		return isRelease(ca)
	}
	if ca == cb {
		return strings.Contains(b, "-") && !strings.Contains(a, "-") // 0.2.0 after 0.2.0-test
	}
	return versionLess(cb, ca)
}

type ghRelease struct {
	Tag    string `json:"tag_name"`
	URL    string `json:"html_url"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func latestRelease(ctx context.Context) (*ghRelease, error) {
	var rel ghRelease
	if err := getJSON(ctx, "https://api.github.com/repos/"+RepoSlug+"/releases/latest", 0, &rel); err != nil {
		return nil, fmt.Errorf("couldn't ask GitHub for the newest MineNest: %v", err)
	}
	return &rel, nil
}

// CheckUpdate says whether there's a newer MineNest (asked at most every few hours, unless now).
func CheckUpdate(ctx context.Context, now bool) UpdateInfo {
	updMu.Lock()
	if !now && time.Since(updAt) < 6*time.Hour && updCache.Current != "" {
		u := updCache
		updMu.Unlock()
		return u
	}
	updMu.Unlock()
	u := UpdateInfo{Current: Version}
	if rel, err := latestRelease(ctx); err != nil {
		u.Error = err.Error()
	} else {
		u.Latest, u.URL = strings.TrimPrefix(rel.Tag, "v"), rel.URL
		u.Newer = newerVersion(u.Latest, Version)
	}
	updMu.Lock()
	updCache, updAt = u, time.Now()
	updMu.Unlock()
	return u
}

// ApplyUpdate downloads the newest release for this computer and puts it in place. It returns the
// program to start again (the path this one had).
func ApplyUpdate(ctx context.Context, job *Job) (string, error) {
	rel, err := latestRelease(ctx)
	if err != nil {
		return "", err
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	if !newerVersion(latest, Version) {
		return "", fmt.Errorf("MineNest %s is the newest already", Version)
	}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	if urls["SHA256SUMS"] == "" {
		return "", fmt.Errorf("the release has no SHA256SUMS, so nothing was changed")
	}
	sums, err := getBytes(ctx, urls["SHA256SUMS"], 0)
	if err != nil {
		return "", err
	}
	want := map[string]string{}
	for _, l := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			want[strings.TrimPrefix(f[1], "*")] = f[0]
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	dir := filepath.Dir(exe)
	cliAsset := fmt.Sprintf("minenest-%s-%s%s", runtime.GOOS, runtime.GOARCH, ext)
	targets := map[string]string{}
	if runtime.GOOS == "windows" {
		// minenest.exe (the command line) and minenest-gui.exe (the window), side by side.
		gui := filepath.Join(dir, "minenest-gui.exe")
		cli := filepath.Join(dir, "minenest.exe")
		if strings.EqualFold(filepath.Base(exe), "minenest-gui.exe") {
			if exists(cli) {
				targets[cliAsset] = cli
			}
		} else {
			targets[cliAsset] = exe
		}
		if exists(gui) {
			targets[fmt.Sprintf("minenest-gui-%s-%s.exe", runtime.GOOS, runtime.GOARCH)] = gui
		}
	} else {
		targets[cliAsset] = exe
	}
	for _, path := range targets {
		if f, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".minenest-write-test"), os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
			return "", fmt.Errorf("MineNest can't change its own folder (%s): run the install command again to update", filepath.Dir(path))
		} else {
			f.Close()
			os.Remove(f.Name())
		}
	}
	// Everything downloaded and checked first; only then is anything replaced.
	staged := map[string]string{}
	for asset, path := range targets {
		u, sum := urls[asset], want[asset]
		if u == "" || sum == "" {
			return "", fmt.Errorf("the release has nothing for this computer (%s)", asset)
		}
		tmp := path + ".new"
		if err := download(ctx, u, tmp, Hash{"sha256", sum}, job, "MineNest "+latest); err != nil {
			return "", err
		}
		os.Chmod(tmp, 0o755)
		staged[tmp] = path
	}
	for tmp, path := range staged {
		old := path + ".old"
		os.Remove(old)
		// A running program can be renamed but not overwritten on Windows: it's moved aside.
		if err := os.Rename(path, old); err != nil && exists(path) {
			return "", err
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Rename(old, path)
			return "", err
		}
	}
	return exe, nil
}

// ------------------------------------------------------------------ restarting after an update

// UpdateAndRestart updates in a job; when it's done, MineNest stops (saving every server) and the
// new one starts, starting the servers that were running.
func (m *Manager) UpdateAndRestart() (*Job, error) {
	m.mu.Lock()
	if m.restartExe != "" {
		m.mu.Unlock()
		return nil, fmt.Errorf("MineNest is restarting already")
	}
	m.mu.Unlock()
	return m.Jobs.start("Updating MineNest", "", func(ctx context.Context, j *Job) (any, error) {
		exe, err := ApplyUpdate(ctx, j)
		if err != nil {
			return nil, err
		}
		running := []string{}
		for _, s := range m.List() {
			if s.State() != Stopped {
				running = append(running, s.ID)
			}
		}
		writeJSON(homePath("resume.json"), running, 0o644)
		m.mu.Lock()
		m.restartExe = exe
		m.mu.Unlock()
		j.Say("Restarting MineNest")
		go func() {
			time.Sleep(1500 * time.Millisecond) // the window hears that it's done first
			if m.api != nil {
				select {
				case <-m.api.quit:
				default:
					close(m.api.quit)
				}
			}
		}()
		return nil, nil
	}), nil
}

// RestartExe is the new program to start once this one has stopped, or "".
func (m *Manager) RestartExe() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restartExe
}

// resumeServers starts the servers that were running before an update restarted MineNest.
func (m *Manager) resumeServers() {
	var ids []string
	readJSON(homePath("resume.json"), &ids)
	os.Remove(homePath("resume.json"))
	for _, id := range ids {
		if s, err := m.Get(id); err == nil && s.State() == Stopped {
			if err := s.Start(); err != nil {
				s.Note("It didn't start again after the update: %v", err)
			} else {
				s.Note("Started again after MineNest's update to %s.", Version)
			}
		}
	}
}
