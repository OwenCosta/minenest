package nest

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A Build is one version of the server software for a Minecraft version: Paper's and Purpur's
// builds, Fabric's loader, Forge's and NeoForge's versions. Vanilla has none.
type Build struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Recommended bool   `json:"recommended"`
}

// Builds lists them, newest first. The recommended one is what MineNest picks by itself.
func Builds(ctx context.Context, flavor, mc string) ([]Build, error) {
	out := []Build{}
	if mc == "" || mc == "latest" {
		var err error
		if mc, err = latestVersion(ctx, flavor); err != nil {
			return nil, err
		}
	}
	switch flavor {
	case "vanilla":
		return out, nil
	case "paper":
		var builds []struct {
			ID      int64
			Channel string
		}
		if err := getJSON(ctx, fmt.Sprintf("%s/versions/%s/builds", paperAPI, mc), 10*time.Minute, &builds); err != nil {
			return nil, err
		}
		rec := false
		for _, b := range builds {
			l := fmt.Sprintf("Build %d", b.ID)
			if b.Channel != "STABLE" {
				l += " (" + strings.ToLower(b.Channel) + ")"
			}
			r := !rec && b.Channel == "STABLE"
			rec = rec || r
			out = append(out, Build{fmt.Sprint(b.ID), l, r})
		}
	case "purpur":
		var doc struct {
			Builds struct {
				Latest string
				All    []string
			}
		}
		if err := getJSON(ctx, purpurAPI+"/"+mc, 10*time.Minute, &doc); err != nil {
			return nil, err
		}
		for i := len(doc.Builds.All) - 1; i >= 0; i-- {
			b := doc.Builds.All[i]
			out = append(out, Build{b, "Build " + b, b == doc.Builds.Latest})
		}
	case "fabric":
		var ls []struct {
			Loader struct {
				Version string
				Stable  bool
			}
		}
		if err := getJSON(ctx, fabricMeta+"/versions/loader/"+mc, time.Hour, &ls); err != nil {
			return nil, err
		}
		rec := false
		for _, l := range ls {
			r := !rec && l.Loader.Stable
			rec = rec || r
			label := "Loader " + l.Loader.Version
			if strings.ContainsAny(l.Loader.Version, "-+") { // Fabric marks only its newest as "stable"
				label += " (beta)"
			}
			out = append(out, Build{l.Loader.Version, label, r})
		}
	case "forge":
		var meta map[string][]string
		if err := getJSON(ctx, "https://files.minecraftforge.net/net/minecraftforge/forge/maven-metadata.json", time.Hour, &meta); err != nil {
			return nil, err
		}
		var promos struct{ Promos map[string]string }
		getJSON(ctx, forgePromos, time.Hour, &promos)
		want := promos.Promos[mc+"-recommended"]
		if want == "" {
			want = promos.Promos[mc+"-latest"]
		}
		list := meta[mc]
		for i := len(list) - 1; i >= 0; i-- {
			v := strings.TrimPrefix(list[i], mc+"-")
			label := "Forge " + v
			if v == promos.Promos[mc+"-recommended"] {
				label += " (recommended)"
			}
			out = append(out, Build{list[i], label, v == want})
		}
	case "neoforge":
		vs, err := neoVersions(ctx)
		if err != nil {
			return nil, err
		}
		rec := false
		for i := len(vs) - 1; i >= 0; i-- {
			if neoMinecraft(vs[i]) != mc {
				continue
			}
			r := !rec && !strings.Contains(vs[i], "-")
			rec = rec || r
			out = append(out, Build{vs[i], "NeoForge " + vs[i], r})
		}
	default:
		return nil, fmt.Errorf("MineNest doesn't know a server type called %q", flavor)
	}
	found := false
	for _, b := range out {
		found = found || b.Recommended
	}
	if !found && len(out) > 0 {
		out[0].Recommended = true
	}
	return out, nil
}
