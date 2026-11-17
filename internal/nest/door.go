package nest

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Players' real addresses through the supporter relay, and the doorway that keeps every other way
// in working while they come.
//
// Through the relay every player would arrive from 127.0.0.1, the end of MineNest's tunnel. Paper
// can be told each player's address instead: with proxies.proxy-protocol on (in
// config/paper-global.yml) it reads a PROXY protocol header at the start of each connection,
// which the relay's router sends for servers that asked. But Paper then wants that header on every
// connection, and Minecraft on this computer and friends at home don't send one. So in this "door
// mode" Paper listens on a port of its own on the loopback, and the doorway takes the players'
// port, passing each connection on with a PROXY v2 header naming who it's from. The relay's
// tunnel goes straight to Paper's port, since the relay sends the header itself.

type door struct {
	s           *Server
	playersPort int
	paperPort   int
	serverIP    string // server-ip as it was: Paper writes the --host and --port it's given into server.properties
	hadIP       bool
	l           net.Listener
	wg          sync.WaitGroup
}

// paperTakesProxy: proxies.proxy-protocol arrived in Paper for 1.19.4.
func paperTakesProxy(mc string) bool { return !versionLess(mc, "1.19.4") }

func prepareDoor(s *Server, playersPort int) (*door, error) {
	c := s.Config()
	if c.Installed == nil || !paperTakesProxy(c.Installed.Minecraft) {
		return nil, fmt.Errorf("this Paper version can't take players' addresses (1.19.4 and later can)")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	paperPort := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if err := paperProxyProtocol(s.Root(), true); err != nil {
		return nil, fmt.Errorf("Paper's settings couldn't be changed: %v", err)
	}
	ip, had := s.props().Get("server-ip")
	d := &door{s: s, playersPort: playersPort, paperPort: paperPort, serverIP: ip, hadIP: had}
	// Kept until server.properties is put back, in case MineNest is closed before it can be.
	writeJSON(filepath.Join(s.Dir, "door.json"), map[string]any{"port": playersPort, "ip": ip, "had_ip": had}, 0o644)
	return d, nil
}

// recoverDoor puts server.properties back after a door-mode run that MineNest couldn't finish.
func recoverDoor(s *Server) {
	var v struct {
		Port  int    `json:"port"`
		IP    string `json:"ip"`
		HadIP bool   `json:"had_ip"`
	}
	path := filepath.Join(s.Dir, "door.json")
	if readJSON(path, &v) == nil && v.Port > 0 {
		(&door{s: s, playersPort: v.Port, serverIP: v.IP, hadIP: v.HadIP}).restoreProperties()
	}
	os.Remove(path)
}

// restoreProperties puts server.properties' port and address back to the players' ones, after
// Paper saved its door-mode ones there.
func (d *door) restoreProperties() {
	p := d.s.props()
	p.Set("server-port", fmt.Sprint(d.playersPort))
	if d.hadIP || p.GetDefault("server-ip", "") != "" {
		p.Set("server-ip", d.serverIP)
	}
	p.Save()
	if d.s.State() == Stopped || d.s.doorLocked() != d {
		os.Remove(filepath.Join(d.s.Dir, "door.json"))
	}
}

func (d *door) start() error {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", d.playersPort))
	if err != nil {
		return err
	}
	d.l = l
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go d.pass(c)
		}
	}()
	return nil
}

func (d *door) stop() {
	if d.l != nil {
		d.l.Close()
		d.wg.Wait()
	}
}

func (d *door) pass(c net.Conn) {
	defer c.Close()
	p, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", d.paperPort), 10*time.Second)
	if err != nil {
		return
	}
	defer p.Close()
	if _, err := p.Write(proxyV2Header(c.RemoteAddr(), c.LocalAddr())); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { copyConn(p, c); done <- struct{}{} }()
	go func() { copyConn(c, p); done <- struct{}{} }()
	<-done
	<-done
}

func copyConn(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	closeWrite(dst)
}

var proxyV2Sig = []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}

// proxyV2Header is a PROXY protocol v2 header saying the connection is from src to dst.
func proxyV2Header(src, dst net.Addr) []byte {
	sa, _ := src.(*net.TCPAddr)
	da, _ := dst.(*net.TCPAddr)
	h := append([]byte{}, proxyV2Sig...)
	h = append(h, 0x21) // version 2, PROXY
	if sa == nil || da == nil {
		return append(h, 0x00, 0, 0) // UNSPEC: Paper uses the connection's own addresses
	}
	s4, d4 := sa.IP.To4(), da.IP.To4()
	if s4 != nil && d4 != nil {
		h = append(h, 0x11, 0, 12)
		h = append(h, s4...)
		h = append(h, d4...)
	} else {
		h = append(h, 0x21, 0, 36)
		h = append(h, sa.IP.To16()...)
		h = append(h, da.IP.To16()...)
	}
	h = binary.BigEndian.AppendUint16(h, uint16(sa.Port))
	h = binary.BigEndian.AppendUint16(h, uint16(da.Port))
	return h
}

// paperProxyProtocol sets proxies.proxy-protocol in config/paper-global.yml, leaving everything
// else as it is.
func paperProxyProtocol(root string, on bool) error {
	path := filepath.Join(root, "config", "paper-global.yml")
	value := "false"
	if on {
		value = "true"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !on {
			return nil
		}
		return writeFile(path, []byte("proxies:\n  proxy-protocol: true\n"), 0o644)
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	block, child, found := -1, 0, -1
	end := len(lines)
	for i, l := range lines {
		trim := strings.TrimLeft(l, " ")
		indent := len(l) - len(trim)
		blank := trim == "" || strings.HasPrefix(trim, "#")
		if block < 0 {
			if indent == 0 && strings.HasPrefix(l, "proxies:") {
				if strings.TrimSpace(strings.TrimPrefix(l, "proxies:")) != "" {
					return fmt.Errorf("proxies isn't a block in paper-global.yml")
				}
				block = i
			}
			continue
		}
		if blank {
			continue
		}
		if indent == 0 {
			end = i
			break
		}
		if child == 0 {
			child = indent
		}
		if indent == child && strings.HasPrefix(trim, "proxy-protocol:") {
			found = i
			break
		}
	}
	if child <= 0 || child > 8 {
		child = 2
	}
	line := strings.Repeat(" ", child) + "proxy-protocol: " + value
	switch {
	case found >= 0:
		if strings.TrimSpace(lines[found]) == strings.TrimSpace(line) {
			return nil
		}
		lines[found] = line
	case block >= 0:
		lines = append(lines[:block+1], append([]string{line}, lines[block+1:]...)...)
	default:
		if !on {
			return nil
		}
		_ = end
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "proxies:", line, "")
	}
	return writeFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}
