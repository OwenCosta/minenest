package nest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Player names and who they are: MineNest checks a name strictly before it goes anywhere near the
// console, and looks it up itself (its UUID and exact spelling) instead of leaning only on the
// server's own lookup, which fails whenever Mojang's servers are busy for a moment.

var (
	// A Java name: letters, digits and underscore, 1 to 16 of them.
	javaNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)
	// A Bedrock gamertag as Floodgate shows it, after its prefix (Floodgate turns spaces into
	// underscores).
	gamertagRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)
	// What a Floodgate prefix may be: a few punctuation marks, never a space or a letter.
	floodgatePrefixRe = regexp.MustCompile(`^[.!*~+\-#$%&=^]{1,3}$`)
)

var (
	errNoSuchPlayer = errors.New("no such player")
	errMojangBusy   = errors.New("Mojang's servers are busy")
)

// player is a checked name: a Java player, or a Bedrock player through Floodgate (Name has the
// prefix, Gamertag hasn't).
type player struct {
	Name     string
	Bedrock  bool
	Gamertag string
}

// floodgatePrefix is the prefix Floodgate puts before Bedrock players' names, or "" when the
// server hasn't Floodgate.
func (s *Server) floodgatePrefix() string {
	entries, _ := os.ReadDir(filepath.Join(s.Root(), "plugins"))
	has := false
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.Contains(n, "floodgate") && strings.HasSuffix(n, ".jar") {
			has = true
		}
	}
	if !has {
		return ""
	}
	prefix := "."
	if b, err := os.ReadFile(filepath.Join(s.Root(), "plugins", "floodgate", "config.yml")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "username-prefix:"); ok {
				v = strings.TrimSpace(v)
				if i := strings.Index(v, " #"); i >= 0 {
					v = strings.TrimSpace(v[:i])
				}
				prefix = strings.Trim(v, `"'`)
			}
		}
	}
	if prefix != "" && !floodgatePrefixRe.MatchString(prefix) {
		return "" // something MineNest can't check safely; Bedrock names then aren't taken
	}
	return prefix
}

// checkName turns what someone typed into a player, refusing anything that isn't a name.
func checkName(raw, floodgatePrefix string) (player, error) {
	name := strings.TrimSpace(raw)
	if javaNameRe.MatchString(name) {
		return player{Name: name}, nil
	}
	if floodgatePrefix != "" {
		if tag, ok := strings.CutPrefix(name, floodgatePrefix); ok && gamertagRe.MatchString(tag) {
			return player{Name: name, Bedrock: true, Gamertag: tag}, nil
		}
	}
	shown := name
	if len(shown) > 40 {
		shown = shown[:40] + "..."
	}
	shown = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, shown)
	if floodgatePrefix != "" {
		return player{}, fmt.Errorf("%q isn't a player's name. Java names have 1 to 16 letters, digits or underscores; Bedrock players start with %s", shown, floodgatePrefix)
	}
	return player{}, fmt.Errorf("%q isn't a Minecraft name. Names have 1 to 16 letters, digits or underscores", shown)
}

// ------------------------------------------------------------------ Mojang's lookup

type mcProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (p mcProfile) uuid() string {
	id := p.ID
	return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

// Where names are looked up, in order (a test points these elsewhere).
var profileLookups = []string{
	"https://api.minecraftservices.com/minecraft/profile/lookup/name/",
	"https://api.mojang.com/users/profiles/minecraft/",
}

type cachedProfile struct {
	p     mcProfile
	err   error
	until time.Time
}

// profileCacheMax bounds the cache: when it is full, what has run out goes, and then the soonest to.
const profileCacheMax = 1000

func cacheProfile(key string, c cachedProfile) {
	profileCache.Lock()
	defer profileCache.Unlock()
	if len(profileCache.m) >= profileCacheMax {
		now := time.Now()
		for k, v := range profileCache.m {
			if now.After(v.until) {
				delete(profileCache.m, k)
			}
		}
		for k := range profileCache.m {
			if len(profileCache.m) < profileCacheMax {
				break
			}
			delete(profileCache.m, k)
		}
	}
	profileCache.m[key] = c
}

var profileCache = struct {
	sync.Mutex
	m map[string]cachedProfile
}{m: map[string]cachedProfile{}}

var hex32Re = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// lookupProfile finds a Java player's UUID and exact name. It answers errNoSuchPlayer when
// Mojang says there's nobody by that name, and errMojangBusy when Mojang couldn't answer.
func lookupProfile(ctx context.Context, name string) (mcProfile, error) {
	key := strings.ToLower(name)
	profileCache.Lock()
	if c, ok := profileCache.m[key]; ok && time.Now().Before(c.until) {
		profileCache.Unlock()
		return c.p, c.err
	}
	profileCache.Unlock()
	notFound := false
	for _, base := range profileLookups {
		p, err := lookupOnce(ctx, base+name, name)
		if err == nil {
			cacheProfile(key, cachedProfile{p: p, until: time.Now().Add(time.Hour)})
			return p, nil
		}
		if errors.Is(err, errNoSuchPlayer) {
			notFound = true
		}
		if ctx.Err() != nil {
			return mcProfile{}, errMojangBusy
		}
	}
	if notFound {
		cacheProfile(key, cachedProfile{err: errNoSuchPlayer, until: time.Now().Add(2 * time.Minute)})
		return mcProfile{}, errNoSuchPlayer
	}
	return mcProfile{}, errMojangBusy
}

// lookupOnce asks one address about name. An answer that is about somebody else is not an answer:
// a wrong or hostile reply must never get a different player opped, banned or whitelisted.
func lookupOnce(ctx context.Context, url, name string) (mcProfile, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return mcProfile{}, err
	}
	req.Header.Set("User-Agent", agent())
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return mcProfile{}, errMojangBusy
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case 200:
		var p mcProfile
		if json.Unmarshal(b, &p) == nil && hex32Re.MatchString(p.ID) && javaNameRe.MatchString(p.Name) && strings.EqualFold(p.Name, name) {
			return p, nil
		}
		return mcProfile{}, errMojangBusy // an HTML page or something else that isn't an answer
	case 204, 404:
		return mcProfile{}, errNoSuchPlayer
	}
	return mcProfile{}, errMojangBusy // 429 (too many lookups), 5xx, a "Service unavailable" page
}

// Where a Bedrock gamertag's XUID is looked up (GeyserMC's global API).
var xuidLookup = "https://api.geysermc.org/v2/xbox/xuid/"

// floodgateUUID is the UUID Floodgate gives a Bedrock player: zeros, then the XUID.
func floodgateUUID(xuid uint64) string {
	h := fmt.Sprintf("%016x", xuid)
	return "00000000-0000-0000-" + h[0:4] + "-" + h[4:]
}

func lookupXUID(ctx context.Context, gamertag string) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", xuidLookup+gamertag, nil)
	req.Header.Set("User-Agent", agent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("couldn't reach GeyserMC to look %s up. Try again in a moment", gamertag)
	}
	defer resp.Body.Close()
	var v struct {
		XUID uint64 `json:"xuid"`
	}
	if resp.StatusCode == 200 && json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v) == nil && v.XUID >= 1e15 && v.XUID < 1e17 { // an Xbox user id has 16 digits
		return v.XUID, nil
	}
	if resp.StatusCode == 400 || resp.StatusCode == 404 || resp.StatusCode == 200 {
		return 0, fmt.Errorf("there's no Bedrock player called %s", gamertag)
	}
	return 0, fmt.Errorf("GeyserMC couldn't look %s up just now (%s). Try again in a moment", gamertag, resp.Status)
}
