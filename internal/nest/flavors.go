package nest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A Flavor is a kind of server: Minecraft's own, Paper and Purpur (plugins), Fabric, Forge and
// NeoForge (mods).
type Flavor struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	About       string   `json:"about"`
	AddonKind   string   `json:"addon_kind"`   // "plugin", "mod" or "datapack"
	AddonFolder string   `json:"addon_folder"` // plugins, mods, or world/datapacks
	Loaders     []string `json:"loaders"`      // Modrinth's loader names that fit
}

var Flavors = []Flavor{
	{"paper", "Paper", "Fast, and takes plugins. The best choice for most servers.", "plugin", "plugins", []string{"paper", "spigot", "bukkit"}},
	{"vanilla", "Vanilla", "Minecraft's own server, exactly as Mojang makes it. Takes data packs.", "datapack", "world/datapacks", []string{"datapack"}},
	{"fabric", "Fabric", "Lightweight mods, quick to update to new Minecraft versions.", "mod", "mods", []string{"fabric"}},
	{"neoforge", "NeoForge", "Big mods and modpacks (the newer Forge).", "mod", "mods", []string{"neoforge"}},
	{"forge", "Forge", "Classic mods and older modpacks.", "mod", "mods", []string{"forge"}},
	{"purpur", "Purpur", "Paper with extra settings to tweak. Takes plugins.", "plugin", "plugins", []string{"purpur", "paper", "spigot", "bukkit"}},
}

func flavorByID(id string) (Flavor, bool) {
	for _, f := range Flavors {
		if f.ID == id {
			return f, true
		}
	}
	return Flavor{}, false
}

// Installed is what's in a server's folder now: what to run and how.
type Installed struct {
	Flavor    string   `json:"flavor"`
	Minecraft string   `json:"minecraft"`
	Build     string   `json:"build,omitempty"` // Paper's build, Fabric's loader, Forge's version...
	Java      int      `json:"java"`
	Launch    []string `json:"launch"` // after java and the memory flags: ["-jar", "server.jar"] or ["@libraries/.../unix_args.txt"]
}

// ------------------------------------------------------------------ versions

const (
	mojangManifest = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"
	paperAPI       = "https://fill.papermc.io/v3/projects/paper"
	purpurAPI      = "https://api.purpurmc.org/v2/purpur"
	fabricMeta     = "https://meta.fabricmc.net/v2"
	forgePromos    = "https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json"
	forgeMaven     = "https://maven.minecraftforge.net/net/minecraftforge/forge"
	neoAPI         = "https://maven.neoforged.net/api/maven/versions/releases/net/neoforged/neoforge"
	neoMaven       = "https://maven.neoforged.net/releases/net/neoforged/neoforge"
)

// isRelease is a plain release id: "1.21.4", "26.3" (no -rc, -pre, snapshots).
func isRelease(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// versionLess compares release ids numerically: 1.21.9 < 1.21.11 < 26.1.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func sortNewestFirst(v []string) []string {
	sort.SliceStable(v, func(i, j int) bool { return versionLess(v[j], v[i]) })
	return v
}

type mojangManifestDoc struct {
	Latest   struct{ Release string } `json:"latest"`
	Versions []struct {
		ID, Type, URL string
	} `json:"versions"`
}

// Versions lists the Minecraft versions a flavor can run, newest first.
func Versions(ctx context.Context, flavor string) ([]string, error) {
	var out []string
	switch flavor {
	case "vanilla":
		var m mojangManifestDoc
		if err := getJSON(ctx, mojangManifest, time.Hour, &m); err != nil {
			return nil, err
		}
		for _, v := range m.Versions {
			if v.Type == "release" {
				out = append(out, v.ID)
			}
		}
	case "paper":
		var doc struct{ Versions map[string][]string }
		if err := getJSON(ctx, paperAPI, time.Hour, &doc); err != nil {
			return nil, err
		}
		for _, fam := range doc.Versions {
			for _, v := range fam {
				if isRelease(v) {
					out = append(out, v)
				}
			}
		}
	case "purpur":
		var doc struct{ Versions []string }
		if err := getJSON(ctx, purpurAPI, time.Hour, &doc); err != nil {
			return nil, err
		}
		for _, v := range doc.Versions {
			if isRelease(v) {
				out = append(out, v)
			}
		}
	case "fabric":
		var doc []struct {
			Version string
			Stable  bool
		}
		if err := getJSON(ctx, fabricMeta+"/versions/game", time.Hour, &doc); err != nil {
			return nil, err
		}
		for _, v := range doc {
			if v.Stable && isRelease(v.Version) {
				out = append(out, v.Version)
			}
		}
	case "forge":
		var doc struct{ Promos map[string]string }
		if err := getJSON(ctx, forgePromos, time.Hour, &doc); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for k := range doc.Promos {
			mc := strings.TrimSuffix(strings.TrimSuffix(k, "-latest"), "-recommended")
			if isRelease(mc) && !seen[mc] {
				seen[mc] = true
				out = append(out, mc)
			}
		}
	case "neoforge":
		vs, err := neoVersions(ctx)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, v := range vs {
			if mc := neoMinecraft(v); mc != "" && !seen[mc] {
				seen[mc] = true
				out = append(out, mc)
			}
		}
	default:
		return nil, fmt.Errorf("MineNest doesn't know a server type called %q", flavor)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the list of versions came back empty; try again later")
	}
	return sortNewestFirst(out), nil
}

func neoVersions(ctx context.Context) ([]string, error) {
	var doc struct{ Versions []string }
	if err := getJSON(ctx, neoAPI, time.Hour, &doc); err != nil {
		return nil, err
	}
	return doc.Versions, nil
}

// neoMinecraft is the Minecraft version a NeoForge version is for: 21.1.77 is 1.21.1, 21.0.5 is
// 1.21, and since Minecraft's own numbering changed, 26.3.0.110 is 26.3 (26.1.2.4 is 26.1.2).
func neoMinecraft(v string) string {
	base := strings.SplitN(v, "-", 2)[0]
	p := strings.Split(base, ".")
	if len(p) < 3 {
		return ""
	}
	major, _ := strconv.Atoi(p[0])
	if major >= 25 && len(p) >= 4 {
		if p[2] == "0" {
			return p[0] + "." + p[1]
		}
		return p[0] + "." + p[1] + "." + p[2]
	}
	if p[1] == "0" {
		return "1." + p[0]
	}
	return "1." + p[0] + "." + p[1]
}

func latestVersion(ctx context.Context, flavor string) (string, error) {
	vs, err := Versions(ctx, flavor)
	if err != nil {
		return "", err
	}
	if flavor == "vanilla" || flavor == "fabric" {
		return vs[0], nil
	}
	// Paper and the mod loaders can lag behind a new Minecraft: the newest they have.
	return vs[0], nil
}

// mojangVersion is Minecraft's own description of a version: its server download and Java.
type mojangVersion struct {
	Downloads struct {
		Server struct {
			URL  string
			SHA1 string
			Size int64
		}
	}
	JavaVersion struct{ MajorVersion int } `json:"javaVersion"`
}

func getMojangVersion(ctx context.Context, mc string) (*mojangVersion, error) {
	var m mojangManifestDoc
	if err := getJSON(ctx, mojangManifest, time.Hour, &m); err != nil {
		return nil, err
	}
	for _, v := range m.Versions {
		if v.ID == mc {
			var mv mojangVersion
			if err := getJSON(ctx, v.URL, 30*24*time.Hour, &mv); err != nil {
				return nil, err
			}
			if mv.JavaVersion.MajorVersion == 0 {
				mv.JavaVersion.MajorVersion = 8
			}
			return &mv, nil
		}
	}
	return nil, fmt.Errorf("Mojang doesn't list Minecraft %s", mc)
}

// javaFor is the Java a Minecraft version needs (Mojang says), with a guess if Mojang can't be asked.
func javaFor(ctx context.Context, mc string) int {
	if mv, err := getMojangVersion(ctx, mc); err == nil {
		return mv.JavaVersion.MajorVersion
	}
	switch {
	case !versionLess(mc, "26.1"):
		return 25
	case !versionLess(mc, "1.20.5"):
		return 21
	case !versionLess(mc, "1.18"):
		return 17
	case !versionLess(mc, "1.17"):
		return 16
	}
	return 8
}

// ------------------------------------------------------------------ install

// installFlavor puts the server software for flavor/mc into dir (the server's folder), with
// Java, and says how to run it. pin, when set, asks for one build (a modpack's loader version).
func installFlavor(ctx context.Context, dir, flavor, mc, pin string, job *Job) (*Installed, error) {
	var err error
	if mc == "" || mc == "latest" {
		if mc, err = latestVersion(ctx, flavor); err != nil {
			return nil, err
		}
	}
	inst := &Installed{Flavor: flavor, Minecraft: mc, Launch: []string{"-jar", "server.jar"}}
	job.Say("Looking up Minecraft %s", mc)
	inst.Java = javaFor(ctx, mc)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	java, err := ensureJava(ctx, inst.Java, job)
	if err != nil {
		return nil, err
	}
	jar := filepath.Join(dir, "server.jar")
	switch flavor {
	case "vanilla":
		mv, err := getMojangVersion(ctx, mc)
		if err != nil {
			return nil, err
		}
		if mv.Downloads.Server.URL == "" {
			return nil, fmt.Errorf("Minecraft %s has no server download", mc)
		}
		err = download(ctx, mv.Downloads.Server.URL, jar, Hash{"sha1", mv.Downloads.Server.SHA1}, job, "Minecraft "+mc)
		if err != nil {
			return nil, err
		}
	case "paper":
		var builds []struct {
			ID        int64
			Channel   string
			Downloads map[string]struct {
				URL       string
				Checksums struct{ SHA256 string }
			}
		}
		if err := getJSON(ctx, fmt.Sprintf("%s/versions/%s/builds", paperAPI, mc), 10*time.Minute, &builds); err != nil {
			return nil, err
		}
		pick := -1
		for i, b := range builds {
			if pin != "" && fmt.Sprint(b.ID) != pin {
				continue
			}
			if b.Channel == "STABLE" {
				pick = i
				break
			}
			if pick < 0 {
				pick = i
			}
		}
		if pick < 0 {
			return nil, fmt.Errorf("Paper has no build for Minecraft %s yet", mc)
		}
		dl, ok := builds[pick].Downloads["server:default"]
		if !ok {
			return nil, fmt.Errorf("Paper's build for %s has no server download", mc)
		}
		inst.Build = fmt.Sprint(builds[pick].ID)
		if err := download(ctx, dl.URL, jar, Hash{"sha256", dl.Checksums.SHA256}, job, "Paper "+mc); err != nil {
			return nil, err
		}
	case "purpur":
		var info struct {
			Build string
			MD5   string
		}
		if err := getJSON(ctx, fmt.Sprintf("%s/%s/latest", purpurAPI, mc), 10*time.Minute, &info); err != nil {
			return nil, err
		}
		inst.Build = info.Build
		if err := download(ctx, fmt.Sprintf("%s/%s/%s/download", purpurAPI, mc, info.Build), jar, Hash{"md5", info.MD5}, job, "Purpur "+mc); err != nil {
			return nil, err
		}
	case "fabric":
		loader := pin
		if loader == "" {
			var loaders []struct {
				Version string
				Stable  bool
			}
			if err := getJSON(ctx, fabricMeta+"/versions/loader", time.Hour, &loaders); err != nil {
				return nil, err
			}
			for _, l := range loaders {
				if l.Stable {
					loader = l.Version
					break
				}
			}
		}
		var installers []struct {
			Version string
			Stable  bool
		}
		if err := getJSON(ctx, fabricMeta+"/versions/installer", time.Hour, &installers); err != nil {
			return nil, err
		}
		installer := ""
		for _, i := range installers {
			if i.Stable {
				installer = i.Version
				break
			}
		}
		if loader == "" || installer == "" {
			return nil, fmt.Errorf("Fabric's lists of loaders came back empty")
		}
		inst.Build = loader
		url := fmt.Sprintf("%s/versions/loader/%s/%s/%s/server/jar", fabricMeta, mc, loader, installer)
		if err := download(ctx, url, jar, Hash{}, job, "Fabric "+mc); err != nil {
			return nil, err
		}
		// Fabric's launcher fetches Minecraft's own server.jar on its first start, as
		// vanilla server.jar beside it. Getting it now, checked, means that start doesn't
		// need to.
		if mv, err := getMojangVersion(ctx, mc); err == nil && mv.Downloads.Server.URL != "" {
			download(ctx, mv.Downloads.Server.URL, filepath.Join(dir, "vanilla-server.jar"), Hash{"sha1", mv.Downloads.Server.SHA1}, job, "Minecraft "+mc)
			os.WriteFile(filepath.Join(dir, "fabric-server-launcher.properties"), []byte("serverJar=vanilla-server.jar\n"), 0o644)
		}
	case "forge", "neoforge":
		v, url, err := forgeInstaller(ctx, flavor, mc, pin)
		if err != nil {
			return nil, err
		}
		inst.Build = v
		installer := filepath.Join(dir, "installer.jar")
		if err := download(ctx, url, installer, Hash{}, job, flavorName(flavor)+" "+v); err != nil {
			return nil, err
		}
		job.Say("Installing %s %s (this takes a minute or two)", flavorName(flavor), v)
		cmd := exec.CommandContext(ctx, java, "-jar", "installer.jar", "--installServer")
		cmd.Dir = dir
		hideWindow(cmd)
		out, err := cmd.CombinedOutput()
		os.WriteFile(filepath.Join(dir, "installer.log"), out, 0o644)
		os.Remove(installer)
		if err != nil {
			return nil, fmt.Errorf("%s's installer failed (see installer.log in the server's folder): %v", flavorName(flavor), err)
		}
		launch, err := forgeLaunch(dir, flavor, mc, v)
		if err != nil {
			return nil, err
		}
		inst.Launch = launch
		os.Remove(filepath.Join(dir, "server.jar")) // an earlier flavor's
	default:
		return nil, fmt.Errorf("MineNest doesn't know a server type called %q", flavor)
	}
	return inst, nil
}

func flavorName(id string) string {
	if f, ok := flavorByID(id); ok {
		return f.Name
	}
	return id
}

// forgeInstaller finds the Forge or NeoForge version for mc (or pin) and its installer's address.
func forgeInstaller(ctx context.Context, flavor, mc, pin string) (version, url string, err error) {
	if flavor == "forge" {
		v := pin
		if v == "" {
			var doc struct{ Promos map[string]string }
			if err := getJSON(ctx, forgePromos, time.Hour, &doc); err != nil {
				return "", "", err
			}
			v = doc.Promos[mc+"-recommended"]
			if v == "" {
				v = doc.Promos[mc+"-latest"]
			}
		}
		if v == "" {
			return "", "", fmt.Errorf("Forge has no version for Minecraft %s", mc)
		}
		full := mc + "-" + v
		if strings.HasPrefix(v, mc+"-") {
			full = v
		}
		return full, fmt.Sprintf("%s/%s/forge-%s-installer.jar", forgeMaven, full, full), nil
	}
	v := pin
	if v == "" {
		vs, err := neoVersions(ctx)
		if err != nil {
			return "", "", err
		}
		var stable, any string
		for _, x := range vs { // oldest first: the last match is the newest
			if neoMinecraft(x) != mc {
				continue
			}
			any = x
			if !strings.Contains(x, "-") {
				stable = x
			}
		}
		v = stable
		if v == "" {
			v = any
		}
	}
	if v == "" {
		return "", "", fmt.Errorf("NeoForge has no version for Minecraft %s", mc)
	}
	return v, fmt.Sprintf("%s/%s/neoforge-%s-installer.jar", neoMaven, v, v), nil
}

// forgeLaunch is how to run what Forge's or NeoForge's installer made: an argument file in
// libraries (1.17 and later), or a jar of its own (older Forge).
func forgeLaunch(dir, flavor, mc, version string) ([]string, error) {
	argsName := "unix_args.txt"
	if runtime.GOOS == "windows" {
		argsName = "win_args.txt"
	}
	lib := "libraries/net/minecraftforge/forge/" + version
	if flavor == "neoforge" {
		lib = "libraries/net/neoforged/neoforge/" + version
	}
	if exists(filepath.Join(dir, filepath.FromSlash(lib), argsName)) {
		return []string{"@" + lib + "/" + argsName}, nil
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "forge-*.jar"))
	for _, m := range matches {
		if !strings.Contains(m, "installer") {
			return []string{"-jar", filepath.Base(m)}, nil
		}
	}
	return nil, fmt.Errorf("%s's installer finished, but MineNest couldn't find how to start it", flavorName(flavor))
}
