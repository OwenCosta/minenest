package nest

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Door mode with a real Paper: MINENEST_IT_HOME is a MineNest home with a stopped Paper server
// MINENEST_IT_SERVER that has been started once. Paper then listens on the loopback with
// proxy-protocol on, and the doorway on the players' port lets a plain client in.
func TestDoorWithPaper(t *testing.T) {
	home, id := os.Getenv("MINENEST_IT_HOME"), os.Getenv("MINENEST_IT_SERVER")
	if home == "" || id == "" {
		t.Skip("set MINENEST_IT_HOME and MINENEST_IT_SERVER to try door mode with a real Paper")
	}
	t.Setenv("MINENEST_HOME", home)
	m, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	m.relay.last = map[string]string{"proxy": "on", "proxy_able": "1"}
	s.cfg.Way = "relay"
	s.cfg.RealIPs = true
	if !m.wantsDoor(s) {
		t.Fatal("door mode should be wanted")
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		s.StopAndWait(t.Context())
		paperProxyProtocol(s.Root(), false)
	}()
	for i := 0; i < 120 && s.State() != Running; i++ {
		time.Sleep(500 * time.Millisecond)
	}
	if s.State() != Running {
		for _, l := range s.Lines(0) {
			t.Log(l.Text)
		}
		t.Fatal("Paper didn't start")
	}
	s.mu.Lock()
	d := s.door
	s.mu.Unlock()
	if d == nil {
		t.Fatal("no doorway")
	}
	// Through the doorway: a normal client gets an answer.
	r, err := Ping("127.0.0.1", s.Port(), 5*time.Second)
	if err != nil {
		for _, l := range s.Lines(0) {
			if !strings.Contains(l.Text, "Preparing spawn") {
				t.Log(l.Text)
			}
		}
		t.Fatal("through the doorway: ", err)
	}
	t.Logf("through the doorway: %+v", r)
	// Straight to Paper without a header: it shouldn't answer (it wants PROXY first).
	if _, err := Ping("127.0.0.1", d.paperPort, 3*time.Second); err == nil {
		t.Fatal("Paper answered without a PROXY header: proxy-protocol isn't on")
	}
	// Paper listens only on the loopback.
	if ip := lanAddress(); ip != "" {
		if c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, itoa(d.paperPort)), time.Second); err == nil {
			c.Close()
			t.Fatal("Paper's own port is open beyond the loopback")
		}
	}
	found := false
	for _, l := range s.Lines(0) {
		if strings.Contains(l.Text, "proxy") || strings.Contains(l.Text, "PROXY") {
			found = true
		}
	}
	_ = found
}

func itoa(n int) string { return strconv.Itoa(n) }
