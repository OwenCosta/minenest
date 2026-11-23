package nest

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The file manager's part of the API. Reading (listing, opening, downloading) is open to whoever
// is signed in. Changing anything (saving, uploading, moving, deleting, unpacking) needs the
// request to come from this computer, or "Let other computers change files" to be on in the local
// settings: someone who can put a plugin in a server's folder can run programs on this PC.

type elsewhereKey struct{}

// The two ways in from another computer.
const (
	doorLAN = "lan" // the home network: answers stream
	doorWeb = "web" // the website: a whole answer is held in memory and passed on in one piece
)

// fromElsewhere reports whether a request came in through the home-network door or the website
// (remote.go marks those when it passes them on), not from this computer.
func fromElsewhere(r *http.Request) bool {
	v, _ := r.Context().Value(elsewhereKey{}).(string)
	return v != ""
}

// fromWeb reports whether it came through the website's door.
func fromWeb(r *http.Request) bool {
	v, _ := r.Context().Value(elsewhereKey{}).(string)
	return v == doorWeb
}

func markElsewhere(r *http.Request, door string) *http.Request {
	return r.Clone(context.WithValue(r.Context(), elsewhereKey{}, door))
}

// webUploadWhy says why an upload can't come through the website: the whole file travels inside
// a polled message, so only small ones do.
func webUploadWhy(r *http.Request) error {
	if fromWeb(r) && r.ContentLength > remoteSendLimit {
		return ftoobig("that file is %s, more than the %s that can be sent through the website. Use the home network, or the computer MineNest runs on", humanSize(r.ContentLength), humanSize(remoteSendLimit))
	}
	return nil
}

const remoteFilesOff = "Changing files from another computer is turned off. On the computer MineNest runs on, open Settings and turn on \"Let other computers change files\""

// filesChangeAllowed is the rule for other computers, kept here in the daemon so no page can get
// around it.
func (m *Manager) filesChangeAllowed(r *http.Request) error {
	if fromElsewhere(r) && !m.Settings().RemoteFiles {
		return apiError{403, remoteFilesOff}
	}
	return nil
}

// An answer through the website is held in memory on its way (and the website will not take a big
// one), so a download through it has a ceiling. The home network streams, so it has none.
var remoteSendLimit int64 = 16 << 20

// sendLimit is the most a download to this request may be (0: no limit).
func sendLimit(r *http.Request) int64 {
	if fromWeb(r) {
		return remoteSendLimit
	}
	return 0
}

type fileListReply struct {
	*FileList
	CanChange bool   `json:"can_change"`
	Why       string `json:"why,omitempty"`
	Elsewhere bool   `json:"elsewhere"`
	Web       bool   `json:"web"`   // through the website: big downloads and uploads don't work
	Trash     int64  `json:"trash"` // what the trash holds, so a delete can say how much it adds to
}

func (a *api) fileRoutes(mux *http.ServeMux, h func(string, handler)) {
	raw := func(pattern string, f func(w http.ResponseWriter, r *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !a.check(w, r) {
				return
			}
			if err := f(w, r); err != nil {
				writeErr(w, err)
			}
		})
	}
	// srv finds the server; a change also has to be allowed for where the request came from.
	srv := func(r *http.Request, changes bool) (*Server, error) {
		if changes {
			if err := a.m.filesChangeAllowed(r); err != nil {
				return nil, err
			}
		}
		return a.server(r)
	}
	q := func(r *http.Request, k string) string { return r.URL.Query().Get(k) }
	base := "/api/servers/{id}/files"

	h("GET "+base, func(r *http.Request) (any, error) {
		s, err := srv(r, false)
		if err != nil {
			return nil, err
		}
		l, err := s.FileList(q(r, "path"))
		if err != nil {
			return nil, err
		}
		rep := fileListReply{FileList: l, CanChange: true, Elsewhere: fromElsewhere(r), Web: fromWeb(r), Trash: s.trashSize()}
		if rep.Web {
			l.MaxUpload = min(l.MaxUpload, remoteSendLimit)
		}
		if err := a.m.filesChangeAllowed(r); err != nil {
			rep.CanChange, rep.Why = false, err.Error()
		}
		return rep, nil
	})
	h("GET "+base+"/read", func(r *http.Request) (any, error) {
		s, err := srv(r, false)
		if err != nil {
			return nil, err
		}
		return s.FileRead(q(r, "path"))
	})
	h("PUT "+base+"/content", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		var o WriteRequest
		if err := bodyN(r, &o, 8<<20); err != nil {
			return nil, err
		}
		return s.FileWrite(o)
	})
	h("POST "+base+"/upload", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		if r.ContentLength > maxUploadBytes {
			return nil, ftoobig("that file is %s, more than the %s MineNest takes at once", humanSize(r.ContentLength), humanSize(maxUploadBytes))
		}
		if err := webUploadWhy(r); err != nil {
			return nil, err
		}
		r.Body = http.MaxBytesReader(nil, r.Body, maxUploadBytes+1)
		return s.FileUpload(q(r, "dir"), q(r, "name"), r.ContentLength, q(r, "overwrite") == "1", r.Body)
	})
	raw("GET "+base+"/download", func(w http.ResponseWriter, r *http.Request) error {
		s, err := srv(r, false)
		if err != nil {
			return err
		}
		f, fi, name, err := s.FileOpen(q(r, "path"))
		if err != nil {
			return err
		}
		defer f.Close()
		if lim := sendLimit(r); lim > 0 && fi.Size() > lim {
			return errTooMuch
		}
		attachment(w, name)
		http.ServeContent(w, r, name, fi.ModTime(), f)
		return nil
	})
	raw("GET "+base+"/zip", func(w http.ResponseWriter, r *http.Request) error {
		s, err := srv(r, false)
		if err != nil {
			return err
		}
		t, err := s.target(q(r, "path"))
		if err != nil {
			return err
		}
		var limit int64
		if limit = sendLimit(r); limit > 0 {
			if t.isDir() {
				if size, _ := sizeOf(t.Abs, t.Info); size > limit {
					return errTooMuch
				}
			}
		}
		name := t.Name
		if name == "" {
			name = s.ID
		}
		if !t.exists() {
			return errNotThere
		}
		if !t.isDir() {
			return fbad("that is a file: download it as it is")
		}
		attachment(w, name+".zip")
		w.Header().Set("Content-Type", "application/zip")
		_, err = s.FileZip(t.Rel, w, limit)
		if err != nil && !fromWeb(r) {
			panic(http.ErrAbortHandler) // a zip that stops halfway must not look finished
		}
		return nil
	})
	h("POST "+base+"/mkdir", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		var o struct{ Path string }
		if err := body(r, &o); err != nil {
			return nil, err
		}
		return nil, s.FileMkdir(o.Path)
	})
	h("POST "+base+"/move", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		var o struct {
			From, To  string
			Overwrite bool
		}
		if err := body(r, &o); err != nil {
			return nil, err
		}
		return nil, s.FileMove(o.From, o.To, o.Overwrite)
	})
	h("POST "+base+"/copy", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		var o struct {
			From, To  string
			Overwrite bool
		}
		if err := body(r, &o); err != nil {
			return nil, err
		}
		return s.FileCopy(o.From, o.To, o.Overwrite)
	})
	h("POST "+base+"/delete", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		var o struct{ Paths []string }
		if err := body(r, &o); err != nil {
			return nil, err
		}
		res, err := s.FileDelete(o.Paths)
		if err != nil {
			return nil, err
		}
		return map[string]any{"trashed": len(res.Trashed), "failed": res.Failed}, nil
	})
	h("POST "+base+"/unzip", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		var o struct {
			Path, Into string
			Overwrite  bool
		}
		if err := body(r, &o); err != nil {
			return nil, err
		}
		return s.FileUnzip(o.Path, o.Into, o.Overwrite)
	})
	h("GET "+base+"/trash", func(r *http.Request) (any, error) {
		s, err := srv(r, false)
		if err != nil {
			return nil, err
		}
		return s.Trash()
	})
	h("POST "+base+"/trash/{tid}/restore", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		var o struct {
			To        string
			Overwrite bool
		}
		if err := body(r, &o); err != nil {
			return nil, err
		}
		to, err := s.TrashRestore(r.PathValue("tid"), o.To, o.Overwrite)
		if err != nil {
			return nil, err
		}
		return map[string]string{"path": to}, nil
	})
	h("DELETE "+base+"/trash/{tid}", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		_, err = s.TrashEmpty(r.PathValue("tid"))
		return nil, err
	})
	h("POST "+base+"/trash/empty", func(r *http.Request) (any, error) {
		s, err := srv(r, true)
		if err != nil {
			return nil, err
		}
		var o struct{ Confirm bool }
		if err := body(r, &o); err != nil {
			return nil, err
		}
		if !o.Confirm {
			return nil, bad("emptying the trash deletes for good: send {\"confirm\": true}")
		}
		n, err := s.TrashEmpty("")
		return map[string]int{"deleted": n}, err
	})
}

// attachment makes a response a download and nothing else: whatever the file is, the browser
// saves it, never shows it, so an uploaded page or picture can't run from MineNest's address.
func attachment(w http.ResponseWriter, name string) {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || strings.ContainsRune(`"\;/%`, r) {
			return '_'
		}
		return r
	}, name)
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, strings.ReplaceAll(url.QueryEscape(name), "+", "%20")))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
}
