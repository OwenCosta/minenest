// Package cli is MineNest's command line, which drives the daemon through its API (starting it
// first if it isn't running).
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/OwenCosta/minenest/internal/nest"
)

type client struct {
	base  string
	token string
	http  *http.Client
}

func readDaemon() (nest.DaemonInfo, bool) {
	var d nest.DaemonInfo
	b, err := os.ReadFile(filepath.Join(nest.Home(), "daemon.json"))
	if err != nil || json.Unmarshal(b, &d) != nil || d.Port == 0 {
		return d, false
	}
	return d, true
}

func newClient() (*client, error) {
	tok, err := nest.APIToken()
	if err != nil {
		return nil, fmt.Errorf("MineNest's key couldn't be read: %v", err)
	}
	port := nest.APIPort
	if d, ok := readDaemon(); ok {
		port = d.Port
	}
	return &client{base: fmt.Sprintf("http://127.0.0.1:%d", port), token: tok, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

func (c *client) alive() bool {
	var info nest.Info
	return c.do("GET", "/api/info", nil, &info) == nil
}

// connect finds the daemon, starting it in the background if it isn't running.
func connect() (*client, error) {
	c, err := newClient()
	if err != nil {
		return nil, err
	}
	if c.alive() {
		return c, nil
	}
	if err := startDaemon(); err != nil {
		return nil, err
	}
	for i := 0; i < 60; i++ {
		time.Sleep(250 * time.Millisecond)
		if c2, err := newClient(); err == nil && c2.alive() {
			return c2, nil
		}
	}
	return nil, fmt.Errorf("MineNest's background service didn't start. See %s", filepath.Join(nest.Home(), "daemon.log"))
}

// startDaemon runs "minenest daemon" on its own, outliving this command. On Windows the GUI
// build (minenest-gui.exe, no console window) runs it if it's beside this one.
func startDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if gui := filepath.Join(filepath.Dir(exe), "minenest-gui.exe"); fileExists(gui) && !strings.EqualFold(filepath.Base(exe), "minenest-gui.exe") {
			exe = gui
		}
	}
	os.MkdirAll(nest.Home(), 0o755)
	log, err := os.OpenFile(filepath.Join(nest.Home(), "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Dir = nest.Home()
	detachCmd(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("MineNest's background service didn't start: %v", err)
	}
	return cmd.Process.Release()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (c *client) do(method, path string, in, out any) error {
	var rd io.Reader
	if in != nil {
		if r, ok := in.(io.Reader); ok {
			rd = r
		} else {
			b, _ := json.Marshal(in)
			rd = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-MineNest", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return errDaemon
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("MineNest answered %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

var errDaemon = errors.New("MineNest's background service isn't running")

func (c *client) get(path string, out any) error      { return c.do("GET", path, nil, out) }
func (c *client) post(path string, in, out any) error { return c.do("POST", path, in, out) }

func sp(id string) string { return "/api/servers/" + url.PathEscape(id) }

// wait follows a job to its end, drawing its progress, and returns its error.
func (c *client) wait(jobID string) error {
	if jobID == "" {
		return nil
	}
	tty := isTerminal()
	lastStep := ""
	for {
		var j struct {
			Title, Step, Error string
			Done, Total        int64
			Finished           bool
		}
		if err := c.get("/api/jobs/"+jobID, &j); err != nil {
			return err
		}
		line := j.Step
		if j.Total > 0 {
			pct := float64(j.Done) / float64(j.Total)
			bar := strings.Repeat("#", int(pct*24)) + strings.Repeat("-", 24-int(pct*24))
			line = fmt.Sprintf("%s [%s] %3.0f%%", j.Step, bar, pct*100)
		}
		if tty {
			fmt.Printf("\r\033[K  %s", line)
		} else if j.Step != lastStep {
			fmt.Println("  " + j.Step)
		}
		lastStep = j.Step
		if j.Finished {
			if tty {
				fmt.Print("\r\033[K")
			}
			if j.Error != "" {
				return errors.New(j.Error)
			}
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
}

type jobResp struct {
	Job string `json:"job"`
	ID  string `json:"id"`
}

func isTerminal() bool {
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
