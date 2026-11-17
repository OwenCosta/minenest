package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/OwenCosta/minenest/internal/nest"
)

// selfUpdate replaces this program (and minenest-gui.exe beside it on Windows) with the newest
// release on GitHub, checked against the release's SHA256SUMS.
func selfUpdate(checkOnly bool) error {
	hc := &http.Client{Timeout: 5 * time.Minute}
	get := func(u string) ([]byte, error) {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", "MineNest/"+nest.Version)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s answered %s", u, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	}
	b, err := get("https://api.github.com/repos/" + nest.RepoSlug + "/releases/latest")
	if err != nil {
		return fmt.Errorf("couldn't ask GitHub for the newest MineNest: %v", err)
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		return err
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	if latest == nest.Version {
		fmt.Printf("MineNest %s is the newest.\n", nest.Version)
		return nil
	}
	fmt.Printf("MineNest %s is out (this is %s).\n", latest, nest.Version)
	if checkOnly {
		return nil
	}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	sums, err := get(urls["SHA256SUMS"])
	if err != nil || urls["SHA256SUMS"] == "" {
		return fmt.Errorf("the release has no SHA256SUMS, so nothing was changed")
	}
	want := map[string]string{}
	for _, l := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			want[strings.TrimPrefix(f[1], "*")] = f[0]
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	targets := map[string]string{fmt.Sprintf("minenest-%s-%s%s", runtime.GOOS, runtime.GOARCH, ext): exe}
	if runtime.GOOS == "windows" {
		gui := filepath.Join(filepath.Dir(exe), "minenest-gui.exe")
		if fileExists(gui) {
			targets[fmt.Sprintf("minenest-gui-%s-%s.exe", runtime.GOOS, runtime.GOARCH)] = gui
		}
	}
	for asset, path := range targets {
		u, sum := urls[asset], want[asset]
		if u == "" || sum == "" {
			return fmt.Errorf("the release has nothing for this computer (%s)", asset)
		}
		fmt.Println("Downloading " + asset)
		data, err := get(u)
		if err != nil {
			return err
		}
		h := sha256.Sum256(data)
		if hex.EncodeToString(h[:]) != sum {
			return fmt.Errorf("%s didn't match its checksum, so nothing was changed", asset)
		}
		// A running program can be renamed but not overwritten (on Windows): move it aside.
		old := path + ".old"
		os.Remove(old)
		if err := os.Rename(path, old); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o755); err != nil {
			os.Rename(old, path)
			return err
		}
	}
	fmt.Printf("Updated to MineNest %s. Running servers keep going on the old one until MineNest restarts (minenest quit, then minenest gui).\n", latest)
	return nil
}
