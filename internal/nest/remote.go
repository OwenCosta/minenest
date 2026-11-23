package nest

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Using MineNest from another computer, two ways, both off until turned on:
//
//   On the home network: a second door (APIPort+1, every address of this computer) that takes
//   only computers on private networks, and only after the password set in MineNest. A signed-in
//   browser gets the whole dashboard, console included.
//
//   Through the website, for supporters on Patreon: MineNest keeps a connection out to XenithOS
//   Cloud (/v1/remote/poll), which passes it what a signed-in browser asks for at
//   https://cloud.owencosta.diy/remote/ (signed in there with the same OCAccount) and takes the
//   answer back (/v1/remote/reply). Nothing has to be opened on the router.
//
// Either way, what comes in goes to the same API the window uses, minus a few things only this
// computer may do (stopping MineNest, these settings), and with files and programs read-only
// unless "Let other computers change files" is on (files_api.go).

const lanSessionDays = 30

type remoteState struct {
	mu       sync.Mutex
	sessions map[string]time.Time // hashed session -> expiry (home network)
	tries    map[string][]time.Time
	lan      *http.Server
	lanErr   string
	webStop  chan struct{}
	webState string
	webNote  string
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	k, _ := pbkdf2.Key(sha256.New, pw, salt, 200000, 32)
	return hex.EncodeToString(salt) + "$" + hex.EncodeToString(k)
}

func passwordOK(stored, pw string) bool {
	salt, want, ok := strings.Cut(stored, "$")
	if !ok {
		return false
	}
	s, err1 := hex.DecodeString(salt)
	w, err2 := hex.DecodeString(want)
	if err1 != nil || err2 != nil {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, pw, s, 200000, 32)
	return err == nil && subtle.ConstantTimeCompare(k, w) == 1
}

func sessionHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// RemoteInfo is what the window shows about it.
type RemoteInfo struct {
	LAN         bool   `json:"lan"`
	LANURL      string `json:"lan_url"`
	LANError    string `json:"lan_error,omitempty"`
	HasPassword bool   `json:"has_password"`
	Web         bool   `json:"web"`
	WebState    string `json:"web_state"` // off, connecting, on, problem
	WebNote     string `json:"web_note,omitempty"`
	WebURL      string `json:"web_url"`
	Here        bool   `json:"here"`  // the window is on this computer (it can change these)
	Files       bool   `json:"files"` // other computers may change files (they can only read them otherwise)
}

func (m *Manager) lanPort() int { return m.Settings().APIPort + 1 }

func (m *Manager) RemoteInfo() RemoteInfo {
	set := m.Settings()
	r := m.remote
	r.mu.Lock()
	defer r.mu.Unlock()
	ri := RemoteInfo{LAN: set.LANAccess, HasPassword: set.LANPassword != "", Web: set.WebRemote, WebState: r.webState,
		WebNote: r.webNote, LANError: r.lanErr, Files: set.RemoteFiles, WebURL: strings.TrimRight(set.Cloud, "/") + "/remote/"}
	if ip := lanAddress(); ip != "" {
		ri.LANURL = fmt.Sprintf("http://%s:%d/", ip, m.lanPort())
	}
	if ri.WebState == "" {
		ri.WebState = "off"
	}
	return ri
}

// SetRemote changes these settings (only from this computer).
func (m *Manager) SetRemote(lan, web *bool, password *string, files *bool) error {
	if password != nil {
		if len(*password) < 8 {
			return fmt.Errorf("pick a password of at least 8 letters")
		}
		h := hashPassword(*password)
		m.updateSettings(func(s *Settings) { s.LANPassword = h })
		m.remote.mu.Lock()
		m.remote.sessions = map[string]time.Time{} // a new password signs everyone out
		m.remote.mu.Unlock()
	}
	if lan != nil {
		if *lan && m.Settings().LANPassword == "" {
			return fmt.Errorf("set a password first: computers at home sign in with it")
		}
		m.updateSettings(func(s *Settings) { s.LANAccess = *lan })
	}
	if files != nil {
		m.updateSettings(func(s *Settings) { s.RemoteFiles = *files })
	}
	if web != nil {
		if *web && m.Settings().AccountToken == "" {
			return fmt.Errorf("sign in with your OCAccount first")
		}
		m.updateSettings(func(s *Settings) { s.WebRemote = *web })
	}
	m.applyRemote()
	return nil
}

// applyRemote starts or stops the home-network door and the website connection as set.
func (m *Manager) applyRemote() {
	set := m.Settings()
	r := m.remote
	r.mu.Lock()
	lan := r.lan
	r.mu.Unlock()
	if set.LANAccess && lan == nil && m.api != nil {
		srv := &http.Server{Addr: fmt.Sprintf(":%d", m.lanPort()), Handler: m.lanHandler(), ReadHeaderTimeout: 10 * time.Second}
		l, err := net.Listen("tcp", srv.Addr)
		r.mu.Lock()
		if err != nil {
			r.lanErr = fmt.Sprintf("port %d is taken on this computer", m.lanPort())
		} else {
			r.lanErr = ""
			r.lan = srv
			go srv.Serve(l)
		}
		r.mu.Unlock()
	} else if !set.LANAccess && lan != nil {
		lan.Close()
		r.mu.Lock()
		r.lan = nil
		r.mu.Unlock()
	}
	r.mu.Lock()
	running := r.webStop != nil
	r.mu.Unlock()
	if set.WebRemote && set.AccountToken != "" && !running && m.api != nil {
		stop := make(chan struct{})
		r.mu.Lock()
		r.webStop, r.webState, r.webNote = stop, "connecting", ""
		r.mu.Unlock()
		go m.webLoop(stop)
	} else if (!set.WebRemote || set.AccountToken == "") && running {
		r.mu.Lock()
		close(r.webStop)
		r.webStop, r.webState, r.webNote = nil, "off", ""
		r.mu.Unlock()
	}
}

// forward runs a request that came from elsewhere through the API, as if from this computer.
// changes says it may change things (it carried X-MineNest, which a page elsewhere can't send).
func (m *Manager) forward(w http.ResponseWriter, r *http.Request, changes bool) {
	p := r.URL.Path
	if p == "/api/shutdown" || strings.HasPrefix(p, "/api/remote") || p == "/open" || strings.HasPrefix(p, "/api/account/oc") {
		http.Error(w, `{"error":"That can only be done on the computer MineNest runs on."}`, 403)
		return
	}
	r2 := markElsewhere(r) // the API can tell this didn't come from this computer (files_api.go)
	r2.Host = fmt.Sprintf("127.0.0.1:%d", m.Settings().APIPort)
	r2.Header = http.Header{}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		r2.Header.Set("Content-Type", ct)
	}
	r2.Header.Set("Authorization", "Bearer "+m.api.token)
	if changes {
		r2.Header.Set("X-MineNest", "1")
	}
	r2.RequestURI = ""
	m.api.mux.ServeHTTP(w, r2)
}

// ------------------------------------------------------------------ the home network

func privateAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

const loginPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>MineNest</title><link rel="icon" href="icon.svg"><body style="margin:0;background:#1d1e21;color:#e6e4df;font:15px system-ui,sans-serif;display:flex;
align-items:center;justify-content:center;min-height:100vh"><form method="post" action="login" style="width:min(340px,90vw)">
<h2 style="margin:0 0 6px">MineNest</h2><p style="color:#a29f98;margin:0 0 16px">Sign in with the password set in MineNest on the computer it runs on.</p>%s
<input type="password" name="password" autofocus autocomplete="current-password" style="width:100%%;box-sizing:border-box;padding:8px;
border-radius:4px;border:1px solid #42454b;background:#2a2b2f;color:inherit;font:inherit"><button style="margin-top:10px;width:100%%;padding:8px;
border:0;border-radius:4px;background:#6fb04a;color:#fff;font:600 15px system-ui">Sign in</button></form>`

func (m *Manager) lanHandler() http.Handler {
	r := m.remote
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !privateAddr(req.RemoteAddr) {
			http.Error(w, "MineNest only answers computers on the home network here.", 403)
			return
		}
		ip, _, _ := net.SplitHostPort(req.RemoteAddr)
		switch req.URL.Path {
		case "/icon.svg":
			m.forward(w, req, false)
			return
		case "/login":
			if req.Method == "POST" {
				r.mu.Lock()
				recent := r.tries[ip][:0]
				for _, t := range r.tries[ip] {
					if time.Since(t) < time.Minute {
						recent = append(recent, t)
					}
				}
				r.tries[ip] = recent
				many := len(recent) >= 8
				r.mu.Unlock()
				if !many && passwordOK(m.Settings().LANPassword, req.PostFormValue("password")) {
					b := make([]byte, 24)
					rand.Read(b)
					tok := hex.EncodeToString(b)
					r.mu.Lock()
					r.sessions[sessionHash(tok)] = time.Now().Add(lanSessionDays * 24 * time.Hour)
					r.mu.Unlock()
					http.SetCookie(w, &http.Cookie{Name: "minenest_lan", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: lanSessionDays * 86400})
					http.Redirect(w, req, "./", http.StatusSeeOther)
					return
				}
				r.mu.Lock()
				r.tries[ip] = append(r.tries[ip], time.Now())
				r.mu.Unlock()
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(401)
				msg := "That password isn't right."
				if many {
					msg = "Too many tries. Wait a minute."
				}
				fmt.Fprintf(w, loginPage, `<p style="color:#d8604f">`+html.EscapeString(msg)+`</p>`)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, loginPage, "")
			return
		}
		ok := false
		if c, err := req.Cookie("minenest_lan"); err == nil {
			r.mu.Lock()
			exp, found := r.sessions[sessionHash(c.Value)]
			ok = found && time.Now().Before(exp)
			r.mu.Unlock()
		}
		if !ok {
			if strings.HasPrefix(req.URL.Path, "/api/") {
				http.Error(w, `{"error":"Sign in again."}`, 401)
			} else {
				http.Redirect(w, req, "/login", http.StatusSeeOther)
			}
			return
		}
		m.forward(w, req, req.Method == "GET" || req.Header.Get("X-MineNest") == "1")
	})
}

// ------------------------------------------------------------------ through the website

type remoteRequest struct {
	ID      string `json:"id"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Query   string `json:"query"`
	CType   string `json:"ctype"`
	Changes bool   `json:"changes"`
	Body    string `json:"body"` // base64
}

func (m *Manager) setWeb(state, note string) {
	m.remote.mu.Lock()
	m.remote.webState, m.remote.webNote = state, note
	m.remote.mu.Unlock()
}

func (m *Manager) webLoop(stop chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-stop; cancel() }()
	wait := time.Duration(0)
	hello := true // the first ask is answered at once, so "Connected" shows straight away
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		reqs, err := m.webPoll(ctx, hello)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.setWeb("problem", err.Error())
			hello = true
			wait = min(wait*2+5*time.Second, time.Minute)
			continue
		}
		m.setWeb("on", "")
		hello, wait = false, 0
		for _, rq := range reqs {
			go m.webServe(ctx, rq)
		}
	}
}

// webPoll waits (up to half a minute) for what browsers have asked for.
func (m *Manager) webPoll(ctx context.Context, hello bool) ([]remoteRequest, error) {
	set := m.Settings()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(set.Cloud, "/")+"/v1/remote/poll",
		strings.NewReader("token="+set.AccountToken+"&version="+Version+map[bool]string{true: "&hello=1"}[hello]))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", agent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("XenithOS Cloud can't be reached just now")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode == 204 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		if e := parseKV(string(b))["error"]; e != "" {
			return nil, fmt.Errorf("%s", e)
		}
		return nil, fmt.Errorf("XenithOS Cloud answered %s", resp.Status)
	}
	var out []remoteRequest
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("XenithOS Cloud's answer couldn't be read")
	}
	return out, nil
}

func (m *Manager) webServe(ctx context.Context, rq remoteRequest) {
	body, _ := base64.StdEncoding.DecodeString(rq.Body)
	target := rq.Path
	if !strings.HasPrefix(target, "/") {
		target = "/" + target
	}
	if rq.Query != "" {
		target += "?" + rq.Query
	}
	hreq, err := http.NewRequestWithContext(ctx, rq.Method, target, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	if err != nil {
		rec.WriteHeader(400)
	} else {
		if rq.CType != "" {
			hreq.Header.Set("Content-Type", rq.CType)
		}
		hreq.RemoteAddr = "203.0.113.1:1" // not this computer's loopback
		m.forward(rec, hreq, rq.Changes || rq.Method == "GET")
	}
	res := rec.Result()
	out, _ := io.ReadAll(res.Body)
	reply, _ := json.Marshal(map[string]any{"id": rq.ID, "status": res.StatusCode, "ctype": res.Header.Get("Content-Type"),
		"body": base64.StdEncoding.EncodeToString(out)})
	set := m.Settings()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, "POST", strings.TrimRight(set.Cloud, "/")+"/v1/remote/reply", bytes.NewReader(reply))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Xenith-Token", set.AccountToken)
	req.Header.Set("User-Agent", agent())
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
}
