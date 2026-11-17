package nest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Settings are MineNest's own (minenest.json in its home folder).
type Settings struct {
	Cloud        string `json:"cloud"`         // XenithOS Cloud, for the account and the supporter relay
	AccountName  string `json:"account_name"`  // the signed-in XenithOS profile
	AccountToken string `json:"account_token"` // its token (the file is readable by this user only)
	OCAccount    string `json:"oc_account"`    // the OCAccount it was signed in with, if any
	APIPort      int    `json:"api_port"`
	LANAccess    bool   `json:"lan_access"` // the GUI can be opened from other computers at home (with the key)
}

// Manager is the daemon's state: every server, the jobs, the account and the shared ways in.
type Manager struct {
	mu       sync.Mutex
	servers  map[string]*Server
	settings Settings
	Jobs     jobList
	playit   *playitAgent
	relay    *relayState

	ocSession, ocKey string // an OCAccount sign-in waiting for the browser
}

func NewManager() (*Manager, error) {
	m := &Manager{servers: map[string]*Server{}}
	if err := os.MkdirAll(homePath("servers"), 0o755); err != nil {
		return nil, err
	}
	readJSON(homePath("minenest.json"), &m.settings)
	if m.settings.Cloud == "" {
		m.settings.Cloud = DefaultCloud
	}
	if env := os.Getenv("MINENEST_CLOUD"); env != "" {
		m.settings.Cloud = strings.TrimRight(env, "/")
	}
	if m.settings.APIPort == 0 {
		m.settings.APIPort = APIPort
	}
	m.playit = newPlayit(m)
	m.relay = newRelay(m)
	entries, _ := os.ReadDir(homePath("servers"))
	for _, e := range entries {
		if !e.IsDir() || !exists(homePath("servers", e.Name(), "nest.json")) {
			continue
		}
		s := &Server{ID: e.Name(), Dir: homePath("servers", e.Name()), m: m, state: Stopped, players: map[string]time.Time{}}
		if err := s.load(); err != nil {
			continue
		}
		m.servers[s.ID] = s
	}
	return m, nil
}

func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

func (m *Manager) updateSettings(f func(s *Settings)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&m.settings)
	return writeJSON(homePath("minenest.json"), m.settings, 0o600)
}

// Autostart starts the servers marked to start with MineNest.
func (m *Manager) Autostart() {
	for _, s := range m.List() {
		if s.Config().Autostart {
			if err := s.Start(); err != nil {
				s.Note("It didn't start with MineNest: %v", err)
			}
		}
	}
}

// Shutdown stops every server, saving their worlds (the daemon is closing).
func (m *Manager) Shutdown() {
	var wg sync.WaitGroup
	for _, s := range m.List() {
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s.StopAndWait(ctx)
		}(s)
	}
	wg.Wait()
	m.playit.stop()
}

func (m *Manager) List() []*Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Server, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cfg.Created.Before(out[j].cfg.Created) })
	return out
}

// Get finds a server by its id or its name (any case). An empty ref is the only server, if
// there's just one.
func (m *Manager) Get(ref string) (*Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref == "" {
		if len(m.servers) == 1 {
			for _, s := range m.servers {
				return s, nil
			}
		}
		if len(m.servers) == 0 {
			return nil, fmt.Errorf("there are no servers yet. Make one first")
		}
		return nil, fmt.Errorf("there's more than one server: say which")
	}
	if s, ok := m.servers[ref]; ok {
		return s, nil
	}
	for _, s := range m.servers {
		if strings.EqualFold(s.cfg.Name, ref) || s.ID == safeName(ref) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("there's no server called %q", ref)
}

// portTaken names another running server on the same port.
func (m *Manager) portTaken(self *Server, port int) string {
	for _, s := range m.List() {
		if s != self && s.State() != Stopped && s.Port() == port {
			return s.Config().Name
		}
	}
	return ""
}

func (m *Manager) freePort() int {
	used := map[int]bool{}
	for _, s := range m.List() {
		used[s.Port()] = true
	}
	for p := DefaultPort; p < DefaultPort+100; p++ {
		if !used[p] {
			return p
		}
	}
	return DefaultPort
}

// CreateOptions describe a new server.
type CreateOptions struct {
	Name      string            `json:"name"`
	Flavor    string            `json:"flavor"`
	Version   string            `json:"version"`  // "" or "latest" for the newest
	Build     string            `json:"build"`    // a loader version or build (Builds); "" for the recommended one
	Settings  map[string]string `json:"settings"` // server.properties to start with
	RAM       int               `json:"ram_mb"`
	Modpack   string            `json:"modpack"` // a Modrinth modpack's id or slug, instead of flavor and version
	PackVer   string            `json:"modpack_version"`
	AgreeEULA bool              `json:"agree_eula"` // the user ticked the box themselves
}

// Create makes a server and installs its software in a job.
func (m *Manager) Create(o CreateOptions) (*Server, *Job, error) {
	if len(strings.TrimSpace(o.Name)) > 40 {
		return nil, nil, fmt.Errorf("pick a shorter name (40 letters at most)")
	}
	if o.Modpack == "" {
		if o.Flavor == "" {
			o.Flavor = "paper"
		}
		if _, ok := flavorByID(o.Flavor); !ok {
			return nil, nil, fmt.Errorf("MineNest doesn't know a server type called %q", o.Flavor)
		}
	}
	if o.Version == "" {
		o.Version = "latest"
	}
	if o.RAM <= 0 {
		o.RAM = suggestedRAM(o.Flavor, o.Modpack != "")
	}
	port := m.freePort()
	s, id, err := m.newServer(o.Name, o.Flavor, o.Version, o.RAM)
	if err != nil {
		return nil, nil, err
	}
	o.Name = s.Config().Name
	p := s.props()
	p.Set("server-port", fmt.Sprint(port))
	p.Set("motd", o.Name+" (a MineNest server)")
	for k, v := range o.Settings {
		if k != "" && !strings.ContainsAny(k, "=:\n #") && !strings.Contains(v, "\n") {
			p.Set(k, v)
		}
	}
	p.Save()
	if o.AgreeEULA {
		s.AgreeEULA()
	}
	s.setBusy("the install", false)
	job := m.Jobs.start("Setting up "+o.Name, id, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		if o.Modpack != "" {
			return nil, installModpack(ctx, s, o.Modpack, o.PackVer, j)
		}
		inst, err := installFlavor(ctx, s.Root(), o.Flavor, o.Version, o.Build, j)
		if err != nil {
			return nil, err
		}
		s.update(func(c *Config) { c.Installed = inst })
		s.Note("Installed %s %s.", flavorName(inst.Flavor), inst.Minecraft)
		return nil, nil
	})
	return s, job, nil
}

func suggestedRAM(flavor string, modpack bool) int {
	total := totalMemoryMB()
	want := 2048
	if flavor == "forge" || flavor == "neoforge" || modpack {
		want = 4096
	}
	if modpack {
		want = 6144
	}
	if total > 0 && want > total/2 {
		want = max(1024, (total/2)/512*512)
	}
	return want
}

// Delete removes a server and all its files (the GUI and CLI ask the user first).
func (m *Manager) Delete(s *Server) error {
	if s.State() != Stopped {
		return fmt.Errorf("stop the server first")
	}
	if err := s.setBusy("deleting", true); err != nil {
		return err
	}
	if s.Config().Way == "relay" {
		m.relay.serverGone(s)
	}
	m.mu.Lock()
	delete(m.servers, s.ID)
	m.mu.Unlock()
	return os.RemoveAll(s.Dir)
}

// ChangeVersion installs another flavor or version into the same server (its world stays).
func (m *Manager) ChangeVersion(s *Server, flavor, version, build string) (*Job, error) {
	if _, ok := flavorByID(flavor); !ok {
		return nil, fmt.Errorf("MineNest doesn't know a server type called %q", flavor)
	}
	if err := s.setBusy("changing the version", true); err != nil {
		return nil, err
	}
	return m.Jobs.start("Changing "+s.Config().Name+" to "+flavorName(flavor)+" "+version, s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		old := s.Config()
		if old.Installed != nil && exists(filepath.Join(s.Root(), "world")) {
			j.Say("Backing up the world first")
			if _, err := s.backupLocked(ctx, "before changing to "+flavorName(flavor)+" "+version, j); err != nil {
				return nil, fmt.Errorf("the backup before the change failed, so nothing was changed: %v", err)
			}
		}
		inst, err := installFlavor(ctx, s.Root(), flavor, version, build, j)
		if err != nil {
			return nil, err
		}
		s.update(func(c *Config) { c.Flavor, c.Want, c.Installed = flavor, version, inst })
		s.Note("Now %s %s.", flavorName(inst.Flavor), inst.Minecraft)
		if old.Installed != nil && old.Installed.Flavor != flavor {
			if fOld, _ := flavorByID(old.Installed.Flavor); fOld.AddonKind != "" {
				if fNew, _ := flavorByID(flavor); fOld.AddonFolder != fNew.AddonFolder {
					s.Note("The %ss from %s stay in %s but won't load in %s.", fOld.AddonKind, flavorName(old.Installed.Flavor), fOld.AddonFolder, flavorName(flavor))
				}
			}
		}
		return nil, nil
	}), nil
}

// ------------------------------------------------------------------ hooks

func (m *Manager) serverReady(s *Server) {
	cfg := s.Config()
	if w := m.wayFor(s, cfg.Way); w != nil {
		s.mu.Lock()
		s.way = w
		s.mu.Unlock()
		w.start(s)
	}
}

func (m *Manager) serverStopped(s *Server) {
	s.mu.Lock()
	w := s.way
	s.way = nil
	s.mu.Unlock()
	if w != nil {
		w.stop()
	}
}

func (m *Manager) wantsDoor(s *Server) bool {
	cfg := s.Config()
	if cfg.Way != "relay" || !cfg.RealIPs || cfg.Installed == nil {
		return false
	}
	if cfg.Installed.Flavor != "paper" && cfg.Installed.Flavor != "purpur" {
		return false
	}
	if cfg.Bedrock && geyserInstalled(s) {
		return false // Geyser's own connection into Paper hasn't been tried with proxy-protocol on
	}
	return m.relay.proxyAble()
}

// ------------------------------------------------------------------ status

type Addresses struct {
	Here   string `json:"here"`   // Minecraft on this computer
	Home   string `json:"home"`   // others on the same network
	Public string `json:"public"` // anyone, through the way in
}

type ServerStatus struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Flavor         string    `json:"flavor"`
	FlavorName     string    `json:"flavor_name"`
	Minecraft      string    `json:"minecraft"`
	Build          string    `json:"build"`
	Java           int       `json:"java"`
	Installed      bool      `json:"installed"`
	State          string    `json:"state"`
	Busy           string    `json:"busy,omitempty"`
	Uptime         int64     `json:"uptime"`
	Players        []string  `json:"players"`
	MaxPlayers     int       `json:"max_players"`
	Port           int       `json:"port"`
	RAM            int       `json:"ram_mb"`
	EulaNeeded     bool      `json:"eula_needed"`
	Why            string    `json:"why,omitempty"`
	Addresses      Addresses `json:"addresses"`
	Way            WayStatus `json:"way"`
	Bedrock        bool      `json:"bedrock"`
	BedrockOn      bool      `json:"bedrock_on"`
	Autostart      bool      `json:"autostart"`
	AutoRestart    bool      `json:"auto_restart"`
	RealIPs        bool      `json:"real_ips"`
	AddonKind      string    `json:"addon_kind"`
	Modpack        string    `json:"modpack,omitempty"`
	JVMArgs        string    `json:"jvm_args"`
	Folder         string    `json:"folder"`
	LastSeq        int64     `json:"last_seq"`
	WhitelistOn    bool      `json:"whitelist_on"`
	WhitelistCount int       `json:"whitelist_count"`
}

func (m *Manager) Status(s *Server) ServerStatus {
	s.mu.Lock()
	cfg := s.cfg
	st := ServerStatus{ID: s.ID, Name: cfg.Name, Flavor: cfg.Flavor, State: s.state, Busy: s.busy, Why: s.why,
		RAM: cfg.RAM, Bedrock: cfg.Bedrock, Autostart: cfg.Autostart, AutoRestart: cfg.AutoRestart, RealIPs: cfg.RealIPs,
		Modpack: cfg.Modpack, JVMArgs: cfg.JVMArgs, Folder: s.Root(), LastSeq: s.seq}
	if s.state != Stopped {
		st.Uptime = int64(time.Since(s.started).Seconds())
	}
	w := s.way
	s.mu.Unlock()
	st.Players = s.Players()
	if cfg.Installed != nil {
		st.Installed = true
		st.Flavor = cfg.Installed.Flavor
		st.Minecraft = cfg.Installed.Minecraft
		st.Build = cfg.Installed.Build
		st.Java = cfg.Installed.Java
	}
	st.FlavorName = flavorName(st.Flavor)
	if f, ok := flavorByID(st.Flavor); ok {
		st.AddonKind = f.AddonKind
	}
	p := s.props()
	st.Port = s.Port()
	fmt.Sscan(p.GetDefault("max-players", "20"), &st.MaxPlayers)
	st.EulaNeeded = !s.eulaAgreed()
	st.WhitelistOn = p.GetDefault("white-list", "false") == "true"
	st.WhitelistCount = len(s.listFile("whitelist.json"))
	st.Addresses.Here = "localhost"
	if st.Port != DefaultPort {
		st.Addresses.Here = fmt.Sprintf("localhost:%d", st.Port)
	}
	if ip := lanAddress(); ip != "" {
		st.Addresses.Home = ip
		if st.Port != DefaultPort {
			st.Addresses.Home = fmt.Sprintf("%s:%d", ip, st.Port)
		}
	}
	st.BedrockOn = cfg.Bedrock && geyserInstalled(s)
	st.Way = WayStatus{Kind: cfg.Way}
	if w != nil {
		st.Way = w.status()
	} else if cfg.Way != "" {
		st.Way = m.idleWayStatus(s, cfg.Way)
	}
	if st.Way.Up && st.Way.Address != "" {
		st.Addresses.Public = st.Way.Address
	}
	return st
}

// lanAddress is this computer's address on the home network (the one its default route uses).
func lanAddress() string {
	c, err := net.Dial("udp", "192.0.2.1:9") // nothing is sent: it just picks the outgoing address
	if err != nil {
		return ""
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP
	if ip == nil || ip.IsLoopback() {
		return ""
	}
	return ip.String()
}

type Info struct {
	Version  string         `json:"version"`
	OS       string         `json:"os"`
	Arch     string         `json:"arch"`
	MemoryMB int            `json:"memory_mb"`
	Home     string         `json:"home"`
	LAN      string         `json:"lan"`
	Flavors  []Flavor       `json:"flavors"`
	Account  string         `json:"account"`
	Cloud    string         `json:"cloud"`
	Jobs     []jobView      `json:"jobs"`
	Props    []PropertyInfo `json:"common_properties"`
}

func (m *Manager) Info() Info {
	set := m.Settings()
	return Info{Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH, MemoryMB: totalMemoryMB(), Home: Home(),
		LAN: lanAddress(), Flavors: Flavors, Account: set.AccountName, Cloud: set.Cloud, Jobs: m.Jobs.list(), Props: CommonProperties}
}
