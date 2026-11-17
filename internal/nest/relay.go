package nest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The supporter relay: for supporters on Patreon, a name of their own, <name>.mc.owencosta.diy,
// for friends to type in, with nothing to set up on a router or a server.
//
// XenithOS Cloud (/v1/relay/status, claim and release, with a XenithOS profile's token) says
// whether the profile belongs to a supporter (OCAccounts knows once Patreon and the profile are
// linked there), gives out the name, and passes MineNest's key on to the relay. While the server
// runs, MineNest keeps an SSH tunnel to the relay that may open one port on its loopback, and the
// relay passes everyone who types the name down it. Its host key comes from XenithOS Cloud and is
// the only one trusted. The last answer is kept in relay/relay.json so the tunnel starts without
// asking again.
//
// Players' real addresses: with Paper or Purpur, MineNest tells XenithOS Cloud the server wants
// them (proxy=1), the relay's router sends each one in a PROXY protocol v2 header, and the
// doorway (door.go) gives everyone else the same header so they still get in.
//
// Java Edition only: the tunnel carries TCP, and Bedrock needs UDP.

type relayState struct {
	m    *Manager
	mu   sync.Mutex
	last map[string]string // XenithOS Cloud's last answer
}

func newRelay(m *Manager) *relayState {
	r := &relayState{m: m, last: map[string]string{}}
	readJSON(homePath("relay", "relay.json"), &r.last)
	return r
}

func (r *relayState) get(k string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[k]
}

func parseKV(body string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.IndexByte(line, '='); i > 0 {
			k := line[:i]
			if _, dup := out[k]; !dup {
				out[k] = line[i+1:]
			}
		}
	}
	return out
}

// cloudPost sends a form to XenithOS Cloud and reads its key=value answer.
func (m *Manager) cloudPost(ctx context.Context, path string, form url.Values) (map[string]string, int, error) {
	base := m.Settings().Cloud
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", agent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("XenithOS Cloud didn't answer: %v", friendlyNetErr(base, err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	kv := parseKV(string(b))
	if resp.StatusCode != 200 {
		if e := kv["error"]; e != "" {
			return kv, resp.StatusCode, fmt.Errorf("%s", e)
		}
		return kv, resp.StatusCode, fmt.Errorf("XenithOS Cloud answered with error %d", resp.StatusCode)
	}
	return kv, 200, nil
}

// ------------------------------------------------------------------ the account

type Account struct {
	Name  string `json:"name"`
	Cloud string `json:"cloud"`
}

// Login signs in to a XenithOS profile (the same one XenithOS uses) with its name and PIN.
func (m *Manager) Login(ctx context.Context, name, pin string, create bool) (*Account, error) {
	form := url.Values{"name": {name}, "pin": {pin}}
	path := "/v1/accounts/login"
	if create {
		path = "/v1/accounts/create"
		form.Set("avatar", "1202001000")
	}
	kv, code, err := m.cloudPost(ctx, path, form)
	if err != nil {
		if code == 401 {
			return nil, fmt.Errorf("that name and PIN don't match a profile")
		}
		return nil, err
	}
	if kv["token"] == "" {
		return nil, fmt.Errorf("XenithOS Cloud didn't send a sign-in")
	}
	m.updateSettings(func(s *Settings) { s.AccountName, s.AccountToken = kv["name"], kv["token"] })
	return &Account{Name: kv["name"], Cloud: m.Settings().Cloud}, nil
}

// UseToken signs in with a token XenithOS already has (the XenithOS app hands its profile over).
func (m *Manager) UseToken(ctx context.Context, token string) (*Account, error) {
	kv, _, err := m.cloudPost(ctx, "/v1/accounts/me", url.Values{"token": {token}})
	if err != nil {
		return nil, err
	}
	m.updateSettings(func(s *Settings) { s.AccountName, s.AccountToken = kv["name"], token })
	return &Account{Name: kv["name"], Cloud: m.Settings().Cloud}, nil
}

func (m *Manager) Logout(ctx context.Context) {
	if tok := m.Settings().AccountToken; tok != "" {
		m.cloudPost(ctx, "/v1/accounts/logout", url.Values{"token": {tok}})
	}
	m.updateSettings(func(s *Settings) { s.AccountName, s.AccountToken = "", "" })
}

// ------------------------------------------------------------------ the relay

// RelayInfo is what the GUI shows on the relay's page.
type RelayInfo struct {
	SignedIn  bool   `json:"signed_in"`
	Account   string `json:"account"`
	Open      bool   `json:"open"`    // the relay is taking supporters
	Step      string `json:"step"`    // ok, link, patreon, tier, error
	Message   string `json:"message"` // what XenithOS Cloud says about where they are
	Tier      string `json:"tier"`
	Name      string `json:"name"`
	State     string `json:"state"` // on, lapsed, off
	Address   string `json:"address"`
	Domain    string `json:"domain"`
	Suggest   string `json:"suggest"`
	Held      string `json:"held"`
	Patreon   string `json:"patreon"`  // where to become a supporter
	LinkURL   string `json:"link_url"` // where to link Patreon and the profile (OCAccounts)
	Until     int64  `json:"until"`
	Proxy     bool   `json:"proxy"`
	ProxyAble bool   `json:"proxy_able"`
	UsedBy    string `json:"used_by"` // the server using the name
}

func (r *relayState) info() RelayInfo {
	set := r.m.Settings()
	r.mu.Lock()
	l := r.last
	r.mu.Unlock()
	until, _ := strconv.ParseInt(l["until"], 10, 64)
	ri := RelayInfo{SignedIn: set.AccountToken != "", Account: set.AccountName, Open: l["relay"] == "on", Step: l["step"],
		Message: l["message"], Tier: l["tier"], Name: l["name"], State: l["state"], Address: l["address"], Domain: l["domain"],
		Suggest: l["suggest"], Held: l["held"], Patreon: l["patreon"], LinkURL: l["account"], Until: until,
		Proxy: l["proxy"] == "on", ProxyAble: l["proxy_able"] == "1"}
	for _, s := range r.m.List() {
		if s.Config().Way == "relay" {
			ri.UsedBy = s.Config().Name
		}
	}
	return ri
}

func (m *Manager) RelayInfo() RelayInfo { return m.relay.info() }

// relayServer is the server using the relay, if any.
func (r *relayState) relayServer() *Server {
	for _, s := range r.m.List() {
		if s.Config().Way == "relay" {
			return s
		}
	}
	return nil
}

// proxyWanted: the relay's server can take players' real addresses (Paper or Purpur, with them
// turned on, without Geyser).
func proxyWanted(s *Server) bool {
	if s == nil {
		return false
	}
	c := s.Config()
	if c.Installed == nil || !c.RealIPs || (c.Installed.Flavor != "paper" && c.Installed.Flavor != "purpur") {
		return false
	}
	return !(c.Bedrock && geyserInstalled(s))
}

// RelayAsk asks XenithOS Cloud: "status", "claim" (with a name) or "release".
func (m *Manager) RelayAsk(ctx context.Context, what, name string) (RelayInfo, error) {
	r := m.relay
	tok := m.Settings().AccountToken
	if tok == "" {
		return r.info(), fmt.Errorf("sign in to your XenithOS profile first (Account)")
	}
	srv := r.relayServer()
	proxy := "0"
	if srv != nil && srv.State() != Stopped {
		srv.mu.Lock()
		if srv.door != nil {
			proxy = "1"
		}
		srv.mu.Unlock()
	} else if proxyWanted(srv) {
		proxy = "1"
	}
	form := url.Values{"token": {tok}, "proxy": {proxy}}
	switch what {
	case "claim":
		pub, err := PublicKey()
		if err != nil {
			return r.info(), err
		}
		f := strings.Fields(pub)
		form.Set("name", strings.ToLower(strings.TrimSpace(name)))
		form.Set("key", f[0]+" "+f[1])
	case "status", "release":
	default:
		return r.info(), fmt.Errorf("unknown relay request %q", what)
	}
	kv, code, err := m.cloudPost(ctx, "/v1/relay/"+what, form)
	if err != nil {
		if code == 401 {
			m.updateSettings(func(s *Settings) { s.AccountToken = "" })
			return r.info(), fmt.Errorf("your XenithOS profile's sign-in has expired: sign in again")
		}
		return r.info(), err
	}
	r.mu.Lock()
	old := r.last
	r.last = kv
	r.mu.Unlock()
	writeJSON(homePath("relay", "relay.json"), kv, 0o600)
	// The relay moved, its identity changed, or the name did: the tunnel starts again.
	if srv != nil && (old["ssh_host"] != kv["ssh_host"] || old["port"] != kv["port"] || old["hostkey"] != kv["hostkey"] || old["name"] != kv["name"] || old["state"] != kv["state"]) {
		srv.mu.Lock()
		w, running := srv.way, srv.state == Running
		if w != nil {
			srv.way = nil
		}
		srv.mu.Unlock()
		if w != nil {
			w.stop()
		}
		if running {
			m.serverReady(srv)
		}
	}
	return r.info(), nil
}

func (r *relayState) proxyAble() bool {
	return r.get("proxy") == "on" && r.get("proxy_able") == "1"
}

// syncProxy tells XenithOS Cloud, before the relay's server starts, whether it wants players'
// addresses, so the relay sends them exactly when Paper expects them.
func (r *relayState) syncProxy(s *Server) {
	if s.Config().Way != "relay" || r.get("name") == "" {
		return
	}
	want := proxyWanted(s)
	if want == r.proxyAble() || (want && r.get("proxy_able") != "1") {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r.m.RelayAsk(ctx, "status", "")
}

func (r *relayState) canUse(s *Server) error {
	if r.m.Settings().AccountToken == "" {
		return fmt.Errorf("sign in to your XenithOS profile first, on the Account page")
	}
	if other := r.relayServer(); other != nil && other != s {
		return fmt.Errorf("%s uses the relay. Your name can lead to one server at a time: switch that one to another way first", other.Config().Name)
	}
	if r.get("name") == "" || r.get("state") != "on" {
		msg := r.get("message")
		if msg == "" {
			msg = "Pick a name for your server on the Account page first."
		}
		return fmt.Errorf("%s", msg)
	}
	if r.get("hostkey") == "" || r.get("ssh_host") == "" {
		return fmt.Errorf("XenithOS Cloud's answer about the relay wasn't complete. Try again in a minute")
	}
	return nil
}

func (r *relayState) serverGone(s *Server) {}

func (r *relayState) idleStatus(s *Server) WayStatus {
	st := WayStatus{Kind: "relay", State: "waiting", Address: r.get("address"), Note: "Starts with the server."}
	if r.get("state") != "on" {
		st.State, st.Note = "problem", r.get("message")
	}
	return st
}

func (r *relayState) wayFor(s *Server) way {
	if r.canUse(s) != nil {
		return nil
	}
	r.mu.Lock()
	l := r.last
	r.mu.Unlock()
	sshPort, _ := strconv.Atoi(l["ssh_port"])
	port, _ := strconv.Atoi(l["port"])
	playersPort, _ := strconv.Atoi(l["players_port"])
	if playersPort == 0 {
		playersPort = DefaultPort
	}
	hk, err := pinnedHostKey(l["hostkey"])
	if err != nil {
		return nil
	}
	cfg := TunnelConfig{Host: l["ssh_host"], User: l["ssh_user"], SSHPort: sshPort, Bind: "127.0.0.1", PublicPort: port}
	if cfg.SSHPort == 0 {
		cfg.SSHPort = 22
	}
	host := l["name"] + "." + l["domain"]
	name := l["name"]
	w := &tunnelWay{kind: "relay", s: s}
	w.t = &tunnel{cfg: cfg, hostKey: hk, name: "the relay", target: func() string {
		// With players' addresses on, the relay's router sends the header itself, so its
		// connections go straight to Paper's own port, past the doorway.
		s.mu.Lock()
		d := s.door
		s.mu.Unlock()
		if d != nil {
			return fmt.Sprintf("127.0.0.1:%d", d.paperPort)
		}
		return fmt.Sprintf("127.0.0.1:%d", s.Port())
	}}
	w.address = func() string { return l["address"] }
	w.target = func() (string, int) { return host, playersPort }
	w.finding = func(err error, res *PingResult) string {
		if err != nil {
			return pingProblem(l["address"], err)
		}
		switch res.Version {
		case "Offline":
			return "The relay doesn't see the tunnel from this computer yet. Try the check again in a moment."
		case "No server":
			return "The relay doesn't know " + l["address"] + " yet. It takes new names every 30 seconds: try the check again in a minute."
		case "Unavailable":
			return "The relay isn't passing players to " + name + ". Check your membership on the Account page."
		case "Busy":
			return "The relay says your server has as many connections as it takes right now."
		}
		return ""
	}
	return w
}
