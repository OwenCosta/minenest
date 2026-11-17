package nest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server states.
const (
	Stopped  = "stopped"
	Starting = "starting"
	Running  = "running"
	Stopping = "stopping"
)

// Config is what MineNest keeps about a server (servers/<id>/nest.json). The server's own files
// are in servers/<id>/server.
type Config struct {
	Name        string     `json:"name"`
	Flavor      string     `json:"flavor"`
	Want        string     `json:"want"` // "latest" or a Minecraft version
	Installed   *Installed `json:"installed,omitempty"`
	RAM         int        `json:"ram_mb"`
	JVMArgs     string     `json:"jvm_args,omitempty"`
	Eula        string     `json:"eula,omitempty"` // when the user agreed to Minecraft's EULA in MineNest
	Autostart   bool       `json:"autostart"`
	AutoRestart bool       `json:"auto_restart"`
	Way         string     `json:"way"` // "", "playit", "vps" or "relay": the way in from the internet
	VPS         VPSConfig  `json:"vps"`
	Bedrock     bool       `json:"bedrock"`
	RealIPs     bool       `json:"real_ips"` // players' real addresses through the relay (Paper)
	Modpack     string     `json:"modpack,omitempty"`
	Created     time.Time  `json:"created"`
}

type Line struct {
	Seq  int64  `json:"seq"`
	Time int64  `json:"t"`
	Text string `json:"text"`
	Kind string `json:"kind"` // info, warn, error, join, chat, sent, nest
}

const keepLines = 2000

type Server struct {
	ID  string
	Dir string // servers/<id>
	m   *Manager

	mu       sync.Mutex
	cfg      Config
	state    string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	started  time.Time
	lines    []Line
	seq      int64
	players  map[string]time.Time
	why      string // why it stopped by itself last time
	stopAsk  bool
	restarts []time.Time
	saved    chan struct{}
	busy     string // a job that has the server's files (install, restore...)
	door     *door
	way      way
	exited   chan struct{}
}

func (s *Server) Root() string { return filepath.Join(s.Dir, "server") }

func (s *Server) load() error {
	if err := readJSON(filepath.Join(s.Dir, "nest.json"), &s.cfg); err != nil {
		return err
	}
	if s.cfg.Name == "" {
		s.cfg.Name = s.ID
	}
	return nil
}

func (s *Server) saveLocked() error {
	return writeJSON(filepath.Join(s.Dir, "nest.json"), s.cfg, 0o644)
}

func (s *Server) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Server) update(f func(c *Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.cfg)
	return s.saveLocked()
}

func (s *Server) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Server) props() *Properties {
	return loadProperties(filepath.Join(s.Root(), "server.properties"))
}

// Port is the port players join at (server.properties).
func (s *Server) Port() int {
	s.mu.Lock()
	d := s.door
	s.mu.Unlock()
	if d != nil {
		return d.playersPort // Paper has its own port on the loopback meanwhile
	}
	p, _ := strconv.Atoi(s.props().GetDefault("server-port", "25565"))
	if p <= 0 || p > 65535 {
		p = DefaultPort
	}
	return p
}

func (s *Server) doorLocked() *door {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.door
}

// ------------------------------------------------------------------ console

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// Newer servers put "System chat: " (or "Chat: ") before what they broadcast.
	joinRe  = regexp.MustCompile(`\]: (?:System chat: )?(?:\[[^\]]*\] )?([A-Za-z0-9_.]{1,17}) joined the game`)
	leftRe  = regexp.MustCompile(`\]: (?:System chat: )?(?:\[[^\]]*\] )?([A-Za-z0-9_.]{1,17}) left the game`)
	chatRe  = regexp.MustCompile(`\]: (?:Chat: )?(?:\[Not Secure\] )?<[^>]+> `)
	doneRe  = regexp.MustCompile(`\]: Done \([0-9.,]+s\)!`)
	savedRe = regexp.MustCompile(`\]: (?:System chat: )?(?:Saved the game|Saved the world)`)
	levelRe = regexp.MustCompile(`(?i)[/ ](WARN|ERROR|FATAL)\]`)
)

func (s *Server) addLineLocked(text, kind string) {
	s.seq++
	s.lines = append(s.lines, Line{s.seq, time.Now().Unix(), text, kind})
	if len(s.lines) > keepLines {
		s.lines = append([]Line(nil), s.lines[len(s.lines)-keepLines:]...)
	}
}

// Note adds a line from MineNest itself to the console.
func (s *Server) Note(format string, a ...any) {
	s.mu.Lock()
	s.addLineLocked(fmt.Sprintf(format, a...), "nest")
	s.mu.Unlock()
}

func (s *Server) Lines(since int64) []Line {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Line{}
	for _, l := range s.lines {
		if l.Seq > since {
			out = append(out, l)
		}
	}
	return out
}

func (s *Server) Players() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.players))
	for p := range s.players {
		out = append(out, p)
	}
	sortStrings(out)
	return out
}

func (s *Server) read(r io.Reader, cmd *exec.Cmd) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		text := strings.TrimRight(ansiRe.ReplaceAllString(sc.Text(), ""), "\r ")
		if text == "" || strings.HasPrefix(text, ">") && strings.TrimSpace(text) == ">" {
			continue
		}
		kind := "info"
		if m := levelRe.FindStringSubmatch(text); m != nil {
			kind = "warn"
			if m[1] != "WARN" {
				kind = "error"
			}
		}
		s.mu.Lock()
		if s.cmd != cmd {
			s.mu.Unlock()
			return
		}
		if m := joinRe.FindStringSubmatch(text); m != nil {
			s.players[m[1]] = time.Now()
			kind = "join"
		} else if m := leftRe.FindStringSubmatch(text); m != nil {
			delete(s.players, m[1])
			kind = "join"
		} else if chatRe.MatchString(text) {
			kind = "chat"
		}
		if savedRe.MatchString(text) && s.saved != nil {
			close(s.saved)
			s.saved = nil
		}
		becameReady := s.state == Starting && doneRe.MatchString(text)
		if becameReady {
			s.state = Running
		}
		s.addLineLocked(text, kind)
		s.mu.Unlock()
		if becameReady {
			if d := s.doorLocked(); d != nil {
				d.restoreProperties()
			}
			s.Note("The server is ready. Players can join now.")
			go s.m.serverReady(s)
		}
	}
}

// ------------------------------------------------------------------ start and stop

func (s *Server) eulaPath() string { return filepath.Join(s.Root(), "eula.txt") }

// AgreeEULA records that the user agreed to Minecraft's EULA in MineNest (never on their behalf).
func (s *Server) AgreeEULA() error {
	if err := writeFile(s.eulaPath(), []byte("# Agreed to in MineNest by the server's owner: "+EulaURL+"\neula=true\n"), 0o644); err != nil {
		return err
	}
	return s.update(func(c *Config) { c.Eula = time.Now().UTC().Format(time.RFC3339) })
}

func (s *Server) eulaAgreed() bool {
	b, err := os.ReadFile(s.eulaPath())
	return err == nil && strings.Contains(strings.ReplaceAll(string(b), " ", ""), "eula=true")
}

var errNeedsEULA = errors.New("agree to Minecraft's EULA first (" + EulaURL + ")")

// Start starts the server. It returns once Java has started; "running" comes when the server
// says it's done loading.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.state != Stopped {
		st := s.state
		s.mu.Unlock()
		return fmt.Errorf("the server is already %s", st)
	}
	if s.busy != "" {
		b := s.busy
		s.mu.Unlock()
		return fmt.Errorf("wait for %s to finish first", b)
	}
	cfg := s.cfg
	s.mu.Unlock()
	if cfg.Installed == nil {
		return fmt.Errorf("the server software isn't installed yet")
	}
	if !s.eulaAgreed() {
		return errNeedsEULA
	}
	java := javaPath(cfg.Installed.Java)
	if java == "" {
		return fmt.Errorf("Java %d isn't downloaded yet: choose Repair on the Version page", cfg.Installed.Java)
	}
	recoverDoor(s)
	port := s.Port()
	if other := s.m.portTaken(s, port); other != "" {
		return fmt.Errorf("port %d is used by %s, which is running. Change this server's port in Settings", port, other)
	}
	if !portFree(port) {
		return fmt.Errorf("something else on this computer is using port %d. Close it, or change the port in Settings", port)
	}

	args := []string{fmt.Sprintf("-Xmx%dM", cfg.RAM), fmt.Sprintf("-Xms%dM", min(cfg.RAM, 1024))}
	if cfg.JVMArgs != "" {
		args = append(args, strings.Fields(cfg.JVMArgs)...)
	}
	args = append(args, cfg.Installed.Launch...)

	// Players' real addresses through the relay: Paper listens on a port of its own on the
	// loopback with proxy-protocol on, and a doorway on the players' port adds the header for
	// everyone else (door.go).
	var d *door
	if cfg.Way == "relay" {
		s.m.relay.syncProxy(s)
	}
	if s.m.wantsDoor(s) {
		var err error
		if d, err = prepareDoor(s, port); err != nil {
			s.Note("Players' real addresses are off for this start: %v", err)
			d = nil
		} else {
			args = append(args, "--host", "127.0.0.1", "--port", strconv.Itoa(d.paperPort))
		}
	} else {
		paperProxyProtocol(s.Root(), false)
	}
	args = append(args, "nogui")

	cmd := exec.Command(java, args...)
	cmd.Dir = s.Root()
	prepareChild(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("Java didn't start: %v", err)
	}
	adoptChild(cmd)

	s.mu.Lock()
	s.cmd, s.stdin, s.state, s.started = cmd, stdin, Starting, time.Now()
	s.players = map[string]time.Time{}
	s.why, s.stopAsk, s.door = "", false, d
	s.exited = make(chan struct{})
	exited := s.exited
	s.addLineLocked(fmt.Sprintf("Starting %s %s with %d MB of memory.", flavorName(cfg.Flavor), cfg.Installed.Minecraft, cfg.RAM), "nest")
	s.mu.Unlock()

	if d != nil {
		if err := d.start(); err != nil {
			s.Note("The doorway for players' real addresses couldn't start: %v", err)
		}
	}
	go s.read(pr, cmd)
	go func() {
		err := cmd.Wait()
		pw.Close()
		s.exitedWith(cmd, err)
		close(exited)
	}()
	return nil
}

func (s *Server) exitedWith(cmd *exec.Cmd, err error) {
	s.mu.Lock()
	if s.cmd != cmd {
		s.mu.Unlock()
		return
	}
	asked := s.stopAsk
	wasRunning := s.state == Running
	s.state = Stopped
	s.cmd, s.stdin = nil, nil
	s.players = map[string]time.Time{}
	d := s.door
	s.door = nil
	why := ""
	if !asked {
		why = s.crashReasonLocked(err)
		s.why = why
	}
	if asked {
		s.addLineLocked("The server has stopped.", "nest")
	} else {
		s.addLineLocked("The server stopped by itself: "+why, "error")
	}
	restart := !asked && s.cfg.AutoRestart && wasRunning && s.restartAllowedLocked()
	s.mu.Unlock()
	if d != nil {
		d.stop()
		d.restoreProperties()
	}
	s.m.serverStopped(s)
	if restart {
		s.Note("Starting it again (automatic restart is on).")
		time.Sleep(3 * time.Second)
		if err := s.Start(); err != nil {
			s.Note("It couldn't start again: %v", err)
		}
	}
}

func (s *Server) restartAllowedLocked() bool {
	now := time.Now()
	recent := s.restarts[:0]
	for _, t := range s.restarts {
		if now.Sub(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	s.restarts = recent
	if len(recent) >= 3 {
		s.addLineLocked("It crashed three times in ten minutes, so MineNest won't start it again by itself.", "nest")
		return false
	}
	s.restarts = append(s.restarts, now)
	return true
}

// crashReasonLocked turns the console's last words into a reason a person can act on.
func (s *Server) crashReasonLocked(err error) string {
	tail := ""
	for i := max(0, len(s.lines)-60); i < len(s.lines); i++ {
		tail += s.lines[i].Text + "\n"
	}
	switch {
	case strings.Contains(tail, "FAILED TO BIND TO PORT") || strings.Contains(tail, "Address already in use"):
		return "its port is in use by another program. Change the port in Settings, or close the other server."
	case strings.Contains(tail, "UnsupportedClassVersionError"):
		return "a mod or plugin needs a newer Java than this Minecraft version uses. Remove it, or update it to one made for this version."
	case strings.Contains(tail, "OutOfMemoryError"):
		return "it ran out of memory. Give it more in Settings, or remove some mods."
	case strings.Contains(tail, "You need to agree to the EULA"):
		return "Minecraft's EULA hasn't been agreed to."
	case strings.Contains(tail, "Missing or unsupported mandatory dependencies") || strings.Contains(tail, "requires") && strings.Contains(tail, "which is missing"):
		return "a mod needs another mod that isn't installed. The console above names it; install it on the Mods page."
	case strings.Contains(tail, "Incompatible mods found") || strings.Contains(tail, "Mod resolution failed"):
		return "some mods don't work together or with this version. The console above says which."
	case strings.Contains(tail, "Could not reserve enough space") || strings.Contains(tail, "Invalid maximum heap size"):
		return "Java couldn't get the memory it was given. Lower the memory in Settings."
	}
	if err != nil {
		return "it exited (" + err.Error() + "). The console above shows what it said last."
	}
	return "it exited. The console above shows what it said last."
}

// Command sends a line to the server's console.
func (s *Server) Command(line string) error {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/"))
	if line == "" {
		return nil
	}
	if strings.ContainsAny(line, "\r\n") {
		return fmt.Errorf("one command at a time")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin == nil || (s.state != Running && s.state != Starting) {
		return fmt.Errorf("the server isn't running")
	}
	s.addLineLocked("> "+line, "sent")
	_, err := io.WriteString(s.stdin, line+"\n")
	return err
}

// Stop asks the server to save and stop, and ends it if it hasn't within two minutes.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.state == Stopped {
		s.mu.Unlock()
		return nil
	}
	if s.state == Stopping {
		s.mu.Unlock()
		return nil
	}
	s.state, s.stopAsk = Stopping, true
	stdin, cmd, exited := s.stdin, s.cmd, s.exited
	s.addLineLocked("Stopping the server (saving the world first).", "nest")
	s.mu.Unlock()
	if stdin != nil {
		io.WriteString(stdin, "stop\n")
	}
	go func() {
		select {
		case <-exited:
		case <-time.After(2 * time.Minute):
			s.Note("It didn't stop within two minutes, so MineNest ended it.")
			if cmd != nil && cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
	}()
	return nil
}

// Kill ends the server at once, without saving.
func (s *Server) Kill() {
	s.mu.Lock()
	cmd := s.cmd
	if cmd != nil {
		s.stopAsk = true
		s.state = Stopping
		s.addLineLocked("Ending the server without saving.", "nest")
	}
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

// StopAndWait stops the server and waits for it (for restarts, backups, updates).
func (s *Server) StopAndWait(ctx context.Context) error {
	s.mu.Lock()
	exited := s.exited
	running := s.state != Stopped
	s.mu.Unlock()
	if !running {
		return nil
	}
	s.Stop()
	select {
	case <-exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// saveAll asks a running server to write the world to disk and waits for it to say it has.
func (s *Server) saveAll(ctx context.Context, flush bool) error {
	ch := make(chan struct{})
	s.mu.Lock()
	s.saved = ch
	s.mu.Unlock()
	cmd := "save-all"
	if flush {
		cmd = "save-all flush"
	}
	if err := s.Command(cmd); err != nil {
		return err
	}
	select {
	case <-ch:
		return nil
	case <-time.After(60 * time.Second):
		return fmt.Errorf("the server didn't say it had saved within a minute")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// setBusy marks the server's files as in use by a job ("" to clear). It fails while it runs.
func (s *Server) setBusy(what string, needStopped bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if what != "" {
		if s.busy != "" {
			return fmt.Errorf("wait for %s to finish first", s.busy)
		}
		if needStopped && s.state != Stopped {
			return fmt.Errorf("stop the server first")
		}
	}
	s.busy = what
	return nil
}

func portFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && strings.ToLower(v[j]) < strings.ToLower(v[j-1]); j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
