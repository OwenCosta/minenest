package nest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Bedrock players (phones, tablets, consoles, Windows): Geyser and Floodgate from GeyserMC, as
// plugins on Paper or Purpur. Geyser takes Bedrock players on UDP 19132 and Floodgate lets them
// in without a Java account. Both are free software (MIT). They come over the home network, and
// from the internet through playit.gg's Bedrock tunnel; SSH tunnels carry TCP only.

const geyserAPI = "https://download.geysermc.org/v2"

var geyserParts = []struct{ project, name, jar string }{
	{"geyser", "Geyser", "Geyser-Spigot.jar"},
	{"floodgate", "Floodgate", "floodgate-spigot.jar"},
}

func geyserInstalled(s *Server) bool {
	for _, p := range geyserParts {
		if !exists(filepath.Join(s.Root(), "plugins", p.jar)) {
			return false
		}
	}
	return true
}

func geyserAPIBase() string {
	if v := os.Getenv("MINENEST_GEYSER_API"); v != "" {
		return v
	}
	return geyserAPI
}

// SetBedrock turns Bedrock players on (downloading Geyser and Floodgate) or off (removing them;
// their settings stay for next time).
func (m *Manager) SetBedrock(s *Server, on bool) (*Job, error) {
	c := s.Config()
	if c.Installed == nil || (c.Installed.Flavor != "paper" && c.Installed.Flavor != "purpur") {
		return nil, fmt.Errorf("Bedrock players need Paper or Purpur. Change the server type on the Version page first")
	}
	if !on {
		for _, p := range geyserParts {
			os.Remove(filepath.Join(s.Root(), "plugins", p.jar))
		}
		s.update(func(c *Config) { c.Bedrock = false })
		s.Note("Bedrock players are off. Restart the server for it to count.")
		return nil, nil
	}
	if err := s.setBusy("getting Geyser", false); err != nil {
		return nil, err
	}
	return m.Jobs.start("Bedrock players for "+c.Name, s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		if err := getGeyser(ctx, s, j); err != nil {
			return nil, err
		}
		s.update(func(c *Config) { c.Bedrock = true })
		s.Note("Bedrock players can join on port %d once the server restarts.", BedrockPort)
		return nil, nil
	}), nil
}

func getGeyser(ctx context.Context, s *Server, j *Job) error {
	for _, p := range geyserParts {
		var b struct {
			Version   string
			Build     int
			Downloads map[string]struct {
				Name   string
				SHA256 string
			}
		}
		u := fmt.Sprintf("%s/projects/%s/versions/latest/builds/latest", geyserAPIBase(), p.project)
		if err := getJSON(ctx, u, 10*time.Minute, &b); err != nil {
			return err
		}
		dl, ok := b.Downloads["spigot"]
		if !ok {
			return fmt.Errorf("%s has no Paper download right now", p.name)
		}
		url := fmt.Sprintf("%s/projects/%s/versions/%s/builds/%d/downloads/spigot", geyserAPIBase(), p.project, b.Version, b.Build)
		if err := download(ctx, url, filepath.Join(s.Root(), "plugins", p.jar), Hash{"sha256", dl.SHA256}, j, p.name); err != nil {
			return err
		}
	}
	return nil
}

// UpdateGeyser fetches the newest Geyser and Floodgate (new Bedrock versions need a new Geyser).
func (m *Manager) UpdateGeyser(s *Server) (*Job, error) {
	if !geyserInstalled(s) {
		return nil, fmt.Errorf("Bedrock players aren't on")
	}
	if err := s.setBusy("updating Geyser", false); err != nil {
		return nil, err
	}
	return m.Jobs.start("Updating Geyser", s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		if err := getGeyser(ctx, s, j); err != nil {
			return nil, err
		}
		s.Note("Updated Geyser and Floodgate. Restart the server to use them.")
		return nil, nil
	}), nil
}
