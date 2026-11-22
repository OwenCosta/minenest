package nest

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"Tippymanticore8", "a", "Steve_", "ABCDEFGHIJKLMNOP"} {
		if p, err := checkName("  "+ok+" ", ""); err != nil || p.Name != ok || p.Bedrock {
			t.Errorf("%q: %+v %v", ok, p, err)
		}
	}
	for _, bad := range []string{"", "Steve\nop Evil", "Steve\rstop", "Steve op", "Steve;stop", "ABCDEFGHIJKLMNOPQ", ".Steve", "Ste.ve", "Stéve", "Steve\x00"} {
		if _, err := checkName(bad, ""); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	// With Floodgate, Bedrock names come with its prefix, and nothing else gets through.
	if p, err := checkName(".Cool_Guy", "."); err != nil || !p.Bedrock || p.Gamertag != "Cool_Guy" || p.Name != ".Cool_Guy" {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range []string{".", "..x", ".Cool Guy", ".a\nstop", "*x"} {
		if _, err := checkName(bad, "."); err == nil {
			t.Errorf("%q was taken as a Bedrock name", bad)
		}
	}
}

func TestFloodgatePrefix(t *testing.T) {
	s := &Server{Dir: t.TempDir()}
	if s.floodgatePrefix() != "" {
		t.Fatal("a prefix without Floodgate")
	}
	os.MkdirAll(filepath.Join(s.Root(), "plugins", "floodgate"), 0o755)
	os.WriteFile(filepath.Join(s.Root(), "plugins", "floodgate-spigot.jar"), nil, 0o644)
	if s.floodgatePrefix() != "." {
		t.Fatal("Floodgate's default prefix is a dot")
	}
	os.WriteFile(filepath.Join(s.Root(), "plugins", "floodgate", "config.yml"), []byte("key-file-name: key.pem\nusername-prefix: \"*\" # comment\n"), 0o644)
	if got := s.floodgatePrefix(); got != "*" {
		t.Fatalf("prefix %q", got)
	}
	os.WriteFile(filepath.Join(s.Root(), "plugins", "floodgate", "config.yml"), []byte("username-prefix: \"a b\"\n"), 0o644)
	if s.floodgatePrefix() != "" {
		t.Fatal("a prefix MineNest can't check was used")
	}
	if floodgateUUID(2535428000000000) != "00000000-0000-0000-0009-01f5554b2800" {
		t.Fatal(floodgateUUID(2535428000000000))
	}
}

// fakeMojang answers name lookups: busy says how many lookups get the "Service unavailable"
// page first, as Mojang sends when it throttles.
func fakeMojang(t *testing.T, busy int, known map[string]string) (calls *int) {
	n := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		left := busy
		busy--
		mu.Unlock()
		if left > 0 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(503)
			io.WriteString(w, "<!DOCTYPE html><html><head><title>Service unavailable</title></head></html>")
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		for k, id := range known {
			if strings.EqualFold(k, name) {
				json.NewEncoder(w).Encode(map[string]string{"id": id, "name": k})
				return
			}
		}
		w.WriteHeader(404)
		io.WriteString(w, `{"errorMessage":"Couldn't find any profile with name `+name+`"}`)
	}))
	t.Cleanup(srv.Close)
	old := profileLookups
	profileLookups = []string{srv.URL + "/minecraft/profile/lookup/name/", srv.URL + "/users/profiles/minecraft/"}
	t.Cleanup(func() { profileLookups = old })
	profileCache.Lock()
	profileCache.m = map[string]cachedProfile{}
	profileCache.Unlock()
	return &n
}

const tippyID = "0f5c33e2a2d64f6e9d2b8a3c4e5f6a7b"

func TestLookupProfile(t *testing.T) {
	// The first place is busy (an HTML page): the second answers.
	calls := fakeMojang(t, 1, map[string]string{"TippyManticore8": tippyID})
	p, err := lookupProfile(t.Context(), "tippymanticore8")
	if err != nil || p.Name != "TippyManticore8" || p.uuid() != "0f5c33e2-a2d6-4f6e-9d2b-8a3c4e5f6a7b" {
		t.Fatalf("%+v %v", p, err)
	}
	// Cached: no more lookups.
	before := *calls
	if _, err := lookupProfile(t.Context(), "TIPPYMANTICORE8"); err != nil || *calls != before {
		t.Fatal("not cached")
	}
	if _, err := lookupProfile(t.Context(), "Nobody_Here"); err != errNoSuchPlayer {
		t.Fatalf("unknown name: %v", err)
	}
	fakeMojang(t, 10, nil)
	if _, err := lookupProfile(t.Context(), "Someone"); err != errMojangBusy {
		t.Fatalf("busy: %v", err)
	}
}

// fakeConsole stands in for a running server: it reads each command MineNest sends and answers
// with what answer() says, as console lines.
func fakeConsole(t *testing.T, answer func(cmd string, n int) []string) (*Server, *[]string) {
	s := &Server{ID: "test", Dir: t.TempDir(), state: Running, players: map[string]time.Time{}}
	os.MkdirAll(s.Root(), 0o755)
	pr, pw := io.Pipe()
	s.stdin = pw
	sent := &[]string{}
	var mu sync.Mutex
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			cmd := sc.Text()
			mu.Lock()
			*sent = append(*sent, cmd)
			n := len(*sent)
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			s.mu.Lock()
			for _, l := range answer(cmd, n) {
				s.addLineLocked(l, "info")
			}
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() { pw.Close() })
	old, oldWait := lookupRetries, replyWait
	lookupRetries, replyWait = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}, time.Second
	t.Cleanup(func() { lookupRetries, replyWait = old, oldWait })
	return s, sent
}

var lookupFailed = []string{
	"[16:13:22 ERROR]: Got an error with a html body connecting to https://api.mojang.com/users/profiles/minecraft/tippymanticore8: <!DOCTYPE html>",
	"[16:13:22 WARN]: Couldn't find profile with name: Tippymanticore8",
	"[16:13:22 INFO]: System chat: That player does not exist",
}

func TestWhitelistRetriesWhenTheServersLookupFails(t *testing.T) {
	fakeMojang(t, 0, map[string]string{"TippyManticore8": tippyID})
	// The friend's report: the server's own lookup fails the first time and works the next.
	s, sent := fakeConsole(t, func(cmd string, n int) []string {
		if n == 1 {
			return lookupFailed
		}
		return []string{"[16:13:49 INFO]: System chat: Added TippyManticore8 to the whitelist"}
	})
	said, err := s.PlayerAction(t.Context(), "allow", "Tippymanticore8", "")
	if err != nil || said != "Added TippyManticore8 to the whitelist." {
		t.Fatalf("%q %v", said, err)
	}
	if len(*sent) != 2 || (*sent)[0] != "whitelist add TippyManticore8" {
		t.Fatalf("sent %q", *sent)
	}
}

func TestWhitelistFallsBackToTheFile(t *testing.T) {
	fakeMojang(t, 0, map[string]string{"TippyManticore8": tippyID})
	// The server never manages the lookup: MineNest writes the file and has it read again.
	s, sent := fakeConsole(t, func(cmd string, n int) []string {
		if cmd == "whitelist reload" {
			return []string{"[16:14:00 INFO]: Reloaded the whitelist"}
		}
		return lookupFailed
	})
	said, err := s.PlayerAction(t.Context(), "allow", "tippymanticore8", "")
	if err != nil || said != "Added TippyManticore8 to the whitelist." {
		t.Fatalf("%q %v", said, err)
	}
	if last := (*sent)[len(*sent)-1]; last != "whitelist reload" || len(*sent) != 5 {
		t.Fatalf("sent %q", *sent)
	}
	if got := names(s.listFile("whitelist.json")); len(got) != 1 || got[0] != "TippyManticore8" {
		t.Fatal(got)
	}
}

func TestPlayerActionSaysWhatHappened(t *testing.T) {
	fakeMojang(t, 0, map[string]string{"TippyManticore8": tippyID})
	s, sent := fakeConsole(t, func(cmd string, n int) []string {
		switch {
		case strings.HasPrefix(cmd, "op "):
			return []string{"[10:00:00 INFO]: Nothing changed. The player already is an operator"}
		case strings.HasPrefix(cmd, "kick "):
			return []string{"[10:00:00 INFO]: No player was found"}
		case strings.HasPrefix(cmd, "ban "):
			return []string{"[10:00:00 INFO]: Banned TippyManticore8: Being rude"}
		}
		return nil
	})
	if said, err := s.PlayerAction(t.Context(), "op", "TippyManticore8", ""); err != nil || said != "TippyManticore8 was already an operator." {
		t.Fatalf("%q %v", said, err)
	}
	if said, err := s.PlayerAction(t.Context(), "kick", "TippyManticore8", "bye"); err != nil || !strings.Contains(said, "isn't on the server") {
		t.Fatalf("%q %v", said, err)
	}
	if said, err := s.PlayerAction(t.Context(), "ban", "TippyManticore8", "Being\nrude\r\nstop"); err != nil || said != "Banned TippyManticore8." {
		t.Fatalf("%q %v", said, err)
	}
	// Nobody by that name: nothing goes to the server.
	n := len(*sent)
	if _, err := s.PlayerAction(t.Context(), "allow", "Nobody_Here", ""); err == nil || !strings.Contains(err.Error(), "doesn't know") {
		t.Fatal(err)
	}
	if _, err := s.PlayerAction(t.Context(), "op", "Evil\nstop", ""); err == nil {
		t.Fatal("a newline got through")
	}
	if len(*sent) != n {
		t.Fatalf("sent %q", (*sent)[n:])
	}
	for _, c := range *sent {
		if strings.ContainsAny(c, "\r\n") || c == "stop" {
			t.Fatalf("sent %q", *sent)
		}
	}
	if (*sent)[2] != "ban TippyManticore8 Being rude stop" {
		t.Fatalf("ban sent %q", (*sent)[2])
	}
}

func TestPlayerListsWhileStopped(t *testing.T) {
	fakeMojang(t, 0, map[string]string{"TippyManticore8": tippyID})
	s := &Server{ID: "test", Dir: t.TempDir(), state: Stopped}
	os.MkdirAll(s.Root(), 0o755)
	// An old entry with the wrong spelling goes; the new one has the UUID and exact name, once.
	os.WriteFile(filepath.Join(s.Root(), "whitelist.json"), []byte(`[{"uuid":"0f5c33e2-a2d6-4f6e-9d2b-8a3c4e5f6a7b","name":"tippy"},{"uuid":"x","name":"Other"}]`), 0o644)
	for i := 0; i < 2; i++ {
		if said, err := s.PlayerAction(t.Context(), "allow", "tippymanticore8", ""); err != nil || said != "Added TippyManticore8 to the whitelist." {
			t.Fatalf("%q %v", said, err)
		}
	}
	var list []map[string]string
	b, _ := os.ReadFile(filepath.Join(s.Root(), "whitelist.json"))
	json.Unmarshal(b, &list)
	if len(list) != 2 || list[1]["name"] != "TippyManticore8" || list[1]["uuid"] != "0f5c33e2-a2d6-4f6e-9d2b-8a3c4e5f6a7b" {
		t.Fatalf("%s", b)
	}
	// Offline mode: the UUID the server itself would give the name, without asking Mojang.
	os.WriteFile(filepath.Join(s.Root(), "server.properties"), []byte("online-mode=false\n"), 0o644)
	fakeMojang(t, 100, nil)
	if _, err := s.PlayerAction(t.Context(), "op", "Notch", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(s.Root(), "ops.json"))
	if !strings.Contains(string(b), "b50ad385-829d-3141-a216-7e7d7539ba7f") {
		t.Fatalf("%s", b)
	}
	if _, err := s.PlayerAction(t.Context(), "kick", "Notch", ""); err == nil {
		t.Fatal("kicked from a stopped server")
	}
	// Mojang busy on an online server: a clear reason, and nothing written.
	os.WriteFile(filepath.Join(s.Root(), "server.properties"), []byte("online-mode=true\n"), 0o644)
	if _, err := s.PlayerAction(t.Context(), "ban", "Someone", ""); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatal(err)
	}
}
