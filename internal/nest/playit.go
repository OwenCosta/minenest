package nest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// playit.gg: a free address on the internet for a server, so friends elsewhere can join without
// anyone changing the router.
//
// The first time, playit.gg is asked for a claim: its link opens in a browser, and the user
// approves this computer with their playit.gg account. MineNest trades the approved claim for the
// agent's secret key, kept in playit/playit.toml (readable only by this user). Then playit's agent
// (downloaded from playit's GitHub releases, checked against GitHub's SHA-256) runs while any
// server using playit.gg runs, and MineNest asks playit.gg for a Minecraft tunnel to each one.

const (
	playitAPI      = "https://api.playit.gg"
	playitReleases = "https://api.github.com/repos/playit-cloud/playit-agent/releases/latest"
	playitVersion  = "2.0.2"
)

type playitAgent struct {
	m  *Manager
	mu sync.Mutex

	code      string
	claimSeen bool
	state     string // off, claim, starting, on, problem
	note      string
	agentID   string
	tunnels   []playitTunnel
	cmd       *exec.Cmd
	users     map[*Server]bool
	loopStop  chan struct{}
	creating  map[string]time.Time
}

type playitTunnel struct {
	Type    string
	Address string
	Port    int
	Off     string
}

func newPlayit(m *Manager) *playitAgent {
	return &playitAgent{m: m, state: "off", users: map[*Server]bool{}, creating: map[string]time.Time{}}
}

func (p *playitAgent) dir() string        { return homePath("playit") }
func (p *playitAgent) secretPath() string { return filepath.Join(p.dir(), "playit.toml") }

func (p *playitAgent) secret() string {
	b, err := os.ReadFile(p.secretPath())
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if i := strings.Index(s, "\""); strings.HasPrefix(s, "secret_key") && i >= 0 {
		s = strings.Trim(s[i:], "\"\n ")
	}
	return s
}

type playitResp struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

func (p *playitAgent) post(ctx context.Context, path string, body any, auth bool) (*playitResp, error) {
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", playitAPI+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", agent())
	if auth {
		req.Header.Set("Authorization", "Agent-Key "+p.secret())
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, friendlyNetErr(playitAPI, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r playitResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("playit.gg answered %s", resp.Status)
	}
	return &r, nil
}

// use marks a server as wanting playit.gg; the loop runs while any server does.
func (p *playitAgent) use(s *Server, on bool) {
	p.mu.Lock()
	if on {
		p.users[s] = true
	} else {
		delete(p.users, s)
	}
	n := len(p.users)
	if n > 0 && p.loopStop == nil {
		p.loopStop = make(chan struct{})
		go p.loop(p.loopStop)
	}
	if n == 0 && p.loopStop != nil {
		close(p.loopStop)
		p.loopStop = nil
	}
	p.mu.Unlock()
	if n == 0 {
		p.stopAgent()
	}
}

func (p *playitAgent) stop() {
	p.mu.Lock()
	if p.loopStop != nil {
		close(p.loopStop)
		p.loopStop = nil
	}
	p.users = map[*Server]bool{}
	p.mu.Unlock()
	p.stopAgent()
}

func (p *playitAgent) set(state, note string) {
	p.mu.Lock()
	p.state, p.note = state, note
	p.mu.Unlock()
}

func (p *playitAgent) loop(stop chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-stop; cancel() }()
	for {
		wait := p.step(ctx)
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
	}
}

// step does the next thing: the claim, the agent, the tunnels. It says how long to wait after.
func (p *playitAgent) step(ctx context.Context) time.Duration {
	if p.secret() == "" {
		return p.claimStep(ctx)
	}
	if err := p.ensureAgent(ctx); err != nil {
		p.set("problem", err.Error())
		return 30 * time.Second
	}
	r, err := p.post(ctx, "/v1/agents/rundata", map[string]any{}, true)
	if err != nil {
		p.set("problem", err.Error()+" MineNest tries again soon.")
		return 15 * time.Second
	}
	if r.Status != "success" {
		var e struct{ Type string }
		json.Unmarshal(r.Data, &e)
		if e.Type == "auth" {
			p.set("problem", "playit.gg doesn't accept this computer's agent any more. Choose Forget playit.gg, then use it again.")
			p.stopAgent()
			return time.Minute
		}
		p.set("problem", "playit.gg had a problem ("+e.Type+"). MineNest tries again soon.")
		return 30 * time.Second
	}
	var data map[string]any
	json.Unmarshal(r.Data, &data)
	tunnels := []playitTunnel{}
	if list, ok := data["tunnels"].([]any); ok {
		for _, t := range list {
			tm, _ := t.(map[string]any)
			pt := playitTunnel{Type: str(tm["tunnel_type"]), Address: str(tm["display_address"]), Off: str(tm["disabled_reason"])}
			pt.Port = findLocalPort(tm)
			tunnels = append(tunnels, pt)
		}
	}
	p.mu.Lock()
	p.agentID = str(data["agent_id"])
	p.tunnels = tunnels
	users := make([]*Server, 0, len(p.users))
	for s := range p.users {
		users = append(users, s)
	}
	p.state, p.note = "on", ""
	p.mu.Unlock()
	// A Minecraft tunnel for every server that hasn't one (and a Bedrock one when it has Geyser).
	pending := false
	for _, s := range users {
		if _, ok := p.tunnelFor(s.Port(), false); !ok {
			pending = true
			p.createTunnel(ctx, s, "minecraft-java", s.Port())
		}
		if s.Config().Bedrock && geyserInstalled(s) {
			if _, ok := p.tunnelFor(BedrockPort, true); !ok {
				pending = true
				p.createTunnel(ctx, s, "minecraft-bedrock", BedrockPort)
			}
		}
	}
	if pending {
		return 5 * time.Second
	}
	return time.Minute
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// findLocalPort looks for the tunnel's local port anywhere inside it (playit.gg's answer has
// moved it around between versions).
func findLocalPort(v any) int {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "local_port" {
				switch n := val.(type) {
				case float64:
					return int(n)
				case string:
					var p int
					fmt.Sscan(n, &p)
					return p
				}
			}
			if k == "name" && val == "local_port" {
				var p int
				fmt.Sscan(fmt.Sprint(x["value"]), &p)
				if p > 0 {
					return p
				}
			}
			if p := findLocalPort(val); p > 0 {
				return p
			}
		}
	case []any:
		for _, e := range x {
			if p := findLocalPort(e); p > 0 {
				return p
			}
		}
	}
	return 0
}

func (p *playitAgent) tunnelFor(port int, bedrock bool) (playitTunnel, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var fallback *playitTunnel
	for i, t := range p.tunnels {
		isBedrock := strings.Contains(t.Type, "bedrock")
		if isBedrock != bedrock {
			continue
		}
		if t.Port == port {
			return t, true
		}
		if t.Port == 0 && fallback == nil {
			fallback = &p.tunnels[i]
		}
	}
	if fallback != nil && len(p.users) <= 1 {
		return *fallback, true
	}
	return playitTunnel{}, false
}

func (p *playitAgent) createTunnel(ctx context.Context, s *Server, kind string, port int) {
	key := fmt.Sprintf("%s:%d", kind, port)
	p.mu.Lock()
	if t, ok := p.creating[key]; ok && time.Since(t) < 2*time.Minute {
		p.mu.Unlock()
		return
	}
	p.creating[key] = time.Now()
	agentID := p.agentID
	p.mu.Unlock()
	var agent any
	if agentID != "" {
		agent = agentID
	}
	body := map[string]any{
		"ports": map[string]any{"type": "tunnel-type", "details": kind},
		"origin": map[string]any{"type": "agent", "data": map[string]any{"agent_id": agent, "config": map[string]any{"fields": []any{
			map[string]any{"name": "local_ip", "value": "127.0.0.1"},
			map[string]any{"name": "local_port", "value": fmt.Sprint(port)},
		}}}},
		"enabled": true, "alloc": nil, "name": "MineNest " + s.Config().Name, "firewall_id": nil,
	}
	r, err := p.post(ctx, "/v1/tunnels/create", body, true)
	if err != nil {
		s.Note("playit.gg couldn't make a tunnel: %v", err)
		return
	}
	if r.Status == "success" {
		s.Note("playit.gg made a tunnel for the server. Its address comes in a moment.")
		return
	}
	var why string
	json.Unmarshal(r.Data, &why)
	if why == "" {
		why = string(r.Data)
	}
	s.Note("playit.gg didn't make a tunnel (%s). You can add one for port %d on playit.gg yourself.", why, port)
}

func (p *playitAgent) claimStep(ctx context.Context) time.Duration {
	p.mu.Lock()
	if p.code == "" {
		b := make([]byte, 5)
		rand.Read(b)
		p.code = hex.EncodeToString(b)
		p.claimSeen = false
	}
	code := p.code
	if p.state != "claim" {
		p.state, p.note = "claim", "Open the link and approve this computer with your playit.gg account."
	}
	p.mu.Unlock()
	r, err := p.post(ctx, "/claim/setup", map[string]any{"code": code, "agent_type": "assignable", "version": "playit " + playitVersion}, false)
	if err != nil {
		p.set("claim", err.Error())
		return 10 * time.Second
	}
	var s string
	json.Unmarshal(r.Data, &s)
	switch {
	case r.Status != "success":
		p.mu.Lock()
		p.code = ""
		p.note = "That link expired, so here's a new one."
		p.mu.Unlock()
	case s == "WaitingForUserVisit":
		p.mu.Lock()
		p.claimSeen, p.note = false, "Open the link and approve this computer with your playit.gg account."
		p.mu.Unlock()
	case s == "WaitingForUser":
		p.mu.Lock()
		p.claimSeen, p.note = true, "Now approve it on playit.gg."
		p.mu.Unlock()
	case s == "UserRejected":
		p.mu.Lock()
		p.code = ""
		p.state, p.note = "problem", "It was turned down on playit.gg. Here's a new link to try again."
		p.mu.Unlock()
	case s == "UserAccepted":
		ex, err := p.post(ctx, "/claim/exchange", map[string]any{"code": code}, false)
		if err == nil && ex.Status == "success" {
			var d struct {
				SecretKey string `json:"secret_key"`
			}
			json.Unmarshal(ex.Data, &d)
			if d.SecretKey != "" {
				writeFile(p.secretPath(), []byte("secret_key = \""+d.SecretKey+"\"\n"), 0o600)
				p.mu.Lock()
				p.code = ""
				p.state, p.note = "starting", "Connected to your playit.gg account."
				p.mu.Unlock()
				return time.Second
			}
		}
	}
	return 2 * time.Second
}

// ensureAgent downloads playit's agent if it isn't here, and starts it if it isn't running.
func (p *playitAgent) ensureAgent(ctx context.Context) error {
	p.mu.Lock()
	running := p.cmd != nil
	p.mu.Unlock()
	if running {
		return nil
	}
	bin := filepath.Join(p.dir(), exeName("playitd"))
	if !exists(bin) {
		p.set("starting", "Downloading playit.gg's agent.")
		if err := p.download(ctx, bin); err != nil {
			return err
		}
	}
	cmd := exec.Command(bin, "--secret-path", p.secretPath(), "--socket-path", filepath.Join(p.dir(), "playitd.sock"))
	cmd.Dir = p.dir()
	log, _ := os.OpenFile(filepath.Join(p.dir(), "playitd.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	cmd.Stdout, cmd.Stderr = log, log
	prepareChild(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("playit.gg's agent didn't start: %v", err)
	}
	adoptChild(cmd)
	p.mu.Lock()
	p.cmd = cmd
	p.mu.Unlock()
	go func() {
		cmd.Wait()
		if log != nil {
			log.Close()
		}
		p.mu.Lock()
		if p.cmd == cmd {
			p.cmd = nil
		}
		p.mu.Unlock()
	}()
	return nil
}

func (p *playitAgent) stopAgent() {
	p.mu.Lock()
	cmd := p.cmd
	p.cmd = nil
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

func (p *playitAgent) download(ctx context.Context, bin string) error {
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := getJSON(ctx, playitReleases, time.Hour, &rel); err != nil {
		return err
	}
	var want string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64", "windows/arm64":
		want = "playitd-windows-x86_64-signed.exe"
	case "windows/386":
		want = "playitd-windows-x86-signed.exe"
	case "linux/amd64":
		want = "playit-linux-amd64"
	case "linux/arm64":
		want = "playit-linux-aarch64"
	case "linux/arm":
		want = "playit-linux-armv7"
	case "linux/386":
		want = "playit-linux-i686"
	default:
		return fmt.Errorf("playit.gg has no agent for this computer")
	}
	for _, a := range rel.Assets {
		if a.Name != want {
			continue
		}
		sum := strings.TrimPrefix(a.Digest, "sha256:")
		if sum == "" {
			return fmt.Errorf("GitHub didn't give a checksum for playit.gg's agent, so it wasn't downloaded")
		}
		if err := download(ctx, a.URL, bin, Hash{"sha256", sum}, nil, "playit.gg's agent"); err != nil {
			return err
		}
		return os.Chmod(bin, 0o755)
	}
	return fmt.Errorf("playit.gg's latest release has no %s", want)
}

// Forget removes this computer's playit.gg secret (it can be claimed again).
func (p *playitAgent) forget() {
	p.stopAgent()
	os.Remove(p.secretPath())
	p.mu.Lock()
	p.code, p.state, p.note, p.tunnels = "", "off", "", nil
	p.mu.Unlock()
}

func (m *Manager) ForgetPlayit() { m.playit.forget() }

func (p *playitAgent) statusFor(s *Server) WayStatus {
	p.mu.Lock()
	st := WayStatus{Kind: "playit", State: p.state, Note: p.note}
	if p.state == "claim" && p.code != "" {
		st.ClaimURL = "https://playit.gg/claim/" + p.code
	}
	p.mu.Unlock()
	if t, ok := p.tunnelFor(s.Port(), false); ok && t.Address != "" {
		st.Address, st.Up, st.State, st.CheckOK = t.Address, true, "up", true
		if t.Off != "" {
			st.Up, st.State, st.Note = false, "problem", "playit.gg has switched the tunnel off: "+t.Off
		}
	} else if p.state == "on" {
		st.State, st.Note = "connecting", "playit.gg is making the tunnel."
	}
	if t, ok := p.tunnelFor(BedrockPort, true); ok {
		st.Bedrock = t.Address
	}
	return st
}

func (p *playitAgent) idleStatus(s *Server) WayStatus {
	if p.secret() == "" {
		return WayStatus{Kind: "playit", State: "waiting", Note: "Start the server to connect this computer to playit.gg."}
	}
	st := WayStatus{Kind: "playit", State: "waiting", Note: "Starts with the server."}
	if t, ok := p.tunnelFor(s.Port(), false); ok {
		st.Address = t.Address
	}
	return st
}

type playitWay struct {
	m *Manager
	s *Server
}

func (w *playitWay) start(s *Server) {
	w.m.playit.use(s, true)
	s.Note("Connecting to playit.gg so people on the internet can join.")
}
func (w *playitWay) stop()             { w.m.playit.use(w.s, false) }
func (w *playitWay) status() WayStatus { return w.m.playit.statusFor(w.s) }
func (w *playitWay) recheck()          {}
