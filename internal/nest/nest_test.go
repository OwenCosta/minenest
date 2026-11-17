package nest

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestVersions(t *testing.T) {
	if !versionLess("1.21.9", "1.21.11") || !versionLess("1.21.11", "26.1") || versionLess("26.1", "1.21.11") {
		t.Fatal("versionLess")
	}
	if !isRelease("26.3") || isRelease("1.21.11-rc3") || isRelease("26.4-snapshot-10") {
		t.Fatal("isRelease")
	}
	for v, want := range map[string]string{"21.1.77": "1.21.1", "21.0.5-beta": "1.21", "20.4.237": "1.20.4", "26.3.0.110": "26.3", "26.1.2.4": "26.1.2"} {
		if got := neoMinecraft(v); got != want {
			t.Errorf("neoMinecraft(%s) = %s, want %s", v, got, want)
		}
	}
	if got := sortNewestFirst([]string{"1.20.1", "26.1", "1.21.11", "1.21.4"}); strings.Join(got, ",") != "26.1,1.21.11,1.21.4,1.20.1" {
		t.Fatal(got)
	}
	if !paperTakesProxy("1.19.4") || paperTakesProxy("1.19.3") || !paperTakesProxy("26.1") {
		t.Fatal("paperTakesProxy")
	}
}

func TestSafeNameAndWithin(t *testing.T) {
	if safeName("  My Cool Server!! ") != "my-cool-server" || safeName("../..") != "" {
		t.Fatal(safeName("  My Cool Server!! "))
	}
	d := t.TempDir()
	if within(d, filepath.Join(d, "..", "x")) || !within(d, filepath.Join(d, "a", "b")) {
		t.Fatal("within")
	}
}

func TestProperties(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.properties")
	os.WriteFile(path, []byte("#Minecraft server properties\r\nmotd=Hello\\: there\r\nserver-port=25565\r\n"), 0o644)
	p := loadProperties(path)
	if v, _ := p.Get("motd"); v != "Hello: there" {
		t.Fatalf("motd %q", v)
	}
	p.Set("server-port", "25570")
	p.Set("difficulty", "hard")
	p.Set("motd", "a=b")
	p.Save()
	b, _ := os.ReadFile(path)
	s := string(b)
	if !strings.HasPrefix(s, "#Minecraft server properties\n") || !strings.Contains(s, "server-port=25570\n") || !strings.Contains(s, "difficulty=hard\n") || !strings.Contains(s, `motd=a\=b`) {
		t.Fatal(s)
	}
}

func TestOfflineUUID(t *testing.T) {
	// The UUID Minecraft gives "Notch" in offline mode.
	if got := offlineUUID("Notch"); got != "b50ad385-829d-3141-a216-7e7d7539ba7f" {
		t.Fatal(got)
	}
}

func TestPaperProxyProtocol(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config", "paper-global.yml")
	// No file: on writes a small one; off leaves nothing.
	if err := paperProxyProtocol(root, false); err != nil || exists(cfg) {
		t.Fatal("off with no file")
	}
	paperProxyProtocol(root, true)
	if b, _ := os.ReadFile(cfg); string(b) != "proxies:\n  proxy-protocol: true\n" {
		t.Fatalf("%q", b)
	}
	// A real-looking file: only the one line changes, velocity's deeper one is left alone.
	os.WriteFile(cfg, []byte("_version: 29\nproxies:\n  bungee-cord:\n    online-mode: true\n  proxy-protocol: false\n  velocity:\n    enabled: false\n    proxy-protocol: false\nscoreboards:\n  x: 1\n"), 0o644)
	paperProxyProtocol(root, true)
	b, _ := os.ReadFile(cfg)
	want := "_version: 29\nproxies:\n  bungee-cord:\n    online-mode: true\n  proxy-protocol: true\n  velocity:\n    enabled: false\n    proxy-protocol: false\nscoreboards:\n  x: 1\n"
	if string(b) != want {
		t.Fatalf("got\n%s", b)
	}
	// A block without the key gets it.
	os.WriteFile(cfg, []byte("proxies:\n    velocity:\n      enabled: false\nother: 1\n"), 0o644)
	paperProxyProtocol(root, true)
	b, _ = os.ReadFile(cfg)
	if string(b) != "proxies:\n    proxy-protocol: true\n    velocity:\n      enabled: false\nother: 1\n" {
		t.Fatalf("got\n%s", b)
	}
}

func TestProxyV2Header(t *testing.T) {
	src := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 51000}
	dst := &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 25565}
	h := proxyV2Header(src, dst)
	if string(h[:12]) != string(proxyV2Sig) || h[12] != 0x21 || h[13] != 0x11 || binary.BigEndian.Uint16(h[14:16]) != 12 {
		t.Fatalf("% x", h)
	}
	if !net.IP(h[16:20]).Equal(src.IP) || !net.IP(h[20:24]).Equal(dst.IP) || binary.BigEndian.Uint16(h[24:26]) != 51000 || binary.BigEndian.Uint16(h[26:28]) != 25565 {
		t.Fatalf("% x", h)
	}
	h6 := proxyV2Header(&net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1}, &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 2})
	if h6[13] != 0x21 || binary.BigEndian.Uint16(h6[14:16]) != 36 || len(h6) != 16+36 {
		t.Fatalf("% x", h6)
	}
}

// The doorway passes connections on with a PROXY header naming the player.
func TestDoor(t *testing.T) {
	paper, _ := net.Listen("tcp", "127.0.0.1:0")
	defer paper.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := paper.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 28+5)
		io.ReadFull(c, buf)
		got <- buf
		c.Write([]byte("pong"))
	}()
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	d := &door{playersPort: port, paperPort: paper.Addr().(*net.TCPAddr).Port}
	if err := d.start(); err != nil {
		t.Fatal(err)
	}
	defer d.stop()
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("hello"))
	b := <-got
	if string(b[:12]) != string(proxyV2Sig) || !net.IP(b[16:20]).Equal(net.IPv4(127, 0, 0, 1)) || string(b[28:]) != "hello" {
		t.Fatalf("% x", b)
	}
	reply := make([]byte, 4)
	io.ReadFull(c, reply)
	if string(reply) != "pong" {
		t.Fatal(string(reply))
	}
	c.Close()
}

// ------------------------------------------------------------------ the tunnel, against a tiny sshd

type fakeSSHD struct {
	addr      string
	hostKey   ssh.Signer
	listening chan string // where it opened the forwarded port
	allowed   ssh.PublicKey
}

func startFakeSSHD(t *testing.T, allowed ssh.PublicKey) *fakeSSHD {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	f := &fakeSSHD{hostKey: signer, listening: make(chan string, 4), allowed: allowed}
	conf := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() == "hostmc" && string(k.Marshal()) == string(f.allowed.Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("no")
	}}
	conf.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	f.addr = l.Addr().String()
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(nc, conf)
		}
	}()
	return f
}

func (f *fakeSSHD) serve(nc net.Conn, conf *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, conf)
	if err != nil {
		return
	}
	defer conn.Close()
	go func() {
		for ch := range chans {
			ch.Reject(ssh.Prohibited, "no")
		}
	}()
	var mu sync.Mutex
	var fwd net.Listener
	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			var p struct {
				Addr string
				Port uint32
			}
			ssh.Unmarshal(req.Payload, &p)
			l, err := net.Listen("tcp", "127.0.0.1:0") // stands in for the public port
			if err != nil {
				req.Reply(false, nil)
				continue
			}
			mu.Lock()
			fwd = l
			mu.Unlock()
			req.Reply(true, ssh.Marshal(struct{ Port uint32 }{p.Port}))
			f.listening <- l.Addr().String()
			go func(bindAddr string, bindPort uint32) {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					ra := c.RemoteAddr().(*net.TCPAddr)
					payload := ssh.Marshal(struct {
						Addr       string
						Port       uint32
						OriginAddr string
						OriginPort uint32
					}{bindAddr, bindPort, ra.IP.String(), uint32(ra.Port)})
					ch, reqs, err := conn.OpenChannel("forwarded-tcpip", payload)
					if err != nil {
						c.Close()
						continue
					}
					go ssh.DiscardRequests(reqs)
					go func() { io.Copy(ch, c); ch.CloseWrite() }()
					go func() { io.Copy(c, ch); c.Close() }()
				}
			}(p.Addr, p.Port)
		case "keepalive@openssh.com":
			req.Reply(true, nil)
		default:
			req.Reply(false, nil)
		}
	}
	mu.Lock()
	if fwd != nil {
		fwd.Close()
	}
	mu.Unlock()
}

func TestTunnelPinnedKey(t *testing.T) {
	t.Setenv("MINENEST_HOME", t.TempDir())
	signer, err := tunnelKey()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := tunnelKey(); string(again.PublicKey().Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Fatal("the key should be kept")
	}
	sshd := startFakeSSHD(t, signer.PublicKey())
	// The "Minecraft server" here.
	mc, _ := net.Listen("tcp", "127.0.0.1:0")
	defer mc.Close()
	go func() {
		for {
			c, err := mc.Accept()
			if err != nil {
				return
			}
			go func() { r := bufio.NewReader(c); l, _ := r.ReadString('\n'); c.Write([]byte("echo " + l)); c.Close() }()
		}
	}()
	host, portS, _ := net.SplitHostPort(sshd.addr)
	var sshPort int
	fmt.Sscan(portS, &sshPort)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshd.hostKey.PublicKey())))
	hk, err := pinnedHostKey(line)
	if err != nil {
		t.Fatal(err)
	}
	up := make(chan struct{}, 1)
	tn := &tunnel{cfg: TunnelConfig{Host: host, User: "hostmc", SSHPort: sshPort, Bind: "127.0.0.1", PublicPort: 30001}, hostKey: hk, name: "the relay",
		target: func() string { return mc.Addr().String() }, onUp: func() { up <- struct{}{} }}
	tn.start()
	defer tn.stop()
	var public string
	select {
	case public = <-sshd.listening:
	case <-time.After(10 * time.Second):
		_, why, _ := tn.snapshot()
		t.Fatal("no forward: " + why)
	}
	<-up
	c, err := net.Dial("tcp", public)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("hi\n"))
	b, _ := io.ReadAll(c)
	if string(b) != "echo hi\n" {
		t.Fatalf("%q", b)
	}
	if st, _, _ := tn.snapshot(); st != "up" {
		t.Fatal(st)
	}
	tn.stop()

	// A different host key: it refuses to log in.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	os, _ := ssh.NewSignerFromKey(other)
	wrong, _ := pinnedHostKey(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(os.PublicKey()))))
	t2 := &tunnel{cfg: tn.cfg, hostKey: wrong, name: "the relay", target: tn.target}
	err = t2.once(make(chan struct{}))
	if err == nil || !strings.Contains(err.Error(), "the relay") {
		t.Fatal(err)
	}
}

func TestTrustFirstSeen(t *testing.T) {
	t.Setenv("MINENEST_HOME", t.TempDir())
	_, a, _ := ed25519.GenerateKey(rand.Reader)
	_, b, _ := ed25519.GenerateKey(rand.Reader)
	ka, _ := ssh.NewSignerFromKey(a)
	kb, _ := ssh.NewSignerFromKey(b)
	cb := trustFirstSeen("vps:22")
	if cb("", nil, ka.PublicKey()) != nil || cb("", nil, ka.PublicKey()) != nil {
		t.Fatal("first and same key should pass")
	}
	if cb("", nil, kb.PublicKey()) == nil {
		t.Fatal("a changed key must fail")
	}
	forgetHost("vps:22")
	if cb("", nil, kb.PublicKey()) != nil {
		t.Fatal("after forgetting, the new key is trusted")
	}
}

// XenithOS Cloud's answers: the account and the relay, from a stand-in.
func TestCloudRelay(t *testing.T) {
	t.Setenv("MINENEST_HOME", t.TempDir())
	var lastForm map[string]string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		lastForm = map[string]string{}
		for k := range r.PostForm {
			lastForm[k] = r.PostForm.Get(k)
		}
		switch r.URL.Path {
		case "/v1/accounts/login":
			if r.PostForm.Get("pin") != "1234" {
				w.WriteHeader(401)
				io.WriteString(w, "error=wrong\n")
				return
			}
			io.WriteString(w, "name=Owen\ntoken=tok123\n")
		case "/v1/relay/claim":
			if r.PostForm.Get("token") != "tok123" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, "relay=on\nstep=ok\nname="+r.PostForm.Get("name")+"\nstate=on\ndomain=mc.example\naddress="+r.PostForm.Get("name")+".mc.example:25566\nplayers_port=25566\nport=30007\nssh_host=relay.example\nssh_port=2222\nssh_user=hostmc\nhostkey=ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\nproxy=off\nproxy_able=1\n")
		default:
			w.WriteHeader(404)
		}
	}))
	defer cloud.Close()
	t.Setenv("MINENEST_CLOUD", cloud.URL)
	m, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Login(t.Context(), "owen", "9999", false); err == nil || !strings.Contains(err.Error(), "don't match") {
		t.Fatal(err)
	}
	a, err := m.Login(t.Context(), "owen", "1234", false)
	if err != nil || a.Name != "Owen" || m.Settings().AccountToken != "tok123" {
		t.Fatal(a, err)
	}
	ri, err := m.RelayAsk(t.Context(), "claim", "Brave-Otter")
	if err != nil {
		t.Fatal(err)
	}
	if lastForm["name"] != "brave-otter" || !strings.HasPrefix(lastForm["key"], "ssh-ed25519 ") || len(strings.Fields(lastForm["key"])) != 2 || lastForm["proxy"] != "0" {
		t.Fatal(lastForm)
	}
	if ri.Name != "brave-otter" || ri.Address != "brave-otter.mc.example:25566" || !ri.ProxyAble || ri.Proxy {
		t.Fatal(ri)
	}
	// What it answered is kept, and a server can use the relay with it.
	m2, _ := NewManager()
	s := &Server{ID: "x", Dir: t.TempDir(), m: m2, state: Stopped}
	s.cfg.Installed = &Installed{Flavor: "paper", Minecraft: "26.3"}
	if err := m2.relay.canUse(s); err != nil {
		t.Fatal(err)
	}
	w := m2.relay.wayFor(s).(*tunnelWay)
	if w.t.cfg.Host != "relay.example" || w.t.cfg.SSHPort != 2222 || w.t.cfg.PublicPort != 30007 || w.t.cfg.Bind != "127.0.0.1" {
		t.Fatal(w.t.cfg)
	}
	if h, p := w.target(); h != "brave-otter.mc.example" || p != 25566 {
		t.Fatal(h, p)
	}
}

// A real server, when one is running here (MINENEST_PING=host:port).
func TestPingLive(t *testing.T) {
	addr := os.Getenv("MINENEST_PING")
	if addr == "" {
		t.Skip("set MINENEST_PING=host:port to ping a running server")
	}
	h, p := splitHostPort(addr, DefaultPort)
	r, err := Ping(h, p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", r)
}

// Signing in with an OCAccount: start, the browser part (the stand-in's "approve"), then poll.
func TestOCSignIn(t *testing.T) {
	t.Setenv("MINENEST_HOME", t.TempDir())
	approved := false
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.URL.Path {
		case "/v1/oc/start":
			io.WriteString(w, "session=s1\nkey=k1\nurl=https://owencosta.diy/account/?client_id=mn&state=s1\n")
		case "/v1/oc/poll":
			if r.PostForm.Get("session") != "s1" || r.PostForm.Get("key") != "k1" {
				w.WriteHeader(404)
				io.WriteString(w, "error=That sign-in isn't here.\n")
				return
			}
			if !approved {
				io.WriteString(w, "state=waiting\n")
				return
			}
			io.WriteString(w, "state=done\nname=Owen\ntoken=tok9\naccount=OwenOC\n")
		default:
			w.WriteHeader(404)
		}
	}))
	defer cloud.Close()
	t.Setenv("MINENEST_CLOUD", cloud.URL)
	m, _ := NewManager()
	if st := m.OCPoll(t.Context()); st.State != "none" {
		t.Fatal(st)
	}
	u, err := m.OCStart(t.Context())
	if err != nil || !strings.Contains(u, "owencosta.diy/account/") {
		t.Fatal(u, err)
	}
	if st := m.OCPoll(t.Context()); st.State != "waiting" {
		t.Fatal(st)
	}
	approved = true
	st := m.OCPoll(t.Context())
	if st.State != "done" || st.Name != "Owen" || st.Account != "OwenOC" {
		t.Fatal(st)
	}
	set := m.Settings()
	if set.AccountToken != "tok9" || set.AccountName != "Owen" || set.OCAccount != "OwenOC" {
		t.Fatal(set)
	}
	if ri := m.RelayInfo(); !ri.SignedIn || ri.OCAccount != "OwenOC" {
		t.Fatal(ri)
	}
	if st := m.OCPoll(t.Context()); st.State != "none" {
		t.Fatal("finished sign-ins are forgotten", st)
	}
}
