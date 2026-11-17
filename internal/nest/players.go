package nest

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The whitelist, operators and bans: through the server's own commands while it runs, and in its
// JSON files while it's stopped.

type PlayerLists struct {
	Online      []string `json:"online"`
	Whitelist   []string `json:"whitelist"`
	Ops         []string `json:"ops"`
	Banned      []string `json:"banned"`
	WhitelistOn bool     `json:"whitelist_on"`
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.]{1,16}$`)

func (s *Server) listFile(name string) []map[string]any {
	var v []map[string]any
	readJSON(filepath.Join(s.Root(), name), &v)
	return v
}

func names(entries []map[string]any) []string {
	out := []string{}
	for _, e := range entries {
		if n, ok := e["name"].(string); ok {
			out = append(out, n)
		}
	}
	sortStrings(out)
	return out
}

func (s *Server) PlayerLists() PlayerLists {
	return PlayerLists{
		Online:      s.Players(),
		Whitelist:   names(s.listFile("whitelist.json")),
		Ops:         names(s.listFile("ops.json")),
		Banned:      names(s.listFile("banned-players.json")),
		WhitelistOn: s.props().GetDefault("white-list", "false") == "true",
	}
}

// PlayerAction does one thing to one player: op, deop, kick, ban, pardon, allow (whitelist add),
// disallow (whitelist remove).
func (s *Server) PlayerAction(ctx context.Context, action, name, reason string) error {
	name = strings.TrimSpace(name)
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%q isn't a Minecraft name", name)
	}
	reason = strings.ReplaceAll(strings.TrimSpace(reason), "\n", " ")
	if s.State() == Running {
		var cmd string
		switch action {
		case "op":
			cmd = "op " + name
		case "deop":
			cmd = "deop " + name
		case "kick":
			cmd = strings.TrimSpace("kick " + name + " " + reason)
		case "ban":
			cmd = strings.TrimSpace("ban " + name + " " + reason)
		case "pardon":
			cmd = "pardon " + name
		case "allow":
			cmd = "whitelist add " + name
		case "disallow":
			cmd = "whitelist remove " + name
		default:
			return fmt.Errorf("unknown action %q", action)
		}
		err := s.Command(cmd)
		time.Sleep(500 * time.Millisecond) // the files change a moment later
		return err
	}
	if action == "kick" {
		return fmt.Errorf("the server isn't running, so nobody is on it")
	}
	file := map[string]string{"op": "ops.json", "deop": "ops.json", "ban": "banned-players.json", "pardon": "banned-players.json",
		"allow": "whitelist.json", "disallow": "whitelist.json"}[action]
	if file == "" {
		return fmt.Errorf("unknown action %q", action)
	}
	entries := s.listFile(file)
	kept := entries[:0]
	for _, e := range entries {
		if n, _ := e["name"].(string); !strings.EqualFold(n, name) {
			kept = append(kept, e)
		}
	}
	entries = kept
	if action == "op" || action == "ban" || action == "allow" {
		uuid, proper, err := s.playerUUID(ctx, name)
		if err != nil {
			return err
		}
		e := map[string]any{"uuid": uuid, "name": proper}
		switch action {
		case "op":
			e["level"] = 4
			e["bypassesPlayerLimit"] = false
		case "ban":
			e["created"] = time.Now().Format("2006-01-02 15:04:05 -0700")
			e["source"] = "MineNest"
			e["expires"] = "forever"
			if reason == "" {
				reason = "Banned by an operator."
			}
			e["reason"] = reason
		}
		entries = append(entries, e)
	}
	b, _ := json.MarshalIndent(entries, "", "  ")
	return writeFile(filepath.Join(s.Root(), file), b, 0o644)
}

// playerUUID is a player's UUID: Mojang's for online-mode servers, or the one an offline-mode
// server gives the name.
func (s *Server) playerUUID(ctx context.Context, name string) (string, string, error) {
	if s.props().GetDefault("online-mode", "true") == "false" {
		return offlineUUID(name), name, nil
	}
	var p struct{ ID, Name string }
	if err := getJSON(ctx, "https://api.mojang.com/users/profiles/minecraft/"+name, 0, &p); err != nil || len(p.ID) != 32 {
		if err != nil && strings.Contains(err.Error(), "404") {
			return "", "", fmt.Errorf("Mojang doesn't know a player called %s", name)
		}
		return "", "", fmt.Errorf("couldn't look %s up with Mojang: %v", name, err)
	}
	id := p.ID
	return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:], p.Name, nil
}

func offlineUUID(name string) string {
	h := md5.Sum([]byte("OfflinePlayer:" + name))
	h[6] = h[6]&0x0f | 0x30
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func (s *Server) logsDir() string { return filepath.Join(s.Root(), "logs") }

// ReadFileIn returns a text file in the server's folder (for the GUI's file viewer), refusing
// anything outside it.
func (s *Server) ReadFileIn(rel string) ([]byte, error) {
	p := filepath.Join(s.Root(), filepath.FromSlash(rel))
	if !within(s.Root(), p) {
		return nil, fmt.Errorf("that's outside the server's folder")
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.Size() > 2<<20 {
		return nil, fmt.Errorf("that file is too big to show here")
	}
	return os.ReadFile(p)
}
