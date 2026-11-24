package nest

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// ------------------------------------------------------------------ helpers

// newFilesServer makes a MineNest home with one stopped server called "srv" (Paper, server.jar).
func newFilesServer(t *testing.T) (*Manager, *Server) {
	t.Helper()
	t.Setenv("MINENEST_HOME", t.TempDir())
	dir := homePath("servers", "srv")
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Name: "Srv", Flavor: "paper", Want: "latest", RAM: 2048,
		Installed: &Installed{Flavor: "paper", Minecraft: "1.21.4", Java: 21, Launch: []string{"-jar", "server.jar"}}}
	if err := writeJSON(filepath.Join(dir, "nest.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Get("srv")
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}

func put(t *testing.T, s *Server, rel, content string) string {
	t.Helper()
	p := filepath.Join(s.Root(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readBack(t *testing.T, s *Server, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Root(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gone(s *Server, rel string) bool {
	_, err := os.Lstat(filepath.Join(s.Root(), filepath.FromSlash(rel)))
	return errors.Is(err, fs.ErrNotExist)
}

func setRunning(s *Server) {
	s.mu.Lock()
	s.state = Running
	s.mu.Unlock()
}

// wantCode checks an error is an apiError with that code.
func wantCode(t *testing.T, err error, code int, what string) {
	t.Helper()
	var ae apiError
	if err == nil || !errors.As(err, &ae) || ae.Code != code {
		t.Errorf("%s: wanted a %d, got %v", what, code, err)
	}
}

func wantRefused(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: allowed", what)
	}
}

// makeDirLink makes a link at link to the folder target: a symbolic link, or on Windows (where
// that needs a privilege) a junction. It skips the test when neither can be made.
func makeDirLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		t.Cleanup(func() { os.Remove(link) })
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err == nil {
			t.Cleanup(func() { os.Remove(link) }) // removes the junction, never what it points to
			return
		} else {
			t.Skipf("no way to make a link here: %v %s", err, out)
		}
	}
	t.Skip("links can't be made here")
}

// ------------------------------------------------------------------ names and paths

func TestHostilePaths(t *testing.T) {
	// Refused on every system: MineNest keeps one rule, so a server moved between them stays safe.
	common := []string{
		"..", "../x", "a/../../x", "a/..", "a/../b", "./a", "a/./b", ".", "a//b", "//a",
		"/etc/passwd", "/", "/a", "a\x00b", "\x00", "a/\x00",
		`..\..\x`, `a\b`, `\a`, "a\nb", "a\tb", "a/\u202eb", "a\u200fb",
		strings.Repeat("a", 256), strings.Repeat("a/", 41) + "a",
		".minenest-tmp-1", "plugins/.minenest-up-x",
	}
	// Windows ways in. They are refused on Linux as well, which is why they can sit in the same list.
	windows := []string{
		`C:\Windows\win.ini`, `C:/Windows`, "C:", "c:x", "a/C:/b", `\\server\share\x`, "//server/share/x",
		`\\?\C:\x`, `\\.\pipe\x`, `\\.\PhysicalDrive0`, "name:stream", "a.txt:hidden", "file.txt::$DATA", "a/b:c/d",
		"con", "CON", "con.txt", "a/NUL", "nul.log", "PRN", "aux.c", "COM1", "com9.txt", "LPT1", "lpt9.x", "COM\u00b9", "CONIN$", "CONOUT$",
		"a/aux .txt", "trail.", "trail ", "a./b", "a /b", "dir/trail.", "a/b. ",
		"x/..\\..", "a/b?c", "a*b", `a"b`, "a<b", "a>b", "a|b",
	}
	for _, p := range append(common, windows...) {
		if c, err := cleanRel(p); err == nil {
			t.Errorf("%q was accepted as %q", p, c)
		}
	}
	for p, want := range map[string]string{"": "", "a": "a", "a/": "a", "plugins/Essentials/config.yml": "plugins/Essentials/config.yml",
		"world_nether/region": "world_nether/region", "a b/c-d_e.f": "a b/c-d_e.f", "config/paper-global.yml": "config/paper-global.yml",
		"åäö/日本語.txt": "åäö/日本語.txt", "console.txt": "console.txt", "com10": "com10", "nullable": "nullable", ".hidden": ".hidden"} {
		if c, err := cleanRel(p); err != nil || c != want {
			t.Errorf("%q gave %q, %v", p, c, err)
		}
	}
}

func TestNothingOutsideTheServersFolder(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "server.properties", "motd=hi\n")
	for _, f := range []string{"nest.json", "door.json", "autobackup.json", "addons.json", "saving-off"} {
		os.WriteFile(filepath.Join(s.Dir, f), []byte("secret"), 0o600)
	}
	os.MkdirAll(filepath.Join(s.Dir, "backups"), 0o755)
	os.WriteFile(homePath("api.token"), []byte("tokentokentokentokentokentokentoken"), 0o600)
	l, err := s.FileList("")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 1 || l.Entries[0].Name != "server.properties" {
		t.Errorf("the server's folder shows %+v", l.Entries)
	}
	for _, p := range []string{"../nest.json", "../door.json", "../backups", "../trash", "../../api.token", "../../minenest.json", "../server/../nest.json"} {
		if _, err := s.FileRead(p); err == nil {
			t.Errorf("%s was readable", p)
		}
		if _, err := s.FileWrite(WriteRequest{Path: p, Text: "x"}); err == nil {
			t.Errorf("%s was writable", p)
		}
		if _, err := s.FileDelete([]string{p}); err == nil {
			t.Errorf("%s was deletable", p)
		}
		if err := s.FileMove("server.properties", p, true); err == nil {
			t.Errorf("a file could be moved to %s", p)
		}
		if _, err := s.FileCopy("server.properties", p, true); err == nil {
			t.Errorf("a file could be copied to %s", p)
		}
		if _, err := s.FileUpload("", p, 1, true, strings.NewReader("x")); err == nil {
			t.Errorf("%s could be uploaded", p)
		}
	}
	if readBack2(t, filepath.Join(s.Dir, "nest.json")) != "secret" {
		t.Error("nest.json changed")
	}
	if readBack2(t, filepath.Join(s.Dir, "door.json")) != "secret" {
		t.Error("door.json changed")
	}
}

func readBack2(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLinksAreNeverFollowed(t *testing.T) {
	_, s := newFilesServer(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o644)
	os.MkdirAll(filepath.Join(outside, "sub"), 0o755)
	os.WriteFile(filepath.Join(outside, "sub", "deep.txt"), []byte("deep"), 0o644)
	put(t, s, "plugins/a.yml", "a: 1\n")
	makeDirLink(t, filepath.Join(s.Root(), "escape"), outside)
	makeDirLink(t, filepath.Join(s.Root(), "plugins", "loop"), filepath.Join(s.Root(), "plugins")) // inside, still not followed

	// Reading, writing, listing, zipping, copying, moving and deleting through the link: all refused.
	if _, err := s.FileList("escape"); err == nil {
		t.Error("listed through a link")
	}
	if _, err := s.FileList("escape/sub"); err == nil {
		t.Error("listed under a link")
	}
	if _, err := s.FileRead("escape/secret.txt"); err == nil {
		t.Error("read through a link")
	}
	if _, err := s.FileWrite(WriteRequest{Path: "escape/secret.txt", Text: "pwned"}); err == nil {
		t.Error("wrote through a link")
	}
	if _, err := s.FileWrite(WriteRequest{Path: "escape/new.txt", Text: "pwned", Create: true}); err == nil {
		t.Error("created a file through a link")
	}
	if _, err := s.FileUpload("escape", "new.txt", 1, false, strings.NewReader("x")); err == nil {
		t.Error("uploaded through a link")
	}
	if err := s.FileMkdir("escape/d"); err == nil {
		t.Error("made a folder through a link")
	}
	if _, err := s.FileDelete([]string{"escape/secret.txt"}); err == nil {
		t.Error("deleted through a link")
	}
	if err := s.FileMove("escape/secret.txt", "got.txt", false); err == nil {
		t.Error("moved out through a link")
	}
	if err := s.FileMove("plugins/a.yml", "escape/a.yml", false); err == nil {
		t.Error("moved in through a link")
	}
	if _, err := s.FileCopy("escape/secret.txt", "got.txt", false); err == nil {
		t.Error("copied out through a link")
	}
	if _, err := s.FileCopy("plugins/a.yml", "escape/a.yml", false); err == nil {
		t.Error("copied in through a link")
	}
	if _, _, _, err := s.FileOpen("escape/secret.txt"); err == nil {
		t.Error("downloaded through a link")
	}
	if _, err := s.FileZip("escape", io.Discard, 0); err == nil {
		t.Error("zipped a link")
	}
	if _, err := s.FileRead("plugins/loop/a.yml"); err == nil {
		t.Error("followed a link that points inside")
	}
	if _, err := s.FileCopy("escape", "copy", false); err == nil {
		t.Error("copied a link")
	}
	// A link inside a folder that is zipped or copied is left out, never followed.
	var buf bytes.Buffer
	if _, err := s.FileZip("", &buf, 0); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	for _, f := range zr.File {
		if strings.Contains(f.Name, "secret") || strings.Contains(f.Name, "deep") || strings.Contains(f.Name, "escape/") {
			t.Errorf("the zip has %s from behind a link", f.Name)
		}
	}
	res, err := s.FileCopy("plugins", "plugins2", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped == 0 {
		t.Error("the link in the copied folder wasn't left out")
	}
	if !gone(s, "plugins2/loop") {
		t.Error("the link was copied")
	}
	// The listing shows it as a link, and it can be deleted: the link goes, what it points to stays.
	l, _ := s.FileList("")
	found := false
	for _, e := range l.Entries {
		if e.Name == "escape" {
			found = e.Link && !e.Dir
		}
	}
	if !found {
		t.Error("the link isn't shown as a link")
	}
	if _, err := s.FileDelete([]string{"escape"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatal("deleting the link deleted what it pointed to")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "outside" {
		t.Fatalf("the file outside changed: %q", b)
	}
	// Emptying the trash doesn't follow it either.
	if _, err := s.TrashEmpty(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "sub", "deep.txt")); err != nil {
		t.Fatal("emptying the trash deleted what a link pointed to")
	}
}

func TestFileSymlinkOut(t *testing.T) {
	_, s := newFilesServer(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("outside"), 0o644)
	link := filepath.Join(s.Root(), "plugins")
	os.MkdirAll(link, 0o755)
	if err := os.Symlink(outside, filepath.Join(link, "x.yml")); err != nil {
		t.Skip("links can't be made here: ", err)
	}
	if _, err := s.FileRead("plugins/x.yml"); err == nil {
		t.Error("read a file link")
	}
	if _, err := s.FileWrite(WriteRequest{Path: "plugins/x.yml", Text: "pwned"}); err == nil {
		t.Error("wrote through a file link")
	}
	if _, err := s.FileUpload("plugins", "x.yml", 1, true, strings.NewReader("p")); err == nil {
		t.Error("replaced a file link")
	}
	if b, _ := os.ReadFile(outside); string(b) != "outside" {
		t.Fatalf("the file outside changed: %q", b)
	}
}

func TestShortNamesAndCaseOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("short names and case-blind names are Windows things")
	}
	_, s := newFilesServer(t)
	put(t, s, "world/region/r.0.0.mca", "data")
	setRunning(s)
	// The same folder under other capitals is the same folder: still protected.
	if _, err := s.FileDelete([]string{"WORLD/REGION/R.0.0.MCA"}); err == nil {
		t.Error("deleted a region file by writing it in capitals")
	}
	if err := s.FileMove("World", "Elsewhere", false); err == nil {
		t.Error("moved the world by writing its name in other capitals")
	}
	// An 8.3 short name reaches the same folder; if the disk has them, it is refused as an alias.
	if _, err := os.Lstat(filepath.Join(s.Root(), "WORLD~1")); err == nil {
		if _, err := s.FileList("WORLD~1"); err == nil {
			t.Error("listed a folder by its short name")
		}
	}
}

// ------------------------------------------------------------------ listing, reading, writing

func TestListing(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "b.txt", "bb")
	put(t, s, "A.yml", "a")
	put(t, s, "plugins/x.jar", "jar")
	put(t, s, "logs/latest.log", "log")
	put(t, s, ".minenest-tmp-123", "temp")
	os.MkdirAll(filepath.Join(s.Root(), "empty"), 0o755)
	l, err := s.FileList("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); got != "empty,logs,plugins,A.yml,b.txt" {
		t.Errorf("order %s", got)
	}
	for _, e := range l.Entries {
		switch e.Name {
		case "b.txt":
			if e.Size != 2 || e.Kind != "text" || e.Mod == 0 {
				t.Errorf("%+v", e)
			}
		case "plugins":
			if !e.Dir || e.Items != 1 {
				t.Errorf("%+v", e)
			}
		}
	}
	if _, err := s.FileList("b.txt"); err == nil {
		t.Error("listed a file")
	}
	wantCode(t, func() error { _, err := s.FileList("nope"); return err }(), 404, "a missing folder")
	l, _ = s.FileList("logs")
	if l.Entries[0].Kind != "log" {
		t.Errorf("%+v", l.Entries[0])
	}
	// Names MineNest won't handle aren't listed.
	if runtime.GOOS != "windows" {
		os.WriteFile(filepath.Join(s.Root(), "bad:name"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(s.Root(), "trail."), []byte("x"), 0o644)
		l, _ = s.FileList("")
		if l.Hidden != 2 {
			t.Errorf("hidden %d", l.Hidden)
		}
	}
}

func TestReadAndWriteKeepEncodingAndLineEndings(t *testing.T) {
	_, s := newFilesServer(t)
	cases := []struct {
		name  string
		bytes []byte
		enc   string
		eol   string
	}{
		{"unix.yml", []byte("a: 1\nb: 2\n"), "utf-8", "lf"},
		{"windows.properties", []byte("a=1\r\nb=2\r\n"), "utf-8", "crlf"},
		{"bom.txt", append([]byte{0xef, 0xbb, 0xbf}, "ünï\r\ncode\r\n"...), "utf-8-bom", "crlf"},
		{"latin.properties", []byte("motd=caf\xe9\n"), "latin1", "lf"},
		{"u16.txt", []byte{0xff, 0xfe, 'h', 0, 'i', 0, '\r', 0, '\n', 0, 0x3d, 0xd8, 0x00, 0xde}, "utf-16le", "crlf"},
		{"none.json", []byte(`{"a": 1}`), "utf-8", "lf"},
		{"old.txt", []byte("one\rtwo\r"), "utf-8", "cr"},
	}
	for _, c := range cases {
		put(t, s, c.name, string(c.bytes))
		got, err := s.FileRead(c.name)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Encoding != c.enc || got.EOL != c.eol || strings.ContainsRune(got.Text, '\r') {
			t.Errorf("%s: %s %s %q", c.name, got.Encoding, got.EOL, got.Text)
		}
		// Saving what was read changes nothing at all.
		if _, err := s.FileWrite(WriteRequest{Path: c.name, Text: got.Text, Encoding: got.Encoding, EOL: got.EOL, Version: got.Version}); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if b := readBack(t, s, c.name); b != string(c.bytes) {
			t.Errorf("%s changed: %q != %q", c.name, b, c.bytes)
		}
	}
	// An edit keeps the style of the file.
	got, _ := s.FileRead("windows.properties")
	s.FileWrite(WriteRequest{Path: "windows.properties", Text: got.Text + "c=3\n", Encoding: got.Encoding, EOL: got.EOL, Version: got.Version})
	if b := readBack(t, s, "windows.properties"); b != "a=1\r\nb=2\r\nc=3\r\n" {
		t.Errorf("%q", b)
	}
	// A letter the old encoding can't hold is refused and the file stays as it was.
	got, _ = s.FileRead("latin.properties")
	if _, err := s.FileWrite(WriteRequest{Path: "latin.properties", Text: "motd=✓\n", Encoding: got.Encoding, EOL: "lf"}); err == nil {
		t.Error("wrote a letter latin1 can't hold")
	}
	if b := readBack(t, s, "latin.properties"); b != "motd=caf\xe9\n" {
		t.Errorf("%q", b)
	}
	// Mixed endings are reported.
	put(t, s, "mixed.txt", "a\r\nb\nc\r\n")
	if got, _ := s.FileRead("mixed.txt"); !got.Mixed || got.EOL != "crlf" {
		t.Errorf("%+v", got)
	}
}

func TestBinaryAndBigFilesAreNotOpenedAsText(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "fake.json", "{\x00\x01\x02binary}")
	put(t, s, "noise.txt", strings.Repeat("\x01\x02\x03\x04", 100))
	put(t, s, "plugins/x.jar", "PK")
	put(t, s, "world/level.dat", "\x1f\x8b")
	if _, err := s.FileRead("fake.json"); err == nil {
		t.Error("opened a binary .json")
	}
	if _, err := s.FileRead("noise.txt"); err == nil {
		t.Error("opened noise")
	}
	for _, p := range []string{"plugins/x.jar", "world/level.dat"} {
		if _, err := s.FileRead(p); err == nil {
			t.Errorf("%s opened as text", p)
		}
		if _, err := s.FileWrite(WriteRequest{Path: p, Text: "x"}); err == nil {
			t.Errorf("%s written as text", p)
		}
	}
	defer func(old int64) { maxEditBytes = old }(maxEditBytes)
	maxEditBytes = 100
	put(t, s, "big.txt", strings.Repeat("x", 101))
	_, err := s.FileRead("big.txt")
	wantCode(t, err, 413, "a big text file")
	_, err = s.FileWrite(WriteRequest{Path: "small.txt", Text: strings.Repeat("y", 101), Create: true})
	wantCode(t, err, 413, "saving too much text")
	if !gone(s, "small.txt") {
		t.Error("a refused save made a file")
	}
}

func TestLogsAreReadOnlyTails(t *testing.T) {
	_, s := newFilesServer(t)
	defer func(a int64, b int) { logTailBytes, logTailLines = a, b }(logTailBytes, logTailLines)
	logTailBytes, logTailLines = 200, 5
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		sb.WriteString("[12:00:00 INFO]: line number " + strings.Repeat("x", i%7) + string(rune('A'+i%26)) + "\n")
	}
	put(t, s, "logs/latest.log", sb.String())
	got, err := s.FileRead("logs/latest.log")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ReadOnly || !got.Tail || got.Lines != 5 || strings.Count(got.Text, "\n") != 4 {
		t.Errorf("%+v", got)
	}
	if !strings.HasSuffix(got.Text, string(rune('A'+100%26))) {
		t.Errorf("not the newest lines: %q", got.Text)
	}
	if _, err := s.FileWrite(WriteRequest{Path: "logs/latest.log", Text: "x"}); err == nil {
		t.Error("wrote a log")
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(sb.String()))
	zw.Close()
	put(t, s, "logs/2024-01-01-1.log.gz", gz.String())
	got, err = s.FileRead("logs/2024-01-01-1.log.gz")
	if err != nil || !got.ReadOnly || !got.Tail || got.Lines > 5 || !strings.Contains(got.Text, "line number") {
		t.Errorf("%+v %v", got, err)
	}
	// A short log is shown whole.
	put(t, s, "logs/short.log", "one\ntwo\n")
	if got, _ = s.FileRead("logs/short.log"); got.Tail || got.Lines != 2 {
		t.Errorf("%+v", got)
	}
}

func TestWritesAreAtomic(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "plugins/a.yml", "old: 1\n")
	got, _ := s.FileRead("plugins/a.yml")
	if _, err := s.FileWrite(WriteRequest{Path: "plugins/a.yml", Text: "new: 2\n", Version: got.Version}); err != nil {
		t.Fatal(err)
	}
	if readBack(t, s, "plugins/a.yml") != "new: 2\n" {
		t.Fatal("not saved")
	}
	// Nothing is left behind beside it.
	ents, _ := os.ReadDir(filepath.Join(s.Root(), "plugins"))
	if len(ents) != 1 {
		t.Errorf("left %v", ents)
	}
	// A save made from an older version is refused, unless it is forced.
	_, err := s.FileWrite(WriteRequest{Path: "plugins/a.yml", Text: "late: 3\n", Version: got.Version})
	wantCode(t, err, 409, "a save over a changed file")
	if readBack(t, s, "plugins/a.yml") != "new: 2\n" {
		t.Error("the refused save changed the file")
	}
	if _, err := s.FileWrite(WriteRequest{Path: "plugins/a.yml", Text: "late: 3\n", Version: got.Version, Force: true}); err != nil {
		t.Fatal(err)
	}
	// A new file needs create, and doesn't replace one.
	if _, err := s.FileWrite(WriteRequest{Path: "plugins/b.yml", Text: "b"}); err == nil {
		t.Error("saved a file that isn't there without create")
	}
	if _, err := s.FileWrite(WriteRequest{Path: "plugins/b.yml", Text: "b", Create: true}); err != nil {
		t.Fatal(err)
	}
	_, err = s.FileWrite(WriteRequest{Path: "plugins/b.yml", Text: "b2", Create: true})
	wantCode(t, err, 409, "creating over a file")
	// Only text files, and not into folders.
	if _, err := s.FileWrite(WriteRequest{Path: "plugins/c.exe", Text: "x", Create: true}); err == nil {
		t.Error("wrote an .exe")
	}
	if _, err := s.FileWrite(WriteRequest{Path: "plugins", Text: "x"}); err == nil {
		t.Error("wrote over a folder")
	}
	// If the write itself fails halfway (here: the text can't be encoded), the old file stays.
	put(t, s, "k.properties", "k=1\n")
	s.FileWrite(WriteRequest{Path: "k.properties", Text: "k=\u2603", Encoding: "latin1"})
	if readBack(t, s, "k.properties") != "k=1\n" {
		t.Error("a failed save changed the file")
	}
}

// ------------------------------------------------------------------ server.properties

func TestPropertiesEditsKeepMineNestsKeys(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "server.properties", "#Minecraft server properties\r\nmotd=Hello\r\nserver-port=25570\r\nserver-ip=\r\n")
	got, err := s.FileRead("server.properties")
	if err != nil || got.EOL != "crlf" {
		t.Fatalf("%+v %v", got, err)
	}
	save := func(text string) error {
		_, err := s.FileWrite(WriteRequest{Path: "server.properties", Text: text, EOL: "crlf"})
		return err
	}
	// A normal edit goes through with the comments, the order and the line endings kept.
	if err := save("#Minecraft server properties\nmotd=Changed\nserver-port=25571\nserver-ip=\nwhite-list=true\n"); err != nil {
		t.Fatal(err)
	}
	if b := readBack(t, s, "server.properties"); b != "#Minecraft server properties\r\nmotd=Changed\r\nserver-port=25571\r\nserver-ip=\r\nwhite-list=true\r\n" {
		t.Errorf("%q", b)
	}
	// The port has to be a real one.
	for _, bad := range []string{"server-port=80", "server-port=abc", "server-port=99999", "server-port=", "server-port=25565 25566"} {
		if err := save("motd=x\n" + bad + "\n"); err == nil {
			t.Errorf("%q was saved", bad)
		}
	}
	// Taking the port out puts it back.
	if err := save("motd=No port\n"); err != nil {
		t.Fatal(err)
	}
	if v, ok := s.props().Get("server-port"); !ok || v != "25571" {
		t.Errorf("the port is %q %v", v, ok)
	}
}

// ------------------------------------------------------------------ a running server

func TestWhileTheServerRuns(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "server.jar", "jar")
	put(t, s, "world/region/r.0.0.mca", "r")
	put(t, s, "world/level.dat", "l")
	put(t, s, "world/session.lock", "x")
	put(t, s, "world/playerdata/a.dat", "p")
	put(t, s, "world_nether/DIM-1/region/r.0.0.mca", "r")
	put(t, s, "world_the_end/level.dat", "l")
	put(t, s, "plugins/a.yml", "a: 1\n")
	put(t, s, "ops.json", "[]")
	put(t, s, "zip/stuff.zip", "")
	setRunning(s)

	blocked := []string{"server.jar", "world/region/r.0.0.mca", "world/level.dat", "world/session.lock", "world", "world/region",
		"world_nether", "world_nether/DIM-1", "world_the_end", "SERVER.JAR", "World/Region/r.0.0.mca"}
	for _, p := range blocked {
		if _, err := s.FileDelete([]string{p}); err == nil && p != "SERVER.JAR" && p != "World/Region/r.0.0.mca" {
			t.Errorf("deleted %s", p)
		}
		if err := s.FileMove(p, "somewhere-else", false); err == nil && p != "SERVER.JAR" && p != "World/Region/r.0.0.mca" {
			t.Errorf("moved %s", p)
		}
	}
	for _, p := range []string{"SERVER.JAR", "World/Region/r.0.0.mca"} { // other capitals, same files (a case-blind disk reaches them)
		if runtime.GOOS == "windows" {
			if _, err := s.FileDelete([]string{p}); err == nil {
				t.Errorf("deleted %s", p)
			}
		}
	}
	if err := s.FileMove("plugins/a.yml", "world/region/a.yml", false); err == nil {
		t.Error("moved a file into the region folder")
	}
	if err := s.FileMove("plugins/a.yml", "world/session.lock", true); err == nil {
		t.Error("replaced session.lock")
	}
	if _, err := s.FileUpload("world/region", "r.1.1.mca", 1, false, strings.NewReader("x")); err == nil {
		t.Error("uploaded into the region folder")
	}
	if _, err := s.FileUpload("", "server.jar", 1, true, strings.NewReader("x")); err == nil {
		t.Error("replaced the running jar")
	}
	if _, err := s.FileCopy("plugins/a.yml", "world/region/b.mca", false); err == nil {
		t.Error("copied into the region folder")
	}
	if err := s.FileMkdir("world/region/sub"); err == nil {
		t.Error("made a folder in region")
	}
	if _, err := s.FileDelete([]string{"plugins/a.yml", "world"}); err == nil {
		t.Error("deleted along with the world")
	}
	if !gone(s, "plugins/a.yml") == true && readBack(t, s, "plugins/a.yml") != "a: 1\n" {
		t.Error("a refused delete still took the other file")
	}
	// Why is said.
	_, err := s.FileDelete([]string{"world"})
	if err == nil || !strings.Contains(err.Error(), "Stop the server") {
		t.Errorf("the reason: %v", err)
	}
	// What is safe still works, and reading and zipping everything does too.
	if _, err := s.FileWrite(WriteRequest{Path: "plugins/a.yml", Text: "a: 2\n"}); err != nil {
		t.Errorf("a plugin's config: %v", err)
	}
	if _, err := s.FileUpload("plugins", "b.jar", 3, false, strings.NewReader("jar")); err != nil {
		t.Errorf("a plugin: %v", err)
	}
	if _, err := s.FileDelete([]string{"ops.json"}); err != nil {
		t.Errorf("ops.json: %v", err)
	}
	if _, err := s.FileZip("world", io.Discard, 0); err != nil {
		t.Errorf("zipping the world: %v", err)
	}
	if _, err := s.FileCopy("world", "world-copy", false); err != nil {
		t.Errorf("copying the world: %v", err)
	}
	l, _ := s.FileList("")
	for _, e := range l.Entries {
		if e.Name == "world" && e.Locked == "" {
			t.Error("the list doesn't say the world is locked")
		}
		if e.Name == "plugins" && e.Locked != "" {
			t.Errorf("plugins locked: %s", e.Locked)
		}
	}
	// Stopped, it all goes.
	s.mu.Lock()
	s.state = Stopped
	s.mu.Unlock()
	if _, err := s.FileDelete([]string{"world"}); err != nil {
		t.Errorf("stopped: %v", err)
	}
}

func TestNothingChangesWhileMineNestIsBusy(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "a.txt", "a")
	s.setBusy("a restore", false)
	if _, err := s.FileWrite(WriteRequest{Path: "a.txt", Text: "b"}); err == nil {
		t.Error("wrote during a restore")
	}
	if _, err := s.FileDelete([]string{"a.txt"}); err == nil {
		t.Error("deleted during a restore")
	}
	if got, err := s.FileRead("a.txt"); err != nil || got.Text != "a" {
		t.Errorf("reading: %v", err)
	}
}

// ------------------------------------------------------------------ upload

func TestUpload(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "plugins/keep.txt", "k")
	res, err := s.FileUpload("plugins", "Cool.jar", 4, false, strings.NewReader("data"))
	if err != nil || !res.Restart || res.Size != 4 || res.Path != "plugins/Cool.jar" {
		t.Fatalf("%+v %v", res, err)
	}
	if readBack(t, s, "plugins/Cool.jar") != "data" {
		t.Fatal("content")
	}
	res, err = s.FileUpload("", "notes.txt", -1, false, strings.NewReader("n"))
	if err != nil || res.Restart {
		t.Fatalf("%+v %v", res, err)
	}
	// It never replaces without being told, and then the old one goes to the trash.
	_, err = s.FileUpload("plugins", "Cool.jar", 3, false, strings.NewReader("new"))
	wantCode(t, err, 409, "uploading over a file")
	if readBack(t, s, "plugins/Cool.jar") != "data" {
		t.Error("changed without a yes")
	}
	if _, err := s.FileUpload("plugins", "Cool.jar", 3, true, strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	tr, _ := s.Trash()
	if len(tr.Items) != 1 || tr.Items[0].Orig != "plugins/Cool.jar" {
		t.Errorf("the old one: %+v", tr)
	}
	// Names, places and kinds.
	for _, bad := range []string{"../x.txt", "a/b.txt", "con.txt", "x.txt:y", "bad.exe", "script.BAT", "a.cmd", "x.ps1", "trail.", ""} {
		if _, err := s.FileUpload("plugins", bad, 1, false, strings.NewReader("x")); err == nil {
			t.Errorf("uploaded %q", bad)
		}
	}
	if _, err := s.FileUpload("nope", "a.txt", 1, false, strings.NewReader("x")); err == nil {
		t.Error("uploaded to a folder that isn't there")
	}
	if _, err := s.FileUpload("plugins/keep.txt", "a.txt", 1, false, strings.NewReader("x")); err == nil {
		t.Error("uploaded into a file")
	}
	// The size limit, announced and not.
	defer func(o int64) { maxUploadBytes = o }(maxUploadBytes)
	maxUploadBytes = 10
	_, err = s.FileUpload("", "big.bin", 11, false, strings.NewReader(strings.Repeat("x", 11)))
	wantCode(t, err, 413, "an announced big file")
	_, err = s.FileUpload("", "big.bin", -1, false, strings.NewReader(strings.Repeat("x", 50)))
	wantCode(t, err, 413, "an unannounced big file")
	// A cut-off upload leaves nothing.
	_, err = s.FileUpload("", "cut.bin", 10, false, strings.NewReader("123"))
	wantRefused(t, err, "a short upload")
	if !gone(s, "big.bin") || !gone(s, "cut.bin") {
		t.Error("a refused upload left a file")
	}
	ents, _ := os.ReadDir(s.Root())
	for _, e := range ents {
		if internalName(e.Name()) {
			t.Errorf("left %s", e.Name())
		}
	}
}

// ------------------------------------------------------------------ move, copy, delete, trash

func TestMoveAndCopyNeverReplaceWithoutAYes(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "a.txt", "A")
	put(t, s, "b.txt", "B")
	put(t, s, "dir/in.txt", "in")
	put(t, s, "dir2/other.txt", "o")
	if err := s.FileMove("a.txt", "b.txt", false); err == nil {
		t.Error("moved over a file")
	} else {
		wantCode(t, err, 409, "a move over a file")
	}
	if readBack(t, s, "a.txt") != "A" || readBack(t, s, "b.txt") != "B" {
		t.Error("a refused move changed something")
	}
	if _, err := s.FileCopy("a.txt", "b.txt", false); err == nil {
		t.Error("copied over a file")
	}
	if err := s.FileMove("a.txt", "dir/a.txt", false); err != nil || !gone(s, "a.txt") || readBack(t, s, "dir/a.txt") != "A" {
		t.Errorf("a plain move: %v", err)
	}
	if err := s.FileMove("b.txt", "dir/a.txt", true); err != nil || readBack(t, s, "dir/a.txt") != "B" {
		t.Errorf("a confirmed move: %v", err)
	}
	if tr, _ := s.Trash(); len(tr.Items) != 1 || tr.Items[0].Name != "a.txt" {
		t.Errorf("what was replaced should be in the trash: %+v", tr)
	}
	// Folders: not into themselves, not over each other, copies are real copies.
	if err := s.FileMove("dir", "dir/sub", false); err == nil {
		t.Error("moved a folder into itself")
	}
	if err := s.FileMove("dir", "dir2", true); err == nil {
		t.Error("replaced a folder")
	}
	if _, err := s.FileCopy("dir", "dir/sub", false); err == nil {
		t.Error("copied a folder into itself")
	}
	if _, err := s.FileCopy("dir", "dir2", true); err == nil {
		t.Error("copied over a folder")
	}
	if res, err := s.FileCopy("dir", "dir3", false); err != nil || res.Files != 2 {
		t.Errorf("%+v %v", res, err)
	}
	if readBack(t, s, "dir3/in.txt") != "in" || readBack(t, s, "dir/in.txt") != "in" {
		t.Error("the copy")
	}
	if err := s.FileMove("dir3", "dir4", false); err != nil || !gone(s, "dir3") {
		t.Errorf("a folder move: %v", err)
	}
	if err := s.FileMove("", "x", false); err == nil {
		t.Error("moved the server's folder")
	}
	if err := s.FileMove("dir4", "dir4", false); err == nil {
		t.Error("moved onto itself")
	}
	if err := s.FileMove("nope", "x", false); err == nil {
		t.Error("moved nothing")
	}
	if err := s.FileMove("dir4", "missing/x", false); err == nil {
		t.Error("moved to a folder that isn't there")
	}
	// Only the capitals change.
	if err := s.FileMove("dir4", "Dir4", false); err != nil {
		t.Errorf("a change of capitals: %v", err)
	}
	if l, _ := s.FileList(""); func() bool {
		for _, e := range l.Entries {
			if e.Name == "Dir4" {
				return false
			}
		}
		return true
	}() {
		t.Error("the new capitals didn't show")
	}
	// Size limits on copies.
	defer func(a int, b int64) { maxCopyFiles, maxCopyBytes = a, b }(maxCopyFiles, maxCopyBytes)
	maxCopyFiles = 1
	if _, err := s.FileCopy("dir2", "c1", false); err != nil {
		t.Errorf("%v", err)
	}
	put(t, s, "dir2/second.txt", "s")
	_, err := s.FileCopy("dir2", "c2", false)
	wantCode(t, err, 413, "too many files to copy")
	if !gone(s, "c2") {
		t.Error("a refused copy left a folder")
	}
}

func TestTrashAndRestore(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "plugins/Essentials/config.yml", "e: 1\n")
	put(t, s, "ops.json", "[]")
	res, err := s.FileDelete([]string{"plugins/Essentials", "ops.json"})
	if err != nil || len(res.Trashed) != 2 {
		t.Fatalf("%v %v", res, err)
	}
	if !gone(s, "plugins/Essentials") || !gone(s, "ops.json") {
		t.Fatal("still there")
	}
	// It is in MineNest's own data, outside the server's folder, with a note of where from.
	if !strings.HasPrefix(s.trashDir(), s.Dir) || strings.HasPrefix(s.trashDir(), s.Root()) {
		t.Fatal(s.trashDir())
	}
	tr, _ := s.Trash()
	if len(tr.Items) != 2 {
		t.Fatalf("%+v", tr)
	}
	var folder, file TrashItem
	for _, it := range tr.Items {
		if it.Dir {
			folder = it
		} else {
			file = it
		}
	}
	if folder.Orig != "plugins/Essentials" || folder.Count != 1 || file.Orig != "ops.json" || file.Size != 2 {
		t.Errorf("%+v %+v", folder, file)
	}
	// It can't be reached through the file manager.
	if _, err := s.FileRead("../trash/" + folder.ID + "/item"); err == nil {
		t.Error("the trash was reachable")
	}
	// Restore, folders and all, even when the folder it was in is gone.
	os.RemoveAll(filepath.Join(s.Root(), "plugins"))
	to, err := s.TrashRestore(folder.ID, "", false)
	if err != nil || to != "plugins/Essentials" || readBack(t, s, "plugins/Essentials/config.yml") != "e: 1\n" {
		t.Fatalf("%q %v", to, err)
	}
	// Restoring onto something that is there asks first, or goes elsewhere.
	put(t, s, "ops.json", "[1]")
	_, err = s.TrashRestore(file.ID, "", false)
	wantCode(t, err, 409, "restoring over a file")
	if _, err := s.TrashRestore(file.ID, "ops-old.json", false); err != nil || readBack(t, s, "ops-old.json") != "[]" {
		t.Fatalf("%v", err)
	}
	if tr, _ = s.Trash(); len(tr.Items) != 0 {
		t.Errorf("restored things stay in the trash: %+v", tr)
	}
	// Bad ids go nowhere.
	for _, bad := range []string{"..", "../x", "1-2", "", "9999999999-zzzzzzzz", "9999999999-abcd1234/../../x"} {
		if _, err := s.TrashRestore(bad, "", false); err == nil {
			t.Errorf("restored %q", bad)
		}
		if _, err := s.TrashEmpty(bad); err == nil && bad != "" {
			t.Errorf("emptied %q", bad)
		}
	}
	// Emptying is the only way to delete for good.
	put(t, s, "x.txt", "x")
	put(t, s, "y.txt", "y")
	s.FileDelete([]string{"x.txt", "y.txt"})
	tr, _ = s.Trash()
	if n, err := s.TrashEmpty(tr.Items[0].ID); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if tr, _ = s.Trash(); len(tr.Items) != 1 {
		t.Fatalf("%+v", tr)
	}
	if n, err := s.TrashEmpty(""); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if tr, _ = s.Trash(); len(tr.Items) != 0 {
		t.Fatalf("%+v", tr)
	}
	// Deleting the server's own folder, or something that isn't there, does nothing.
	if _, err := s.FileDelete([]string{""}); err == nil {
		t.Error("deleted the server's folder")
	}
	if _, err := s.FileDelete([]string{"nope"}); err == nil {
		t.Error("deleted nothing")
	}
	if _, err := s.FileDelete(nil); err == nil {
		t.Error("deleted an empty list")
	}
	// A list with one bad path deletes none of them.
	put(t, s, "keep.txt", "k")
	if _, err := s.FileDelete([]string{"keep.txt", "../nest.json"}); err == nil || gone(s, "keep.txt") {
		t.Error("a half-done delete")
	}
}

// ------------------------------------------------------------------ zip

type zipEntry struct {
	name string
	data string
	mode fs.FileMode
}

func makeZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e.data))
	}
	zw.Close()
	return buf.Bytes()
}

func TestUnzip(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "packs/a.zip", string(makeZip(t,
		zipEntry{"pack/", "", fs.ModeDir | 0o755}, zipEntry{"pack/one.txt", "1", 0}, zipEntry{"pack/sub/two.txt", "22", 0},
		zipEntry{"mods/m.jar", "jar", 0}, zipEntry{"start.bat", "echo", 0})))
	res, err := s.FileUnzip("packs/a.zip", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Restart != false && false {
		t.Errorf("%+v", res)
	}
	if readBack(t, s, "packs/pack/sub/two.txt") != "22" || readBack(t, s, "packs/mods/m.jar") != "jar" {
		t.Error("content")
	}
	if !gone(s, "packs/start.bat") || len(res.Skipped) != 1 || res.Skipped[0] != "start.bat" {
		t.Errorf("a program was unpacked: %+v", res)
	}
	// Into a folder of its own, and a jar in mods asks for a restart.
	put(t, s, "up.zip", string(makeZip(t, zipEntry{"mods/x.jar", "x", 0})))
	res, err = s.FileUnzip("up.zip", "", false)
	if err != nil || !res.Restart {
		t.Fatalf("%+v %v", res, err)
	}
	if res, err = s.FileUnzip("up.zip", "sub", false); err != nil || readBack(t, s, "sub/mods/x.jar") != "x" {
		t.Fatalf("%+v %v", res, err)
	}
	// Files that are there already need a yes, and then go to the trash.
	_, err = s.FileUnzip("up.zip", "", false)
	wantCode(t, err, 409, "unpacking over files")
	put(t, s, "mods/x.jar", "changed")
	if _, err := s.FileUnzip("up.zip", "", true); err != nil {
		t.Fatal(err)
	}
	if readBack(t, s, "mods/x.jar") != "x" {
		t.Error("not replaced")
	}
	if tr, _ := s.Trash(); len(tr.Items) == 0 {
		t.Error("the replaced file isn't in the trash")
	}
	// Not a zip, not there.
	if _, err := s.FileUnzip("ops.json", "", false); err == nil {
		t.Error("unpacked a non-zip")
	}
	put(t, s, "broken.zip", "not a zip")
	if _, err := s.FileUnzip("broken.zip", "", false); err == nil {
		t.Error("unpacked garbage")
	}
	for _, bad := range []string{"../x", "a/b", "con", "x:y"} {
		if _, err := s.FileUnzip("up.zip", bad, false); err == nil {
			t.Errorf("unpacked into %q", bad)
		}
	}
}

func TestZipSlipAndHostileArchives(t *testing.T) {
	_, s := newFilesServer(t)
	outside := t.TempDir()
	hostile := map[string][]zipEntry{
		"slip":        {{"../evil.txt", "x", 0}},
		"slip deep":   {{"a/../../evil.txt", "x", 0}},
		"slip start":  {{"ok.txt", "ok", 0}, {"../../evil.txt", "x", 0}},
		"absolute":    {{"/etc/evil.txt", "x", 0}},
		"drive":       {{"C:/evil.txt", "x", 0}},
		"drive plain": {{"C:evil.txt", "x", 0}},
		"backslash":   {{`..\evil.txt`, "x", 0}},
		"backslash b": {{`a\b.txt`, "x", 0}},
		"unc":         {{`\\host\share\evil.txt`, "x", 0}},
		"stream":      {{"a.txt:evil", "x", 0}},
		"device":      {{"nul", "x", 0}, {"aux.txt", "x", 0}},
		"trailing":    {{"evil.", "x", 0}},
		"dot":         {{"./evil.txt", "x", 0}},
		"empty part":  {{"a//evil.txt", "x", 0}},
		"symlink":     {{"link", "../../secret", fs.ModeSymlink | 0o777}},
		"symlink dir": {{"d/", "", fs.ModeSymlink | 0o777}},
		"twice":       {{"a.txt", "1", 0}, {"a.txt", "2", 0}},
		"twice cases": {{"a.txt", "1", 0}, {"A.TXT", "2", 0}},
		"deep":        {{strings.Repeat("d/", 60) + "x.txt", "x", 0}},
		"internal":    {{".minenest-x/evil.txt", "x", 0}},
		"newline":     {{"a\nb.txt", "x", 0}},
	}
	for name, es := range hostile {
		put(t, s, "h/"+strings.ReplaceAll(name, " ", "_")+".zip", string(makeZip(t, es...)))
		if _, err := s.FileUnzip("h/"+strings.ReplaceAll(name, " ", "_")+".zip", "", true); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
	// Nothing was written anywhere: only the zips are in h, nothing leaked up or out.
	var found []string
	filepath.WalkDir(s.Root(), func(p string, d fs.DirEntry, err error) error {
		if !d.IsDir() && !strings.HasSuffix(p, ".zip") {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("files appeared: %v", found)
	}
	for _, dir := range []string{s.Dir, filepath.Dir(s.Dir), outside} {
		if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
			t.Errorf("evil.txt appeared in %s", dir)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(s.Root(), "h")); len(ents) != len(hostile) {
		t.Errorf("a staging folder was left: %d", len(ents))
	}
}

func TestUnzipOverALink(t *testing.T) {
	_, s := newFilesServer(t)
	outside := t.TempDir()
	put(t, s, "up.zip", string(makeZip(t, zipEntry{"inner/evil.txt", "x", 0})))
	makeDirLink(t, filepath.Join(s.Root(), "inner"), outside)
	if _, err := s.FileUnzip("up.zip", "", true); err == nil {
		t.Error("unpacked through a link")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Error("a file was written outside")
	}
}

func TestUnzipLimits(t *testing.T) {
	_, s := newFilesServer(t)
	defer func(a int, b, c int64) { unzipMaxFiles, unzipMaxFile, unzipMaxTotal = a, b, c }(unzipMaxFiles, unzipMaxFile, unzipMaxTotal)

	// Too many files.
	unzipMaxFiles = 3
	var es []zipEntry
	for i := 0; i < 4; i++ {
		es = append(es, zipEntry{"f" + string(rune('a'+i)) + ".txt", "x", 0})
	}
	put(t, s, "many.zip", string(makeZip(t, es...)))
	_, err := s.FileUnzip("many.zip", "", false)
	wantCode(t, err, 413, "too many files")
	unzipMaxFiles = 50000

	// One file too big, and everything too big together.
	unzipMaxFile, unzipMaxTotal = 100, 150
	put(t, s, "bigfile.zip", string(makeZip(t, zipEntry{"big.txt", strings.Repeat("a", 101), 0})))
	_, err = s.FileUnzip("bigfile.zip", "", false)
	wantCode(t, err, 413, "one file too big")
	put(t, s, "bigtotal.zip", string(makeZip(t, zipEntry{"a.txt", strings.Repeat("a", 80), 0}, zipEntry{"b.txt", strings.Repeat("b", 80), 0})))
	_, err = s.FileUnzip("bigtotal.zip", "", false)
	wantCode(t, err, 413, "too much together")

	// An archive that says its files are small and then isn't: what comes out is counted.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var comp bytes.Buffer
	fw, _ := flate.NewWriter(&comp, flate.BestCompression)
	fw.Write(bytes.Repeat([]byte("z"), 5000))
	fw.Close()
	h := &zip.FileHeader{Name: "liar.txt", Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(bytes.Repeat([]byte("z"), 5000)),
		CompressedSize64: uint64(comp.Len()), UncompressedSize64: 10}
	w, err := zw.CreateRaw(h)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(comp.Bytes())
	zw.Close()
	put(t, s, "liar.zip", buf.String())
	if _, err := s.FileUnzip("liar.zip", "", false); err == nil {
		t.Error("an archive that lied about its size was unpacked")
	}
	if !gone(s, "liar.txt") {
		t.Error("the lying file was kept")
	}
	if ents, _ := os.ReadDir(s.Root()); func() bool {
		for _, e := range ents {
			if internalName(e.Name()) {
				return true
			}
		}
		return false
	}() {
		t.Error("a staging folder was left behind")
	}
	// A bomb: a small zip that unpacks to a lot.
	unzipMaxFile, unzipMaxTotal = 1<<20, 1<<20
	put(t, s, "bomb.zip", string(makeZip(t, zipEntry{"zeros.bin", strings.Repeat("\x00", 4<<20), 0})))
	if _, err := s.FileUnzip("bomb.zip", "", false); err == nil {
		t.Error("a bomb was unpacked")
	}
}

func TestUnzipWhileRunning(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "w.zip", string(makeZip(t, zipEntry{"world/region/r.0.0.mca", "x", 0}, zipEntry{"ok.txt", "x", 0})))
	setRunning(s)
	if _, err := s.FileUnzip("w.zip", "", false); err == nil {
		t.Error("unpacked region files into a running server")
	}
	if !gone(s, "ok.txt") {
		t.Error("part of a refused archive was unpacked")
	}
}

func TestZipDownload(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "plugins/a.yml", "a")
	put(t, s, "plugins/sub/b.yml", "b")
	put(t, s, "plugins/.minenest-tmp-1", "temp")
	var buf bytes.Buffer
	name, err := s.FileZip("plugins", &buf, 0)
	if err != nil || name != "plugins.zip" {
		t.Fatalf("%q %v", name, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range zr.File {
		got = append(got, f.Name)
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, "minenest-tmp") {
			t.Errorf("odd name %s", f.Name)
		}
	}
	if strings.Join(got, ",") != "plugins/a.yml,plugins/sub/,plugins/sub/b.yml" {
		t.Errorf("%v", got)
	}
	if _, err := s.FileZip("plugins/a.yml", io.Discard, 0); err == nil {
		t.Error("zipped a file")
	}
	// A ceiling for sending to other computers.
	big := strings.Repeat("0123456789", 1000)
	put(t, s, "data/big.txt", big)
	_, err = s.FileZip("data", io.Discard, 20)
	wantCode(t, err, 413, "a zip over the limit")
}

// ------------------------------------------------------------------ the API: headers and the rule for other computers

type apiHarness struct {
	t *testing.T
	m *Manager
	a *api
}

func newAPIHarness(t *testing.T) (*apiHarness, *Server) {
	m, s := newFilesServer(t)
	a := &api{m: m, token: strings.Repeat("k", 32), web: fstest.MapFS{"index.html": {Data: []byte("x")}}, quit: make(chan struct{})}
	a.mux = a.routes()
	m.api = a
	return &apiHarness{t, m, a}, s
}

// do sends a request as the window on this computer would, or (elsewhere) the way the home-network
// door and the website pass one on.
func (h *apiHarness) do(method, path string, body any, elsewhere bool) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host = "127.0.0.1:47575"
	w := httptest.NewRecorder()
	if elsewhere {
		r.Host = "192.168.1.5:47576"
		r.RemoteAddr = "192.168.1.5:5555"
		h.m.forward(w, r, true, doorWeb)
		return w
	}
	r.Header.Set("Authorization", "Bearer "+h.a.token)
	if method != "GET" {
		r.Header.Set("X-MineNest", "1")
	}
	h.a.mux.ServeHTTP(w, r)
	return w
}

const fp = "/api/servers/srv/files"

func TestOtherComputersCanOnlyReadUnlessAllowed(t *testing.T) {
	h, s := newAPIHarness(t)
	put(t, s, "plugins/a.yml", "a: 1\n")
	put(t, s, "up.zip", string(makeZip(t, zipEntry{"z.txt", "z", 0})))
	put(t, s, "old.txt", "old")
	s.FileDelete([]string{"old.txt"})
	tr, _ := s.Trash()
	tid := tr.Items[0].ID

	changing := []struct {
		method, path string
		body         any
	}{
		{"PUT", fp + "/content", map[string]any{"path": "plugins/a.yml", "text": "hacked"}},
		{"POST", fp + "/upload?dir=plugins&name=evil.jar", "MZ"},
		{"POST", fp + "/mkdir", map[string]any{"path": "newdir"}},
		{"POST", fp + "/move", map[string]any{"from": "plugins/a.yml", "to": "b.yml"}},
		{"POST", fp + "/copy", map[string]any{"from": "plugins/a.yml", "to": "c.yml"}},
		{"POST", fp + "/delete", map[string]any{"paths": []string{"plugins/a.yml"}}},
		{"POST", fp + "/unzip", map[string]any{"path": "up.zip"}},
		{"POST", fp + "/trash/" + tid + "/restore", map[string]any{}},
		{"DELETE", fp + "/trash/" + tid, nil},
		{"POST", fp + "/trash/empty", map[string]any{"confirm": true}},
		{"POST", "/api/servers/srv/addons/upload?name=evil.jar", "MZ"},
		{"POST", "/api/import/zip?name=x", "PK"},
	}
	reading := []string{fp + "?path=plugins", fp + "/read?path=plugins/a.yml", fp + "/download?path=plugins/a.yml", fp + "/zip?path=plugins", fp + "/trash"}

	// Off by default: other computers read, and only read.
	if h.m.Settings().RemoteFiles {
		t.Fatal("on by default")
	}
	for _, c := range changing {
		if w := h.do(c.method, c.path, c.body, true); w.Code != 403 || !strings.Contains(w.Body.String(), "Let other computers change files") {
			t.Errorf("%s %s from elsewhere: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	for _, p := range reading {
		if w := h.do("GET", p, nil, true); w.Code != 200 {
			t.Errorf("GET %s from elsewhere: %d %s", p, w.Code, w.Body.String())
		}
	}
	if readBack(t, s, "plugins/a.yml") != "a: 1\n" || !gone(s, "newdir") || !gone(s, "b.yml") || !gone(s, "plugins/evil.jar") || !gone(s, "z.txt") {
		t.Fatal("something changed although it was refused")
	}
	// The listing says so, so the page can show it.
	var l struct {
		CanChange bool   `json:"can_change"`
		Why       string `json:"why"`
		Elsewhere bool   `json:"elsewhere"`
	}
	json.Unmarshal(h.do("GET", fp, nil, true).Body.Bytes(), &l)
	if l.CanChange || !l.Elsewhere || l.Why == "" {
		t.Errorf("%+v", l)
	}
	json.Unmarshal(h.do("GET", fp, nil, false).Body.Bytes(), &l)
	if !l.CanChange || l.Elsewhere {
		t.Errorf("on this computer: %+v", l)
	}
	// This computer is never held back, and the switch itself is only reachable from here.
	if w := h.do("PUT", fp+"/content", map[string]any{"path": "plugins/a.yml", "text": "mine: 1\n"}, false); w.Code != 200 {
		t.Errorf("on this computer: %d %s", w.Code, w.Body.String())
	}
	if w := h.do("POST", "/api/remote", map[string]any{"files": true}, true); w.Code != 403 {
		t.Errorf("the switch was reachable from elsewhere: %d", w.Code)
	}
	if h.m.Settings().RemoteFiles {
		t.Fatal("turned on from elsewhere")
	}
	if w := h.do("POST", "/api/remote", map[string]any{"files": true}, false); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !h.m.Settings().RemoteFiles || !h.m.RemoteInfo().Files {
		t.Fatal("the switch didn't turn on")
	}
	// On: the rest of the rules still apply (links, names, the running server...), but changes go through.
	if w := h.do("POST", fp+"/mkdir", map[string]any{"path": "newdir"}, true); w.Code != 200 {
		t.Errorf("with the switch on: %d %s", w.Code, w.Body.String())
	}
	if w := h.do("POST", fp+"/mkdir", map[string]any{"path": "../escape"}, true); w.Code == 200 {
		t.Errorf("a hostile path got through when on")
	}
	if w := h.do("POST", fp+"/upload?dir=plugins&name=ok.jar", "PK", true); w.Code != 200 {
		t.Errorf("an upload with the switch on: %d %s", w.Code, w.Body.String())
	}
	// And off again.
	h.do("POST", "/api/remote", map[string]any{"files": false}, false)
	if w := h.do("POST", fp+"/mkdir", map[string]any{"path": "newdir2"}, true); w.Code != 403 {
		t.Errorf("after turning it off: %d", w.Code)
	}
	// Bringing in a folder by its path on this computer, and looking for them, are only for here.
	if w := h.do("POST", "/api/import", map[string]any{"path": t.TempDir()}, true); w.Code != 403 {
		t.Errorf("import by path from elsewhere: %d", w.Code)
	}
	if w := h.do("GET", "/api/import/find", nil, true); w.Code != 403 {
		t.Errorf("finding servers from elsewhere: %d", w.Code)
	}
}

func TestChangingRoutesKeepTheExistingProtections(t *testing.T) {
	h, s := newAPIHarness(t)
	put(t, s, "a.txt", "a")
	rec := func(method, path, body string, mod func(r *http.Request)) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "127.0.0.1:47575"
		mod(r)
		w := httptest.NewRecorder()
		h.a.mux.ServeHTTP(w, r)
		return w.Code
	}
	key := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+h.a.token) }
	hdr := func(r *http.Request) { key(r); r.Header.Set("X-MineNest", "1") }
	changes := [][3]string{{"PUT", fp + "/content", `{"path":"a.txt","text":"x"}`}, {"POST", fp + "/upload?dir=&name=u.txt", "x"},
		{"POST", fp + "/mkdir", `{"path":"d"}`}, {"POST", fp + "/move", `{"from":"a.txt","to":"b.txt"}`}, {"POST", fp + "/copy", `{"from":"a.txt","to":"b.txt"}`},
		{"POST", fp + "/delete", `{"paths":["a.txt"]}`}, {"POST", fp + "/unzip", `{"path":"a.txt"}`}, {"POST", fp + "/trash/empty", `{"confirm":true}`}}
	for _, c := range changes {
		if code := rec(c[0], c[1], c[2], func(r *http.Request) {}); code != 401 {
			t.Errorf("%s %s without the key: %d", c[0], c[1], code)
		}
		if code := rec(c[0], c[1], c[2], key); code != 403 {
			t.Errorf("%s %s without the header: %d", c[0], c[1], code)
		}
		if code := rec(c[0], c[1], c[2], func(r *http.Request) { hdr(r); r.Host = "evil.example.com" }); code != 403 {
			t.Errorf("%s %s with another host name: %d", c[0], c[1], code)
		}
		if code := rec(c[0], c[1], c[2], func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "minenest", Value: h.a.token}) }); code != 403 {
			t.Errorf("%s %s with the cookie only: %d", c[0], c[1], code)
		}
	}
	for _, p := range []string{fp + "?path=", fp + "/read?path=a.txt", fp + "/download?path=a.txt", fp + "/zip?path=", fp + "/trash"} {
		if code := rec("GET", p, "", func(r *http.Request) {}); code != 401 {
			t.Errorf("GET %s without the key: %d", p, code)
		}
		if code := rec("GET", p, "", func(r *http.Request) { key(r); r.Host = "evil.example.com" }); code != 403 {
			t.Errorf("GET %s with another host name: %d", p, code)
		}
	}
	if !strings.Contains(readBack(t, s, "a.txt"), "a") || !gone(s, "b.txt") || !gone(s, "d") || !gone(s, "u.txt") {
		t.Error("a refused request changed something")
	}
	// The trash won't empty without a confirm.
	if w := h.do("POST", fp+"/trash/empty", map[string]any{}, false); w.Code != 400 {
		t.Errorf("emptied without a confirm: %d", w.Code)
	}
}

func TestDownloadsAreAlwaysAttachments(t *testing.T) {
	h, s := newAPIHarness(t)
	html := "<html><script>alert(document.cookie)</script></html>"
	put(t, s, "evil.html", html)
	put(t, s, "evil.svg", `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)
	put(t, s, "evil.txt", html)
	weird := "we'ird;name é.yml" // a quote can't be in a name at all, so these are the awkward ones left
	put(t, s, "plugins/"+weird, "a")
	put(t, s, "folder/x.html", html)
	check := func(path, ctype string) *httptest.ResponseRecorder {
		w := h.do("GET", path, nil, false)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		hd := w.Header()
		if !strings.HasPrefix(hd.Get("Content-Disposition"), "attachment;") || hd.Get("Content-Type") != ctype ||
			hd.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(hd.Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: %v", path, hd)
		}
		return w
	}
	if w := check(fp+"/download?path=evil.html", "application/octet-stream"); w.Body.String() != html {
		t.Error("the file")
	}
	check(fp+"/download?path=evil.svg", "application/octet-stream")
	w := check(fp+"/download?path=plugins/"+url.QueryEscape(weird), "application/octet-stream")
	if cd := w.Header().Get("Content-Disposition"); strings.Count(cd, `"`) != 2 || !strings.Contains(cd, "filename*=UTF-8''") || strings.Contains(cd, `"ird`) {
		t.Errorf("a name with a quote in it breaks the header: %s", cd)
	}
	zw := check(fp+"/zip?path=folder", "application/zip")
	if zr, err := zip.NewReader(bytes.NewReader(zw.Body.Bytes()), int64(zw.Body.Len())); err != nil || len(zr.File) != 1 {
		t.Errorf("the zip: %v", err)
	}
	// JSON answers are not sniffed either, and a file's text only ever comes inside JSON.
	w = h.do("GET", fp+"/read?path=evil.txt", nil, false)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%d %v", w.Code, w.Header())
	}
	// Hostile paths over HTTP.
	for _, p := range []string{"../nest.json", "%2e%2e/nest.json", "..%2fnest.json", "/etc/passwd", "C%3A/Windows/win.ini", "a%00b", "evil.html%00.png"} {
		if w := h.do("GET", fp+"/download?path="+p, nil, false); w.Code == 200 {
			t.Errorf("downloaded %q", p)
		}
		if w := h.do("GET", fp+"/read?path="+p, nil, false); w.Code == 200 {
			t.Errorf("read %q", p)
		}
	}
	// A folder isn't a download; a file isn't a zip.
	if w := h.do("GET", fp+"/download?path=folder", nil, false); w.Code != 400 {
		t.Errorf("%d", w.Code)
	}
	if w := h.do("GET", fp+"/zip?path=evil.html", nil, false); w.Code != 400 {
		t.Errorf("%d", w.Code)
	}
	// Big downloads aren't sent to other computers.
	big := make([]byte, 1<<20)
	put(t, s, "big.bin", string(big))
	_ = big
	old := remoteSendLimitForTest(1 << 10)
	defer old()
	if w := h.do("GET", fp+"/download?path=big.bin", nil, true); w.Code != 413 {
		t.Errorf("a big download to another computer: %d", w.Code)
	}
	if w := h.do("GET", fp+"/zip?path=", nil, true); w.Code != 413 {
		t.Errorf("a big zip to another computer: %d", w.Code)
	}
	if w := h.do("GET", fp+"/download?path=big.bin", nil, false); w.Code != 200 {
		t.Errorf("here: %d", w.Code)
	}
}

func TestUploadOverHTTP(t *testing.T) {
	h, s := newAPIHarness(t)
	put(t, s, "plugins/keep.txt", "k")
	w := h.do("POST", fp+"/upload?dir=plugins&name=Cool.jar", []byte("PK\x03\x04"), false)
	var res UploadResult
	json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || !res.Restart || readBack(t, s, "plugins/Cool.jar") != "PK\x03\x04" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := h.do("POST", fp+"/upload?dir=plugins&name=Cool.jar", []byte("x"), false); w.Code != 409 {
		t.Errorf("%d", w.Code)
	}
	if w := h.do("POST", fp+"/upload?dir=plugins&name=Cool.jar&overwrite=1", []byte("x"), false); w.Code != 200 {
		t.Errorf("%d", w.Code)
	}
	for _, q := range []string{"dir=..&name=x.txt", "dir=&name=..%2Fx.txt", "dir=&name=a.exe", "dir=%2Fetc&name=x.txt"} {
		if w := h.do("POST", fp+"/upload?"+q, []byte("x"), false); w.Code == 200 {
			t.Errorf("uploaded with %s", q)
		}
	}
	defer func(o int64) { maxUploadBytes = o }(maxUploadBytes)
	maxUploadBytes = 8
	if w := h.do("POST", fp+"/upload?dir=&name=big.bin", []byte(strings.Repeat("x", 20)), false); w.Code != 413 {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

func remoteSendLimitForTest(n int64) func() {
	old := remoteSendLimit
	remoteSendLimit = n
	return func() { remoteSendLimit = old }
}

func TestNewFileNeedsItsFolder(t *testing.T) {
	_, s := newFilesServer(t)
	if _, err := s.FileWrite(WriteRequest{Path: "nofolder/a.txt", Text: "x", Create: true}); err == nil {
		t.Error("made a file in a folder that isn't there")
	}
	if !gone(s, "nofolder") {
		t.Error("a folder appeared")
	}
}
