package nest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// An SSH reverse tunnel: MineNest logs in to a server on the internet and has it listen on a
// public port, passing every connection back down the tunnel to the Minecraft server here. The
// connection goes outward, so nothing changes on the home router, and the other end needs only
// sshd. MineNest logs in with a key of its own (ed25519, in tunnel/key), never a password.
//
// The player's own VPS: its host key is trusted the first time it's seen and checked after.
// The supporter relay: its host key comes from XenithOS Cloud and is the only one trusted.

type TunnelConfig struct {
	Host       string `json:"host"`
	User       string `json:"user"`
	SSHPort    int    `json:"ssh_port"`
	Bind       string `json:"bind"`        // where it listens there: "0.0.0.0", or "127.0.0.1" for the relay
	PublicPort int    `json:"public_port"` // what players connect to there
}

func (c TunnelConfig) check() error {
	if c.Host == "" || strings.HasPrefix(c.Host, "-") || len(c.Host) > 120 {
		return errors.New("type the VPS's address: its IP address or its name")
	}
	for _, r := range c.Host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == ':') {
			return errors.New("the address can only have letters, numbers, dots, dashes and colons")
		}
	}
	if c.User == "" || strings.ContainsAny(c.User, " @:/\\") {
		return errors.New("type the user name MineNest logs in to the VPS as")
	}
	if c.SSHPort <= 0 || c.SSHPort > 65535 || c.PublicPort <= 0 || c.PublicPort > 65535 {
		return errors.New("the ports have to be between 1 and 65535")
	}
	return nil
}

// tunnelKey is MineNest's own SSH key, made the first time it's needed.
func tunnelKey() (ssh.Signer, error) {
	path := homePath("tunnel", "key")
	if b, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(b)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "MineNest")
	if err != nil {
		return nil, err
	}
	if err := writeFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// PublicKey is the line to put in authorized_keys on a VPS.
func PublicKey() (string, error) {
	s, err := tunnelKey()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + " minenest", nil
}

// trustFirstSeen is the VPS's host key check: kept in tunnel/known_hosts the first time, the same
// key required after that.
func trustFirstSeen(hostport string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		path := homePath("tunnel", "known_hosts")
		line := hostport + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		b, _ := os.ReadFile(path)
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) >= 3 && f[0] == hostport {
				if f[0]+" "+f[1]+" "+f[2] == line {
					return nil
				}
				return fmt.Errorf("the VPS's identity has changed since MineNest last connected. If you reinstalled it, choose Trust its new identity")
			}
		}
		return writeFile(path, append(b, []byte(line+"\n")...), 0o600)
	}
}

func forgetHost(hostport string) {
	path := homePath("tunnel", "known_hosts")
	b, _ := os.ReadFile(path)
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 1 && f[0] != hostport {
			keep = append(keep, l)
		}
	}
	writeFile(path, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
}

func pinnedHostKey(line string) (ssh.HostKeyCallback, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, fmt.Errorf("the relay's identity from XenithOS Cloud couldn't be read")
	}
	return ssh.FixedHostKey(key), nil
}

type tunnel struct {
	cfg     TunnelConfig
	hostKey ssh.HostKeyCallback
	target  func() string // where connections go here, "127.0.0.1:25565"
	name    string

	mu      sync.Mutex
	state   string // connecting, up, retrying, stopped
	why     string
	tries   int
	upSince time.Time
	stopCh  chan struct{}
	client  *ssh.Client
	onUp    func()
}

func (t *tunnel) start() {
	t.mu.Lock()
	if t.stopCh != nil {
		t.mu.Unlock()
		return
	}
	t.stopCh = make(chan struct{})
	t.state = "connecting"
	stop := t.stopCh
	t.mu.Unlock()
	go t.loop(stop)
}

func (t *tunnel) stop() {
	t.mu.Lock()
	if t.stopCh != nil {
		close(t.stopCh)
		t.stopCh = nil
	}
	if t.client != nil {
		t.client.Close()
	}
	t.state = "stopped"
	t.mu.Unlock()
}

func (t *tunnel) snapshot() (state, why string, tries int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state, t.why, t.tries
}

func (t *tunnel) loop(stop chan struct{}) {
	wait := 0
	for {
		t.mu.Lock()
		t.tries++
		t.state = "connecting"
		t.mu.Unlock()
		began := time.Now()
		err := t.once(stop)
		select {
		case <-stop:
			return
		default:
		}
		if time.Since(began) > 2*time.Minute {
			wait = 0
		}
		wait = min(wait+5, 60)
		t.mu.Lock()
		t.state = "retrying"
		if err != nil {
			t.why = err.Error()
		}
		t.mu.Unlock()
		select {
		case <-stop:
			return
		case <-time.After(time.Duration(wait) * time.Second):
		}
	}
}

func (t *tunnel) once(stop chan struct{}) error {
	signer, err := tunnelKey()
	if err != nil {
		return fmt.Errorf("MineNest's key couldn't be made: %v", err)
	}
	addr := net.JoinHostPort(t.cfg.Host, strconv.Itoa(t.cfg.SSHPort))
	conf := &ssh.ClientConfig{
		User:            t.cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: t.hostKey,
		Timeout:         20 * time.Second,
		ClientVersion:   "SSH-2.0-MineNest_" + Version,
	}
	client, err := ssh.Dial("tcp", addr, conf)
	if err != nil {
		return explainSSH(t.name, err)
	}
	t.mu.Lock()
	t.client = client
	t.mu.Unlock()
	defer client.Close()
	bind := t.cfg.Bind
	if bind == "" {
		bind = "0.0.0.0"
	}
	l, err := client.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(t.cfg.PublicPort)))
	if err != nil {
		return fmt.Errorf("%s didn't let MineNest open port %d there (it may be in use, or sshd doesn't allow it): %v", t.name, t.cfg.PublicPort, err)
	}
	defer l.Close()
	t.mu.Lock()
	t.state, t.why, t.upSince = "up", "", time.Now()
	onUp := t.onUp
	t.mu.Unlock()
	if onUp != nil {
		go onUp()
	}
	// Keepalives notice a dead connection within a minute or so.
	dead := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Second):
			}
			if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				dead <- fmt.Errorf("the connection to %s dropped", t.name)
				client.Close()
				return
			}
		}
	}()
	go func() {
		<-stop
		l.Close()
		client.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			select {
			case e := <-dead:
				return e
			default:
			}
			return fmt.Errorf("the connection to %s dropped", t.name)
		}
		go pipeTo(c, t.target())
	}
}

func pipeTo(c net.Conn, target string) {
	defer c.Close()
	d, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		return
	}
	defer d.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(d, c); closeWrite(d); done <- struct{}{} }()
	go func() { io.Copy(c, d); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

func explainSSH(name string, err error) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "unable to authenticate"):
		return fmt.Errorf("%s didn't accept MineNest's key. Check it's in authorized_keys for that user", name)
	case strings.Contains(s, "identity has changed") || strings.Contains(s, "host key mismatch"):
		return fmt.Errorf("%s's identity isn't the one MineNest expects, so it didn't connect (%v)", name, err)
	case strings.Contains(s, "no such host"):
		return fmt.Errorf("%s's address doesn't lead anywhere", name)
	case strings.Contains(s, "refused"):
		return fmt.Errorf("%s refused the connection: is sshd running on that port?", name)
	case strings.Contains(s, "timeout"):
		return fmt.Errorf("%s didn't answer", name)
	}
	return fmt.Errorf("couldn't connect to %s: %v", name, err)
}
