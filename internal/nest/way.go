package nest

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// The ways in from the internet, one per server at a time:
//   playit   playit.gg's free tunnel (playit.go)
//   vps      a VPS of the user's own, through an SSH tunnel (this file)
//   relay    the supporter relay: a name of their own for supporters on Patreon (relay.go)
// Each starts when the server is ready and stops with it. Players at home and on this computer
// always join directly.

type WayStatus struct {
	Kind     string `json:"kind"`  // "", playit, vps, relay
	State    string `json:"state"` // off, waiting, connecting, up, retrying, problem
	Up       bool   `json:"up"`
	Address  string `json:"address,omitempty"`
	Note     string `json:"note,omitempty"`
	Check    string `json:"check,omitempty"` // what the check from outside found
	CheckOK  bool   `json:"check_ok"`
	ClaimURL string `json:"claim_url,omitempty"` // playit.gg: the link to approve this computer
	Bedrock  string `json:"bedrock,omitempty"`   // playit.gg's Bedrock address, if any
}

type way interface {
	start(s *Server)
	stop()
	status() WayStatus
	recheck()
}

func (m *Manager) wayFor(s *Server, kind string) way {
	switch kind {
	case "playit":
		return &playitWay{m: m, s: s}
	case "vps":
		cfg := s.Config().VPS.Tunnel
		if cfg.check() != nil {
			return nil
		}
		hp := fmt.Sprintf("%s:%d", cfg.Host, cfg.SSHPort)
		w := &tunnelWay{kind: "vps", s: s}
		w.t = &tunnel{cfg: cfg, hostKey: trustFirstSeen(hp), name: "your VPS", target: func() string { return fmt.Sprintf("127.0.0.1:%d", s.Port()) }}
		w.address = func() string { return joinAddr(cfg.Host, cfg.PublicPort) }
		w.target = func() (string, int) { return cfg.Host, cfg.PublicPort }
		return w
	case "relay":
		return m.relay.wayFor(s)
	}
	return nil
}

func (m *Manager) idleWayStatus(s *Server, kind string) WayStatus {
	switch kind {
	case "playit":
		return m.playit.idleStatus(s)
	case "vps":
		c := s.Config().VPS.Tunnel
		return WayStatus{Kind: "vps", State: "waiting", Address: joinAddr(c.Host, c.PublicPort), Note: "Starts with the server."}
	case "relay":
		return m.relay.idleStatus(s)
	}
	return WayStatus{Kind: kind, State: "off"}
}

func joinAddr(host string, port int) string {
	if host == "" {
		return ""
	}
	if port == DefaultPort || port == 0 {
		return host
	}
	return host + ":" + strconv.Itoa(port)
}

type VPSConfig struct {
	Tunnel TunnelConfig `json:"tunnel"`
}

// SetWay chooses how people on the internet reach the server.
func (m *Manager) SetWay(s *Server, kind string, vps *TunnelConfig) error {
	switch kind {
	case "", "off":
		kind = ""
	case "playit":
	case "vps":
		if vps != nil {
			if vps.SSHPort == 0 {
				vps.SSHPort = 22
			}
			if vps.PublicPort == 0 {
				vps.PublicPort = DefaultPort
			}
			if vps.Bind == "" {
				vps.Bind = "0.0.0.0"
			}
			if err := vps.check(); err != nil {
				return err
			}
			if _, err := tunnelKey(); err != nil {
				return err
			}
			s.update(func(c *Config) { c.VPS.Tunnel = *vps })
		} else if err := s.Config().VPS.Tunnel.check(); err != nil {
			return err
		}
	case "relay":
		if err := m.relay.canUse(s); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown way in %q (off, playit, vps or relay)", kind)
	}
	// Stop the old way, if the server is up, and start the new one.
	s.mu.Lock()
	old := s.way
	s.way = nil
	running := s.state == Running
	s.mu.Unlock()
	if old != nil {
		old.stop()
	}
	if err := s.update(func(c *Config) { c.Way = kind }); err != nil {
		return err
	}
	if kind == "relay" {
		m.relay.syncProxy(s)
	}
	if running && kind != "" {
		m.serverReady(s)
	}
	return nil
}

// ForgetVPSHost trusts a new identity for the VPS the next time it connects.
func (m *Manager) ForgetVPSHost(s *Server) {
	c := s.Config().VPS.Tunnel
	forgetHost(fmt.Sprintf("%s:%d", c.Host, c.SSHPort))
}

// RecheckWay checks again from outside whether players can get in.
func (s *Server) RecheckWay() {
	s.mu.Lock()
	w := s.way
	s.mu.Unlock()
	if w != nil {
		w.recheck()
	}
}

// ------------------------------------------------------------------ tunnel ways

type tunnelWay struct {
	kind    string
	s       *Server
	t       *tunnel
	address func() string
	target  func() (string, int)
	finding func(err error, res *PingResult) string // "" when friends get in

	mu      sync.Mutex
	check   string
	checkOK bool
	note    string
}

func (w *tunnelWay) start(s *Server) {
	w.t.onUp = func() {
		time.Sleep(3 * time.Second)
		w.recheck()
	}
	w.t.start()
	s.Note("Connecting to %s so people on the internet can join.", w.t.name)
}

func (w *tunnelWay) stop() { w.t.stop() }

func (w *tunnelWay) recheck() {
	host, port := w.target()
	addr := joinAddr(host, port)
	res, err := Ping(host, port, 8*time.Second)
	msg := ""
	if w.finding != nil {
		msg = w.finding(err, res)
	} else if err != nil {
		msg = pingProblem(addr, err)
		if w.kind == "vps" {
			msg += " The tunnel is up, so the VPS may only be listening on its own loopback: set GatewayPorts clientspecified in its sshd_config, and open the port in its firewall."
		}
	}
	w.mu.Lock()
	w.check, w.checkOK = msg, msg == ""
	if msg == "" {
		w.check = "Checked from the internet: friends can join at " + addr + "."
	}
	w.mu.Unlock()
	if msg == "" {
		w.s.Note("Checked from the internet: friends can join at %s.", addr)
	} else {
		w.s.Note("The check from the internet found a problem: %s", msg)
	}
}

func (w *tunnelWay) status() WayStatus {
	state, why, tries := w.t.snapshot()
	w.mu.Lock()
	defer w.mu.Unlock()
	st := WayStatus{Kind: w.kind, State: state, Up: state == "up", Address: w.address(), Check: w.check, CheckOK: w.checkOK, Note: w.note}
	switch state {
	case "connecting":
		st.Note = "Connecting to " + w.t.name + "."
		if tries > 1 && why != "" {
			st.Note = why
		}
	case "retrying":
		st.Note = why + " MineNest keeps trying."
		st.State = "problem"
	}
	return st
}
