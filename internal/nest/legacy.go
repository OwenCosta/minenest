package nest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// What the supporter relay left behind. The relay is gone, but a server that used it may have
// Paper's proxy-protocol turned on in config/paper-global.yml and, after a crash, the port and
// address of server.properties as they were for a doorway. Both are put back the first time such
// a server is loaded, and a server whose way was "relay" is moved to home only.

// recoverDoor puts server.properties back after a run with the old doorway that MineNest couldn't
// finish.
func recoverDoor(s *Server) {
	var v struct {
		Port  int    `json:"port"`
		IP    string `json:"ip"`
		HadIP bool   `json:"had_ip"`
	}
	path := filepath.Join(s.Dir, "door.json")
	if readJSON(path, &v) == nil && v.Port > 0 {
		p := s.props()
		p.Set("server-port", fmt.Sprint(v.Port))
		if v.HadIP || p.GetDefault("server-ip", "") != "" {
			p.Set("server-ip", v.IP)
		}
		p.Save()
	}
	os.Remove(path)
}

// leaveRelay is for a server saved with the way "relay": it goes back to home only, and the
// console says so once (the saved way is changed, so the note isn't repeated).
func leaveRelay(s *Server) {
	if s.cfg.Way != "relay" {
		return
	}
	recoverDoor(s)
	paperProxyProtocol(s.Root(), false)
	s.cfg.Way = ""
	s.saveLocked()
	s.Note("The supporter relay has ended, so this server is back to home only. Pick playit.gg or your own VPS on the Internet page to let friends join from outside.")
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
