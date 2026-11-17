package nest

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The daemon's API: JSON over HTTP on 127.0.0.1, for the CLI, the GUI and XenithOS. Every request
// carries the key in api.token (readable only by this user): as "Authorization: Bearer <key>", or
// the GUI's cookie, set by opening /open?key=<key>. Requests that change things also need the
// header "X-MineNest: 1", which a web page elsewhere can't send, and the Host must be this
// computer's loopback, so another site can't reach the API through the browser.

func APIToken() (string, error) {
	path := homePath("api.token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	if err := writeFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// DaemonInfo is written to daemon.json while the daemon runs, so the CLI finds it.
type DaemonInfo struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Version string `json:"version"`
}

type api struct {
	m     *Manager
	mux   http.Handler
	token string
	web   fs.FS
	quit  chan struct{}
}

type apiError struct {
	Code int
	Msg  string
}

func (e apiError) Error() string { return e.Msg }

func bad(format string, a ...any) error { return apiError{400, fmt.Sprintf(format, a...)} }

// Serve runs the daemon's API until it's told to stop (or the listener fails).
func Serve(m *Manager, web fs.FS, quit chan struct{}) error {
	tok, err := APIToken()
	if err != nil {
		return err
	}
	port := m.Settings().APIPort
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("MineNest's port %d on this computer is taken (is MineNest already running?): %v", port, err)
	}
	writeJSON(homePath("daemon.json"), DaemonInfo{os.Getpid(), port, Version}, 0o644)
	defer os.Remove(homePath("daemon.json"))
	a := &api{m: m, token: tok, web: web, quit: quit}
	a.mux = a.routes()
	m.api = a
	m.applyRemote() // the home-network door and the website connection, if they're on
	srv := &http.Server{Handler: a.mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-quit
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	err = srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type handler func(r *http.Request) (any, error)

func (a *api) authed(r *http.Request) bool {
	got := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie("minenest"); err == nil {
		got = c.Value
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
}

func localHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	return h == "127.0.0.1" || h == "localhost" || h == "[::1]" || h == "::1"
}

func (a *api) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !localHost(r.Host) {
			http.Error(w, "MineNest only answers on this computer's own address", 403)
			return
		}
		if !a.authed(r) {
			writeErr(w, apiError{401, "MineNest's key is missing or wrong"})
			return
		}
		if r.Method != "GET" && r.Header.Get("X-MineNest") != "1" {
			writeErr(w, apiError{403, "missing the X-MineNest header"})
			return
		}
		v, err := h(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if v == nil {
			v = map[string]bool{"ok": true}
		}
		json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, err error) {
	code := 400
	var ae apiError
	if errors.As(err, &ae) {
		code = ae.Code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	msg := err.Error()
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
		if !strings.HasSuffix(msg, ".") && !strings.HasSuffix(msg, "?") && !strings.HasSuffix(msg, ")") {
			msg += "."
		}
	}
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func body(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return bad("the request wasn't valid JSON: %v", err)
	}
	return nil
}

func (a *api) server(r *http.Request) (*Server, error) {
	s, err := a.m.Get(r.PathValue("id"))
	if err != nil {
		return nil, apiError{404, err.Error()}
	}
	return s, nil
}

func jobRef(j *Job) any {
	if j == nil {
		return nil
	}
	return map[string]string{"job": j.ID}
}

func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, f handler) { mux.HandleFunc(pattern, a.wrap(f)) }

	// The GUI: /open?key=... sets the cookie, then everything else is the page.
	mux.HandleFunc("GET /open", func(w http.ResponseWriter, r *http.Request) {
		if !localHost(r.Host) {
			http.Error(w, "MineNest only answers on this computer's own address", 403)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("key")), []byte(a.token)) != 1 {
			http.Error(w, "That link to MineNest is old. Open MineNest again.", 401)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "minenest", Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 400 * 86400})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	static := http.FileServerFS(a.web)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if !localHost(r.Host) {
			http.Error(w, "MineNest only answers on this computer's own address", 403)
			return
		}
		if !a.authed(r) && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(401)
			io.WriteString(w, `<!doctype html><meta charset=utf-8><title>MineNest</title><body style="font:16px system-ui;background:#111;color:#eee;padding:40px">`+
				`<h1>MineNest</h1><p>Open MineNest from its icon, or run <code>minenest gui</code>, to get in.</p>`)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})

	h("GET /api/info", func(r *http.Request) (any, error) { return a.m.Info(), nil })
	h("POST /api/shutdown", func(r *http.Request) (any, error) {
		go func() {
			time.Sleep(200 * time.Millisecond)
			close(a.quit)
		}()
		return nil, nil
	})
	h("GET /api/jobs/{id}", func(r *http.Request) (any, error) {
		j := a.m.Jobs.get(r.PathValue("id"))
		if j == nil {
			return nil, apiError{404, "no such job"}
		}
		return j.view(), nil
	})
	h("POST /api/jobs/{id}/cancel", func(r *http.Request) (any, error) {
		if j := a.m.Jobs.get(r.PathValue("id")); j != nil {
			j.cancel()
		}
		return nil, nil
	})
	h("GET /api/versions/{flavor}", func(r *http.Request) (any, error) {
		return Versions(r.Context(), r.PathValue("flavor"))
	})
	h("GET /api/builds/{flavor}/{mc}", func(r *http.Request) (any, error) {
		return Builds(r.Context(), r.PathValue("flavor"), r.PathValue("mc"))
	})
	h("GET /api/modpacks", func(r *http.Request) (any, error) {
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		return SearchModpacks(r.Context(), r.URL.Query().Get("q"), off)
	})

	// Servers
	h("GET /api/servers", func(r *http.Request) (any, error) {
		out := []ServerStatus{}
		for _, s := range a.m.List() {
			out = append(out, a.m.Status(s))
		}
		return out, nil
	})
	h("POST /api/servers", func(r *http.Request) (any, error) {
		var o CreateOptions
		if err := body(r, &o); err != nil {
			return nil, err
		}
		s, j, err := a.m.Create(o)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": s.ID, "job": j.ID}, nil
	})
	h("GET /api/import/find", func(r *http.Request) (any, error) { return a.m.FindServers(r.Context()), nil })
	h("POST /api/import/zip", func(r *http.Request) (any, error) {
		q := r.URL.Query()
		s, j, err := a.m.ImportZip(r.Body, ImportOptions{Name: q.Get("name"), Flavor: q.Get("flavor"), Version: q.Get("version"), ZipName: q.Get("filename")})
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": s.ID, "job": j.ID}, nil
	})
	h("POST /api/import", func(r *http.Request) (any, error) {
		var o ImportOptions
		if err := body(r, &o); err != nil {
			return nil, err
		}
		s, j, err := a.m.Import(o)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": s.ID, "job": j.ID}, nil
	})
	h("GET /api/servers/{id}", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		return a.m.Status(s), nil
	})
	h("PATCH /api/servers/{id}", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var o struct {
			Name        *string `json:"name"`
			RAM         *int    `json:"ram_mb"`
			Autostart   *bool   `json:"autostart"`
			AutoRestart *bool   `json:"auto_restart"`
			RealIPs     *bool   `json:"real_ips"`
			JVMArgs     *string `json:"jvm_args"`
		}
		if err := body(r, &o); err != nil {
			return nil, err
		}
		if o.RAM != nil && (*o.RAM < 512 || (totalMemoryMB() > 0 && *o.RAM > totalMemoryMB())) {
			return nil, bad("memory has to be between 512 MB and what this computer has (%d MB)", totalMemoryMB())
		}
		if o.Name != nil && (strings.TrimSpace(*o.Name) == "" || len(*o.Name) > 40) {
			return nil, bad("pick a name of 1 to 40 letters")
		}
		return nil, s.update(func(c *Config) {
			if o.Name != nil {
				c.Name = strings.TrimSpace(*o.Name)
			}
			if o.RAM != nil {
				c.RAM = *o.RAM
			}
			if o.Autostart != nil {
				c.Autostart = *o.Autostart
			}
			if o.AutoRestart != nil {
				c.AutoRestart = *o.AutoRestart
			}
			if o.RealIPs != nil {
				c.RealIPs = *o.RealIPs
			}
			if o.JVMArgs != nil {
				c.JVMArgs = strings.TrimSpace(*o.JVMArgs)
			}
		})
	})
	h("DELETE /api/servers/{id}", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		return nil, a.m.Delete(s)
	})
	h("POST /api/servers/{id}/eula", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var o struct{ Agree bool }
		body(r, &o)
		if !o.Agree {
			return nil, bad("send {\"agree\": true} once the server's owner has read and agreed to %s", EulaURL)
		}
		return nil, s.AgreeEULA()
	})
	h("POST /api/servers/{id}/{action}", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		switch r.PathValue("action") {
		case "start":
			return nil, s.Start()
		case "stop":
			return nil, s.Stop()
		case "kill":
			s.Kill()
			return nil, nil
		case "restart":
			j := a.m.Jobs.start("Restarting "+s.Config().Name, s.ID, func(ctx context.Context, j *Job) (any, error) {
				j.Say("Stopping")
				if err := s.StopAndWait(ctx); err != nil {
					return nil, err
				}
				j.Say("Starting")
				return nil, s.Start()
			})
			return jobRef(j), nil
		case "command":
			var o struct{ Command string }
			body(r, &o)
			return nil, s.Command(o.Command)
		case "repair":
			c := s.Config()
			b := ""
			if c.Installed != nil {
				b = c.Installed.Build
			}
			j, err := a.m.ChangeVersion(s, c.Flavor, c.Want, b)
			return jobRef(j), err
		case "recheck":
			go s.RecheckWay()
			return nil, nil
		}
		return nil, apiError{404, "unknown action"}
	})
	h("GET /api/servers/{id}/console", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		wait := r.URL.Query().Get("wait") == "1"
		deadline := time.Now().Add(25 * time.Second)
		for {
			lines := s.Lines(since)
			if len(lines) > 0 || !wait || time.Now().After(deadline) {
				return map[string]any{"lines": lines, "state": s.State()}, nil
			}
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	})

	// Settings (server.properties)
	h("GET /api/servers/{id}/properties", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		return s.props().All(), nil
	})
	h("PUT /api/servers/{id}/properties", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var set map[string]string
		if err := body(r, &set); err != nil {
			return nil, err
		}
		p := s.props()
		for k, v := range set {
			if k == "" || strings.ContainsAny(k, "=:\n #") || strings.Contains(v, "\n") {
				return nil, bad("%q can't be set", k)
			}
			if k == "server-port" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1024 || n > 65535 {
					return nil, bad("the port has to be a number from 1024 to 65535")
				}
			}
			p.Set(k, v)
		}
		if err := p.Save(); err != nil {
			return nil, err
		}
		if s.State() != Stopped {
			s.Note("Settings changed. Restart the server for them to count.")
		}
		return nil, nil
	})

	// Version
	h("POST /api/servers/{id}/version", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var o struct{ Flavor, Version, Build string }
		body(r, &o)
		if o.Version == "" {
			o.Version = "latest"
		}
		j, err := a.m.ChangeVersion(s, o.Flavor, o.Version, o.Build)
		return jobRef(j), err
	})

	// Players
	h("GET /api/servers/{id}/players", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		return s.PlayerLists(), nil
	})
	h("POST /api/servers/{id}/players", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var o struct{ Action, Name, Reason string }
		body(r, &o)
		return nil, s.PlayerAction(r.Context(), o.Action, o.Name, o.Reason)
	})

	// World and backups
	h("GET /api/servers/{id}/backups", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		return map[string]any{"backups": s.Backups(), "world": s.WorldExists()}, nil
	})
	h("POST /api/servers/{id}/backups", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		j, err := a.m.Backup(s, "Made by hand")
		return jobRef(j), err
	})
	h("POST /api/servers/{id}/backups/{name}/restore", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		j, err := a.m.Restore(s, r.PathValue("name"))
		return jobRef(j), err
	})
	h("DELETE /api/servers/{id}/backups/{name}", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		return nil, s.DeleteBackup(r.PathValue("name"))
	})
	h("POST /api/servers/{id}/world/reset", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var o struct{ Seed string }
		body(r, &o)
		j, err := a.m.ResetWorld(s, o.Seed)
		return jobRef(j), err
	})

	// Add-ons (mods, plugins, data packs)
	h("GET /api/servers/{id}/addons", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		return s.Addons()
	})
	h("GET /api/servers/{id}/addons/search", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		return s.SearchAddons(r.Context(), r.URL.Query().Get("q"), off)
	})
	h("POST /api/servers/{id}/addons", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var o struct{ Project string }
		body(r, &o)
		if o.Project == "" {
			return nil, bad("say which add-on to install")
		}
		j, err := a.m.InstallAddon(s, o.Project)
		return jobRef(j), err
	})
	h("POST /api/servers/{id}/addons/upload", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		r.Body = http.MaxBytesReader(nil, r.Body, 512<<20)
		return nil, s.AddAddonFile(r.URL.Query().Get("name"), r.Body)
	})
	h("POST /api/servers/{id}/addons/check", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		return s.CheckUpdates(r.Context())
	})
	h("POST /api/servers/{id}/addons/update", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var o struct{ Project string }
		body(r, &o)
		j, err := a.m.UpdateAddons(s, o.Project)
		return jobRef(j), err
	})
	h("DELETE /api/servers/{id}/addons/{ref}", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		name, err := s.RemoveAddon(r.PathValue("ref"))
		if err == nil && s.State() != Stopped {
			s.Note("Removed %s. Restart the server for it to go.", name)
		}
		return map[string]string{"removed": name}, err
	})

	// The way in from the internet, and Bedrock
	h("POST /api/servers/{id}/way", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var o struct {
			Kind string        `json:"kind"`
			VPS  *TunnelConfig `json:"vps"`
		}
		if err := body(r, &o); err != nil {
			return nil, err
		}
		return nil, a.m.SetWay(s, o.Kind, o.VPS)
	})
	h("POST /api/servers/{id}/way/forget-host", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		a.m.ForgetVPSHost(s)
		return nil, nil
	})
	h("POST /api/servers/{id}/bedrock", func(r *http.Request) (any, error) {
		s, err := a.server(r)
		if err != nil {
			return nil, err
		}
		var o struct {
			On     bool `json:"on"`
			Update bool `json:"update"`
		}
		body(r, &o)
		var j *Job
		if o.Update {
			j, err = a.m.UpdateGeyser(s)
		} else {
			j, err = a.m.SetBedrock(s, o.On)
		}
		return jobRef(j), err
	})
	h("GET /api/sshkey", func(r *http.Request) (any, error) {
		k, err := PublicKey()
		return map[string]string{"key": k}, err
	})
	h("POST /api/playit/forget", func(r *http.Request) (any, error) {
		a.m.ForgetPlayit()
		return nil, nil
	})

	// The account and the supporter relay
	h("GET /api/account", func(r *http.Request) (any, error) { return a.m.RelayInfo(), nil })
	h("POST /api/account/login", func(r *http.Request) (any, error) {
		var o struct {
			Name, Pin string
			Create    bool
			Token     string
		}
		body(r, &o)
		if o.Token != "" {
			return a.m.UseToken(r.Context(), o.Token)
		}
		return a.m.Login(r.Context(), o.Name, o.Pin, o.Create)
	})
	h("GET /api/update", func(r *http.Request) (any, error) {
		return CheckUpdate(r.Context(), r.URL.Query().Get("now") == "1"), nil
	})
	h("POST /api/update", func(r *http.Request) (any, error) {
		j, err := a.m.UpdateAndRestart()
		return jobRef(j), err
	})
	h("GET /api/remote", func(r *http.Request) (any, error) {
		ri := a.m.RemoteInfo()
		ri.Here = true
		return ri, nil
	})
	h("POST /api/remote", func(r *http.Request) (any, error) {
		var o struct {
			LAN      *bool   `json:"lan"`
			Web      *bool   `json:"web"`
			Password *string `json:"password"`
		}
		if err := body(r, &o); err != nil {
			return nil, err
		}
		if err := a.m.SetRemote(o.LAN, o.Web, o.Password); err != nil {
			return nil, err
		}
		ri := a.m.RemoteInfo()
		ri.Here = true
		return ri, nil
	})
	h("POST /api/account/oc/start", func(r *http.Request) (any, error) {
		u, err := a.m.OCStart(r.Context())
		return map[string]string{"url": u}, err
	})
	h("POST /api/account/oc/poll", func(r *http.Request) (any, error) { return a.m.OCPoll(r.Context()), nil })
	h("POST /api/account/logout", func(r *http.Request) (any, error) {
		a.m.Logout(r.Context())
		return nil, nil
	})
	h("POST /api/relay/{what}", func(r *http.Request) (any, error) {
		var o struct{ Name string }
		body(r, &o)
		return a.m.RelayAsk(r.Context(), r.PathValue("what"), o.Name)
	})
	return mux
}
