package nest

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Mods, plugins and data packs ("add-ons") come from Modrinth: searched with the server's
// loader and Minecraft version, so only ones that fit show up, installed with the add-ons they
// need, checked against Modrinth's hashes, and kept track of in servers/<id>/addons.json so they
// can be updated and removed. Modrinth modpacks (.mrpack) become a whole server.

const modrinth = "https://api.modrinth.com/v2"

type AddonHit struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Downloads   int64    `json:"downloads"`
	Icon        string   `json:"icon"`
	Categories  []string `json:"categories"`
	Type        string   `json:"type"`
	Installed   bool     `json:"installed"`
}

type AddonSearch struct {
	Hits  []AddonHit `json:"hits"`
	Total int        `json:"total"`
	Kind  string     `json:"kind"`
}

type Addon struct {
	Project   string `json:"project"`
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Icon      string `json:"icon,omitempty"`
	VersionID string `json:"version_id"`
	Version   string `json:"version"`
	File      string `json:"file"`             // relative to the server's folder
	Needed    bool   `json:"needed"`           // installed because another one needs it
	Update    string `json:"update,omitempty"` // a newer version's number, once checked
}

type AddonList struct {
	Kind   string   `json:"kind"`
	Folder string   `json:"folder"`
	Addons []Addon  `json:"addons"`
	Other  []string `json:"other"` // files in the folder MineNest didn't install (by hand, or a modpack's)
}

func modrinthProjectType(kind string) string {
	switch kind {
	case "plugin":
		return "plugin"
	case "datapack":
		return "datapack"
	}
	return "mod"
}

func (s *Server) flavor() (Flavor, *Installed, error) {
	cfg := s.Config()
	if cfg.Installed == nil {
		return Flavor{}, nil, fmt.Errorf("the server software isn't installed yet")
	}
	f, ok := flavorByID(cfg.Installed.Flavor)
	if !ok {
		return Flavor{}, nil, fmt.Errorf("unknown server type")
	}
	return f, cfg.Installed, nil
}

func (s *Server) addonFolder(f Flavor) string {
	if f.AddonKind == "datapack" {
		return filepath.Join(s.Root(), s.props().GetDefault("level-name", "world"), "datapacks")
	}
	return filepath.Join(s.Root(), f.AddonFolder)
}

// SearchAddons searches Modrinth for add-ons that fit this server. An empty query lists the most
// popular ones.
func (s *Server) SearchAddons(ctx context.Context, query string, offset int) (*AddonSearch, error) {
	f, inst, err := s.flavor()
	if err != nil {
		return nil, err
	}
	loaders := []string{}
	for _, l := range f.Loaders {
		loaders = append(loaders, `"categories:`+l+`"`)
	}
	facets := fmt.Sprintf(`[[%s],["versions:%s"],["project_type:%s"],["server_side:required","server_side:optional"]]`,
		strings.Join(loaders, ","), inst.Minecraft, modrinthProjectType(f.AddonKind))
	index := "relevance"
	if strings.TrimSpace(query) == "" {
		index = "downloads"
	}
	u := fmt.Sprintf("%s/search?query=%s&facets=%s&index=%s&limit=20&offset=%d", modrinth,
		url.QueryEscape(query), url.QueryEscape(facets), index, max(0, offset))
	var res struct {
		Hits []struct {
			ProjectID   string   `json:"project_id"`
			Slug        string   `json:"slug"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Author      string   `json:"author"`
			Downloads   int64    `json:"downloads"`
			IconURL     string   `json:"icon_url"`
			Categories  []string `json:"categories"`
			ProjectType string   `json:"project_type"`
		} `json:"hits"`
		TotalHits int `json:"total_hits"`
	}
	if err := getJSON(ctx, u, 5*time.Minute, &res); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, a := range s.addons() {
		have[a.Project] = true
	}
	out := &AddonSearch{Total: res.TotalHits, Kind: f.AddonKind, Hits: []AddonHit{}}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, AddonHit{h.ProjectID, h.Slug, h.Title, h.Description, h.Author, h.Downloads, h.IconURL, h.Categories, h.ProjectType, have[h.ProjectID]})
	}
	return out, nil
}

func (s *Server) addonsPath() string { return filepath.Join(s.Dir, "addons.json") }

func (s *Server) addons() []Addon {
	var v []Addon
	readJSON(s.addonsPath(), &v)
	return v
}

func (s *Server) saveAddons(v []Addon) error {
	sort.Slice(v, func(i, j int) bool { return strings.ToLower(v[i].Title) < strings.ToLower(v[j].Title) })
	return writeJSON(s.addonsPath(), v, 0o644)
}

func (s *Server) Addons() (*AddonList, error) {
	f, _, err := s.flavor()
	if err != nil {
		return nil, err
	}
	dir := s.addonFolder(f)
	rel, _ := filepath.Rel(s.Root(), dir)
	list := &AddonList{Kind: f.AddonKind, Folder: filepath.ToSlash(rel), Addons: []Addon{}, Other: []string{}}
	known := map[string]bool{}
	for _, a := range s.addons() {
		if exists(filepath.Join(s.Root(), filepath.FromSlash(a.File))) {
			list.Addons = append(list.Addons, a)
			known[strings.ToLower(filepath.Base(a.File))] = true
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || known[strings.ToLower(n)] {
			continue
		}
		if strings.HasSuffix(strings.ToLower(n), ".jar") || strings.HasSuffix(strings.ToLower(n), ".zip") {
			list.Other = append(list.Other, n)
		}
	}
	return list, nil
}

type mrVersion struct {
	ID            string   `json:"id"`
	ProjectID     string   `json:"project_id"`
	Name          string   `json:"name"`
	VersionNumber string   `json:"version_number"`
	VersionType   string   `json:"version_type"`
	Loaders       []string `json:"loaders"`
	GameVersions  []string `json:"game_versions"`
	Files         []struct {
		URL      string            `json:"url"`
		Filename string            `json:"filename"`
		Primary  bool              `json:"primary"`
		Hashes   map[string]string `json:"hashes"`
		Size     int64             `json:"size"`
	} `json:"files"`
	Dependencies []struct {
		ProjectID      string `json:"project_id"`
		VersionID      string `json:"version_id"`
		DependencyType string `json:"dependency_type"`
	} `json:"dependencies"`
}

type mrProject struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	IconURL     string `json:"icon_url"`
	ProjectType string `json:"project_type"`
	ServerSide  string `json:"server_side"`
}

func getProject(ctx context.Context, ref string) (*mrProject, error) {
	var p mrProject
	if err := getJSON(ctx, modrinth+"/project/"+url.PathEscape(ref), 10*time.Minute, &p); err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil, fmt.Errorf("Modrinth has nothing called %q", ref)
		}
		return nil, err
	}
	return &p, nil
}

// bestVersion is the newest version of a project for these loaders and Minecraft version: a
// release if there is one.
func bestVersion(ctx context.Context, project string, loaders []string, mc string) (*mrVersion, error) {
	lj, _ := json.Marshal(loaders)
	u := fmt.Sprintf("%s/project/%s/version?loaders=%s&game_versions=%s", modrinth, url.PathEscape(project),
		url.QueryEscape(string(lj)), url.QueryEscape(`["`+mc+`"]`))
	var vs []mrVersion
	if err := getJSON(ctx, u, 5*time.Minute, &vs); err != nil {
		return nil, err
	}
	for _, v := range vs {
		if v.VersionType == "release" {
			return &v, nil
		}
	}
	if len(vs) > 0 {
		return &vs[0], nil
	}
	return nil, nil
}

func getVersion(ctx context.Context, id string) (*mrVersion, error) {
	var v mrVersion
	if err := getJSON(ctx, modrinth+"/version/"+url.PathEscape(id), time.Hour, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// InstallAddon installs a Modrinth project (id or slug) and the ones it needs, in a job.
func (m *Manager) InstallAddon(s *Server, ref string) (*Job, error) {
	f, inst, err := s.flavor()
	if err != nil {
		return nil, err
	}
	if err := s.setBusy("installing "+ref, false); err != nil {
		return nil, err
	}
	return m.Jobs.start("Installing "+ref, s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		added := []string{}
		if err := s.installAddon(ctx, f, inst, ref, false, map[string]bool{}, &added, j); err != nil {
			return nil, err
		}
		if s.State() != Stopped {
			s.Note("Installed %s. Restart the server to load it.", strings.Join(added, ", "))
		} else {
			s.Note("Installed %s.", strings.Join(added, ", "))
		}
		return added, nil
	}), nil
}

func (s *Server) installAddon(ctx context.Context, f Flavor, inst *Installed, ref string, needed bool, seen map[string]bool, added *[]string, j *Job) error {
	p, err := getProject(ctx, ref)
	if err != nil {
		return err
	}
	if seen[p.ID] {
		return nil
	}
	seen[p.ID] = true
	if p.ProjectType == "modpack" {
		return fmt.Errorf("%s is a modpack: make a new server from it instead", p.Title)
	}
	if p.ServerSide == "unsupported" {
		return fmt.Errorf("%s only works in the game itself, not on a server", p.Title)
	}
	existing := s.addons()
	for _, a := range existing {
		if a.Project == p.ID {
			if !needed {
				return fmt.Errorf("%s is already installed", p.Title)
			}
			return nil
		}
	}
	j.Say("Finding %s for %s %s", p.Title, f.Name, inst.Minecraft)
	v, err := bestVersion(ctx, p.ID, f.Loaders, inst.Minecraft)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("%s has no version for %s %s yet", p.Title, f.Name, inst.Minecraft)
	}
	file := -1
	for i, fl := range v.Files {
		if fl.Primary || file < 0 {
			file = i
		}
		if fl.Primary {
			break
		}
	}
	if file < 0 {
		return fmt.Errorf("%s's version %s has no file", p.Title, v.VersionNumber)
	}
	fl := v.Files[file]
	name := filepath.Base(fl.Filename)
	if name == "." || name == "" || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%s's file has a strange name", p.Title)
	}
	dir := s.addonFolder(f)
	dest := filepath.Join(dir, name)
	h := Hash{"sha512", fl.Hashes["sha512"]}
	if h.Hex == "" {
		h = Hash{"sha1", fl.Hashes["sha1"]}
	}
	if err := download(ctx, fl.URL, dest, h, j, p.Title); err != nil {
		return err
	}
	rel, _ := filepath.Rel(s.Root(), dest)
	existing = append(existing, Addon{Project: p.ID, Slug: p.Slug, Title: p.Title, Icon: p.IconURL, VersionID: v.ID,
		Version: v.VersionNumber, File: filepath.ToSlash(rel), Needed: needed})
	if err := s.saveAddons(existing); err != nil {
		return err
	}
	*added = append(*added, p.Title)
	for _, d := range v.Dependencies {
		if d.DependencyType != "required" {
			continue
		}
		dep := d.ProjectID
		if dep == "" && d.VersionID != "" {
			if dv, err := getVersion(ctx, d.VersionID); err == nil {
				dep = dv.ProjectID
			}
		}
		if dep == "" {
			continue
		}
		if err := s.installAddon(ctx, f, inst, dep, true, seen, added, j); err != nil {
			return fmt.Errorf("%s needs another add-on that couldn't be installed: %v", p.Title, err)
		}
	}
	return nil
}

// RemoveAddon removes an installed add-on (by project id, slug or file name in the folder).
func (s *Server) RemoveAddon(ref string) (string, error) {
	f, _, err := s.flavor()
	if err != nil {
		return "", err
	}
	list := s.addons()
	for i, a := range list {
		if a.Project == ref || strings.EqualFold(a.Slug, ref) || strings.EqualFold(a.Title, ref) || strings.EqualFold(filepath.Base(a.File), ref) {
			p := filepath.Join(s.Root(), filepath.FromSlash(a.File))
			if within(s.Root(), p) {
				os.Remove(p)
			}
			s.saveAddons(append(list[:i:i], list[i+1:]...))
			return a.Title, nil
		}
	}
	// A file put there by hand.
	name := filepath.Base(ref)
	p := filepath.Join(s.addonFolder(f), name)
	if name != ref || !exists(p) {
		return "", fmt.Errorf("%q isn't installed", ref)
	}
	return name, os.Remove(p)
}

// CheckUpdates looks for newer versions of every installed add-on.
func (s *Server) CheckUpdates(ctx context.Context) ([]Addon, error) {
	f, inst, err := s.flavor()
	if err != nil {
		return nil, err
	}
	list := s.addons()
	for i := range list {
		v, err := bestVersion(ctx, list[i].Project, f.Loaders, inst.Minecraft)
		list[i].Update = ""
		if err == nil && v != nil && v.ID != list[i].VersionID {
			list[i].Update = v.VersionNumber
		}
	}
	s.saveAddons(list)
	return list, nil
}

// UpdateAddons updates every add-on that has a newer version (or just ref's).
func (m *Manager) UpdateAddons(s *Server, ref string) (*Job, error) {
	f, inst, err := s.flavor()
	if err != nil {
		return nil, err
	}
	if err := s.setBusy("updating add-ons", false); err != nil {
		return nil, err
	}
	return m.Jobs.start("Updating add-ons", s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		list := s.addons()
		updated := []string{}
		for i, a := range list {
			if ref != "" && a.Project != ref && !strings.EqualFold(a.Slug, ref) {
				continue
			}
			j.Say("Checking %s", a.Title)
			v, err := bestVersion(ctx, a.Project, f.Loaders, inst.Minecraft)
			if err != nil || v == nil || v.ID == a.VersionID || len(v.Files) == 0 {
				continue
			}
			fl := v.Files[0]
			for _, x := range v.Files {
				if x.Primary {
					fl = x
				}
			}
			name := filepath.Base(fl.Filename)
			dest := filepath.Join(s.addonFolder(f), name)
			if err := download(ctx, fl.URL, dest+".new", Hash{"sha512", fl.Hashes["sha512"]}, j, a.Title); err != nil {
				return updated, err
			}
			old := filepath.Join(s.Root(), filepath.FromSlash(a.File))
			if within(s.Root(), old) {
				os.Remove(old)
			}
			os.Rename(dest+".new", dest)
			rel, _ := filepath.Rel(s.Root(), dest)
			list[i].File, list[i].VersionID, list[i].Version, list[i].Update = filepath.ToSlash(rel), v.ID, v.VersionNumber, ""
			updated = append(updated, a.Title+" "+v.VersionNumber)
		}
		s.saveAddons(list)
		if len(updated) > 0 {
			s.Note("Updated %s.", strings.Join(updated, ", "))
		}
		return updated, nil
	}), nil
}

// AddAddonFile puts a jar or zip the user chose into the add-ons folder.
func (s *Server) AddAddonFile(name string, r io.Reader) error {
	f, _, err := s.flavor()
	if err != nil {
		return err
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	low := strings.ToLower(name)
	if !strings.HasSuffix(low, ".jar") && !strings.HasSuffix(low, ".zip") {
		return fmt.Errorf("only .jar and .zip files go in %s", f.AddonFolder)
	}
	dest := filepath.Join(s.addonFolder(f), name)
	os.MkdirAll(filepath.Dir(dest), 0o755)
	out, err := os.Create(dest + ".part")
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(r, 512<<20))
	out.Close()
	if err != nil {
		os.Remove(dest + ".part")
		return err
	}
	return os.Rename(dest+".part", dest)
}

// ------------------------------------------------------------------ modpacks

type PackHit = AddonHit

// SearchModpacks searches Modrinth's modpacks that can run as a server.
func SearchModpacks(ctx context.Context, query string, offset int) (*AddonSearch, error) {
	facets := `[["project_type:modpack"],["server_side:required","server_side:optional"],["categories:fabric","categories:forge","categories:neoforge"]]`
	index := "relevance"
	if strings.TrimSpace(query) == "" {
		index = "downloads"
	}
	u := fmt.Sprintf("%s/search?query=%s&facets=%s&index=%s&limit=20&offset=%d", modrinth, url.QueryEscape(query), url.QueryEscape(facets), index, max(0, offset))
	var res struct {
		Hits []struct {
			ProjectID   string   `json:"project_id"`
			Slug        string   `json:"slug"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Author      string   `json:"author"`
			Downloads   int64    `json:"downloads"`
			IconURL     string   `json:"icon_url"`
			Categories  []string `json:"categories"`
		} `json:"hits"`
		TotalHits int `json:"total_hits"`
	}
	if err := getJSON(ctx, u, 5*time.Minute, &res); err != nil {
		return nil, err
	}
	out := &AddonSearch{Total: res.TotalHits, Kind: "modpack", Hits: []AddonHit{}}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, AddonHit{h.ProjectID, h.Slug, h.Title, h.Description, h.Author, h.Downloads, h.IconURL, h.Categories, "modpack", false})
	}
	return out, nil
}

type mrIndex struct {
	Name         string            `json:"name"`
	VersionID    string            `json:"versionId"`
	Dependencies map[string]string `json:"dependencies"`
	Files        []struct {
		Path      string            `json:"path"`
		Hashes    map[string]string `json:"hashes"`
		Env       map[string]string `json:"env"`
		Downloads []string          `json:"downloads"`
		FileSize  int64             `json:"fileSize"`
	} `json:"files"`
}

// installModpack makes s the modpack: its loader and Minecraft version, its server-side files
// and its overrides.
func installModpack(ctx context.Context, s *Server, ref, wantVersion string, j *Job) error {
	p, err := getProject(ctx, ref)
	if err != nil {
		return err
	}
	if p.ProjectType != "modpack" {
		return fmt.Errorf("%s isn't a modpack", p.Title)
	}
	j.Say("Finding %s", p.Title)
	var v *mrVersion
	if wantVersion != "" {
		v, err = getVersion(ctx, wantVersion)
		if err != nil {
			return err
		}
	} else {
		var vs []mrVersion
		if err := getJSON(ctx, modrinth+"/project/"+p.ID+"/version", 5*time.Minute, &vs); err != nil {
			return err
		}
		for i := range vs {
			if vs[i].VersionType == "release" {
				v = &vs[i]
				break
			}
		}
		if v == nil && len(vs) > 0 {
			v = &vs[0]
		}
	}
	if v == nil || len(v.Files) == 0 {
		return fmt.Errorf("%s has no version to download", p.Title)
	}
	fl := v.Files[0]
	for _, x := range v.Files {
		if x.Primary {
			fl = x
		}
	}
	pack := filepath.Join(s.Dir, "modpack.mrpack")
	if err := download(ctx, fl.URL, pack, Hash{"sha512", fl.Hashes["sha512"]}, j, p.Title); err != nil {
		return err
	}
	zr, err := zip.OpenReader(pack)
	if err != nil {
		return fmt.Errorf("the modpack couldn't be opened: %v", err)
	}
	defer zr.Close()
	var idx mrIndex
	for _, f := range zr.File {
		if f.Name == "modrinth.index.json" {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = json.NewDecoder(rc).Decode(&idx)
			rc.Close()
			if err != nil {
				return fmt.Errorf("the modpack's list of files couldn't be read: %v", err)
			}
		}
	}
	mc := idx.Dependencies["minecraft"]
	if mc == "" {
		return fmt.Errorf("the modpack doesn't say which Minecraft it's for")
	}
	flavor, pin := "vanilla", ""
	switch {
	case idx.Dependencies["fabric-loader"] != "":
		flavor, pin = "fabric", idx.Dependencies["fabric-loader"]
	case idx.Dependencies["neoforge"] != "":
		flavor, pin = "neoforge", idx.Dependencies["neoforge"]
	case idx.Dependencies["forge"] != "":
		flavor, pin = "forge", idx.Dependencies["forge"]
	case idx.Dependencies["quilt-loader"] != "":
		return fmt.Errorf("%s uses Quilt, which MineNest doesn't run yet", p.Title)
	}
	inst, err := installFlavor(ctx, s.Root(), flavor, mc, pin, j)
	if err != nil {
		return err
	}
	var total int64
	for _, f := range idx.Files {
		if f.Env["server"] != "unsupported" {
			total += f.FileSize
		}
	}
	var done int64
	for _, f := range idx.Files {
		if f.Env["server"] == "unsupported" || len(f.Downloads) == 0 {
			continue
		}
		dest := filepath.Join(s.Root(), filepath.FromSlash(f.Path))
		if !within(s.Root(), dest) {
			return fmt.Errorf("the modpack has a file outside the server's folder (%s)", f.Path)
		}
		h := Hash{"sha512", f.Hashes["sha512"]}
		if h.Hex == "" {
			h = Hash{"sha1", f.Hashes["sha1"]}
		}
		if err := download(ctx, f.Downloads[0], dest, h, nil, f.Path); err != nil {
			return err
		}
		done += f.FileSize
		j.Progress("Downloading the modpack's files", done, total)
	}
	j.Say("Copying the modpack's settings")
	for _, prefix := range []string{"overrides/", "server-overrides/"} {
		for _, f := range zr.File {
			if !strings.HasPrefix(f.Name, prefix) || f.FileInfo().IsDir() {
				continue
			}
			dest := filepath.Join(s.Root(), filepath.FromSlash(strings.TrimPrefix(f.Name, prefix)))
			if !within(s.Root(), dest) {
				continue
			}
			os.MkdirAll(filepath.Dir(dest), 0o755)
			rc, err := f.Open()
			if err != nil {
				return err
			}
			out, err := os.Create(dest)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(out, rc)
			rc.Close()
			out.Close()
			if err != nil {
				return err
			}
		}
	}
	zr.Close()
	os.Remove(pack)
	s.update(func(c *Config) {
		c.Installed = inst
		c.Flavor = flavor
		c.Want = mc
		c.Modpack = p.Title + " " + v.VersionNumber
	})
	s.Note("Installed the modpack %s %s (%s %s).", p.Title, v.VersionNumber, flavorName(flavor), mc)
	return nil
}
