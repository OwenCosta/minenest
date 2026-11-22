package nest

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testZone = time.FixedZone("Here", 2*3600)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, testZone)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextAuto(t *testing.T) {
	every6 := AutoBackup{Every: "hours", Hours: 6}
	daily := AutoBackup{Every: "day", At: "03:30"}
	for _, c := range []struct {
		a    AutoBackup
		base string
		want string
	}{
		{every6, "2026-11-22 10:00", "2026-11-22 16:00"},
		{every6, "2026-11-22 21:15", "2026-11-23 03:15"},
		{daily, "2026-11-22 01:00", "2026-11-22 03:30"},
		{daily, "2026-11-22 03:30", "2026-11-23 03:30"},
		{daily, "2026-11-22 14:00", "2026-11-23 03:30"},
		{daily, "2026-12-31 23:59", "2027-01-01 03:30"},
	} {
		if got := nextAuto(c.a, at(c.base)); !got.Equal(at(c.want)) {
			t.Errorf("%+v from %s: %s, want %s", c.a, c.base, got.Format("2006-01-02 15:04"), c.want)
		}
	}
	if got := nextAuto(AutoBackup{}, at("2026-11-22 10:00")); !got.IsZero() {
		t.Errorf("off gave %s", got)
	}
}

func TestAutoNextCountsFromTheLastBackup(t *testing.T) {
	a := AutoBackup{Every: "hours", Hours: 6, Since: at("2026-11-20 08:00").Unix()}
	// Nothing yet: from when the schedule was set.
	if got := autoNext(a, autoState{}, time.Time{}, testZone); !got.Equal(at("2026-11-20 14:00")) {
		t.Errorf("first: %s", got)
	}
	// A backup made by hand at 12:00 counts too.
	if got := autoNext(a, autoState{}, at("2026-11-20 12:00"), testZone); !got.Equal(at("2026-11-20 18:00")) {
		t.Errorf("after one by hand: %s", got)
	}
	// One skipped at 20:00 moves it on as well.
	st := autoState{Last: at("2026-11-20 20:00").Unix(), Result: "skipped"}
	if got := autoNext(a, st, at("2026-11-20 12:00"), testZone); !got.Equal(at("2026-11-21 02:00")) {
		t.Errorf("after a skip: %s", got)
	}
	// One that failed is tried again within an hour, even with a daily schedule.
	d := AutoBackup{Every: "day", At: "03:00"}
	st = autoState{Last: at("2026-11-21 03:00").Unix(), Result: "failed"}
	if got := autoNext(d, st, at("2026-11-20 03:00"), testZone); !got.Equal(at("2026-11-21 04:00")) {
		t.Errorf("after a failure: %s", got)
	}
	// Off is never.
	if got := autoNext(AutoBackup{}, st, time.Time{}, testZone); !got.IsZero() {
		t.Errorf("off: %s", got)
	}
}

func TestAutoRunAtAfterARestart(t *testing.T) {
	started := at("2026-11-22 09:00")
	jitter := 37 * time.Second
	// Missed while MineNest was off: soon after it starts, not at once.
	if got := autoRunAt(at("2026-11-22 03:30"), started, jitter); !got.Equal(started.Add(autoStartGrace + jitter)) {
		t.Errorf("missed: %s", got)
	}
	// Due later: at its time, plus this server's offset.
	if got := autoRunAt(at("2026-11-22 15:00"), started, jitter); !got.Equal(at("2026-11-22 15:00").Add(jitter)) {
		t.Errorf("later: %s", got)
	}
	if got := autoRunAt(time.Time{}, started, jitter); !got.IsZero() {
		t.Errorf("off: %s", got)
	}
	// Each server gets an offset of its own, kept while MineNest runs, under the limit.
	sc := newAutoSched()
	a, b := sc.offset("a"), sc.offset("b")
	if a != sc.offset("a") || a < 0 || a >= autoMaxJitter || b < 0 || b >= autoMaxJitter {
		t.Errorf("offsets %s %s", a, b)
	}
}

func TestCheckAuto(t *testing.T) {
	for _, ok := range []AutoBackup{{}, {Every: "off"}, {Every: "hours", Hours: 1}, {Every: "hours", Hours: 168, Keep: 3}, {Every: "day", At: "3:05"}, {Every: "daily", At: "23:59"}} {
		if _, err := checkAuto(ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []AutoBackup{{Every: "hours"}, {Every: "hours", Hours: 200}, {Every: "day", At: "25:00"}, {Every: "day"}, {Every: "weekly"}, {Every: "hours", Hours: 2, Keep: -1}} {
		if _, err := checkAuto(bad); err == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
	if a, _ := checkAuto(AutoBackup{Every: "day", At: "3:05", Hours: 4}); a.At != "03:05" || a.Hours != 0 || a.Every != "day" {
		t.Errorf("%+v", a)
	}
	if (AutoBackup{}).keep() != defaultKeepAuto || (AutoBackup{Keep: 4}).keep() != 4 {
		t.Error("keep")
	}
}

func TestPruneList(t *testing.T) {
	bs := []Backup{ // newest first, as Backups gives them
		{Name: "9", Auto: true, Why: autoWhy},
		{Name: "8", Why: handWhy},
		{Name: "7", Auto: true, Why: autoWhy},
		{Name: "6", Why: "before changing to Paper 1.21.4"},
		{Name: "5", Auto: true, Why: autoWhy, Keep: true},
		{Name: "4", Auto: true, Why: autoWhy},
		{Name: "3", Why: "The world before a new one was made"},
		{Name: "2", Auto: true, Why: autoWhy},
		{Name: "1", Why: handWhy},
	}
	got := strings.Join(pruneList(bs, 2, 1), " ")
	if got != "4 3 2" {
		t.Errorf("deleted %q", got)
	}
	if got := pruneList(bs, 10, 20); len(got) != 0 {
		t.Errorf("deleted %q", got)
	}
}

func TestDiskTooFull(t *testing.T) {
	if !diskTooFull(500<<20, 10<<20) || diskTooFull(5<<30, 1<<30) || diskTooFull(-1, 1<<30) {
		t.Error("diskTooFull")
	}
}

// worldServer is a stopped server with a small world.
func worldServer(t *testing.T) *Server {
	s := &Server{ID: "test", Dir: t.TempDir(), state: Stopped, players: map[string]time.Time{}}
	writeWorld(t, s, "region/r.0.0.mca", "chunks")
	writeWorld(t, s, "level.dat", "level")
	return s
}

func writeWorld(t *testing.T, s *Server, name, text string) {
	p := filepath.Join(s.Root(), "world", filepath.FromSlash(name))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func plenty(string) (int64, error) { return 100 << 30, nil }

func TestAutoBackupSkipsWhenNothingChanged(t *testing.T) {
	s := worldServer(t)
	ctx := t.Context()
	b, err := s.autoBackupLocked(ctx, nil, plenty)
	if err != nil || b == nil || !b.Auto {
		t.Fatalf("first: %+v %v", b, err)
	}
	// Nobody joined and the files are the same: skipped, and written down.
	if b, err := s.autoBackupLocked(ctx, nil, plenty); err != nil || b != nil {
		t.Fatalf("second: %+v %v", b, err)
	}
	if st := s.autoState(); st.Result != "skipped" || !strings.Contains(st.Note, "nothing had changed") {
		t.Fatalf("%+v", st)
	}
	// level.dat alone (the clock) doesn't count.
	writeWorld(t, s, "level.dat", "level, later")
	if b, _ := s.autoBackupLocked(ctx, nil, plenty); b != nil {
		t.Fatal("made one for level.dat alone")
	}
	// Someone joined: made, even though the files look the same.
	s.mu.Lock()
	s.lastJoin = time.Now().Add(time.Second)
	s.mu.Unlock()
	if b, err := s.autoBackupLocked(ctx, nil, plenty); err != nil || b == nil {
		t.Fatalf("after a join: %+v %v", b, err)
	}
	// The world changed: made.
	s.mu.Lock()
	s.lastJoin = time.Time{}
	s.mu.Unlock()
	writeWorld(t, s, "region/r.0.1.mca", "more chunks")
	if b, err := s.autoBackupLocked(ctx, nil, plenty); err != nil || b == nil {
		t.Fatalf("after a change: %+v %v", b, err)
	}
	if n := len(s.Backups()); n != 3 {
		t.Fatalf("%d backups", n)
	}
	if !strings.Contains(s.Lines(0)[len(s.Lines(0))-1].Text, "Backed up the world (Automatic backup") {
		t.Fatalf("%+v", s.Lines(0))
	}
}

func TestAutoBackupSkipsWhenTheDiskIsNearlyFull(t *testing.T) {
	s := worldServer(t)
	full := func(string) (int64, error) { return 300 << 20, nil }
	if b, err := s.autoBackupLocked(t.Context(), nil, full); err != nil || b != nil {
		t.Fatalf("%+v %v", b, err)
	}
	if st := s.autoState(); st.Result != "skipped" || !strings.Contains(st.Note, "nearly full (300 MB free)") {
		t.Fatalf("%+v", st)
	}
	if len(s.Backups()) != 0 {
		t.Fatal("it made one")
	}
}

func TestAutoBackupKeepsTheNewest(t *testing.T) {
	s := worldServer(t)
	s.cfg.AutoBackup = AutoBackup{Every: "hours", Hours: 1, Keep: 2}
	for i := 0; i < 4; i++ {
		writeWorld(t, s, "region/r.0.0.mca", strings.Repeat("x", i+1))
		if _, err := s.autoBackupLocked(t.Context(), nil, plenty); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if _, err := s.backupLocked(t.Context(), "", nil); err != nil { // one by hand
				t.Fatal(err)
			}
		}
	}
	var auto, hand int
	for _, b := range s.Backups() {
		if b.Auto {
			auto++
		} else if b.Why == handWhy {
			hand++
		}
	}
	if auto != 2 || hand != 1 {
		t.Fatalf("%d automatic, %d by hand: %+v", auto, hand, s.Backups())
	}
	// Marked to keep: it stays when newer ones come.
	oldest := ""
	for _, b := range s.Backups() {
		if b.Auto {
			oldest = b.Name
		}
	}
	if err := s.KeepBackup(oldest, true); err != nil {
		t.Fatal(err)
	}
	writeWorld(t, s, "region/r.0.0.mca", "changed again")
	s.autoBackupLocked(t.Context(), nil, plenty)
	found := false
	for _, b := range s.Backups() {
		if b.Name == oldest {
			found = b.Keep && b.Auto
		}
	}
	if !found {
		t.Fatalf("the kept one went: %+v", s.Backups())
	}
	if err := s.KeepBackup("nope.zip", true); err == nil {
		t.Fatal("kept a backup that isn't there")
	}
}

// savingServer fakes a running server for backups: it answers save-all and save-on the way
// Minecraft does, unless told not to.
func savingServer(t *testing.T, saves, savesOn bool) (*Server, *[]string) {
	var srv *Server
	s, sent := fakeConsole(t, func(cmd string, n int) []string {
		switch {
		case cmd == "save-off":
			return []string{"[12:00:00 INFO]: Automatic saving is now disabled"}
		case strings.HasPrefix(cmd, "save-all") && saves:
			// What read() does when the server says it saved (the fake holds s.mu here).
			if srv.saved != nil {
				close(srv.saved)
				srv.saved = nil
			}
			return []string{"[12:00:01 INFO]: Saved the game"}
		case cmd == "save-on" && savesOn:
			return []string{"[12:00:02 INFO]: Automatic saving is now enabled"}
		}
		return nil
	})
	srv = s
	// A real server's stdin is buffered by the system: commands sent one after another don't
	// wait for the server to read each (Command holds s.mu while it writes).
	s.stdin = newQueuedWriter(s.stdin)
	writeWorld(t, s, "region/r.0.0.mca", "chunks")
	old := saveWait
	saveWait = 300 * time.Millisecond
	t.Cleanup(func() { saveWait = old })
	return s, sent
}

type queuedWriter struct {
	ch chan []byte
	w  io.WriteCloser
}

func newQueuedWriter(w io.WriteCloser) *queuedWriter {
	q := &queuedWriter{ch: make(chan []byte, 64), w: w}
	go func() {
		for b := range q.ch {
			w.Write(b)
		}
	}()
	return q
}

func (q *queuedWriter) Write(b []byte) (int, error) {
	q.ch <- append([]byte(nil), b...)
	return len(b), nil
}

func (q *queuedWriter) Close() error { close(q.ch); return q.w.Close() }

func count(v []string, want string) int {
	n := 0
	for _, x := range v {
		if x == want {
			n++
		}
	}
	return n
}

func TestBackupWhileRunningTurnsSavingBackOn(t *testing.T) {
	s, sent := savingServer(t, true, true)
	b, err := s.autoBackupLocked(t.Context(), nil, plenty)
	if err != nil || b == nil {
		t.Fatal(b, err)
	}
	if strings.Join(*sent, ",") != "save-off,save-all flush,save-on" {
		t.Fatalf("sent %q", *sent)
	}
	if exists(s.savingOffPath()) {
		t.Fatal("the saving-off mark was left")
	}
}

func TestSavingIsTurnedBackOnWhenTheBackupFails(t *testing.T) {
	// The server never says it saved: the backup fails, and saving is still turned on again.
	s, sent := savingServer(t, false, true)
	if _, err := s.autoBackupLocked(t.Context(), nil, plenty); err == nil {
		t.Fatal("no error")
	}
	if count(*sent, "save-on") != 1 || exists(s.savingOffPath()) {
		t.Fatalf("sent %q", *sent)
	}
	if st := s.autoState(); st.Result != "failed" {
		t.Fatalf("%+v", st)
	}
	if !strings.Contains(strings.Join(lineTexts(s), "\n"), "The automatic backup didn't work") {
		t.Fatal(lineTexts(s))
	}
	if parts, _ := filepath.Glob(filepath.Join(s.backupDir(), "*.part")); len(parts) != 0 {
		t.Fatal(parts)
	}
}

func TestSavingIsTurnedBackOnWhenCancelled(t *testing.T) {
	s, sent := savingServer(t, true, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.backupLocked(ctx, autoWhy, nil); err == nil {
		t.Fatal("no error")
	}
	if count(*sent, "save-on") != 1 || exists(s.savingOffPath()) {
		t.Fatalf("sent %q", *sent)
	}
}

func TestSaveOnIsAskedAgainUntilTheServerAnswers(t *testing.T) {
	s, sent := fakeConsole(t, func(cmd string, n int) []string {
		if cmd == "save-on" && n >= 2 {
			return []string{"[12:00:02 INFO]: Automatic saving is now enabled"}
		}
		return nil
	})
	replyWait = 200 * time.Millisecond
	s.resumeSaving()
	if count(*sent, "save-on") != 2 {
		t.Fatalf("sent %q", *sent)
	}
	// Never an answer: three tries, then a note.
	s2, sent2 := fakeConsole(t, func(string, int) []string { return nil })
	replyWait = 100 * time.Millisecond
	s2.resumeSaving()
	if count(*sent2, "save-on") != 3 || !strings.Contains(strings.Join(lineTexts(s2), "\n"), "didn't hear back") {
		t.Fatalf("sent %q, %q", *sent2, lineTexts(s2))
	}
	// Stopped meanwhile: nothing to send, the next start saves as usual.
	s2.mu.Lock()
	s2.state = Stopped
	s2.mu.Unlock()
	writeFile(s2.savingOffPath(), []byte("x"), 0o644)
	s2.resumeSaving()
	if count(*sent2, "save-on") != 3 || exists(s2.savingOffPath()) {
		t.Fatalf("sent %q", *sent2)
	}
}

func lineTexts(s *Server) []string {
	var out []string
	for _, l := range s.Lines(0) {
		out = append(out, l.Text)
	}
	return out
}

func TestMineNestTidiesUpAnUnfinishedBackupWhenItStarts(t *testing.T) {
	t.Setenv("MINENEST_HOME", t.TempDir())
	dir := homePath("servers", "survival")
	os.MkdirAll(filepath.Join(dir, "backups"), 0o755)
	writeJSON(filepath.Join(dir, "nest.json"), Config{Name: "Survival"}, 0o644)
	os.WriteFile(filepath.Join(dir, "saving-off"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "backups", "2026-11-22_03-30-00.zip.part"), []byte("half"), 0o644)
	os.WriteFile(filepath.Join(dir, "backups", "2026-11-21_03-30-00.zip"), []byte("whole"), 0o644)
	m, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Get("survival")
	if err != nil {
		t.Fatal(err)
	}
	if exists(s.savingOffPath()) || exists(filepath.Join(dir, "backups", "2026-11-22_03-30-00.zip.part")) {
		t.Fatal("not tidied")
	}
	if len(s.Backups()) != 1 {
		t.Fatal("the whole backup went")
	}
	if !strings.Contains(strings.Join(lineTexts(s), "\n"), "MineNest stopped during a backup") {
		t.Fatal(lineTexts(s))
	}
}

func TestSetAutoBackup(t *testing.T) {
	t.Setenv("MINENEST_HOME", t.TempDir())
	dir := homePath("servers", "survival")
	os.MkdirAll(dir, 0o755)
	writeJSON(filepath.Join(dir, "nest.json"), Config{Name: "Survival"}, 0o644)
	m, _ := NewManager()
	s, _ := m.Get("survival")
	st, err := m.SetAutoBackup(s, AutoBackup{Every: "hours", Hours: 6})
	if err != nil || st.Keep != defaultKeepAuto || st.Next == 0 {
		t.Fatalf("%+v %v", st, err)
	}
	since := s.Config().AutoBackup.Since
	// Only how many to keep: the next one doesn't move.
	s.update(func(c *Config) { c.AutoBackup.Since = since - 100 })
	if st, _ := m.SetAutoBackup(s, AutoBackup{Every: "hours", Hours: 6, Keep: 3}); st.Since != since-100 || st.Keep != 3 {
		t.Fatalf("%+v", st)
	}
	if _, err := m.SetAutoBackup(s, AutoBackup{Every: "hours", Hours: 0}); err == nil {
		t.Fatal("took 0 hours")
	}
	// It's kept in nest.json, so it's there after MineNest starts again.
	m2, _ := NewManager()
	s2, _ := m2.Get("survival")
	if a := s2.Config().AutoBackup; a.Every != "hours" || a.Hours != 6 || a.Keep != 3 {
		t.Fatalf("%+v", a)
	}
	if st, _ := m.SetAutoBackup(s, AutoBackup{}); st.Next != 0 || st.Every != "" {
		t.Fatalf("%+v", st)
	}
}

func TestAutoTickNeverRunsTwoAtOnce(t *testing.T) {
	t.Setenv("MINENEST_HOME", t.TempDir())
	dir := homePath("servers", "survival")
	os.MkdirAll(dir, 0o755)
	writeJSON(filepath.Join(dir, "nest.json"), Config{Name: "Survival", AutoBackup: AutoBackup{Every: "hours", Hours: 1, Since: 1}}, 0o644)
	m, _ := NewManager()
	s, _ := m.Get("survival")
	later := time.Now().Add(time.Hour)
	// Not due yet (MineNest has only just started): nothing.
	m.autoTick(time.Now())
	// One is already running for this server: no second one.
	m.auto.running[s.ID] = true
	m.autoTick(later)
	delete(m.auto.running, s.ID)
	// Busy with something else: it waits for the next tick.
	s.setBusy("the install", false)
	m.autoTick(later)
	if _, err := m.autoBackup(s); err == nil {
		t.Fatal("started while busy")
	}
	s.setBusy("", false)
	if s.autoState().Result != "" {
		t.Fatalf("ran when it shouldn't: %+v", s.autoState())
	}
	// Due and free: it runs (and, with no world yet, says so).
	m.autoTick(later)
	deadline := time.Now().Add(5 * time.Second)
	for (m.auto.isRunning(s.ID) || s.autoState().Result == "") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := s.autoState(); st.Result != "skipped" || st.Note != "there's no world yet" {
		t.Fatalf("%+v", st)
	}
	if s.busy != "" {
		t.Fatal("still busy")
	}
}
