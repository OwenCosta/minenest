package nest

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests here are for what the review of the earlier work found: what another computer can
// reach, names that lead out of a server's folder, and the places two jobs could meet.

func TestLevelNameStaysInsideTheServerFolder(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "world/level.dat", "x")
	for _, bad := range []string{"..", "../..", "a/b", "", ".", "x\\y", "C:", "con", "..\\.."} {
		if levelOK(bad) {
			t.Errorf("%q accepted as a level-name", bad)
		}
	}
	if !levelOK("world") || !levelOK("My World") {
		t.Fatal("plain names refused")
	}
	// A properties file that already says "..": the world code treats it as the default.
	put(t, s, "server.properties", "level-name=..\n")
	if got := s.levelName(); got != "world" {
		t.Fatalf("level name %q", got)
	}
	for _, f := range s.worldFolders() {
		if f == ".." || strings.Contains(f, "/") {
			t.Fatalf("world folder %q", f)
		}
	}
	// And nothing outside a plain folder is ever removed.
	for _, bad := range []string{"..", "../x", "a/b", "", "."} {
		if err := s.removeWorldFolder(bad); err == nil {
			t.Errorf("removed %q", bad)
		}
	}
	if !exists(s.Dir) || !exists(filepath.Join(s.Dir, "nest.json")) {
		t.Fatal("the server's data was touched")
	}
	// Saving it from the Files tab or the settings is refused.
	if _, err := s.checkProperties("level-name=..\n"); err == nil {
		t.Error("the editor accepted level-name=..")
	}
	if err := checkPropertySet("level-name", "../.."); err == nil {
		t.Error("the settings accepted level-name=../..")
	}
}

func TestResetWorldWithABadLevelNameKeepsTheServer(t *testing.T) {
	m, s := newFilesServer(t)
	put(t, s, "world/level.dat", "x")
	put(t, s, "server.properties", "level-name=..\n")
	j, err := m.ResetWorld(s, "")
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, j)
	if !exists(filepath.Join(s.Dir, "nest.json")) || !exists(s.Root()) {
		t.Fatal("a reset reached outside the world")
	}
}

func waitJob(t *testing.T, j *Job) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if j.view().Finished {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the job didn't finish")
}

func TestPropertiesRefuseLineBreaksOfAnyKind(t *testing.T) {
	for _, v := range []string{"hi\rlevel-name ../..", "a\fb", "a b", "a\nb", "a\x00b"} {
		if err := checkPropertySet("motd", v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	if err := checkPropertySet("mo td", "x"); err == nil {
		t.Error("a key with a space")
	}
	// Even if one gets as far as Set, it is escaped and reads back whole.
	p := propertiesFromText("")
	p.Set("motd", "a\rb\fc")
	if strings.ContainsAny(p.Text(), "\r\f") {
		t.Fatalf("%q", p.Text())
	}
	if v, _ := propertiesFromText(p.Text()).Get("motd"); v != "a\rb\fc" {
		t.Fatalf("%q", v)
	}
	// A file with a lone CR is read the way Java reads it.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "p"), []byte("a=1\rserver-port=80\r"), 0o644)
	if v, _ := loadProperties(filepath.Join(dir, "p")).Get("server-port"); v != "80" {
		t.Fatalf("port %q", v)
	}
}

func TestPropertiesPutThroughTheDoor(t *testing.T) {
	h, s := newAPIHarness(t)
	for _, body := range []map[string]string{
		{"level-name": ".."},
		{"motd": "hi\rlevel-name ../..\rserver-port 80"},
	} {
		if w := h.do("PUT", "/api/servers/srv/properties", body, true); w.Code != 400 {
			t.Errorf("%v: %d", body, w.Code)
		}
	}
	if exists(filepath.Join(s.Root(), "server.properties")) {
		t.Fatal("a refused change was written")
	}
	if w := h.do("PUT", "/api/servers/srv/properties", map[string]string{"motd": "Hello"}, true); w.Code != 200 {
		t.Fatalf("a plain change: %d %s", w.Code, w.Body)
	}
}

func TestJavaArgumentsAndAddonsAreNotForOtherComputers(t *testing.T) {
	h, s := newAPIHarness(t)
	// Even with "Let other computers change files" on, Java's own arguments stay here.
	on := true
	if err := h.m.SetRemote(nil, nil, nil, &on); err != nil {
		t.Fatal(err)
	}
	for _, elsewhere := range []bool{true} {
		w := h.do("PATCH", "/api/servers/srv", map[string]string{"jvm_args": "-XX:OnError=calc.exe"}, elsewhere)
		if w.Code != 403 {
			t.Errorf("jvm_args from elsewhere: %d", w.Code)
		}
	}
	if s.Config().JVMArgs != "" {
		t.Fatal("the arguments were stored")
	}
	if w := h.do("PATCH", "/api/servers/srv", map[string]any{"ram_mb": 1024}, true); w.Code != 200 {
		t.Errorf("memory from elsewhere: %d", w.Code)
	}
	if w := h.do("PATCH", "/api/servers/srv", map[string]string{"jvm_args": "-XX:+UseG1GC"}, false); w.Code != 200 || s.Config().JVMArgs != "-XX:+UseG1GC" {
		t.Errorf("jvm_args here: %d", w.Code)
	}
	off := false
	h.m.SetRemote(nil, nil, nil, &off)
	for _, p := range []string{"/addons", "/addons/update"} {
		if w := h.do("POST", "/api/servers/srv"+p, map[string]string{"project": "x"}, true); w.Code != 403 {
			t.Errorf("%s from elsewhere with the switch off: %d", p, w.Code)
		}
	}
}

func TestStayOnThisComputerList(t *testing.T) {
	for _, c := range []struct {
		method, path string
		local        bool
	}{
		{"POST", "/api/shutdown", true}, {"POST", "/api/update", true}, {"GET", "/api/update", false},
		{"POST", "/api/account/login", true}, {"POST", "/api/account/logout", true}, {"POST", "/api/account/oc/start", true},
		{"GET", "/api/account", false}, {"POST", "/api/playit/forget", true}, {"POST", "/api/remote", true},
		{"GET", "/api/remote", true}, {"GET", "/open", true}, {"GET", "/api/servers", false}, {"POST", "/api/playit/restart", false},
	} {
		if got := localOnly(c.method, c.path); got != c.local {
			t.Errorf("%s %s: %v", c.method, c.path, got)
		}
	}
	h, _ := newAPIHarness(t)
	for _, p := range []string{"/api/update", "/api/account/login", "/api/account/logout", "/api/playit/forget"} {
		if w := h.do("POST", p, map[string]string{"Token": "x"}, true); w.Code != 403 {
			t.Errorf("%s from elsewhere: %d", p, w.Code)
		}
	}
}

func TestNewFilesCantBecomeProgramsOrReplaceTheProperties(t *testing.T) {
	_, s := newFilesServer(t)
	put(t, s, "server.properties", "server-port=25599\nlevel-name=world\n")
	put(t, s, "a.txt", "hi")
	put(t, s, "x.js", "hi")

	// A rename or a copy can't make a program, and nothing goes over server.properties.
	wantRefused(t, s.FileMove("a.txt", "run.bat", false), "move to .bat")
	wantRefused(t, s.FileMove("a.txt", "SERVER.PROPERTIES", true), "move over the properties")
	_, err := s.FileCopy("a.txt", "run.cmd", false)
	wantRefused(t, err, "copy to .cmd")
	_, err = s.FileCopy("a.txt", "server.properties", true)
	wantRefused(t, err, "copy over the properties")
	_, err = s.FileUpload("", "server.properties", 15, true, strings.NewReader("server-port=abc"))
	wantRefused(t, err, "upload over the properties")
	for _, n := range []string{"x.url", "x.js", "x.jse", "y.PIF", "z.msc", "a.application", "b.ps1xml"} {
		if _, err := s.FileUpload("", n, 1, false, strings.NewReader("x")); err == nil {
			t.Errorf("uploaded %s", n)
		}
	}
	if got := readBack(t, s, "server.properties"); !strings.Contains(got, "25599") {
		t.Fatalf("the properties were changed: %q", got)
	}
	// A program that is already there can still be renamed, and ordinary names are fine.
	put(t, s, "old.bat", "x")
	if err := s.FileMove("old.bat", "older.bat", false); err != nil {
		t.Errorf("renaming a .bat that was there: %v", err)
	}
	if err := s.FileMove("a.txt", "b.txt", false); err != nil {
		t.Errorf("an ordinary move: %v", err)
	}
	// An archive's server.properties and programs are left out, not unpacked.
	put(t, s, "up.zip", string(makeZip(t,
		zipEntry{"server.properties", "server-port=zzz\nlevel-name=..\n", 0},
		zipEntry{"run.bat", "x", 0}, zipEntry{"ok.txt", "ok", 0})))
	res, err := s.FileUnzip("up.zip", "", true)
	if err != nil || res.Files != 1 || len(res.Skipped) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := readBack(t, s, "server.properties"); strings.Contains(got, "zzz") {
		t.Fatalf("unpacked the properties: %q", got)
	}
	// A link entry beside a folder of the same name is refused in plain words.
	put(t, s, "l.zip", string(makeZip(t,
		zipEntry{"lnk", "target", 0}, zipEntry{"lnk/inner.txt", "i", 0})))
	_, err = s.FileUnzip("l.zip", "", true)
	if err == nil || strings.Contains(err.Error(), s.Dir) {
		t.Fatalf("%v", err)
	}
	// A zip made on Unix with a link entry is refused whatever this system is.
	put(t, s, "s.zip", string(makeZip(t, zipEntry{"sym", "../../etc", fs.ModeSymlink | 0o777})))
	if _, err := s.FileUnzip("s.zip", "", true); err == nil {
		t.Fatal("a link entry was unpacked")
	}
}

func TestAddonUploadFollowsTheFileRules(t *testing.T) {
	h, s := newAPIHarness(t)
	put(t, s, "plugins/keep.txt", "x")
	up := func(name string, body string) int {
		r := httptest.NewRequest("POST", "/api/servers/srv/addons/upload?name="+name, strings.NewReader(body))
		r.Host = "127.0.0.1:47575"
		r.Header.Set("Authorization", "Bearer "+h.a.token)
		r.Header.Set("X-MineNest", "1")
		w := httptest.NewRecorder()
		h.a.mux.ServeHTTP(w, r)
		return w.Code
	}
	for _, n := range []string{"ads.jar%3Ahidden.jar", "con.jar", "x.exe", "a%2A.jar"} {
		if c := up(n, "x"); c == 200 {
			t.Errorf("%s accepted", n)
		}
	}
	if c := up("ok.jar", "one"); c != 200 {
		t.Fatalf("a plain jar: %d", c)
	}
	if c := up("ok.jar", "two"); c != 409 {
		t.Fatalf("a second one over it: %d", c)
	}
	if readBack(t, s, "plugins/ok.jar") != "one" {
		t.Fatal("replaced without asking")
	}
	// While the server runs it is the same as in the Files tab: the busy rule is checked, and a
	// busy server takes nothing.
	s.mu.Lock()
	s.busy = "a backup"
	s.mu.Unlock()
	if c := up("busy.jar", "x"); c != 403 || !gone(s, "plugins/busy.jar") {
		t.Fatalf("while the server's files are busy: %d", c)
	}
}

func TestErrorsLeaveOutPaths(t *testing.T) {
	_, err := os.ReadFile(filepath.Join(t.TempDir(), "nope.txt"))
	w := httptest.NewRecorder()
	writeErr(w, err)
	var out map[string]string
	json.Unmarshal(w.Body.Bytes(), &out)
	if strings.Contains(out["error"], "nope.txt") || strings.Contains(out["error"], string(os.PathSeparator)) {
		t.Fatalf("%q", out["error"])
	}
	t.Setenv("MINENEST_HOME", filepath.Join(t.TempDir(), "home"))
	w = httptest.NewRecorder()
	writeErr(w, &os.LinkError{Op: "rename", Old: filepath.Join(Home(), "servers", "a"), New: filepath.Join(Home(), "trash", "b"), Err: fs.ErrPermission})
	if strings.Contains(w.Body.String(), Home()) {
		t.Fatalf("%s", w.Body)
	}
}

func TestMojangAnswerForAnotherPlayerIsNotTaken(t *testing.T) {
	// Every query is answered with Notch, whoever was asked about.
	fakeMojang(t, 0, map[string]string{"Notch": "069a79f444e94726a5befca90e38aaf5"})
	srvURL := profileLookups[0]
	profileLookups = []string{srvURL, srvURL}
	if _, err := lookupProfile(t.Context(), "SomeoneElse"); err == nil {
		t.Fatal("a reply about Notch was taken for SomeoneElse")
	}
	// The one asked about is still found.
	if p, err := lookupProfile(t.Context(), "notch"); err != nil || p.Name != "Notch" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestProfileCacheIsBounded(t *testing.T) {
	profileCache.Lock()
	profileCache.m = map[string]cachedProfile{}
	profileCache.Unlock()
	for i := 0; i < profileCacheMax*3; i++ {
		cacheProfile(strings.Repeat("a", 1)+string(rune('a'+i%26))+time.Duration(i).String(), cachedProfile{until: time.Now().Add(time.Hour)})
	}
	profileCache.Lock()
	n := len(profileCache.m)
	profileCache.Unlock()
	if n > profileCacheMax {
		t.Fatalf("%d entries", n)
	}
}

func TestPlayitLogCleanerHidesQuotedSecrets(t *testing.T) {
	for _, in := range []string{
		`{"secret_key":"abcDEF123456abcDEF123456"}`,
		`Agent-Key: shortsecretvalue`,
		`secret_key = "shortsecretvalue"`,
		`Agent-Key shortsecretvalue`,
	} {
		out := cleanAgentLine(in)
		if strings.Contains(out, "shortsecretvalue") || strings.Contains(out, "abcDEF123456") || !strings.Contains(out, "(hidden)") {
			t.Errorf("%q came out as %q", in, out)
		}
	}
	if got := cleanAgentLine("the secret key was not found"); strings.Contains(got, "hidden") {
		t.Errorf("an ordinary line was changed: %q", got)
	}
}

func TestTrashIsPruned(t *testing.T) {
	_, s := newFilesServer(t)
	defer func(a time.Duration, b int64) { trashMaxAge, trashMaxBytes = a, b }(trashMaxAge, trashMaxBytes)
	put(t, s, "old.txt", "old")
	if _, err := s.FileDelete([]string{"old.txt"}); err != nil {
		t.Fatal(err)
	}
	// Make it look 40 days old.
	tr, _ := s.Trash()
	metaPath := filepath.Join(s.trashDir(), tr.Items[0].ID, "meta.json")
	var m trashMeta
	readJSON(metaPath, &m)
	m.Deleted = time.Now().Add(-40 * 24 * time.Hour).Unix()
	writeJSON(metaPath, m, 0o644)
	put(t, s, "new.txt", "new")
	if _, err := s.FileDelete([]string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	tr, _ = s.Trash()
	if len(tr.Items) != 1 || tr.Items[0].Name != "new.txt" {
		t.Fatalf("%+v", tr.Items)
	}
	// Over the size: the oldest go first, the newest stays.
	trashMaxBytes = 10
	put(t, s, "a.txt", strings.Repeat("a", 8))
	s.FileDelete([]string{"a.txt"})
	put(t, s, "b.txt", strings.Repeat("b", 8))
	s.FileDelete([]string{"b.txt"})
	tr, _ = s.Trash()
	if len(tr.Items) != 1 || tr.Items[0].Name != "b.txt" {
		t.Fatalf("%+v", tr.Items)
	}
}

func TestLeftoverTempFilesAreSwept(t *testing.T) {
	_, s := newFilesServer(t)
	old := time.Now().Add(-3 * time.Hour)
	stale := put(t, s, "plugins/.minenest-up-123", "x")
	fresh := put(t, s, "plugins/.minenest-up-456", "x")
	staleDir := put(t, s, ".minenest-unzip-9/a.txt", "x")
	os.Chtimes(stale, old, old)
	os.Chtimes(filepath.Dir(staleDir), old, old)
	s.sweepTemp()
	if exists(stale) || exists(filepath.Dir(staleDir)) {
		t.Error("an old leftover stayed")
	}
	if !exists(fresh) {
		t.Error("something still being written was removed")
	}
	put(t, s, "world/.minenest-up-1", "x")
	p := filepath.Join(s.Root(), "world", ".minenest-up-1")
	os.Chtimes(p, old, old)
	if _, err := s.FileList("world"); err != nil {
		t.Fatal(err)
	}
	if exists(p) {
		t.Error("listing a folder left an old leftover")
	}
}

func TestDownloadCeilingIsForTheWebsiteOnly(t *testing.T) {
	h, s := newAPIHarness(t)
	put(t, s, "big.bin", strings.Repeat("x", 4096))
	defer remoteSendLimitForTest(1 << 10)()
	get := func(door string) int {
		r := httptest.NewRequest("GET", "/api/servers/srv/files/download?path=big.bin", nil)
		r.Host = "192.168.1.5:47576"
		r.RemoteAddr = "192.168.1.5:5555"
		w := httptest.NewRecorder()
		h.m.forward(w, r, true, door)
		return w.Code
	}
	if c := get(doorWeb); c != 413 {
		t.Errorf("website: %d", c)
	}
	if c := get(doorLAN); c != 200 {
		t.Errorf("home network: %d", c)
	}
	// An upload through the website that is over the ceiling is refused at once.
	r := httptest.NewRequest("POST", "/api/servers/srv/files/upload?dir=&name=b.bin", bytes.NewReader(make([]byte, 4096)))
	r.Host = "192.168.1.5:47576"
	w := httptest.NewRecorder()
	on := true
	h.m.SetRemote(nil, nil, nil, &on)
	h.m.forward(w, r, true, doorWeb)
	if w.Code != 413 {
		t.Errorf("a big upload through the website: %d", w.Code)
	}
}

func TestStopWaitsForABackupAndStartCantSlipIn(t *testing.T) {
	_, s := newFilesServer(t)
	setRunning(s)
	s.mu.Lock()
	s.busy = "a backup"
	s.mu.Unlock()
	if err := s.Stop(); err == nil {
		t.Fatal("Stop went ahead in the middle of a backup")
	}
	if s.State() != Running {
		t.Fatal("the state changed")
	}
	s.mu.Lock()
	s.busy = ""
	s.state = Stopped
	s.mu.Unlock()
	// Start holds the server's files while it works, and lets go when it is done or fails.
	if err := s.Start(); err == nil {
		t.Fatal("started without Java")
	}
	if s.busyNow() != "" {
		t.Fatalf("still busy with %q", s.busyNow())
	}
	// A backup of a server that is starting or stopping is refused.
	m := s.m
	s.mu.Lock()
	s.state = Starting
	s.mu.Unlock()
	if _, err := m.Backup(s, ""); err == nil {
		t.Fatal("backed up a server that is starting")
	}
	if s.busyNow() != "" {
		t.Fatal("left busy")
	}
	if _, err := m.autoBackup(s); err == nil || s.busyNow() != "" {
		t.Fatal("an automatic backup of a server that is starting")
	}
}

func TestStopAndWaitLetsABackupFinishFirst(t *testing.T) {
	_, s := newFilesServer(t)
	setRunning(s)
	s.mu.Lock()
	s.busy = "an automatic backup"
	s.exited = make(chan struct{})
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- s.StopAndWait(t.Context()) }()
	time.Sleep(400 * time.Millisecond)
	if s.State() != Running {
		t.Fatal("stopped during the backup")
	}
	s.setBusy("", false)
	// With the backup over, the stop goes ahead (the fake server has no process; it just asks).
	for i := 0; i < 50 && s.State() == Running; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if s.State() != Stopping {
		t.Fatalf("state %s", s.State())
	}
	close(s.exited)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestShutdownStopsBackupsAndTheScheduler(t *testing.T) {
	m, _ := newFilesServer(t)
	j := m.Jobs.start("Backing up X", "srv", func(ctx context.Context, j *Job) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	m.Shutdown()
	waitJob(t, j)
	if !m.auto.stopped() {
		t.Fatal("the scheduler wasn't told to stop")
	}
}

func TestLettingGoOfAKeptBackupDeletesNothing(t *testing.T) {
	_, s := newFilesServer(t)
	cfg := s.Config()
	cfg.AutoBackup = AutoBackup{Every: "hours", Hours: 1, Keep: 1}
	s.update(func(c *Config) { c.AutoBackup = cfg.AutoBackup })
	os.MkdirAll(s.backupDir(), 0o755)
	for i, n := range []string{"2026-01-01_00-00-00.zip", "2026-01-02_00-00-00.zip", "2026-01-03_00-00-00.zip"} {
		p := filepath.Join(s.backupDir(), n)
		os.WriteFile(p, []byte("z"), 0o644)
		os.WriteFile(p+".txt", []byte(autoWhy+"\n"), 0o644)
		ts := time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC)
		os.Chtimes(p, ts, ts)
	}
	if err := s.KeepBackup("2026-01-01_00-00-00.zip", true); err != nil {
		t.Fatal(err)
	}
	if err := s.KeepBackup("2026-01-01_00-00-00.zip", false); err != nil {
		t.Fatal(err)
	}
	if len(s.Backups()) != 3 {
		t.Fatalf("%d backups left", len(s.Backups()))
	}
}

func TestBackupsAreOrderedByWhenTheyWereMade(t *testing.T) {
	_, s := newFilesServer(t)
	os.MkdirAll(s.backupDir(), 0o755)
	// When the clocks go back, the name of the later one sorts first.
	for _, c := range []struct {
		name string
		at   time.Time
	}{
		{"2026-11-01_01-50-00.zip", time.Date(2026, 11, 1, 5, 50, 0, 0, time.UTC)},
		{"2026-11-01_01-10-00.zip", time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC)},
	} {
		p := filepath.Join(s.backupDir(), c.name)
		os.WriteFile(p, []byte("z"), 0o644)
		os.Chtimes(p, c.at, c.at)
	}
	bs := s.Backups()
	if bs[0].Name != "2026-11-01_01-10-00.zip" {
		t.Fatalf("newest first: %+v", bs)
	}
}

func TestADailyBackupKeepsItsTime(t *testing.T) {
	a := AutoBackup{Every: "day", At: "03:30"}
	loc := time.UTC
	last := time.Date(2026, 11, 20, 3, 30, 0, 0, loc)
	// A hand backup at 03:00 the next day doesn't pull the schedule to 03:30 the same day.
	hand := time.Date(2026, 11, 21, 3, 0, 0, 0, loc)
	st := autoState{Last: last.Unix()}
	next := autoNext(a, st, hand, loc)
	if !next.Equal(time.Date(2026, 11, 21, 3, 30, 0, 0, loc)) {
		t.Fatalf("%v", next)
	}
	// A hand backup at 04:00 doesn't push it to the day after either.
	hand = time.Date(2026, 11, 21, 4, 0, 0, 0, loc)
	if next := autoNext(a, st, hand, loc); !next.Equal(time.Date(2026, 11, 21, 3, 30, 0, 0, loc)) {
		t.Fatalf("%v", next)
	}
	// Hours still count from any backup.
	h := AutoBackup{Every: "hours", Hours: 6}
	if next := autoNext(h, autoState{}, hand, loc); !next.Equal(hand.Add(6 * time.Hour)) {
		t.Fatalf("%v", next)
	}
}

func TestAChatLineIsNotTheServersAnswer(t *testing.T) {
	fakeMojang(t, 0, map[string]string{"TippyManticore8": tippyID})
	s, _ := fakeConsole(t, func(cmd string, n int) []string { return nil })
	go func() {
		time.Sleep(150 * time.Millisecond)
		s.mu.Lock()
		s.addLineLocked("<Steve> Unknown command", "chat")
		s.addLineLocked("<Steve> Added TippyManticore8 to the whitelist", "chat")
		s.mu.Unlock()
	}()
	said, err := s.PlayerAction(t.Context(), "allow", "TippyManticore8", "")
	if err != nil {
		t.Fatalf("a chat line ended it: %v", err)
	}
	if !strings.HasPrefix(said, "Sent to the server") {
		t.Fatalf("a chat line counted as the answer: %q", said)
	}
}

func TestAPlayerActionStopsWaitingInTime(t *testing.T) {
	fakeMojang(t, 0, map[string]string{"TippyManticore8": tippyID})
	s, _ := fakeConsole(t, func(cmd string, n int) []string { return lookupFailed })
	old := actionWait
	actionWait = 100 * time.Millisecond
	defer func() { actionWait = old }()
	_, err := s.PlayerAction(t.Context(), "allow", "TippyManticore8", "")
	if err == nil || strings.Contains(strings.ToLower(err.Error()), "context") {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(err.Error(), "stopped waiting") {
		t.Fatalf("%v", err)
	}
}

func TestPlayitTryAgainLooksForANewerAgent(t *testing.T) {
	m, _ := newFilesServer(t)
	m.playit.restart()
	m.playit.mu.Lock()
	ok := m.playit.newerCheck
	m.playit.mu.Unlock()
	if !ok {
		t.Fatal("Try again doesn't ask for a new look")
	}
}
