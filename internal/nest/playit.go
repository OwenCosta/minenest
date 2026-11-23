package nest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
// (playitd, downloaded from playit's GitHub releases, checked against GitHub's SHA-256) runs while
// any server using playit.gg runs, and MineNest asks playit.gg for a Minecraft tunnel to each one.
// MineNest reads what the agent prints, so the Internet page shows its real state and its last
// words when something goes wrong (playitparse.go).

const (
	playitAPI      = "https://api.playit.gg"
	playitReleases = "https://api.github.com/repos/playit-cloud/playit-agent/releases?per_page=10"
	playitVersion  = "2.0.2" // what MineNest says it is when it asks for a claim
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
	pending   []string // names of tunnels playit.gg is still making
	notices   []string // what playit.gg says needs doing on the account
	users     map[*Server]bool
	loopStop  chan struct{}
	kick      chan struct{}
	creating  map[string]time.Time
	tunnelErr map[string]string // why playit.gg didn't make a tunnel, by kind:port

	// The agent itself.
	cmd        *exec.Cmd
	started    time.Time
	connected  bool
	tail       []string // its last lines, secrets taken out
	fails      int      // stops in a row without connecting
	lastExit   time.Time
	diag       *playitDiag // why it stopped or isn't connecting
	checks     map[*Server]wayCheck
	newerCheck bool // look for a newer agent at the next start, whatever the last look said
}

type wayCheck struct {
	text string
	ok   bool
	addr string
}

type playitTunnel struct {
	Type    string
	Address string
	Port    int
	Off     string
	Name    string
}

func newPlayit(m *Manager) *playitAgent {
	return &playitAgent{m: m, state: "off", users: map[*Server]bool{}, creating: map[string]time.Time{},
		tunnelErr: map[string]string{}, kick: make(chan struct{}, 1), checks: map[*Server]wayCheck{}}
}

func (p *playitAgent) dir() string        { return homePath("playit") }
func (p *playitAgent) secretPath() string { return filepath.Join(p.dir(), "playit.toml") }
func (p *playitAgent) bin() string        { return filepath.Join(p.dir(), exeName("playitd")) }

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

// playitSocket is where the agent listens for its own control channel. On Windows that has to
// be a named pipe (playitd refuses a file path there and exits at once), elsewhere a socket file
// in MineNest's folder. Each MineNest home has its own, so it never meets a playit installed
// by hand.
func playitSocket(goos, dir string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(dir)))
	id := hex.EncodeToString(sum[:4])
	if goos == "windows" {
		return `\\.\pipe\minenest-playitd-` + id
	}
	path := filepath.Join(dir, "playitd.sock")
	if len(path) > 100 { // a Unix socket's path has to be short
		return "@minenest-playitd-" + id
	}
	return path
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
		if resp.StatusCode == 429 {
			return nil, fmt.Errorf("playit.gg says MineNest asked too often. It waits a little.")
		}
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
		delete(p.checks, s)
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

// poke makes the loop take its next step now.
func (p *playitAgent) poke() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *playitAgent) loop(stop chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-stop; cancel() }()
	for {
		wait := p.step(ctx)
		select {
		case <-stop:
			return
		case <-p.kick:
		case <-time.After(wait):
		}
	}
}

// step does the next thing: the claim, the agent, the tunnels. It says how long to wait after.
func (p *playitAgent) step(ctx context.Context) time.Duration {
	if p.secret() == "" {
		return p.claimStep(ctx)
	}
	if wait, err := p.ensureAgent(ctx); err != nil {
		p.set("problem", err.Error())
		return wait
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
			p.set("problem", "playit.gg doesn't accept this computer's agent any more. Choose Forget playit.gg, then start the server to connect it again.")
			p.stopAgent()
			return time.Minute
		}
		p.set("problem", "playit.gg had a problem ("+e.Type+"). MineNest tries again soon.")
		return 30 * time.Second
	}
	rd := parseRunData(r.Data)
	p.mu.Lock()
	p.agentID = rd.agentID
	p.tunnels = rd.tunnels
	p.pending = rd.pending
	p.notices = rd.notices
	users := make([]*Server, 0, len(p.users))
	for s := range p.users {
		users = append(users, s)
	}
	p.state, p.note = "on", ""
	connected := p.connected
	p.mu.Unlock()
	// A Minecraft tunnel for every server that hasn't one (and a Bedrock one when it has Geyser).
	waiting := false
	for _, s := range users {
		if _, ok := p.tunnelFor(s.Port(), false); !ok {
			waiting = true
			if !p.isPending(s) {
				p.createTunnel(ctx, s, "minecraft-java", s.Port())
			}
		} else if connected {
			p.checkOnce(s)
		}
		if s.Config().Bedrock && geyserInstalled(s) {
			if _, ok := p.tunnelFor(BedrockPort, true); !ok {
				waiting = true
				p.createTunnel(ctx, s, "minecraft-bedrock", BedrockPort)
			}
		}
	}
	if !connected {
		return 10 * time.Second
	}
	if waiting {
		return 5 * time.Second
	}
	return time.Minute
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

type runData struct {
	agentID string
	tunnels []playitTunnel
	pending []string
	notices []string
}

// parseRunData reads playit.gg's /v1/agents/rundata answer.
func parseRunData(raw json.RawMessage) runData {
	var data map[string]any
	json.Unmarshal(raw, &data)
	rd := runData{agentID: str(data["agent_id"]), tunnels: []playitTunnel{}}
	if list, ok := data["tunnels"].([]any); ok {
		for _, t := range list {
			tm, _ := t.(map[string]any)
			pt := playitTunnel{Type: str(tm["tunnel_type"]), Address: str(tm["display_address"]), Off: str(tm["disabled_reason"]), Name: str(tm["name"])}
			pt.Port = findLocalPort(tm)
			rd.tunnels = append(rd.tunnels, pt)
		}
	}
	if list, ok := data["pending"].([]any); ok {
		for _, t := range list {
			tm, _ := t.(map[string]any)
			rd.pending = append(rd.pending, str(tm["name"]))
		}
	}
	if list, ok := data["notices"].([]any); ok {
		for _, n := range list {
			nm, _ := n.(map[string]any)
			if pr := str(nm["priority"]); (pr == "Critical" || pr == "High") && str(nm["message"]) != "" {
				msg := strings.TrimSpace(str(nm["message"]))
				if link := str(nm["resolve_link"]); strings.HasPrefix(link, "https://") {
					msg += " (" + link + ")"
				}
				rd.notices = append(rd.notices, msg)
			}
		}
	}
	return rd
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

func tunnelName(s *Server) string { return "MineNest " + s.Config().Name }

// isPending says playit.gg is still making the server's tunnel.
func (p *playitAgent) isPending(s *Server) bool {
	name := tunnelName(s)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range p.pending {
		if n == name {
			return true
		}
	}
	return false
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
		"enabled": true, "alloc": nil, "name": tunnelName(s), "firewall_id": nil,
	}
	r, err := p.post(ctx, "/v1/tunnels/create", body, true)
	if err != nil {
		s.Note("playit.gg couldn't make a tunnel: %v", err)
		return
	}
	if r.Status == "success" {
		p.mu.Lock()
		delete(p.tunnelErr, key)
		p.mu.Unlock()
		s.Note("playit.gg made a tunnel for the server. Its address comes in a moment.")
		return
	}
	var why string
	json.Unmarshal(r.Data, &why)
	if why == "" {
		why = string(r.Data)
	}
	p.mu.Lock()
	p.tunnelErr[key] = why
	p.mu.Unlock()
	s.Note("playit.gg didn't make a tunnel (%s). You can add one for port %d on playit.gg yourself.", why, port)
}

// tunnelProblem turns playit.gg's reason for not making a tunnel into words and a next step.
func tunnelProblem(why string, port int, bedrock bool) (string, string) {
	kind := "Minecraft Java"
	if bedrock {
		kind = "Minecraft Bedrock"
	}
	yourself := fmt.Sprintf("Or add a %s tunnel to 127.0.0.1 port %d for this agent on playit.gg yourself.", kind, port)
	switch why {
	case "RequiresVerifiedAccount":
		return "playit.gg wants your account's email address verified before it makes a tunnel.", "Verify it on playit.gg, then choose Try again."
	case "AgentVersionTooOld":
		return "playit.gg says the agent is too old for a new tunnel.", "Choose Try again: MineNest fetches the newest agent."
	case "RequiresPlayitPremium", "RegionRequiresPlayitPremium", "PublicPortRequiresPlayitPremium":
		return "playit.gg only makes this kind of tunnel with playit Premium.", yourself
	}
	return "playit.gg didn't make the tunnel (" + why + ").", "Choose Try again. " + yourself
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

// ------------------------------------------------------------------ the agent

// agentRecord is what MineNest knows about the agent it downloaded (playit/agent.json).
type agentRecord struct {
	Tag     string    `json:"tag"`
	Asset   string    `json:"asset"`
	SHA256  string    `json:"sha256"`
	Checked time.Time `json:"checked"`
}

func (p *playitAgent) recordPath() string { return filepath.Join(p.dir(), "agent.json") }

// ensureAgent makes sure the agent is here, whole and current, and running. While it's waiting
// to start it again after a stop, it says how long to wait.
func (p *playitAgent) ensureAgent(ctx context.Context) (time.Duration, error) {
	p.mu.Lock()
	running := p.cmd != nil
	fails, lastExit, diag := p.fails, p.lastExit, p.diag
	forceCheck := p.newerCheck
	p.mu.Unlock()
	if running {
		// An agent that stays up for days is looked over once a day: when playit.gg has a newer
		// one, it is stopped, and the next start fetches the new one.
		var rec agentRecord
		readJSON(p.recordPath(), &rec)
		if rec.SHA256 != "" && time.Since(rec.Checked) > 24*time.Hour {
			if rel, err := latestPlayitAgent(ctx, true); err == nil {
				rec.Checked = time.Now()
				writeJSON(p.recordPath(), rec, 0o644)
				if rel.Tag != rec.Tag {
					p.mu.Lock()
					p.newerCheck = true
					p.mu.Unlock()
					p.stopAgent()
					p.poke()
				}
			}
		}
		return 0, nil
	}
	if forceCheck {
		p.mu.Lock()
		p.newerCheck = false
		p.mu.Unlock()
	}
	if fails > 0 {
		if wait := playitBackoff(fails) - time.Since(lastExit); wait > 0 {
			if diag != nil {
				return wait, errors.New(diag.Note)
			}
			return wait, fmt.Errorf("playit.gg's agent stopped. MineNest starts it again in a moment.")
		}
	}
	bin := p.bin()
	var rec agentRecord
	readJSON(p.recordPath(), &rec)
	fresh := ""
	switch {
	case !exists(bin):
		if rec.SHA256 != "" {
			fresh = "playit.gg's agent was missing (an antivirus may have taken it away), so MineNest downloads it again."
		} else {
			fresh = "Downloading playit.gg's agent."
		}
	case rec.SHA256 == "":
		fresh = "Getting playit.gg's newest agent (MineNest couldn't tell which one it had)."
	default:
		if sum, err := fileHash(bin, "sha256"); err != nil || !strings.EqualFold(sum, rec.SHA256) {
			fresh = "playit.gg's agent had changed on disk (an antivirus or a broken download), so MineNest downloads it again."
		} else if forceCheck || time.Since(rec.Checked) > 24*time.Hour {
			if rel, err := latestPlayitAgent(ctx, true); err == nil {
				rec.Checked = time.Now()
				writeJSON(p.recordPath(), rec, 0o644)
				if rel.Tag != rec.Tag {
					fresh = "Updating playit.gg's agent to " + rel.Tag + "."
				}
			}
		}
	}
	if fresh != "" {
		p.set("starting", fresh)
		if err := p.download(ctx, bin, forceCheck); err != nil {
			return 30 * time.Second, err
		}
	}
	return p.startAgent(bin)
}

func playitBackoff(fails int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < fails && d < 2*time.Minute; i++ {
		d *= 2
	}
	return min(d, 2*time.Minute)
}

func (p *playitAgent) startAgent(bin string) (time.Duration, error) {
	cmd := exec.Command(bin, "--secret-path", p.secretPath(), "--socket-path", playitSocket(runtime.GOOS, p.dir()))
	cmd.Dir = p.dir()
	cmd.Env = append(os.Environ(), "PLAYIT_LOG=info")
	log, _ := os.OpenFile(filepath.Join(p.dir(), "playitd.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	prepareChild(cmd)
	if err := cmd.Start(); err != nil {
		pw.Close()
		if log != nil {
			log.Close()
		}
		d := diagnoseStart(err, p.dir())
		p.mu.Lock()
		p.fails++
		p.lastExit, p.diag = time.Now(), &d
		p.mu.Unlock()
		if d.Redownload {
			os.Remove(bin)
			os.Remove(p.recordPath()) // so the next start says "downloading", not "missing"
		}
		return playitBackoff(p.fails), errors.New(d.Note)
	}
	adoptChild(cmd)
	p.mu.Lock()
	p.cmd, p.started, p.connected, p.tail, p.diag = cmd, time.Now(), false, nil, nil
	p.mu.Unlock()
	p.set("starting", "Starting playit.gg's agent.")
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := cleanAgentLine(sc.Text())
			if line == "" {
				continue
			}
			if log != nil {
				fmt.Fprintln(log, line)
			}
			p.agentSaid(cmd, line)
		}
		io.Copy(io.Discard, pr)
	}()
	go func() {
		err := cmd.Wait()
		pw.Close()
		<-done
		if log != nil {
			log.Close()
		}
		p.agentExited(cmd, err)
	}()
	return 0, nil
}

// agentSaid takes one line the agent printed.
func (p *playitAgent) agentSaid(cmd *exec.Cmd, line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != cmd {
		return
	}
	p.tail = append(p.tail, line)
	if len(p.tail) > 40 {
		p.tail = append([]string(nil), p.tail[len(p.tail)-40:]...)
	}
	switch ev := agentEvent(line); ev {
	case "connected":
		if !p.connected {
			p.connected, p.fails, p.diag = true, 0, nil
			p.poke()
		}
	case "":
	default:
		if d, ok := playitProblems[ev]; ok && !p.connected {
			d := d
			p.diag = &d
		}
	}
}

// agentExited notes why the agent stopped, and has the loop start it again after a pause.
func (p *playitAgent) agentExited(cmd *exec.Cmd, err error) {
	p.mu.Lock()
	if p.cmd != cmd {
		p.mu.Unlock()
		return
	}
	p.cmd = nil
	p.connected = false
	p.fails++
	p.lastExit = time.Now()
	d := diagnosePlayit(p.tail, err)
	p.diag = &d
	tail := strings.Join(lastLines(p.tail, 8), "\n")
	p.mu.Unlock()
	if d.Redownload {
		os.Remove(p.bin())
		os.Remove(p.recordPath())
	}
	for _, s := range p.servers() {
		s.Note("playit.gg's agent stopped: %s", d.Note)
		if tail != "" {
			s.Note("Its last words:\n%s", tail)
		}
	}
	p.poke()
}

func (p *playitAgent) servers() []*Server {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []*Server{}
	for s := range p.users {
		out = append(out, s)
	}
	return out
}

func (p *playitAgent) stopAgent() {
	p.mu.Lock()
	cmd := p.cmd
	p.cmd = nil
	p.connected = false
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

// restart stops the agent and starts it again at once, forgetting why it stopped before (the
// Internet page's Try again).
func (p *playitAgent) restart() {
	p.stopAgent()
	p.mu.Lock()
	p.fails, p.diag, p.tail = 0, nil, nil
	p.newerCheck = true // Try again also looks for a newer agent, as the message about an old one says
	p.creating = map[string]time.Time{}
	p.tunnelErr = map[string]string{}
	p.checks = map[*Server]wayCheck{}
	if p.state == "problem" {
		p.state, p.note = "starting", "Starting playit.gg's agent again."
	}
	p.mu.Unlock()
	p.poke()
}

func (m *Manager) RestartPlayit() { m.playit.restart() }

type playitRelease struct {
	Tag, Asset, URL, SHA256 string
}

// playitAsset is the agent's file in playit's releases for this computer: playitd, the agent
// without a window (from playit 1.0 on; before that these names were a different program).
func playitAsset(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "windows/amd64", "windows/arm64":
		return "playitd-windows-x86_64-signed.exe"
	case "windows/386":
		return "playitd-windows-x86-signed.exe"
	case "linux/amd64":
		return "playit-linux-amd64"
	case "linux/arm64":
		return "playit-linux-aarch64"
	case "linux/arm":
		return "playit-linux-armv7"
	case "linux/386":
		return "playit-linux-i686"
	}
	return ""
}

type playitGHRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// pickPlayitRelease finds the newest finished release that has the agent for this computer with
// a checksum (playit signs the Windows files after a release is out, so the newest may not
// have them yet), and that is playitd: 1.0 or newer.
func pickPlayitRelease(rels []playitGHRelease, want string) (playitRelease, error) {
	for _, r := range rels {
		if r.Draft || r.Prerelease || !playitdTag(r.Tag) {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == want && strings.HasPrefix(a.Digest, "sha256:") && len(a.Digest) == 71 {
				return playitRelease{Tag: r.Tag, Asset: a.Name, URL: a.URL, SHA256: strings.TrimPrefix(a.Digest, "sha256:")}, nil
			}
		}
	}
	return playitRelease{}, fmt.Errorf("playit.gg's recent releases have no %s with a checksum, so MineNest didn't download one", want)
}

func playitdTag(tag string) bool {
	v := strings.TrimPrefix(tag, "v")
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= 1
}

// latestPlayitAgent is the newest agent for this computer. The list of releases is kept for an
// hour, unless fresh asks for a new look.
func latestPlayitAgent(ctx context.Context, fresh bool) (playitRelease, error) {
	want := playitAsset(runtime.GOOS, runtime.GOARCH)
	if want == "" {
		return playitRelease{}, fmt.Errorf("playit.gg has no agent for this computer")
	}
	var rels []playitGHRelease
	age := time.Hour
	if fresh {
		age = 0
	}
	if err := getJSON(ctx, playitReleases, age, &rels); err != nil {
		return playitRelease{}, err
	}
	return pickPlayitRelease(rels, want)
}

func (p *playitAgent) download(ctx context.Context, bin string, fresh bool) error {
	rel, err := latestPlayitAgent(ctx, fresh)
	if err != nil {
		return err
	}
	if err := download(ctx, rel.URL, bin, Hash{"sha256", rel.SHA256}, nil, "playit.gg's agent"); err != nil {
		return err
	}
	os.Chmod(bin, 0o755)
	// An antivirus sometimes takes a new program away the moment it's written.
	time.Sleep(500 * time.Millisecond)
	if sum, err := fileHash(bin, "sha256"); err != nil || !strings.EqualFold(sum, rel.SHA256) {
		return fmt.Errorf("playit.gg's agent was taken away right after it was downloaded, most likely by an antivirus. Allow %s in it, then choose Try again", bin)
	}
	return writeJSON(p.recordPath(), agentRecord{Tag: rel.Tag, Asset: rel.Asset, SHA256: rel.SHA256, Checked: time.Now()}, 0o644)
}

// Forget removes this computer's playit.gg secret (it can be claimed again).
func (p *playitAgent) forget() {
	p.stopAgent()
	os.Remove(p.secretPath())
	p.mu.Lock()
	p.code, p.state, p.note, p.tunnels = "", "off", "", nil
	p.fails, p.diag, p.tail = 0, nil, nil
	p.checks = map[*Server]wayCheck{}
	p.mu.Unlock()
	p.poke()
}

func (m *Manager) ForgetPlayit() { m.playit.forget() }

// ------------------------------------------------------------------ the check from outside

// playitTarget is where a game really connects for a playit.gg address: its own port when it
// has one, or the one its SRV record names (playit's Java addresses carry their port there,
// so trying 25565 would be a false alarm).
func playitTarget(addr string, lookupSRV func(host string) (string, int, error)) (string, int) {
	if h, p, err := net.SplitHostPort(addr); err == nil {
		n, _ := strconv.Atoi(p)
		return h, n
	}
	if lookupSRV != nil {
		if h, p, err := lookupSRV(addr); err == nil && p > 0 {
			return h, p
		}
	}
	return addr, DefaultPort
}

func lookupMinecraftSRV(host string) (string, int, error) {
	_, srvs, err := net.LookupSRV("minecraft", "tcp", host)
	if err != nil || len(srvs) == 0 {
		return "", 0, fmt.Errorf("no SRV record")
	}
	return strings.TrimSuffix(srvs[0].Target, "."), int(srvs[0].Port), nil
}

// checkOnce checks the server's address once after the agent connects (and again on request).
func (p *playitAgent) checkOnce(s *Server) {
	t, ok := p.tunnelFor(s.Port(), false)
	if !ok || t.Address == "" || t.Off != "" {
		return
	}
	p.mu.Lock()
	c, done := p.checks[s]
	if done && c.addr == t.Address {
		p.mu.Unlock()
		return
	}
	p.checks[s] = wayCheck{addr: t.Address, text: "Checking from the internet..."}
	p.mu.Unlock()
	go p.check(s, t.Address)
}

func (p *playitAgent) check(s *Server, addr string) {
	host, port := playitTarget(addr, lookupMinecraftSRV)
	var err error
	for try := 0; try < 3; try++ { // a new tunnel takes a few seconds to answer everywhere
		if _, err = Ping(host, port, 8*time.Second); err == nil {
			break
		}
		time.Sleep(5 * time.Second)
	}
	c := wayCheck{addr: addr, ok: err == nil}
	if err == nil {
		c.text = "Checked from the internet: friends can join at " + addr + "."
		s.Note("Checked from the internet: friends can join at %s.", addr)
	} else {
		c.text = pingProblem(addr, err) + " playit.gg's agent is connected, so it may only need a minute. Choose Check again."
		s.Note("The check from the internet found a problem: %s", c.text)
	}
	p.mu.Lock()
	if _, still := p.users[s]; still {
		p.checks[s] = c
	}
	p.mu.Unlock()
}

func (p *playitAgent) recheck(s *Server) {
	p.mu.Lock()
	delete(p.checks, s)
	p.mu.Unlock()
	p.checkOnce(s)
}

// ------------------------------------------------------------------ status

func lastLines(v []string, n int) []string {
	if len(v) > n {
		v = v[len(v)-n:]
	}
	return append([]string(nil), v...)
}

func (p *playitAgent) statusFor(s *Server) WayStatus {
	p.mu.Lock()
	st := WayStatus{Kind: "playit", State: p.state, Note: p.note, CanRetry: true}
	if p.state == "claim" && p.code != "" {
		st.ClaimURL = "https://playit.gg/claim/" + p.code
	}
	running, connected, started, diag := p.cmd != nil, p.connected, p.started, p.diag
	tail := lastLines(p.tail, 10)
	check, checked := p.checks[s]
	notices := append([]string(nil), p.notices...)
	javaErr := p.tunnelErr[fmt.Sprintf("minecraft-java:%d", s.Port())]
	state := p.state
	p.mu.Unlock()
	if state == "claim" || state == "off" {
		st.CanRetry = state != "off"
		return st
	}
	if t, ok := p.tunnelFor(BedrockPort, true); ok {
		st.Bedrock = t.Address
	}
	t, hasTunnel := p.tunnelFor(s.Port(), false)
	if hasTunnel {
		st.Address = t.Address
	}
	switch {
	case diag != nil && !connected:
		st.State, st.Note, st.Fix, st.Log = "problem", diag.Note, diag.Fix, tail
		if running {
			st.Agent = "running"
		} else {
			st.Agent = "stopped"
		}
	case state == "problem":
		st.Log = tail
	case !running && state == "on":
		st.State, st.Note, st.Agent = "connecting", "Starting playit.gg's agent.", "stopped"
	case running && !connected:
		st.Agent = "running"
		st.State, st.Note = "connecting", "playit.gg's agent is connecting."
		if time.Since(started) > 45*time.Second {
			st.State = "problem"
			st.Note = "playit.gg's agent has been running for a while without connecting."
			st.Fix = "It needs to reach playit.gg over UDP: check that this computer is online and that a firewall or VPN isn't blocking playitd. Then choose Try again."
			st.Log = tail
		}
	case !hasTunnel:
		st.Agent = "connected"
		st.State, st.Note = "connecting", "playit.gg is making the tunnel."
		if javaErr != "" {
			st.State = "problem"
			st.Note, st.Fix = tunnelProblem(javaErr, s.Port(), false)
		}
		if len(notices) > 0 {
			st.Note += " playit.gg says: " + strings.Join(notices, " ")
		}
	case t.Off != "":
		st.Agent = "connected"
		st.State, st.Note = "problem", "playit.gg has switched the tunnel off: "+t.Off
		st.Fix = "Look at the tunnel on playit.gg to switch it back on."
		if len(notices) > 0 {
			st.Note += " playit.gg says: " + strings.Join(notices, " ")
		}
	default:
		st.Agent = "connected"
		st.Up, st.State, st.Note = true, "up", ""
		if checked {
			st.Check, st.CheckOK = check.text, check.ok
		}
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
func (w *playitWay) recheck()          { w.m.playit.recheck(w.s) }
