package nest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The account: a XenithOS profile on XenithOS Cloud, signed in with an OCAccount (or a profile's
// name and PIN). MineNest needs it only for the Remote dashboard for supporters (remote.go);
// everything else works without signing in.

func parseKV(body string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.IndexByte(line, '='); i > 0 {
			k := line[:i]
			if _, dup := out[k]; !dup {
				out[k] = line[i+1:]
			}
		}
	}
	return out
}

// cloudPost sends a form to XenithOS Cloud and reads its key=value answer.
func (m *Manager) cloudPost(ctx context.Context, path string, form url.Values) (map[string]string, int, error) {
	base := m.Settings().Cloud
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", agent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("XenithOS Cloud didn't answer: %v", friendlyNetErr(base, err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	kv := parseKV(string(b))
	if resp.StatusCode != 200 {
		if e := kv["error"]; e != "" {
			return kv, resp.StatusCode, fmt.Errorf("%s", e)
		}
		return kv, resp.StatusCode, fmt.Errorf("XenithOS Cloud answered with error %d", resp.StatusCode)
	}
	return kv, 200, nil
}

// ------------------------------------------------------------------ the account

type Account struct {
	Name  string `json:"name"`
	Cloud string `json:"cloud"`
}

// Login signs in to a XenithOS profile (the same one XenithOS uses) with its name and PIN.
func (m *Manager) Login(ctx context.Context, name, pin string, create bool) (*Account, error) {
	form := url.Values{"name": {name}, "pin": {pin}}
	path := "/v1/accounts/login"
	if create {
		path = "/v1/accounts/create"
		form.Set("avatar", "1202001000")
	}
	kv, code, err := m.cloudPost(ctx, path, form)
	if err != nil {
		if code == 401 {
			return nil, fmt.Errorf("that name and PIN don't match a profile")
		}
		return nil, err
	}
	if kv["token"] == "" {
		return nil, fmt.Errorf("XenithOS Cloud didn't send a sign-in")
	}
	m.updateSettings(func(s *Settings) { s.AccountName, s.AccountToken, s.OCAccount = kv["name"], kv["token"], "" })
	return &Account{Name: kv["name"], Cloud: m.Settings().Cloud}, nil
}

// UseToken signs in with a token XenithOS already has (the XenithOS app hands its profile over).
func (m *Manager) UseToken(ctx context.Context, token string) (*Account, error) {
	kv, _, err := m.cloudPost(ctx, "/v1/accounts/me", url.Values{"token": {token}})
	if err != nil {
		return nil, err
	}
	m.updateSettings(func(s *Settings) { s.AccountName, s.AccountToken = kv["name"], token })
	return &Account{Name: kv["name"], Cloud: m.Settings().Cloud}, nil
}

func (m *Manager) Logout(ctx context.Context) {
	if tok := m.Settings().AccountToken; tok != "" {
		m.cloudPost(ctx, "/v1/accounts/logout", url.Values{"token": {tok}})
	}
	m.updateSettings(func(s *Settings) { s.AccountName, s.AccountToken, s.OCAccount = "", "", "" })
	m.applyRemote()
}

// AccountInfo is what the GUI shows on the Settings page.
type AccountInfo struct {
	SignedIn  bool   `json:"signed_in"`
	Account   string `json:"account"`
	OCAccount string `json:"oc_account"`
}

func (m *Manager) AccountInfo() AccountInfo {
	set := m.Settings()
	return AccountInfo{SignedIn: set.AccountToken != "", Account: set.AccountName, OCAccount: set.OCAccount}
}

// ------------------------------------------------------------------ signing in with an OCAccount

// OCStart begins signing in with an OCAccount: XenithOS Cloud gives the OCAccounts page to open
// in a browser, and a session MineNest then polls (OCPoll) until the person has agreed there. The
// XenithOS profile linked to the OCAccount is what's signed in, as with a name and PIN.
func (m *Manager) OCStart(ctx context.Context) (string, error) {
	kv, code, err := m.cloudPost(ctx, "/v1/oc/start", url.Values{})
	if err != nil {
		if code == 404 {
			return "", fmt.Errorf("signing in with an OCAccount isn't open yet. Use a XenithOS profile's name and PIN for now")
		}
		return "", err
	}
	if kv["url"] == "" || kv["session"] == "" {
		return "", fmt.Errorf("XenithOS Cloud's answer was incomplete. Try again")
	}
	m.mu.Lock()
	m.ocSession, m.ocKey = kv["session"], kv["key"]
	m.mu.Unlock()
	return kv["url"], nil
}

type OCState struct {
	State   string `json:"state"` // waiting, done, error, none
	Error   string `json:"error,omitempty"`
	Name    string `json:"name,omitempty"`
	Account string `json:"account,omitempty"`
}

func (m *Manager) OCPoll(ctx context.Context) OCState {
	m.mu.Lock()
	sid, key := m.ocSession, m.ocKey
	m.mu.Unlock()
	if sid == "" {
		return OCState{State: "none"}
	}
	kv, _, err := m.cloudPost(ctx, "/v1/oc/poll", url.Values{"session": {sid}, "key": {key}})
	if err != nil {
		m.mu.Lock()
		m.ocSession = ""
		m.mu.Unlock()
		return OCState{State: "error", Error: err.Error()}
	}
	switch kv["state"] {
	case "done":
		m.mu.Lock()
		m.ocSession = ""
		m.mu.Unlock()
		m.updateSettings(func(s *Settings) { s.AccountName, s.AccountToken, s.OCAccount = kv["name"], kv["token"], kv["account"] })
		m.applyRemote()
		return OCState{State: "done", Name: kv["name"], Account: kv["account"]}
	case "error":
		m.mu.Lock()
		m.ocSession = ""
		m.mu.Unlock()
		return OCState{State: "error", Error: kv["error"]}
	}
	return OCState{State: "waiting"}
}
