package nest

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// The file manager: browse, read, edit, upload, copy, move and delete the files in one server's
// own folder (servers/<name>/server), and nothing else. MineNest's own files (nest.json, door.json,
// the backups, the trash) sit beside that folder, so they are out of reach by construction.
//
// Every path the outside world hands in goes through cleanRel (words only: no "..", no drive
// letters, no streams, no device names) and then through (*Server).target, which walks the folders
// one by one and refuses any link, junction or alias on the way. A link is never followed, for
// reading, writing, copying, zipping or deleting.

// Limits, variables so the tests can make them small.
var (
	maxEditBytes   int64 = 2 << 20   // a text file bigger than this isn't opened for editing
	logTailBytes   int64 = 512 << 10 // how much of the end of a log is read
	logTailLines         = 3000
	maxUploadBytes int64 = 1 << 30 // one uploaded file
	maxListEntries       = 5000
	maxCopyFiles         = 100000
	maxCopyBytes   int64 = 20 << 30
	unzipMaxFiles        = 50000
	unzipMaxFile   int64 = 2 << 30 // one file inside an archive
	unzipMaxTotal  int64 = 4 << 30 // everything inside an archive
	maxPathDepth         = 40
	diskHeadroom   int64 = 64 << 20 // kept free when MineNest writes a big file
)

const internalPrefix = ".minenest-" // MineNest's own temporary names inside a server's folder

var (
	errNotThere = apiError{404, "there's nothing at that path"}
	trashIDRe   = regexp.MustCompile(`^[0-9]{10,16}-[0-9a-f]{8}$`)
)

func fbad(format string, a ...any) error      { return apiError{400, fmt.Sprintf(format, a...)} }
func fblocked(format string, a ...any) error  { return apiError{403, fmt.Sprintf(format, a...)} }
func fconflict(format string, a ...any) error { return apiError{409, fmt.Sprintf(format, a...)} }
func ftoobig(format string, a ...any) error   { return apiError{413, fmt.Sprintf(format, a...)} }

// ------------------------------------------------------------------ names and paths

var reservedDevice = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}

var superscripts = strings.NewReplacer("¹", "1", "²", "2", "³", "3")

// reservedName reports a Windows device name (CON, NUL, COM1...), with or without an extension.
// It's refused on every system, so a server moved to Windows never has one.
func reservedName(name string) bool {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	up := superscripts.Replace(strings.ToUpper(strings.TrimRight(base, " ")))
	if reservedDevice[up] {
		return true
	}
	if len(up) == 4 && (strings.HasPrefix(up, "COM") || strings.HasPrefix(up, "LPT")) && up[3] >= '1' && up[3] <= '9' {
		return true
	}
	return false
}

// checkComponent says whether name is fine as one file or folder name.
func checkComponent(name string) error {
	switch {
	case name == "":
		return fbad("a file or folder name is empty")
	case name == "." || name == "..":
		return fbad("%q can't be used in a path", name)
	case len(name) > 255:
		return fbad("a name is longer than 255 letters")
	case !utf8.ValidString(name):
		return fbad("a name isn't valid text")
	case strings.HasPrefix(name, internalPrefix):
		return fbad("names starting with %q are MineNest's own", internalPrefix)
	}
	for _, r := range name {
		switch {
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return fbad("a name has a control character in it")
		case strings.ContainsRune(`<>:"|?*\/`, r):
			return fbad("names can't contain %q", string(r))
		case r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff:
			return fbad("a name has hidden direction characters in it")
		case unicode.Is(unicode.Cs, r):
			return fbad("a name isn't valid text")
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fbad("a name can't end with a dot or a space")
	}
	if reservedName(name) {
		return fbad("%q is a name Windows keeps for devices", name)
	}
	return nil
}

// cleanRel checks a path as the outside world wrote it (always with "/" between folders, relative
// to the server's folder) and returns it in its plain form; "" is the folder itself.
func cleanRel(rel string) (string, error) {
	if len(rel) > 1000 {
		return "", fbad("that path is too long")
	}
	if strings.ContainsRune(rel, 0) {
		return "", fbad("that path has a NUL in it")
	}
	if strings.Contains(rel, `\`) {
		return "", fbad(`write paths with / between the folders, not \`)
	}
	if strings.HasPrefix(rel, "/") {
		return "", fbad("paths start inside the server's folder, not at the root of the disk")
	}
	rel = strings.TrimSuffix(rel, "/")
	if rel == "" {
		return "", nil
	}
	parts := strings.Split(rel, "/")
	if len(parts) > maxPathDepth {
		return "", fbad("that path goes too many folders deep")
	}
	for _, p := range parts {
		if err := checkComponent(p); err != nil {
			return "", err
		}
	}
	return strings.Join(parts, "/"), nil
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// insideRoot reports whether p is root itself or in it, comparing like the system does.
func insideRoot(root, p string) bool {
	if runtime.GOOS == "windows" {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	return within(root, p)
}

func isLinkMode(m fs.FileMode) bool { return m&(fs.ModeSymlink|fs.ModeIrregular) != 0 }

func internalName(name string) bool { return strings.HasPrefix(name, internalPrefix) }

// fsTarget is a path inside a server's folder that has been checked.
type fsTarget struct {
	Abs  string
	Rel  string      // cleaned, with /
	Name string      // the last part ("" for the folder itself)
	Info fs.FileInfo // Lstat of it, nil if it doesn't exist
}

func (t *fsTarget) exists() bool { return t.Info != nil }
func (t *fsTarget) isLink() bool { return t.Info != nil && isLinkMode(t.Info.Mode()) }
func (t *fsTarget) isDir() bool  { return t.Info != nil && t.Info.IsDir() && !t.isLink() }
func (t *fsTarget) isFile() bool { return t.Info != nil && t.Info.Mode().IsRegular() }

func (s *Server) rootReal() (string, error) {
	r, err := filepath.EvalSymlinks(s.Root())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", apiError{404, "this server has no files yet"}
		}
		return "", err
	}
	return filepath.Abs(r)
}

// target resolves rel against the server's folder. Every folder on the way has to be a real folder
// that is where its name says (no link, junction, mount point or short-name alias), so what is
// reached can only be inside. The last part may be a link: it is then reported as one, never
// followed.
func (s *Server) target(rel string) (*fsTarget, error) {
	clean, err := cleanRel(rel)
	if err != nil {
		return nil, err
	}
	rr, err := s.rootReal()
	if err != nil {
		return nil, err
	}
	t := &fsTarget{Abs: rr, Rel: clean}
	if clean == "" {
		t.Info, err = os.Lstat(rr)
		return t, err
	}
	parts := strings.Split(clean, "/")
	t.Name = parts[len(parts)-1]
	t.Abs = filepath.Join(rr, filepath.FromSlash(clean))
	cur := rr
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return t, nil // the rest isn't there either
		}
		if err != nil {
			return nil, err
		}
		if isLinkMode(fi.Mode()) {
			return nil, fblocked("%q is a link, and MineNest doesn't follow links out of a server's folder", p)
		}
		if !fi.IsDir() {
			return nil, fbad("%q isn't a folder", p)
		}
		if real, err := filepath.EvalSymlinks(cur); err != nil || !samePath(real, cur) || !insideRoot(rr, real) {
			return nil, fblocked("%q isn't where its name says (a link or an alias), so it was left alone", p)
		}
	}
	fi, err := os.Lstat(t.Abs)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	t.Info = fi
	if !isLinkMode(fi.Mode()) {
		if real, err := filepath.EvalSymlinks(t.Abs); err != nil || !samePath(real, t.Abs) || !insideRoot(rr, real) {
			return nil, fblocked("%q isn't where its name says (a link or an alias), so it was left alone", t.Name)
		}
	}
	return t, nil
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func parentRel(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

func lowerRel(rel string) string { return strings.ToLower(rel) }

// isUnder reports whether rel is dir or inside it (both cleaned, case-blind).
func isUnder(dir, rel string) bool {
	d, r := lowerRel(dir), lowerRel(rel)
	return d == "" || r == d || strings.HasPrefix(r, d+"/")
}

// ------------------------------------------------------------------ while the server runs

// levelOK says whether a level-name is one plain folder name inside the server's folder (not "",
// not "..", with no folders in it). The world code backs up and removes whatever this names, so
// nothing else is ever accepted.
func levelOK(n string) bool {
	c, err := cleanRel(n)
	return err == nil && c != "" && c == n && !strings.Contains(n, "/")
}

// levelName is the world's folder name; an unusable level-name counts as the default.
func (s *Server) levelName() string {
	n := s.props().GetDefault("level-name", "world")
	if !levelOK(n) {
		return "world"
	}
	return n
}

// worldDirs are the folders the server keeps its worlds in.
func (s *Server) worldDirs() []string {
	l := s.levelName()
	dirs := []string{l, l + "_nether", l + "_the_end"}
	if ents, err := os.ReadDir(s.Root()); err == nil {
		for _, e := range ents {
			if e.IsDir() && !isLinkMode(e.Type()) && exists(filepath.Join(s.Root(), e.Name(), "level.dat")) {
				dirs = append(dirs, e.Name())
			}
		}
	}
	return dirs
}

// runningFiles are the files the running server program is made of, relative to its folder.
func (s *Server) runningFiles() []string {
	c := s.Config()
	if c.Installed == nil {
		return nil
	}
	var out []string
	for _, a := range c.Installed.Launch {
		a = strings.TrimPrefix(a, "@")
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		if cl, err := cleanRel(filepath.ToSlash(a)); err == nil && cl != "" && (strings.HasSuffix(strings.ToLower(cl), ".jar") || strings.HasSuffix(strings.ToLower(cl), ".txt")) {
			out = append(out, cl)
		}
	}
	return out
}

const liveWhy = "The server is running and has this open. Stop the server first"

// liveLexical is the part of liveWhy that can be told from the name alone.
func liveLexical(rel string) bool {
	parts := strings.Split(lowerRel(rel), "/")
	for _, p := range parts {
		if p == "region" || p == "entities" || p == "poi" {
			return true
		}
	}
	last := parts[len(parts)-1]
	switch last {
	case "session.lock", "level.dat", "level.dat_old":
		return true
	}
	return strings.HasSuffix(last, ".mca") || strings.HasSuffix(last, ".mcr")
}

// liveBlock returns why changes to rel must wait, or "". destructive is for deleting, moving
// away or replacing something that exists; deep also looks inside a folder for the world's files.
func (s *Server) liveBlock(rel string, isDir, destructive, deep bool) string {
	s.mu.Lock()
	state, busy := s.state, s.busy
	s.mu.Unlock()
	if busy != "" {
		return "MineNest is busy with " + busy + " on this server. Wait for it to finish"
	}
	if state == Stopped {
		return ""
	}
	if liveLexical(rel) {
		return liveWhy + " (it's part of the world it is using)"
	}
	for _, f := range s.runningFiles() {
		if lowerRel(f) == lowerRel(rel) {
			return liveWhy + " (it's the server's own program)"
		}
	}
	if destructive && isDir {
		for _, w := range s.worldDirs() {
			if isUnder(rel, w) {
				return liveWhy + " (it holds the world)"
			}
		}
		for _, f := range s.runningFiles() {
			if isUnder(rel, f) {
				return liveWhy + " (it holds the server's own program)"
			}
		}
		if deep {
			if abs, err := s.target(rel); err == nil && abs.exists() && holdsLive(abs.Abs) {
				return liveWhy + " (there are world files in it)"
			}
		}
	}
	return ""
}

// holdsLive looks (a bounded number of files deep) for a world's files inside a folder.
func holdsLive(dir string) bool {
	n := 0
	found := false
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if isLinkMode(d.Type()) {
			return nil
		}
		if n++; n > 5000 {
			return fs.SkipAll
		}
		if liveLexical(d.Name()) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// managedFile says why a file MineNest writes itself can't be changed by hand right now.
func (s *Server) managedFile(rel string) string {
	return ""
}

// ------------------------------------------------------------------ what kind of file

var textExts = map[string]bool{"properties": true, "yml": true, "yaml": true, "json": true, "toml": true, "txt": true,
	"conf": true, "cfg": true, "mcfunction": true, "js": true, "sk": true, "md": true, "ini": true, "xml": true,
	"csv": true, "json5": true, "mcmeta": true, "lang": true}

// blockedUpload: programs and scripts that run when opened on Windows. A server's folder has no
// use for them, and they can't be uploaded or unpacked into it.
var blockedUpload = map[string]bool{"exe": true, "com": true, "scr": true, "msi": true, "bat": true, "cmd": true,
	"ps1": true, "vbs": true, "vbe": true, "wsf": true, "lnk": true, "hta": true, "cpl": true, "reg": true,
	"js": true, "jse": true, "wsh": true, "url": true, "pif": true, "msc": true, "scf": true, "inf": true,
	"msp": true, "application": true, "appref-ms": true, "gadget": true, "psm1": true, "ps1xml": true, "psc1": true}

// newFileWhy is the one place that says whether a file may be created under this name by
// anything but the text editor (an upload, a move or copy to a new name, unpacking a zip,
// putting something back). srcName is what it was called before ("" for something new): a program
// that is already there can still be renamed, but a new one can't be made by renaming.
// server.properties is only changed through the editor, where MineNest checks it.
func newFileWhy(rel, srcName string) string {
	name := path.Base(rel)
	if ext := extOf(name); blockedUpload[ext] && ext != extOf(srcName) {
		return "programs and scripts (." + ext + ") can't be put in a server's folder"
	}
	if lowerRel(rel) == "server.properties" {
		return "changing server.properties is done by opening it in the editor, where MineNest checks it, not by putting a file over it"
	}
	return ""
}

func extOf(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
}

// fileKind is "text" (can be edited), "log" (can be read) or "other" (can only be downloaded).
func fileKind(name string) string {
	l := strings.ToLower(name)
	switch {
	case strings.HasSuffix(l, ".log"), strings.HasSuffix(l, ".log.gz"):
		return "log"
	case textExts[extOf(name)]:
		return "text"
	}
	return "other"
}

func needsRestart(rel string) bool {
	l := strings.ToLower(rel)
	return strings.HasSuffix(l, ".jar") && (strings.HasPrefix(l, "plugins/") || strings.HasPrefix(l, "mods/"))
}

// ------------------------------------------------------------------ listing

type FileEntry struct {
	Name   string `json:"name"`
	Dir    bool   `json:"dir"`
	Link   bool   `json:"link,omitempty"`
	Size   int64  `json:"size"`
	Items  int    `json:"items"` // in a folder
	Mod    int64  `json:"mod"`
	Kind   string `json:"kind"`             // dir, text, log, other, link
	Locked string `json:"locked,omitempty"` // why changing it is refused while the server runs
}

type FileList struct {
	Path      string      `json:"path"`
	Entries   []FileEntry `json:"entries"`
	Hidden    int         `json:"hidden"` // names MineNest won't handle
	Truncated bool        `json:"truncated"`
	Running   bool        `json:"running"`
	Free      int64       `json:"free"`
	MaxUpload int64       `json:"max_upload"`
	MaxEdit   int64       `json:"max_edit"`
}

func (s *Server) FileList(rel string) (*FileList, error) {
	t, err := s.target(rel)
	if err != nil {
		return nil, err
	}
	if !t.exists() {
		return nil, apiError{404, "there's no such folder"}
	}
	if t.isLink() {
		return nil, fblocked("that is a link, and MineNest doesn't open what links point to")
	}
	if !t.isDir() {
		return nil, fbad("that is a file, not a folder")
	}
	ents, err := os.ReadDir(t.Abs)
	if err != nil {
		return nil, err
	}
	sweepDir(t.Abs, ents)
	out := &FileList{Path: t.Rel, Entries: []FileEntry{}, Running: s.State() != Stopped, MaxUpload: maxUploadBytes, MaxEdit: maxEditBytes}
	if f, err := diskFree(t.Abs); err == nil {
		out.Free = f
	} else {
		out.Free = -1
	}
	for _, e := range ents {
		name := e.Name()
		if internalName(name) {
			continue
		}
		if checkComponent(name) != nil {
			out.Hidden++
			continue
		}
		if len(out.Entries) >= maxListEntries {
			out.Truncated = true
			break
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		fe := FileEntry{Name: name, Mod: fi.ModTime().Unix()}
		child := joinRel(t.Rel, name)
		switch {
		case isLinkMode(fi.Mode()):
			fe.Link, fe.Kind = true, "link"
		case fi.IsDir():
			fe.Dir, fe.Kind = true, "dir"
			if sub, err := os.ReadDir(filepath.Join(t.Abs, name)); err == nil {
				fe.Items = len(sub)
			}
		case fi.Mode().IsRegular():
			fe.Size, fe.Kind = fi.Size(), fileKind(name)
		default:
			fe.Kind = "other"
		}
		if out.Running {
			fe.Locked = s.liveBlock(child, fe.Dir, true, false)
		}
		out.Entries = append(out.Entries, fe)
	}
	sort.SliceStable(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out, nil
}

// ------------------------------------------------------------------ reading text

type FileContent struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Mod      int64  `json:"mod"`
	Version  string `json:"version"`
	Text     string `json:"text"`
	Encoding string `json:"encoding"` // utf-8, utf-8-bom, utf-16le, utf-16be, latin1
	EOL      string `json:"eol"`      // lf, crlf, cr
	Mixed    bool   `json:"mixed"`    // the file had more than one kind of line ending
	Kind     string `json:"kind"`
	ReadOnly bool   `json:"read_only"`
	Why      string `json:"why,omitempty"` // why it is read only
	Tail     bool   `json:"tail"`          // only the newest lines are shown
	Lines    int    `json:"lines"`
}

func versionOf(fi fs.FileInfo) string {
	return fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())
}

var errBinary = fbad("that doesn't look like a text file, so it isn't opened here. You can download it instead")

// decodeText turns the bytes of a text file into text, saying how they were encoded.
func decodeText(b []byte) (string, string, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}):
		b = b[3:]
		if !utf8.Valid(b) {
			return "", "", errBinary
		}
		return string(b), "utf-8-bom", nil
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}), bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		be := b[0] == 0xfe
		b = b[2:]
		if len(b)%2 != 0 {
			return "", "", errBinary
		}
		u := make([]uint16, len(b)/2)
		for i := range u {
			if be {
				u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
			} else {
				u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
			}
		}
		return string(utf16.Decode(u)), map[bool]string{true: "utf-16be", false: "utf-16le"}[be], nil
	}
	head := b
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "", "", errBinary
	}
	ctl := 0
	for _, c := range b {
		if (c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f') || c == 0x7f {
			ctl++
		}
	}
	if ctl > 0 && ctl*100 > len(b) {
		return "", "", errBinary
	}
	if utf8.Valid(b) {
		return string(b), "utf-8", nil
	}
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	return string(rs), "latin1", nil
}

func encodeText(text, enc string) ([]byte, error) {
	switch enc {
	case "", "utf-8":
		return []byte(text), nil
	case "utf-8-bom":
		return append([]byte{0xef, 0xbb, 0xbf}, text...), nil
	case "utf-16le", "utf-16be":
		u := utf16.Encode([]rune(text))
		be := enc == "utf-16be"
		out := make([]byte, 0, 2+2*len(u))
		if be {
			out = append(out, 0xfe, 0xff)
		} else {
			out = append(out, 0xff, 0xfe)
		}
		for _, c := range u {
			if be {
				out = append(out, byte(c>>8), byte(c))
			} else {
				out = append(out, byte(c), byte(c>>8))
			}
		}
		return out, nil
	case "latin1":
		out := make([]byte, 0, len(text))
		for _, r := range text {
			if r > 255 {
				return nil, fbad("this file is saved in a plain Western encoding that can't hold %q. Take that letter out, or put it in as a code like \\u%04X", string(r), r)
			}
			out = append(out, byte(r))
		}
		return out, nil
	}
	return nil, fbad("unknown encoding %q", enc)
}

// splitEOL turns every kind of line ending into "\n" and says which one the text used most.
func splitEOL(raw string) (text, eol string, mixed bool) {
	crlf := strings.Count(raw, "\r\n")
	lf := strings.Count(raw, "\n") - crlf
	cr := strings.Count(raw, "\r") - crlf
	kinds := 0
	for _, n := range []int{crlf, lf, cr} {
		if n > 0 {
			kinds++
		}
	}
	eol = "lf"
	switch {
	case crlf >= lf && crlf >= cr && crlf > 0:
		eol = "crlf"
	case cr > lf && cr > crlf:
		eol = "cr"
	}
	if kinds == 0 {
		return raw, "lf", false
	}
	text = strings.ReplaceAll(raw, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text, eol, kinds > 1
}

func joinEOL(text, eol string) (string, error) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	switch eol {
	case "", "lf":
		return text, nil
	case "crlf":
		return strings.ReplaceAll(text, "\n", "\r\n"), nil
	case "cr":
		return strings.ReplaceAll(text, "\n", "\r"), nil
	}
	return "", fbad("unknown line ending %q", eol)
}

func (s *Server) FileRead(rel string) (*FileContent, error) {
	t, err := s.target(rel)
	if err != nil {
		return nil, err
	}
	if !t.exists() {
		return nil, errNotThere
	}
	if t.isLink() {
		return nil, fblocked("that is a link, and MineNest doesn't open links")
	}
	if !t.isFile() {
		return nil, fbad("that's a folder: open it from the list, or download it as a zip")
	}
	kind := fileKind(t.Name)
	if kind == "other" {
		return nil, fbad("MineNest opens text files here (%s). Download this one instead", textKinds())
	}
	f, err := os.Open(t.Abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fbad("that isn't a plain file")
	}
	c := &FileContent{Path: t.Rel, Name: t.Name, Size: fi.Size(), Mod: fi.ModTime().Unix(), Version: versionOf(fi), Kind: kind}
	if kind == "log" {
		raw, tail, err := readLogTail(f, t.Name, fi.Size())
		if err != nil {
			return nil, err
		}
		text := strings.ToValidUTF8(string(raw), "�")
		text = strings.ReplaceAll(text, "\x00", "")
		text, c.EOL, _ = splitEOL(text)
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
		if len(lines) > logTailLines {
			lines, tail = lines[len(lines)-logTailLines:], true
		}
		c.Text, c.Tail, c.Lines, c.Encoding = strings.Join(lines, "\n"), tail, len(lines), "utf-8"
		c.ReadOnly, c.Why = true, "Logs can be read here but not changed."
		return c, nil
	}
	if fi.Size() > maxEditBytes {
		return nil, ftoobig("that file is %s, too big to open here (the limit is %s). Download it instead", humanSize(fi.Size()), humanSize(maxEditBytes))
	}
	b, err := io.ReadAll(io.LimitReader(f, maxEditBytes+1))
	if err != nil {
		return nil, err
	}
	text, enc, err := decodeText(b)
	if err != nil {
		return nil, err
	}
	c.Encoding = enc
	var mixed bool
	c.Text, c.EOL, mixed = splitEOL(text)
	c.Mixed = mixed
	c.Lines = strings.Count(c.Text, "\n") + 1
	return c, nil
}

func textKinds() string {
	return "properties, yml, yaml, json, toml, txt, log, conf, cfg, mcfunction, js, sk and md files"
}

// readLogTail reads the newest part of a log (a .log.gz is unpacked as far as it is safe to).
func readLogTail(f *os.File, name string, size int64) ([]byte, bool, error) {
	if strings.HasSuffix(strings.ToLower(name), ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, false, fbad("that log can't be unpacked")
		}
		var keep []byte
		buf := make([]byte, 64<<10)
		var total int64
		cut := false
		for {
			n, err := zr.Read(buf)
			keep = append(keep, buf[:n]...)
			total += int64(n)
			if int64(len(keep)) > 2*logTailBytes {
				keep, cut = append([]byte(nil), keep[int64(len(keep))-logTailBytes:]...), true
			}
			if err != nil {
				break
			}
			if total > 512<<20 {
				cut = true // a log that large is cut off, whatever it holds
				break
			}
		}
		if int64(len(keep)) > logTailBytes {
			keep, cut = keep[int64(len(keep))-logTailBytes:], true
		}
		if cut {
			keep = dropFirstLine(keep)
		}
		return keep, cut, nil
	}
	cut := false
	if size > logTailBytes {
		if _, err := f.Seek(size-logTailBytes, io.SeekStart); err != nil {
			return nil, false, err
		}
		cut = true
	}
	b, err := io.ReadAll(io.LimitReader(f, logTailBytes+(1<<20)))
	if err != nil {
		return nil, false, err
	}
	if cut {
		b = dropFirstLine(b)
	}
	return b, cut, nil
}

func dropFirstLine(b []byte) []byte {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[i+1:]
	}
	return b
}

// ------------------------------------------------------------------ writing

// atomicWrite writes a file through a temporary file in the same folder, then renames it into
// place, so a crash or a full disk never leaves half a file.
func atomicWrite(dst string, perm fs.FileMode, fill func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(dst), internalPrefix+"tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := fill(f); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Chmod(tmp, perm)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("the file couldn't be replaced (is something using it?): %v", err)
	}
	return nil
}

// WriteRequest is a save from the editor.
type WriteRequest struct {
	Path     string `json:"path"`
	Text     string `json:"text"`
	Encoding string `json:"encoding"`
	EOL      string `json:"eol"`
	Version  string `json:"version"` // the version that was opened; a different one on disk means it changed since
	Force    bool   `json:"force"`
	Create   bool   `json:"create"` // a new file
}

func (s *Server) FileWrite(w WriteRequest) (*FileContent, error) {
	t, err := s.target(w.Path)
	if err != nil {
		return nil, err
	}
	if t.Rel == "" {
		return nil, fbad("say which file")
	}
	if fileKind(t.Name) != "text" {
		if fileKind(t.Name) == "log" {
			return nil, fblocked("logs can't be changed")
		}
		return nil, fbad("MineNest edits text files here (%s)", textKinds())
	}
	if t.isLink() {
		return nil, fblocked("that is a link, and MineNest doesn't write through links")
	}
	if t.exists() && !t.isFile() {
		return nil, fbad("that is a folder")
	}
	if t.exists() && w.Create {
		return nil, fconflict("%q already exists", t.Name)
	}
	if !t.exists() && !w.Create {
		return nil, errNotThere
	}
	if !t.exists() {
		if pt, err := s.target(parentRel(t.Rel)); err != nil {
			return nil, err
		} else if !pt.isDir() {
			return nil, fbad("the folder to put it in isn't there")
		}
	}
	if why := s.liveBlock(t.Rel, false, t.exists(), false); why != "" {
		return nil, fblocked("%s", why)
	}
	if why := s.managedFile(t.Rel); why != "" {
		return nil, fblocked("%s", why)
	}
	perm := fs.FileMode(0o644)
	if t.exists() {
		perm = t.Info.Mode().Perm()
		if !w.Force && w.Version != "" && versionOf(t.Info) != w.Version {
			return nil, fconflict("this file was changed by something else since you opened it")
		}
	}
	if w.Encoding == "" {
		w.Encoding = "utf-8"
	}
	text := strings.ReplaceAll(strings.ReplaceAll(w.Text, "\r\n", "\n"), "\r", "\n")
	if lowerRel(t.Rel) == "server.properties" {
		var err error
		if text, err = s.checkProperties(text); err != nil {
			return nil, err
		}
	}
	text, err = joinEOL(text, w.EOL)
	if err != nil {
		return nil, err
	}
	b, err := encodeText(text, w.Encoding)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxEditBytes {
		return nil, ftoobig("that is more than %s of text, too much for this editor", humanSize(maxEditBytes))
	}
	if err := atomicWrite(t.Abs, perm, func(wr io.Writer) error { _, err := wr.Write(b); return err }); err != nil {
		return nil, err
	}
	// A save of the same length within one tick of the clock would look like the same version, and a
	// second window's older copy could then be saved over it. Move the time on so it can't.
	if t.exists() {
		if fi, err := os.Stat(t.Abs); err == nil && versionOf(fi) == versionOf(t.Info) {
			later := t.Info.ModTime().Add(time.Millisecond)
			os.Chtimes(t.Abs, later, later)
		}
	}
	if t.Rel != "" && s.State() != Stopped {
		s.Note("%s was changed in the file manager. Restart the server for it to count.", t.Name)
	}
	return s.FileRead(t.Rel)
}

// checkProperties makes a hand-edited server.properties safe for MineNest: the port has to be a
// real one. Everything else, the
// comments and the order included, is the user's.
func (s *Server) checkProperties(text string) (string, error) {
	p := propertiesFromText(text)
	if v, ok := p.Get("level-name"); ok && !levelOK(v) {
		return "", fbad("the world's folder (level-name) has to be one plain folder name, like world")
	}
	cur := s.props()
	if v, ok := p.Get("server-port"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1024 || n > 65535 {
			return "", fbad("the port (server-port) has to be a number from 1024 to 65535")
		}
	} else if v, ok := cur.Get("server-port"); ok {
		p.Set("server-port", v) // MineNest needs to know the port
	}
	return p.Text(), nil
}

// ------------------------------------------------------------------ making folders, moving, copying

func (s *Server) FileMkdir(rel string) error {
	t, err := s.target(rel)
	if err != nil {
		return err
	}
	if t.Rel == "" {
		return fbad("say a name for the new folder")
	}
	if t.exists() {
		return fconflict("%q already exists", t.Name)
	}
	if why := s.liveBlock(t.Rel, true, false, false); why != "" {
		return fblocked("%s", why)
	}
	if pt, err := s.target(parentRel(t.Rel)); err != nil {
		return err
	} else if !pt.isDir() {
		return fbad("the folder to put it in isn't there")
	}
	return os.Mkdir(t.Abs, 0o755)
}

func sameFile(a, b fs.FileInfo) bool { return os.SameFile(a, b) }

// FileMove renames or moves a file or folder. Something already at the new place is only replaced
// when overwrite is set, and then it goes to the trash, not the void.
func (s *Server) FileMove(from, to string, overwrite bool) error {
	src, err := s.target(from)
	if err != nil {
		return err
	}
	dst, err := s.target(to)
	if err != nil {
		return err
	}
	if src.Rel == "" || dst.Rel == "" {
		return fbad("the server's own folder can't be moved")
	}
	if !src.exists() {
		return errNotThere
	}
	if why := s.liveBlock(src.Rel, src.isDir(), true, true); why != "" {
		return fblocked("%s", why)
	}
	if why := s.liveBlock(dst.Rel, src.isDir(), false, false); why != "" {
		return fblocked("%s", why)
	}
	if why := s.managedFile(src.Rel); why != "" {
		return fblocked("%s", why)
	}
	if why := s.managedFile(dst.Rel); why != "" {
		return fblocked("%s", why)
	}
	if src.Rel == dst.Rel {
		return fbad("that's where it already is")
	}
	if !src.isDir() {
		if why := newFileWhy(dst.Rel, src.Name); why != "" {
			return fbad("%s", why)
		}
	}
	// A folder can't go inside itself (a change of capital letters only is fine).
	if src.isDir() && isUnder(src.Rel, dst.Rel) && lowerRel(src.Rel) != lowerRel(dst.Rel) {
		return fbad("a folder can't be moved into itself")
	}
	if pt, err := s.target(parentRel(dst.Rel)); err != nil {
		return err
	} else if !pt.isDir() {
		return fbad("the folder to move it to isn't there")
	}
	if dst.exists() && !sameFile(src.Info, dst.Info) {
		if !overwrite {
			return fconflict("%q is already there", dst.Name)
		}
		if dst.isDir() || src.isDir() {
			return fbad("a folder is in the way, so nothing was replaced")
		}
		if why := s.liveBlock(dst.Rel, false, true, false); why != "" {
			return fblocked("%s", why)
		}
		if _, err := s.toTrash(dst); err != nil {
			return err
		}
	}
	if err := os.Rename(src.Abs, dst.Abs); err != nil {
		return fmt.Errorf("it couldn't be moved (is something using it?)")
	}
	return nil
}

// CopyResult says what a copy left out.
type CopyResult struct {
	Files   int `json:"files"`
	Skipped int `json:"skipped"` // links and other things that aren't plain files
}

func (s *Server) FileCopy(from, to string, overwrite bool) (*CopyResult, error) {
	src, err := s.target(from)
	if err != nil {
		return nil, err
	}
	dst, err := s.target(to)
	if err != nil {
		return nil, err
	}
	if src.Rel == "" || dst.Rel == "" {
		return nil, fbad("the server's own folder can't be copied")
	}
	if !src.exists() {
		return nil, errNotThere
	}
	if src.isLink() || (!src.isFile() && !src.isDir()) {
		return nil, fblocked("links and special files aren't copied")
	}
	if src.isDir() && isUnder(src.Rel, dst.Rel) {
		return nil, fbad("a folder can't be copied into itself")
	}
	if lowerRel(src.Rel) == lowerRel(dst.Rel) {
		return nil, fbad("pick a different name for the copy")
	}
	if !src.isDir() {
		if why := newFileWhy(dst.Rel, src.Name); why != "" {
			return nil, fbad("%s", why)
		}
	}
	if why := s.liveBlock(dst.Rel, src.isDir(), false, false); why != "" {
		return nil, fblocked("%s", why)
	}
	if why := s.managedFile(dst.Rel); why != "" {
		return nil, fblocked("%s", why)
	}
	if pt, err := s.target(parentRel(dst.Rel)); err != nil {
		return nil, err
	} else if !pt.isDir() {
		return nil, fbad("the folder to copy it to isn't there")
	}
	if dst.exists() {
		if !overwrite {
			return nil, fconflict("%q is already there", dst.Name)
		}
		if dst.isDir() || src.isDir() {
			return nil, fbad("a folder is in the way, so nothing was replaced")
		}
		if why := s.liveBlock(dst.Rel, false, true, false); why != "" {
			return nil, fblocked("%s", why)
		}
	}
	res := &CopyResult{}
	if src.isFile() {
		if err := s.roomFor(dst.Abs, src.Info.Size()); err != nil {
			return nil, err
		}
		if dst.exists() {
			if _, err := s.toTrash(dst); err != nil {
				return nil, err
			}
		}
		if err := copyFileTo(src.Abs, dst.Abs, src.Info.Mode().Perm()); err != nil {
			return nil, err
		}
		res.Files = 1
		return res, nil
	}
	stage, err := os.MkdirTemp(filepath.Dir(dst.Abs), internalPrefix+"copy-*")
	if err != nil {
		return nil, err
	}
	defer safeRemoveAll(stage)
	var bytesCopied int64
	err = filepath.WalkDir(src.Abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relp, _ := filepath.Rel(src.Abs, p)
		if relp == "." {
			return nil
		}
		if internalName(d.Name()) || checkComponent(d.Name()) != nil {
			res.Skipped++
			return filepath.SkipDir
		}
		if isLinkMode(d.Type()) || (!d.IsDir() && !d.Type().IsRegular()) {
			res.Skipped++
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		out := filepath.Join(stage, relp)
		if d.IsDir() {
			return os.Mkdir(out, 0o755)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if res.Files++; res.Files > maxCopyFiles {
			return ftoobig("that folder has more than %d files, too many to copy here", maxCopyFiles)
		}
		if bytesCopied += fi.Size(); bytesCopied > maxCopyBytes {
			return ftoobig("that folder is bigger than %s, too big to copy here", humanSize(maxCopyBytes))
		}
		if res.Files%200 == 0 {
			if err := s.roomFor(stage, bytesCopied/int64(max(1, res.Files/200))); err != nil {
				return err
			}
		}
		return copyFileTo(p, out, fi.Mode().Perm())
	})
	if err != nil {
		return nil, err
	}
	if dst.exists() {
		if _, err := s.toTrash(dst); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(stage, dst.Abs); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Server) roomFor(near string, need int64) error {
	if free, err := diskFree(filepath.Dir(near)); err == nil && free < need+diskHeadroom {
		return ftoobig("there isn't enough room on the disk (%s free, %s needed)", humanSize(free), humanSize(need))
	}
	return nil
}

// copyFileTo copies one plain file; the source is opened and checked to be one first.
func copyFileTo(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
		return fbad("%q isn't a plain file", filepath.Base(src))
	}
	return atomicWrite(dst, perm, func(w io.Writer) error { _, err := io.Copy(w, in); return err })
}

// safeRemoveAll deletes a folder's contents for good without ever following a link: a link inside
// is removed itself, never what it points to.
func safeRemoveAll(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.IsDir() && !isLinkMode(fi.Mode()) {
		ents, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if err := safeRemoveAll(filepath.Join(p, e.Name())); err != nil {
				return err
			}
		}
	}
	err = os.Remove(p)
	if err != nil && runtime.GOOS == "windows" {
		os.Chmod(p, 0o666) // read-only files can't be removed on Windows
		err = os.Remove(p)
	}
	return err
}

// FileOpen opens a plain file for sending. The caller closes it.
func (s *Server) FileOpen(rel string) (*os.File, fs.FileInfo, string, error) {
	t, err := s.target(rel)
	if err != nil {
		return nil, nil, "", err
	}
	if !t.exists() {
		return nil, nil, "", errNotThere
	}
	if t.isLink() {
		return nil, nil, "", fblocked("that is a link, and MineNest doesn't send what links point to")
	}
	if !t.isFile() {
		return nil, nil, "", fbad("that's a folder: download it as a zip")
	}
	f, err := os.Open(t.Abs)
	if err != nil {
		return nil, nil, "", err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, "", fbad("that isn't a plain file")
	}
	return f, fi, t.Name, nil
}

// ------------------------------------------------------------------ upload

type UploadResult struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Restart bool   `json:"restart"` // a plugin or mod: the server needs a restart to load it
	Running bool   `json:"running"`
}

// FileUpload stores an uploaded file in dir. size is what the sender said (-1 if unknown).
func (s *Server) FileUpload(dir, name string, size int64, overwrite bool, body io.Reader) (*UploadResult, error) {
	if err := checkComponent(name); err != nil {
		return nil, err
	}
	d, err := s.target(dir)
	if err != nil {
		return nil, err
	}
	if !d.isDir() {
		return nil, fbad("the folder to upload to isn't there")
	}
	t, err := s.target(joinRel(d.Rel, name))
	if err != nil {
		return nil, err
	}
	if why := newFileWhy(t.Rel, ""); why != "" {
		return nil, fbad("%s", why)
	}
	if t.isLink() || (t.exists() && !t.isFile()) {
		return nil, fbad("%q is in the way and isn't a plain file", name)
	}
	if t.exists() && !overwrite {
		return nil, fconflict("%q is already there", name)
	}
	if why := s.liveBlock(t.Rel, false, t.exists(), false); why != "" {
		return nil, fblocked("%s", why)
	}
	if why := s.managedFile(t.Rel); why != "" {
		return nil, fblocked("%s", why)
	}
	if size > maxUploadBytes {
		return nil, ftoobig("that file is %s, more than the %s MineNest takes at once", humanSize(size), humanSize(maxUploadBytes))
	}
	// An upload that doesn't say its length (chunked) is made room for as if it were the most
	// MineNest takes, and is held to that.
	room := size
	if room < 0 {
		room = maxUploadBytes
	}
	if room > 0 {
		if err := s.roomFor(t.Abs, room); err != nil {
			return nil, err
		}
	}
	var written int64
	perm := fs.FileMode(0o644)
	if t.exists() {
		perm = t.Info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(d.Abs, internalPrefix+"up-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	fail := func(err error) (*UploadResult, error) {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	written, err = io.Copy(tmp, io.LimitReader(body, maxUploadBytes+1))
	if err != nil {
		return fail(fbad("the upload was cut off: %v", err))
	}
	if written > maxUploadBytes {
		return fail(ftoobig("that file is more than the %s MineNest takes at once", humanSize(maxUploadBytes)))
	}
	if size >= 0 && written != size {
		return fail(fbad("the upload was cut off (%s of %s arrived)", humanSize(written), humanSize(size)))
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	os.Chmod(tmpName, perm)
	// The folder is looked at again: it must still be what it was when the upload began.
	if t2, err := s.target(t.Rel); err != nil || t2.isLink() || (t2.exists() && !t2.isFile()) {
		os.Remove(tmpName)
		if err == nil {
			err = fbad("%q changed while it was uploading", name)
		}
		return nil, err
	}
	if t.exists() {
		if _, err := s.toTrash(t); err != nil {
			os.Remove(tmpName)
			return nil, err
		}
	}
	if err := os.Rename(tmpName, t.Abs); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("the file couldn't be put in place: %v", err)
	}
	return &UploadResult{Path: t.Rel, Size: written, Restart: needsRestart(t.Rel), Running: s.State() != Stopped}, nil
}

// ------------------------------------------------------------------ leftovers

// tempMaxAge is how old one of MineNest's temporary names has to be before it is a leftover (a
// crash in the middle of an upload or an unpack), not something still being written.
var tempMaxAge = time.Hour

func leftover(name string, mod time.Time) bool {
	if !internalName(name) || time.Since(mod) < tempMaxAge {
		return false
	}
	for _, p := range []string{"up-", "unzip-", "copy-"} {
		if strings.HasPrefix(name, internalPrefix+p) {
			return true
		}
	}
	return false
}

// sweepDir removes the leftovers directly in one folder.
func sweepDir(dir string, ents []os.DirEntry) {
	for _, e := range ents {
		if !internalName(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil && leftover(e.Name(), fi.ModTime()) {
			safeRemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// sweepTemp looks through a server's folders for leftovers. It runs when MineNest starts.
func (s *Server) sweepTemp() {
	if s.State() != Stopped {
		return
	}
	n := 0
	filepath.WalkDir(s.Root(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if n++; n > 20000 {
			return fs.SkipAll
		}
		if internalName(d.Name()) {
			if fi, err := d.Info(); err == nil && leftover(d.Name(), fi.ModTime()) {
				safeRemoveAll(p)
			}
			return fs.SkipDir
		}
		if ents, err := os.ReadDir(p); err == nil {
			sweepDir(p, ents)
		}
		return nil
	})
}
