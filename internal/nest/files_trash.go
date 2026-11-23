package nest

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The trash: what the file manager "deletes" is moved to servers/<name>/trash/<id>/item, with a
// note of where it came from, so it can come back. It sits beside the server's folder, in
// MineNest's own data, so a server never sees it and the file manager can't reach into it.
// Only emptying the trash deletes for good.

func (s *Server) trashDir() string { return filepath.Join(s.Dir, "trash") }

type trashMeta struct {
	Orig    string `json:"orig"` // the path it had, relative to the server's folder
	Name    string `json:"name"`
	Deleted int64  `json:"deleted"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	Count   int    `json:"count"` // how many files, for a folder
}

type TrashItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Orig    string `json:"orig"`
	Deleted int64  `json:"deleted"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	Count   int    `json:"count"`
}

type TrashInfo struct {
	Items []TrashItem `json:"items"`
	Size  int64       `json:"size"`
}

func newTrashID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

// sizeOf adds up a folder (without following links), looking at a bounded number of files.
func sizeOf(p string, fi fs.FileInfo) (size int64, count int) {
	if !fi.IsDir() || isLinkMode(fi.Mode()) {
		return fi.Size(), 1
	}
	n := 0
	filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err != nil || isLinkMode(d.Type()) || d.IsDir() {
			return nil
		}
		if n++; n > 50000 {
			return fs.SkipAll
		}
		if i, err := d.Info(); err == nil {
			size += i.Size()
		}
		return nil
	})
	return size, n
}

// toTrash moves a checked target into the trash and returns its id. It never deletes: if the
// move can't be made, nothing happens.
func (s *Server) toTrash(t *fsTarget) (string, error) {
	if !t.exists() {
		return "", errNotThere
	}
	id := newTrashID()
	dir := filepath.Join(s.trashDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	size, count := sizeOf(t.Abs, t.Info)
	meta := trashMeta{Orig: t.Rel, Name: t.Name, Deleted: time.Now().Unix(), Dir: t.Info.IsDir() && !t.isLink(), Size: size, Count: count}
	if err := writeJSON(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
		os.Remove(dir)
		return "", err
	}
	if err := os.Rename(t.Abs, filepath.Join(dir, "item")); err != nil {
		safeRemoveAll(dir)
		return "", fmt.Errorf("%q couldn't be moved to the trash (is something using it?)", t.Name)
	}
	s.pruneTrash(id)
	return id, nil
}

// The trash is not for ever: what is older than trashMaxAge goes, and when it holds more than
// trashMaxBytes the oldest go first. The item just put in is always kept.
var (
	trashMaxAge   = 30 * 24 * time.Hour
	trashMaxBytes = int64(5) << 30
)

func (s *Server) pruneTrash(keep string) {
	tr, err := s.Trash()
	if err != nil {
		return
	}
	total := tr.Size
	now := time.Now()
	for i := len(tr.Items) - 1; i >= 0; i-- { // oldest first
		it := tr.Items[i]
		if it.ID == keep {
			continue
		}
		old := now.Sub(time.Unix(it.Deleted, 0)) > trashMaxAge
		if !old && total <= trashMaxBytes {
			continue
		}
		if safeRemoveAll(filepath.Join(s.trashDir(), it.ID)) == nil {
			total -= it.Size
		}
	}
}

// DeleteResult says what a delete did: what went to the trash, and what could not.
type DeleteResult struct {
	Trashed []string     `json:"-"`
	Failed  []DeleteFail `json:"failed"`
}

type DeleteFail struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

// FileDelete moves each path to the trash. It checks all of them first, so a refusal leaves
// everything as it was. Then it moves what it can: one that is in use is reported, and the rest
// still go.
func (s *Server) FileDelete(rels []string) (*DeleteResult, error) {
	if len(rels) == 0 {
		return nil, fbad("say what to delete")
	}
	var ts []*fsTarget
	for _, r := range rels {
		t, err := s.target(r)
		if err != nil {
			return nil, err
		}
		if t.Rel == "" {
			return nil, fbad("the server's own folder can't be deleted here (delete the server instead)")
		}
		if !t.exists() {
			return nil, errNotThere
		}
		if why := s.liveBlock(t.Rel, t.isDir(), true, true); why != "" {
			return nil, fblocked("%q: %s", t.Name, why)
		}
		if why := s.managedFile(t.Rel); why != "" {
			return nil, fblocked("%s", why)
		}
		ts = append(ts, t)
	}
	res := &DeleteResult{Failed: []DeleteFail{}}
	for _, t := range ts {
		id, err := s.toTrash(t)
		if err != nil {
			res.Failed = append(res.Failed, DeleteFail{Path: t.Rel, Why: "something is using it, so it stayed where it was"})
			continue
		}
		res.Trashed = append(res.Trashed, id)
	}
	if len(res.Trashed) == 0 {
		return nil, fblocked("%q couldn't be moved to the trash (is something using it?)", ts[0].Name)
	}
	return res, nil
}

func (s *Server) Trash() (*TrashInfo, error) {
	out := &TrashInfo{Items: []TrashItem{}}
	ents, err := os.ReadDir(s.trashDir())
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !trashIDRe.MatchString(e.Name()) {
			continue
		}
		var m trashMeta
		if readJSON(filepath.Join(s.trashDir(), e.Name(), "meta.json"), &m) != nil || m.Name == "" {
			continue
		}
		out.Items = append(out.Items, TrashItem{ID: e.Name(), Name: m.Name, Orig: m.Orig, Deleted: m.Deleted, Dir: m.Dir, Size: m.Size, Count: m.Count})
		out.Size += m.Size
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID > out.Items[j].ID })
	return out, nil
}

func (s *Server) trashItem(id string) (string, *trashMeta, error) {
	if !trashIDRe.MatchString(id) {
		return "", nil, fbad("that isn't something in the trash")
	}
	dir := filepath.Join(s.trashDir(), id)
	var m trashMeta
	if err := readJSON(filepath.Join(dir, "meta.json"), &m); err != nil || m.Name == "" {
		return "", nil, apiError{404, "that isn't in the trash any more"}
	}
	return dir, &m, nil
}

// ensureDirs makes the folders a path needs, one checked step at a time.
func (s *Server) ensureDirs(rel string) error {
	if rel == "" {
		return nil
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		sub := strings.Join(parts[:i+1], "/")
		t, err := s.target(sub)
		if err != nil {
			return err
		}
		if t.exists() {
			if !t.isDir() {
				return fbad("%q isn't a folder", t.Name)
			}
			continue
		}
		if why := s.liveBlock(t.Rel, true, false, false); why != "" {
			return fblocked("%s", why)
		}
		if err := os.Mkdir(t.Abs, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// TrashRestore puts an item back where it was (or at "to"), making the folders it needs.
func (s *Server) TrashRestore(id, to string, overwrite bool) (string, error) {
	dir, m, err := s.trashItem(id)
	if err != nil {
		return "", err
	}
	if to == "" {
		to = m.Orig
	}
	dst, err := s.target(to)
	if err != nil {
		return "", err
	}
	if dst.Rel == "" {
		return "", fbad("say where to put it")
	}
	if !m.Dir {
		if why := newFileWhy(dst.Rel, m.Name); why != "" && lowerRel(dst.Rel) != "server.properties" {
			return "", fbad("%s", why)
		}
	}
	if why := s.liveBlock(dst.Rel, m.Dir, false, false); why != "" {
		return "", fblocked("%s", why)
	}
	if why := s.managedFile(dst.Rel); why != "" {
		return "", fblocked("%s", why)
	}
	item := filepath.Join(dir, "item")
	if _, err := os.Lstat(item); err != nil {
		return "", apiError{404, "that isn't in the trash any more"}
	}
	if err := s.ensureDirs(parentRel(dst.Rel)); err != nil {
		return "", err
	}
	if dst, err = s.target(dst.Rel); err != nil {
		return "", err
	}
	if dst.exists() {
		if !overwrite {
			return "", fconflict("%q is already there", dst.Name)
		}
		if dst.isDir() || m.Dir {
			return "", fbad("a folder is in the way, so nothing was replaced")
		}
		if why := s.liveBlock(dst.Rel, false, true, false); why != "" {
			return "", fblocked("%s", why)
		}
		if _, err := s.toTrash(dst); err != nil {
			return "", err
		}
	}
	if err := os.Rename(item, dst.Abs); err != nil {
		return "", fmt.Errorf("it couldn't be put back (is something using that place?)")
	}
	safeRemoveAll(dir)
	return dst.Rel, nil
}

// TrashEmpty deletes for good: one item, or all of them when id is "".
func (s *Server) TrashEmpty(id string) (int, error) {
	if id != "" {
		dir, _, err := s.trashItem(id)
		if err != nil {
			return 0, err
		}
		return 1, safeRemoveAll(dir)
	}
	ents, err := os.ReadDir(s.trashDir())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		if !trashIDRe.MatchString(e.Name()) {
			continue
		}
		if err := safeRemoveAll(filepath.Join(s.trashDir(), e.Name())); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ------------------------------------------------------------------ zip: download a folder

type limitWriter struct {
	w io.Writer
	n int64
}

var errTooMuch = ftoobig("that is too much to send through the website. Download it on the home network, or on the computer MineNest runs on")

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n -= int64(len(p)); l.n < 0 {
		return 0, errTooMuch
	}
	return l.w.Write(p)
}

// FileZip writes a folder (or the whole server folder) as a zip. Links and special files are
// left out, never followed. limit > 0 stops it after that many bytes.
func (s *Server) FileZip(rel string, w io.Writer, limit int64) (string, error) {
	t, err := s.target(rel)
	if err != nil {
		return "", err
	}
	if !t.exists() {
		return "", errNotThere
	}
	if !t.isDir() {
		return "", fbad("that is a file: download it as it is")
	}
	base := t.Name
	if base == "" {
		base = s.ID
	}
	if limit > 0 {
		w = &limitWriter{w, limit}
	}
	return base + ".zip", s.zipTo(t, base, w)
}

func (s *Server) zipTo(t *fsTarget, base string, w io.Writer) error {
	zw := zip.NewWriter(w)
	n := 0
	err := filepath.WalkDir(t.Abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // something vanished or can't be read: leave it out
		}
		relp, _ := filepath.Rel(t.Abs, p)
		if relp == "." {
			return nil
		}
		if internalName(d.Name()) || checkComponent(d.Name()) != nil || isLinkMode(d.Type()) || (!d.IsDir() && !d.Type().IsRegular()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if n++; n > 200000 {
			return fs.SkipAll
		}
		name := base + "/" + filepath.ToSlash(relp)
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		hdr := &zip.FileHeader{Name: name, Modified: fi.ModTime(), Method: zip.Deflate}
		if d.IsDir() {
			hdr.Name += "/"
			hdr.Method = zip.Store
			_, err := zw.CreateHeader(hdr)
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
			return nil
		}
		hdr.SetMode(0o644)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, f)
		return err
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

// ------------------------------------------------------------------ zip: unpack in place

type UnzipResult struct {
	Files   int      `json:"files"`
	Folders int      `json:"folders"`
	Bytes   int64    `json:"bytes"`
	Skipped []string `json:"skipped,omitempty"` // programs and scripts, left out
	Restart bool     `json:"restart"`
}

type unzipEntry struct {
	f   *zip.File
	rel string // where it goes, relative to the server's folder
	dir bool
}

// FileUnzip unpacks a .zip in the folder it is in (or into the folder "into" made there). The
// whole archive is checked first: an entry that would land outside, a link, an odd name, too many
// files or too much data stops it before anything is written. Files are unpacked into a hidden
// folder and only then moved into place.
func (s *Server) FileUnzip(rel, into string, overwrite bool) (*UnzipResult, error) {
	zt, err := s.target(rel)
	if err != nil {
		return nil, err
	}
	if !zt.exists() {
		return nil, errNotThere
	}
	if !zt.isFile() || extOf(zt.Name) != "zip" {
		return nil, fbad("only .zip files can be unpacked here")
	}
	destRel := parentRel(zt.Rel)
	if into != "" {
		if err := checkComponent(into); err != nil {
			return nil, err
		}
		destRel = joinRel(destRel, into)
	}
	if why := s.liveBlock(destRel, true, false, false); why != "" {
		return nil, fblocked("%s", why)
	}
	zr, err := zip.OpenReader(zt.Abs)
	if err != nil {
		return nil, fbad("that zip can't be read: %v", err)
	}
	defer zr.Close()
	if len(zr.File) > unzipMaxFiles {
		return nil, ftoobig("that zip holds %d files, more than the %d MineNest unpacks", len(zr.File), unzipMaxFiles)
	}
	res := &UnzipResult{}
	var entries []unzipEntry
	seen := map[string]bool{}
	var total int64
	var conflicts []string
	for _, f := range zr.File {
		name := f.Name
		if strings.Contains(name, `\`) {
			return nil, fbad("that zip has a backslash in the name %q, which could reach outside the folder, so it isn't unpacked", cutName(name))
		}
		isDir := strings.HasSuffix(name, "/")
		clean, err := cleanRel(name)
		if err != nil || clean == "" {
			return nil, fbad("that zip has a name that isn't allowed (%q), so it isn't unpacked", cutName(name))
		}
		m := f.Mode()
		if m&fs.ModeSymlink != 0 || (f.ExternalAttrs>>16)&0xF000 == 0xA000 || m&(fs.ModeDevice|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeIrregular|fs.ModeCharDevice) != 0 {
			return nil, fbad("that zip has a link or special file in it (%q), so it isn't unpacked", cutName(name))
		}
		if m.IsDir() {
			isDir = true
		}
		key := lowerRel(clean)
		if seen[key] {
			return nil, fbad("that zip has %q twice (or twice with different capitals), so it isn't unpacked", cutName(name))
		}
		seen[key] = true
		final := joinRel(destRel, clean)
		if !isDir && newFileWhy(final, "") != "" {
			res.Skipped = append(res.Skipped, clean)
			continue
		}
		if !isDir {
			if int64(f.UncompressedSize64) > unzipMaxFile {
				return nil, ftoobig("%q is %s inside the zip, more than the %s one file may be", cutName(name), humanSize(int64(f.UncompressedSize64)), humanSize(unzipMaxFile))
			}
			if total += int64(f.UncompressedSize64); total > unzipMaxTotal || total < 0 {
				return nil, ftoobig("that zip unpacks to more than %s, more than MineNest unpacks", humanSize(unzipMaxTotal))
			}
		}
		if len(strings.Split(final, "/")) > maxPathDepth {
			return nil, fbad("that zip goes too many folders deep")
		}
		if why := s.liveBlock(final, isDir, false, false); why != "" {
			return nil, fblocked("%s", why)
		}
		entries = append(entries, unzipEntry{f, final, isDir})
	}
	// A name can't be a file and a folder at once (a link entry beside a folder of the same name).
	isFile := map[string]bool{}
	for _, e := range entries {
		if !e.dir {
			isFile[lowerRel(e.rel)] = true
		}
	}
	for _, e := range entries {
		for p := parentRel(e.rel); p != ""; p = parentRel(p) {
			if isFile[lowerRel(p)] {
				return nil, fbad("that zip has %q as both a file and a folder, so it isn't unpacked", cutName(p))
			}
		}
	}
	// Where each one would land: no links on the way, nothing in the way of the wrong kind.
	for _, e := range entries {
		t, err := s.target(e.rel)
		if err != nil {
			return nil, err
		}
		if t.exists() {
			switch {
			case t.isLink():
				return nil, fbad("%q is a link in the way, so nothing was unpacked", t.Name)
			case e.dir && !t.isDir(), !e.dir && !t.isFile():
				return nil, fbad("%q is in the way and isn't the same kind of thing, so nothing was unpacked", t.Name)
			case !e.dir:
				conflicts = append(conflicts, e.rel)
			}
		}
	}
	if len(conflicts) > 0 && !overwrite {
		sort.Strings(conflicts)
		all, more := len(conflicts), ""
		if all > 5 {
			more = fmt.Sprintf(" and %d more", all-5)
			conflicts = conflicts[:5]
		}
		return nil, fconflict("%d files are already there (%s%s). Unpacking would replace them", all, strings.Join(conflicts, ", "), more)
	}
	dt, err := s.target(destRel)
	if err != nil {
		return nil, err
	}
	if !dt.exists() {
		if err := s.ensureDirs(destRel); err != nil {
			return nil, err
		}
		if dt, err = s.target(destRel); err != nil {
			return nil, err
		}
	}
	if !dt.isDir() {
		return nil, fbad("the folder to unpack into isn't a folder")
	}
	if err := s.roomFor(dt.Abs, total); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(dt.Abs, internalPrefix+"unzip-*")
	if err != nil {
		return nil, err
	}
	defer safeRemoveAll(stage)
	var written int64
	stageOf := func(rel string) string {
		return filepath.Join(stage, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(rel, destRel), "/")))
	}
	for _, e := range entries {
		out := stageOf(e.rel)
		if e.dir {
			if err := os.MkdirAll(out, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, err
		}
		rc, err := e.f.Open()
		if err != nil {
			return nil, fbad("%q can't be read from the zip: %v", cutName(e.f.Name), err)
		}
		// What really comes out is counted, whatever the archive said it would be.
		n, err := copyLimited(out, rc, min(unzipMaxFile, unzipMaxTotal-written))
		rc.Close()
		if err != nil {
			return nil, err
		}
		written += n
		os.Chtimes(out, e.f.Modified, e.f.Modified)
	}
	// Into place.
	for _, e := range entries {
		t, err := s.target(e.rel)
		if err != nil {
			return nil, err
		}
		if e.dir {
			if !t.exists() {
				if err := os.Mkdir(t.Abs, 0o755); err != nil {
					return nil, err
				}
			}
			res.Folders++
			continue
		}
		if t.isLink() || (t.exists() && !t.isFile()) {
			return nil, fbad("%q is in the way, so the rest wasn't unpacked", t.Name)
		}
		if t.exists() {
			if _, err := s.toTrash(t); err != nil {
				return nil, err
			}
		}
		if err := os.MkdirAll(filepath.Dir(t.Abs), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(stageOf(e.rel), t.Abs); err != nil {
			return nil, fmt.Errorf("%q couldn't be put in place: %v", t.Name, err)
		}
		res.Files++
		if needsRestart(e.rel) {
			res.Restart = true
		}
	}
	res.Bytes = written
	return res, nil
}

func cutName(n string) string {
	if len(n) > 80 {
		return n[:80] + "..."
	}
	return n
}

// copyLimited writes r to a new file at path, failing (and removing it) past limit bytes.
func copyLimited(path string, r io.Reader, limit int64) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = ftoobig("a file in that zip is bigger than it said, or the zip unpacks to more than MineNest allows")
	}
	if err != nil {
		os.Remove(path)
		return 0, err
	}
	return n, nil
}

// trashSize is how much the trash holds.
func (s *Server) trashSize() int64 {
	tr, err := s.Trash()
	if err != nil {
		return 0
	}
	return tr.Size
}
