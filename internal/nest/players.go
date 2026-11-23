package nest

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
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

// What the server says back to each command, so MineNest can tell people what really happened
// instead of "done" (the wording is Minecraft's own, which Paper, Fabric and Forge keep).
var (
	replyLookupFailed = regexp.MustCompile(`(?i)That player does not exist|Couldn't find profile with name|Unknown player`)
	replyUnknownCmd   = regexp.MustCompile(`(?i)Unknown or incomplete command|Unknown command`)
	replies           = map[string]struct{ ok, same *regexp.Regexp }{
		"allow":    {regexp.MustCompile(`(?i)Added (\S+) to the whitelist`), regexp.MustCompile(`(?i)Player is already whitelisted`)},
		"disallow": {regexp.MustCompile(`(?i)Removed (\S+) from the whitelist`), regexp.MustCompile(`(?i)Player is not whitelisted`)},
		"op":       {regexp.MustCompile(`(?i)Made (\S+) a server operator`), regexp.MustCompile(`(?i)Nothing changed\. The player already is an operator`)},
		"deop":     {regexp.MustCompile(`(?i)Made (\S+) no longer a server operator`), regexp.MustCompile(`(?i)Nothing changed\. The player is not an operator`)},
		"ban":      {regexp.MustCompile(`(?i)Banned (\S+?):? `), regexp.MustCompile(`(?i)Nothing changed\. The player is already banned`)},
		"pardon":   {regexp.MustCompile(`(?i)Unbanned (\S+)`), regexp.MustCompile(`(?i)Nothing changed\. The player isn't banned`)},
		"kick":     {regexp.MustCompile(`(?i)Kicked (\S+?):? `), regexp.MustCompile(`(?i)No player was found`)},
		// Floodgate's own whitelist command for Bedrock players.
		"fallow":    {regexp.MustCompile(`(?i)(?:added|whitelisted)`), regexp.MustCompile(`(?i)already`)},
		"fdisallow": {regexp.MustCompile(`(?i)removed`), regexp.MustCompile(`(?i)(?:isn't|is not|wasn't|was not)`)},
		"reload":    {regexp.MustCompile(`(?i)Reloaded the whitelist`), nil},
		"save-on":   {regexp.MustCompile(`(?i)Automatic saving is now enabled|Saving is (?:now )?(?:turned |switched )?on`), regexp.MustCompile(`(?i)Saving is already (?:turned )?on`)},
	}
	replyNoBedrock = regexp.MustCompile(`(?i)not found|could not find|couldn't find|does not exist|doesn't exist`)
	// The pauses between tries when the server's own lookup fails.
	lookupRetries = []time.Duration{3 * time.Second, 6 * time.Second, 10 * time.Second}
	replyWait     = 6 * time.Second
)

type replyKind int

const (
	replyNone replyKind = iota
	replyOK
	replySame
	replyLookup
	replyUnknown
	replyNotFound // Floodgate: no such Bedrock player
)

// commandReply sends a command and reads the console until the server answers it.
func (s *Server) commandReply(ctx context.Context, action, cmd string) (replyKind, string, error) {
	s.mu.Lock()
	since := s.seq
	s.mu.Unlock()
	if err := s.Command(cmd); err != nil {
		return replyNone, "", err
	}
	want := replies[action]
	deadline := time.Now().Add(replyWait)
	for time.Now().Before(deadline) {
		for _, l := range s.Lines(since) {
			since = l.Seq
			// Only what the server itself says counts: a player's chat line or a join message
			// can't pass for its answer.
			if l.Kind == "sent" || l.Kind == "nest" || l.Kind == "chat" || l.Kind == "join" {
				continue
			}
			t := l.Text
			switch {
			case replyLookupFailed.MatchString(t):
				return replyLookup, t, nil
			case replyUnknownCmd.MatchString(t):
				return replyUnknown, t, nil
			case want.same != nil && want.same.MatchString(t):
				return replySame, t, nil
			case strings.HasPrefix(action, "f") && replyNoBedrock.MatchString(t):
				return replyNotFound, t, nil
			case want.ok != nil && want.ok.MatchString(t):
				exact := ""
				if m := want.ok.FindStringSubmatch(t); len(m) > 1 {
					exact = strings.TrimSuffix(m[1], ":")
				}
				return replyOK, exact, nil
			}
		}
		select {
		case <-ctx.Done():
			return replyNone, "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return replyNone, "", nil
}

// PlayerAction does one thing to one player: op, deop, kick, ban, pardon, allow (whitelist add),
// disallow (whitelist remove). It says what happened, in words for the Players page.
func (s *Server) PlayerAction(ctx context.Context, action, name, reason string) (string, error) {
	if _, ok := doneWords[action]; !ok {
		return "", fmt.Errorf("unknown action %q", action)
	}
	pl, err := checkName(name, s.floodgatePrefix())
	if err != nil {
		return "", err
	}
	reason = cleanReason(reason)
	if s.State() == Running {
		// The website door gives up on a request after 30 seconds, so this stops a little before.
		ctx, cancel := context.WithTimeout(ctx, actionWait)
		defer cancel()
		said, err := s.playerActionLive(ctx, action, pl, reason)
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			return "", errStoppedWaiting
		}
		return said, err
	}
	return s.playerActionFiles(ctx, action, pl, reason)
}

// actionWait is the longest one player action waits for the server and Mojang.
var actionWait = 25 * time.Second

var errStoppedWaiting = apiError{504, "MineNest stopped waiting. Open the Console to see whether it worked"}

// cleanReason keeps a kick or ban reason to one plain line.
func cleanReason(r string) string {
	r = strings.Map(func(c rune) rune {
		if c < 32 || c == 127 {
			return ' '
		}
		return c
	}, r)
	r = strings.Join(strings.Fields(r), " ")
	if len(r) > 200 {
		r = r[:200]
	}
	return r
}

func (s *Server) onlineMode() bool { return s.props().GetDefault("online-mode", "true") != "false" }

var doneWords = map[string]string{
	"allow": "Added %s to the whitelist.", "disallow": "Removed %s from the whitelist.",
	"op": "%s is an operator now.", "deop": "%s isn't an operator any more.",
	"ban": "Banned %s.", "pardon": "%s isn't banned any more.", "kick": "Kicked %s.",
}
var sameWords = map[string]string{
	"allow": "%s was already on the whitelist.", "disallow": "%s wasn't on the whitelist.",
	"op": "%s was already an operator.", "deop": "%s wasn't an operator.",
	"ban": "%s was already banned.", "pardon": "%s wasn't banned.", "kick": "%s isn't on the server right now.",
}

// playerActionLive goes through the running server's own commands, reading what it answers.
func (s *Server) playerActionLive(ctx context.Context, action string, pl player, reason string) (string, error) {
	// Bedrock players on the whitelist go through Floodgate's own command.
	if pl.Bedrock && (action == "allow" || action == "disallow") {
		verb := map[string]string{"allow": "add", "disallow": "remove"}[action]
		kind, text, err := s.commandReply(ctx, "f"+action, "fwhitelist "+verb+" "+pl.Gamertag)
		if err != nil {
			return "", err
		}
		switch kind {
		case replyOK:
			return fmt.Sprintf(doneWords[action], pl.Name), nil
		case replySame:
			return fmt.Sprintf(sameWords[action], pl.Name), nil
		case replyNotFound:
			return "", fmt.Errorf("Floodgate doesn't know a Bedrock player called %s", pl.Gamertag)
		case replyUnknown:
			return "", fmt.Errorf("this server hasn't Floodgate's whitelist command, so Bedrock players can't be added from here")
		case replyLookup:
			return "", fmt.Errorf("the server couldn't look %s up: %s", pl.Gamertag, text)
		}
		return "Sent to the server. It didn't answer within a few seconds; the Console shows what it said.", nil
	}

	exact := pl.Name
	var prof *mcProfile
	busy := false
	if !pl.Bedrock && s.onlineMode() && action != "kick" {
		p, err := lookupProfile(ctx, pl.Name)
		switch {
		case errors.Is(err, errNoSuchPlayer):
			if action == "allow" || action == "op" || action == "ban" {
				return "", fmt.Errorf("Mojang doesn't know a player called %s. Check the spelling", pl.Name)
			}
		case err != nil:
			busy = true // the server may still know the name from before
		default:
			prof, exact = &p, p.Name
		}
	}
	cmd := map[string]string{"allow": "whitelist add", "disallow": "whitelist remove", "op": "op", "deop": "deop",
		"ban": "ban", "pardon": "pardon", "kick": "kick"}[action] + " " + exact
	if reason != "" && (action == "ban" || action == "kick") {
		cmd += " " + reason
	}
	for try := 0; ; try++ {
		kind, text, err := s.commandReply(ctx, action, cmd)
		if err != nil {
			return "", err
		}
		switch kind {
		case replyOK:
			if text != "" && strings.EqualFold(text, exact) {
				exact = text
			}
			time.Sleep(300 * time.Millisecond) // the server writes its file a moment later
			return fmt.Sprintf(doneWords[action], exact), nil
		case replySame:
			return fmt.Sprintf(sameWords[action], exact), nil
		case replyNone:
			return "Sent to the server. It didn't answer within a few seconds; the Console shows what it said.", nil
		case replyUnknown:
			return "", fmt.Errorf("the server didn't know the command: %s", text)
		}
		// The server's lookup of the name failed (Mojang busy, most often).
		if action == "kick" {
			return "", fmt.Errorf("%s isn't on the server right now", exact)
		}
		if try >= len(lookupRetries) {
			break
		}
		s.Note("The server couldn't look %s up with Mojang, so MineNest tries again in %d seconds.", exact, int(lookupRetries[try].Seconds()))
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(lookupRetries[try]):
		}
	}
	// MineNest knows who it is even though the server couldn't look it up: put them on the
	// whitelist file and have the server read it again.
	if action == "allow" && prof != nil {
		if err := s.editList("allow", player{Name: prof.Name}, prof.uuid(), prof.Name, ""); err != nil {
			return "", err
		}
		if kind, _, err := s.commandReply(ctx, "reload", "whitelist reload"); err == nil && kind != replyUnknown {
			s.Note("Added %s to the whitelist file and had the server read it again.", prof.Name)
			return fmt.Sprintf(doneWords[action], prof.Name), nil
		}
		return fmt.Sprintf("Added %s to the whitelist file. It counts after a restart.", prof.Name), nil
	}
	if busy || prof != nil {
		return "", fmt.Errorf("Mojang's servers are busy, so the server couldn't look %s up. Try again in a moment", exact)
	}
	return "", fmt.Errorf("the server couldn't find a player called %s", exact)
}

// playerActionFiles changes the server's JSON files while it's stopped.
func (s *Server) playerActionFiles(ctx context.Context, action string, pl player, reason string) (string, error) {
	if action == "kick" {
		return "", fmt.Errorf("the server isn't running, so nobody is on it")
	}
	if action == "op" || action == "ban" || action == "allow" {
		uuid, exact, err := s.playerUUID(ctx, pl)
		if err != nil {
			return "", err
		}
		if err := s.editList(action, pl, uuid, exact, reason); err != nil {
			return "", err
		}
		return fmt.Sprintf(doneWords[action], exact), nil
	}
	if err := s.editList(action, pl, "", "", ""); err != nil {
		return "", err
	}
	return fmt.Sprintf(doneWords[action], pl.Name), nil
}

var listFiles = map[string]string{"op": "ops.json", "deop": "ops.json", "ban": "banned-players.json", "pardon": "banned-players.json",
	"allow": "whitelist.json", "disallow": "whitelist.json"}

// editList takes the player out of the action's file and, to add them, puts them back once with
// their UUID and exact name. The file is replaced in one go.
func (s *Server) editList(action string, pl player, uuid, exact, reason string) error {
	file := listFiles[action]
	entries := s.listFile(file)
	kept := []map[string]any{}
	for _, e := range entries {
		n, _ := e["name"].(string)
		u, _ := e["uuid"].(string)
		if strings.EqualFold(n, pl.Name) || (uuid != "" && strings.EqualFold(u, uuid)) {
			continue
		}
		kept = append(kept, e)
	}
	if uuid != "" {
		e := map[string]any{"uuid": uuid, "name": exact}
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
		kept = append(kept, e)
	}
	b, _ := json.MarshalIndent(kept, "", "  ")
	return writeFile(filepath.Join(s.Root(), file), b, 0o644)
}

// playerUUID is a player's UUID and exact name: Mojang's for online-mode servers, the one an
// offline-mode server gives the name, or Floodgate's for a Bedrock player.
func (s *Server) playerUUID(ctx context.Context, pl player) (string, string, error) {
	if pl.Bedrock {
		xuid, err := lookupXUID(ctx, pl.Gamertag)
		if err != nil {
			return "", "", err
		}
		return floodgateUUID(xuid), pl.Name, nil
	}
	if !s.onlineMode() {
		return offlineUUID(pl.Name), pl.Name, nil
	}
	p, err := lookupProfile(ctx, pl.Name)
	switch {
	case errors.Is(err, errNoSuchPlayer):
		return "", "", fmt.Errorf("Mojang doesn't know a player called %s. Check the spelling", pl.Name)
	case err != nil:
		return "", "", fmt.Errorf("Mojang's servers are busy, so %s couldn't be looked up. Try again in a moment", pl.Name)
	}
	return p.uuid(), p.Name, nil
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
