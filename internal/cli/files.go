package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const filesUsage = `minenest files ls [PATH] [-s SERVER]             list a folder in the server's folder
minenest files cat PATH [-s SERVER]              show a text file (a log shows its newest lines)
minenest files put FILE... [--to DIR] [--replace] [-s SERVER]   upload files
minenest files get PATH [LOCAL] [--replace] [-s SERVER]         download a file, or a folder as a zip
minenest files mkdir PATH [-s SERVER]            make a folder
minenest files mv FROM TO [--replace] [-s SERVER]   move or rename (TO ending in / is a folder to put it in)
minenest files cp FROM TO [--replace] [-s SERVER]   copy
minenest files rm PATH... [-s SERVER]            move to the trash (nothing is deleted for good)
minenest files unzip PATH [--into NAME] [--replace] [-s SERVER]   unpack a .zip where it is
minenest files trash [ls|restore ID|empty] [-s SERVER]   what rm moved: bring it back, or delete it for good
Paths are inside the server's folder, with / between folders.`

// pathArg turns what was typed into a path inside the server's folder. "/" and "." mean the folder itself.
func pathArg(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "/" || p == "." || p == "./" {
		return ""
	}
	return strings.TrimPrefix(p, "./")
}

// raw sends a request whose body or answer isn't JSON (uploads and downloads), with no time limit.
func (c *client) raw(method, p string, body io.Reader, length int64) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+p, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-MineNest", "1")
	if body != nil {
		req.ContentLength = length
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, errDaemon
	}
	return resp, nil
}

// answerError turns a non-200 answer into an error with MineNest's own words, remembering the status.
type statusError struct {
	code int
	msg  string
}

func (e statusError) Error() string { return e.msg }

func answerError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e struct{ Error string }
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return statusError{resp.StatusCode, e.Error}
	}
	return statusError{resp.StatusCode, "MineNest answered " + resp.Status}
}

func isStatus(err error, code int) bool {
	var se statusError
	return errors.As(err, &se) && se.code == code
}

// filesDo is c.do, but keeps the status of a refusal so a 409 ("already there") can be told apart.
func (c *client) filesDo(method, p string, in, out any) error {
	var rd io.Reader
	var n int64
	if in != nil {
		b, _ := json.Marshal(in)
		rd, n = bytes.NewReader(b), int64(len(b))
	}
	resp, err := c.raw(method, p, rd, n)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return answerError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func files(c *client, f flags) error {
	if len(f.pos) == 0 {
		fmt.Println(filesUsage)
		return nil
	}
	sub, args := f.pos[0], f.pos[1:]
	id, err := pick(c, f.server(false))
	if err != nil {
		return err
	}
	base := sp(id) + "/files"
	replace := f.bools["replace"] || f.bools["yes"]
	need := func(n int, what string) error {
		if len(args) < n {
			return fmt.Errorf("say %s: minenest files %s", what, sub)
		}
		return nil
	}
	switch sub {
	case "ls", "list":
		p := ""
		if len(args) > 0 {
			p = pathArg(args[0])
		}
		var l struct {
			Path    string
			Entries []struct {
				Name       string
				Dir, Link  bool
				Size       int64
				Items, Mod int64
			}
			Hidden int
		}
		if err := c.filesDo("GET", base+"?path="+url.QueryEscape(p), nil, &l); err != nil {
			return err
		}
		fmt.Printf("/%s\n", l.Path)
		for _, e := range l.Entries {
			when := time.Unix(e.Mod, 0).Format("2006-01-02 15:04")
			switch {
			case e.Dir:
				fmt.Printf("  %-9s %-16s %s/\n", plural(e.Items), when, e.Name)
			case e.Link:
				fmt.Printf("  %-9s %-16s %s (a link, not followed)\n", "", when, e.Name)
			default:
				fmt.Printf("  %-9s %-16s %s\n", fsz(e.Size), when, e.Name)
			}
		}
		if len(l.Entries) == 0 {
			fmt.Println("  (empty)")
		}
		if l.Hidden > 0 {
			fmt.Printf("%d names MineNest won't handle aren't shown.\n", l.Hidden)
		}
		return nil
	case "cat", "show":
		if err := need(1, "which file"); err != nil {
			return err
		}
		var r struct {
			Text, Name string
			Tail       bool
			Lines      int
		}
		if err := c.filesDo("GET", base+"/read?path="+url.QueryEscape(pathArg(args[0])), nil, &r); err != nil {
			return err
		}
		if r.Tail {
			fmt.Fprintf(os.Stderr, "(the newest %d lines; minenest files get downloads it all)\n", r.Lines)
		}
		fmt.Print(r.Text)
		if r.Text != "" && !strings.HasSuffix(r.Text, "\n") {
			fmt.Println()
		}
		return nil
	case "put", "upload":
		if err := need(1, "which files"); err != nil {
			return err
		}
		dir := pathArg(f.vals["to"])
		for _, local := range args {
			if err := putFile(c, base, dir, local, replace); err != nil {
				return err
			}
		}
		return nil
	case "get", "download":
		if err := need(1, "which file"); err != nil {
			return err
		}
		return getFile(c, base, pathArg(args[0]), args[1:], replace)
	case "mkdir":
		if err := need(1, "the folder's name"); err != nil {
			return err
		}
		if err := c.filesDo("POST", base+"/mkdir", map[string]string{"path": pathArg(args[0])}, nil); err != nil {
			return err
		}
		fmt.Println("Made.")
		return nil
	case "mv", "move", "rename":
		if err := need(2, "from and to"); err != nil {
			return err
		}
		from, to := pathArg(args[0]), pathArg(args[1])
		if strings.HasSuffix(args[1], "/") && to != "" {
			to = strings.TrimSuffix(to, "/") + "/" + path.Base(from)
		}
		return twoPaths(c, base+"/move", from, to, replace, "Moved.")
	case "cp", "copy":
		if err := need(2, "from and to"); err != nil {
			return err
		}
		from, to := pathArg(args[0]), pathArg(args[1])
		if strings.HasSuffix(args[1], "/") && to != "" {
			to = strings.TrimSuffix(to, "/") + "/" + path.Base(from)
		}
		return twoPaths(c, base+"/copy", from, to, replace, "Copied.")
	case "rm", "delete":
		if err := need(1, "what to remove"); err != nil {
			return err
		}
		var paths []string
		for _, a := range args {
			paths = append(paths, pathArg(a))
		}
		if err := c.filesDo("POST", base+"/delete", map[string]any{"paths": paths}, nil); err != nil {
			return err
		}
		fmt.Printf("Moved %d to the trash. Bring it back with: minenest files trash restore ID (minenest files trash lists them)\n", len(paths))
		return nil
	case "unzip":
		if err := need(1, "which zip"); err != nil {
			return err
		}
		body := map[string]any{"path": pathArg(args[0]), "into": f.vals["into"], "overwrite": replace}
		var r struct {
			Files   int
			Skipped []string
			Restart bool
		}
		if err := c.filesDo("POST", base+"/unzip", body, &r); err != nil {
			return replaceHint(err)
		}
		fmt.Printf("Unpacked %d files.\n", r.Files)
		if len(r.Skipped) > 0 {
			fmt.Printf("Left out (programs and scripts): %s\n", strings.Join(r.Skipped, ", "))
		}
		if r.Restart {
			fmt.Println("Plugins or mods were added: restart the server to load them.")
		}
		return nil
	case "trash":
		return trashCmd(c, base, args, replace)
	}
	fmt.Println(filesUsage)
	return fmt.Errorf("unknown files command %q", sub)
}

func replaceHint(err error) error {
	if isStatus(err, 409) {
		return fmt.Errorf("%s. Add --replace to replace it (what is replaced goes to the trash)", strings.TrimSuffix(err.Error(), "."))
	}
	return err
}

func twoPaths(c *client, p, from, to string, replace bool, done string) error {
	if err := c.filesDo("POST", p, map[string]any{"from": from, "to": to, "overwrite": replace}, nil); err != nil {
		return replaceHint(err)
	}
	fmt.Println(done)
	return nil
}

func putFile(c *client, base, dir, local string, replace bool) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a folder: put it in a zip, upload that, then unpack it with: minenest files unzip", local)
	}
	q := fmt.Sprintf("/upload?dir=%s&name=%s&overwrite=%d", url.QueryEscape(dir), url.QueryEscape(filepath.Base(local)), map[bool]int{true: 1, false: 0}[replace])
	resp, err := c.raw("POST", base+q, f, st.Size())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return replaceHint(answerError(resp))
	}
	var r struct {
		Path    string
		Restart bool
		Running bool
	}
	json.NewDecoder(resp.Body).Decode(&r)
	fmt.Printf("Uploaded %s (%s).\n", r.Path, fsz(st.Size()))
	if r.Restart {
		fmt.Println("It loads when the server starts" + map[bool]string{true: ": restart it now.", false: "."}[r.Running])
	}
	return nil
}

func getFile(c *client, base, remote string, rest []string, replace bool) error {
	kind, name := "/download", path.Base(remote)
	if remote == "" {
		kind, name = "/zip", "server"
	}
	fetch := func(kind string) (*http.Response, error) {
		resp, err := c.raw("GET", base+kind+"?path="+url.QueryEscape(remote), nil, 0)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			defer resp.Body.Close()
			return nil, answerError(resp)
		}
		return resp, nil
	}
	resp, err := fetch(kind)
	if err != nil && kind == "/download" && isStatus(err, 400) && strings.Contains(err.Error(), "folder") {
		kind, name = "/zip", name+".zip"
		resp, err = fetch(kind)
	} else if kind == "/zip" && err == nil && !strings.HasSuffix(name, ".zip") {
		name += ".zip"
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dest := name
	if len(rest) > 0 {
		dest = rest[0]
		if st, err := os.Stat(dest); err == nil && st.IsDir() {
			dest = filepath.Join(dest, name)
		}
	}
	if _, err := os.Stat(dest); err == nil && !replace {
		return fmt.Errorf("%s is already here. Add --replace to write over it", dest)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".minenest-get-*")
	if err != nil {
		return err
	}
	n, err := io.Copy(tmp, resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("the download stopped: %v", err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	fmt.Printf("Saved %s (%s).\n", dest, fsz(n))
	return nil
}

func trashCmd(c *client, base string, args []string, replace bool) error {
	sub := "ls"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "ls", "list":
		var t struct {
			Items []struct {
				ID, Name, Orig string
				Deleted, Size  int64
				Dir            bool
			}
			Size int64
		}
		if err := c.filesDo("GET", base+"/trash", nil, &t); err != nil {
			return err
		}
		if len(t.Items) == 0 {
			fmt.Println("The trash is empty.")
			return nil
		}
		for _, it := range t.Items {
			fmt.Printf("  %-26s %9s  %s  from /%s\n", it.ID, fsz(it.Size), time.Unix(it.Deleted, 0).Format("2006-01-02 15:04"), it.Orig)
		}
		fmt.Printf("It holds %s. Bring one back: minenest files trash restore ID. Delete everything for good: minenest files trash empty\n", fsz(t.Size))
		return nil
	case "restore":
		if len(args) < 2 {
			return fmt.Errorf("say which one (minenest files trash lists them)")
		}
		var r struct{ Path string }
		if err := c.filesDo("POST", base+"/trash/"+url.PathEscape(args[1])+"/restore", map[string]any{"overwrite": replace}, &r); err != nil {
			return replaceHint(err)
		}
		fmt.Printf("Put back as /%s.\n", r.Path)
		return nil
	case "empty":
		if !replace && !confirm("Delete everything in the trash for good? This can't be undone.") {
			return nil
		}
		var r struct{ Deleted int }
		if err := c.filesDo("POST", base+"/trash/empty", map[string]bool{"confirm": true}, &r); err != nil {
			return err
		}
		fmt.Printf("Deleted %d items for good.\n", r.Deleted)
		return nil
	}
	return fmt.Errorf("say ls, restore ID or empty")
}

func fsz(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

func plural(n int64) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}
