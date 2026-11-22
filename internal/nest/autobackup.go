package nest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Automatic backups: a schedule per server (in its Config), run by the daemon. What happened last
// is kept in servers/<id>/autobackup.json, so the schedule carries on after MineNest restarts.

// AutoBackup is a server's schedule.
type AutoBackup struct {
	Every string `json:"every"`           // "" (off), "hours" or "day"
	Hours int    `json:"hours,omitempty"` // every this many hours
	At    string `json:"at,omitempty"`    // each day at this time ("03:30", this computer's clock)
	Keep  int    `json:"keep,omitempty"`  // how many automatic ones to keep (0: defaultKeepAuto)
	Since int64  `json:"since,omitempty"` // when the schedule was set, so the first one counts from then
}

const (
	defaultKeepAuto = 10
	autoWhy         = "Automatic backup"
	handWhy         = "Made by hand"
	autoRetry       = time.Hour        // after one that didn't work
	autoStartGrace  = time.Minute      // a missed one waits this long after MineNest starts
	autoMaxJitter   = 90 * time.Second // so several servers don't start in the same second
	diskReserve     = 1 << 30          // what has to stay free after a backup
)

// autoState is what happened the last time (autobackup.json).
type autoState struct {
	Last   int64  `json:"last"`             // the last try
	Result string `json:"result,omitempty"` // made, skipped or failed
	Note   string `json:"note,omitempty"`   // why it was skipped or what went wrong
	Made   int64  `json:"made,omitempty"`   // the last one that was made
	Print  string `json:"print,omitempty"`  // the world's files then (worldPrint)
}

// AutoStatus is the schedule and how it's going, for the World page and the command line.
type AutoStatus struct {
	AutoBackup
	Last    int64  `json:"last"`
	Result  string `json:"result"`
	Note    string `json:"note"`
	Next    int64  `json:"next"`
	Running bool   `json:"running"`
}

// EveryWords says how often, like "every 6 hours" or "every day at 03:30".
func (a AutoBackup) EveryWords() string {
	switch {
	case a.Every == "hours" && a.Hours == 1:
		return "every hour"
	case a.Every == "hours":
		return fmt.Sprintf("every %d hours", a.Hours)
	case a.Every == "day":
		return "every day at " + a.At
	}
	return "off"
}

func (a AutoBackup) keep() int {
	if a.Keep <= 0 {
		return defaultKeepAuto
	}
	return a.Keep
}

// checkAuto tidies a schedule someone asked for, or says what's wrong with it.
func checkAuto(a AutoBackup) (AutoBackup, error) {
	switch a.Every {
	case "", "off", "never":
		a.Every, a.Hours, a.At = "", 0, ""
	case "hours", "hour":
		a.Every, a.At = "hours", ""
		if a.Hours < 1 || a.Hours > 168 {
			return a, fmt.Errorf("pick a number of hours from 1 to 168")
		}
	case "day", "daily":
		a.Every, a.Hours = "day", 0
		t, err := time.Parse("15:04", strings.TrimSpace(a.At))
		if err != nil {
			return a, fmt.Errorf("say the time of day like 03:30")
		}
		a.At = t.Format("15:04")
	default:
		return a, fmt.Errorf("automatic backups are off, every few hours or once a day")
	}
	if a.Keep < 0 || a.Keep > 1000 {
		return a, fmt.Errorf("keep between 1 and 1000 automatic backups")
	}
	return a, nil
}

// nextAuto is when the schedule wants a backup after base (the zero time when it's off).
func nextAuto(a AutoBackup, base time.Time) time.Time {
	switch a.Every {
	case "hours":
		return base.Add(time.Duration(max(a.Hours, 1)) * time.Hour)
	case "day":
		at, err := time.Parse("15:04", a.At)
		if err != nil {
			return time.Time{}
		}
		y, mo, d := base.Date()
		t := time.Date(y, mo, d, at.Hour(), at.Minute(), 0, 0, base.Location())
		if !t.After(base) {
			t = time.Date(y, mo, d+1, at.Hour(), at.Minute(), 0, 0, base.Location())
		}
		return t
	}
	return time.Time{}
}

// autoNext counts from the last backup of any kind (or the last try, or when the schedule was
// set, whichever is newest). One that didn't work is tried again within an hour.
func autoNext(a AutoBackup, st autoState, newest time.Time, loc *time.Location) time.Time {
	if a.Every == "" {
		return time.Time{}
	}
	base := newest
	for _, u := range []int64{st.Last, a.Since} {
		if t := time.Unix(u, 0); u > 0 && t.After(base) {
			base = t
		}
	}
	if base.IsZero() {
		base = time.Unix(0, 0)
	}
	next := nextAuto(a, base.In(loc))
	if st.Result == "failed" && st.Last > 0 {
		if r := time.Unix(st.Last, 0).Add(autoRetry).In(loc); r.Before(next) {
			next = r
		}
	}
	return next
}

// autoRunAt is when the daemon really starts it: a missed one runs a minute after MineNest
// started, and each server has a small offset of its own.
func autoRunAt(next, started time.Time, jitter time.Duration) time.Time {
	if next.IsZero() {
		return next
	}
	if g := started.Add(autoStartGrace); next.Before(g) {
		next = g
	}
	return next.Add(jitter)
}

// pruneList names the backups to delete, newest first in bs: automatic ones past keepAuto, and
// the ones made before risky changes past keepOther. Ones made by hand or marked to keep stay.
func pruneList(bs []Backup, keepAuto, keepOther int) []string {
	var out []string
	nAuto, nOther := 0, 0
	for _, b := range bs {
		switch {
		case b.Keep || b.Why == handWhy:
		case b.Auto:
			if nAuto++; nAuto > keepAuto {
				out = append(out, b.Name)
			}
		default:
			if nOther++; nOther > keepOther {
				out = append(out, b.Name)
			}
		}
	}
	return out
}

// diskTooFull says whether a backup of about estimate bytes would leave the disk nearly full.
func diskTooFull(free, estimate int64) bool {
	return free >= 0 && free < estimate+diskReserve
}

// worldPrint sums up the world's files (names, sizes and times), so MineNest can tell when
// nothing has changed. level.dat is left out: a running server rewrites it with the clock alone.
func (s *Server) worldPrint() string {
	var lines []string
	for _, f := range s.worldFolders() {
		filepath.WalkDir(filepath.Join(s.Root(), f), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			switch d.Name() {
			case "session.lock", "level.dat", "level.dat_old":
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(s.Root(), p)
			lines = append(lines, fmt.Sprintf("%s\x00%d\x00%d", filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano()))
			return nil
		})
	}
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}

func (s *Server) worldSize() int64 {
	var total int64
	for _, f := range s.worldFolders() {
		filepath.WalkDir(filepath.Join(s.Root(), f), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, e := d.Info(); e == nil {
					total += info.Size()
				}
			}
			return nil
		})
	}
	return total
}

func (s *Server) autoStatePath() string { return filepath.Join(s.Dir, "autobackup.json") }

func (s *Server) autoState() autoState {
	var st autoState
	readJSON(s.autoStatePath(), &st)
	return st
}

// joinedSince says whether anyone is on now, or joined after t.
func (s *Server) joinedSince(t time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.players) > 0 || s.lastJoin.After(t)
}

// ------------------------------------------------------------------ keeping saving on

// While a running server's world is zipped, saving is off. A file beside the server says so until
// it's on again, so that if MineNest stops half way it can tidy up when it starts.
func (s *Server) savingOffPath() string { return filepath.Join(s.Dir, "saving-off") }

func (s *Server) pauseSaving() {
	writeFile(s.savingOffPath(), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
	s.Command("save-off")
}

// resumeSaving turns saving on again, and checks that the server says it is. It's tried three
// times; a server that has stopped meanwhile starts again with saving on by itself.
func (s *Server) resumeSaving() {
	defer os.Remove(s.savingOffPath())
	for try := 0; try < 3; try++ {
		if st := s.State(); st != Running && st != Starting {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), replyWait+time.Second)
		kind, _, err := s.commandReply(ctx, "save-on", "save-on")
		cancel()
		if err == nil && kind != replyNone {
			return
		}
	}
	s.Note("MineNest asked the server to save again after the backup but didn't hear back. If the world stops saving, restart the server.")
}

// recoverBackup tidies up after a backup MineNest didn't finish (it stopped half way): the
// unfinished zip goes, and the server, which stopped with MineNest, starts with saving on.
func (s *Server) recoverBackup() {
	parts, _ := filepath.Glob(filepath.Join(s.backupDir(), "*.zip.part"))
	for _, p := range parts {
		os.Remove(p)
	}
	if exists(s.savingOffPath()) {
		os.Remove(s.savingOffPath())
		s.Note("MineNest stopped during a backup last time. The unfinished backup was removed, and the server saves as usual when it starts.")
	}
}

// ------------------------------------------------------------------ the schedule

type autoSched struct {
	mu      sync.Mutex
	started time.Time
	jitter  map[string]time.Duration
	running map[string]bool
}

func newAutoSched() *autoSched {
	return &autoSched{started: time.Now(), jitter: map[string]time.Duration{}, running: map[string]bool{}}
}

func (a *autoSched) offset(id string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, ok := a.jitter[id]
	if !ok {
		j = time.Duration(rand.Int64N(int64(autoMaxJitter)))
		a.jitter[id] = j
	}
	return j
}

func (a *autoSched) isRunning(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running[id]
}

// newestBackup is when the newest backup of any kind was made.
func (s *Server) newestBackup() time.Time {
	var t time.Time
	for _, b := range s.Backups() {
		if bt := time.Unix(b.Time, 0); bt.After(t) {
			t = bt
		}
	}
	return t
}

func (m *Manager) autoNextFor(s *Server) time.Time {
	a := s.Config().AutoBackup
	next := autoNext(a, s.autoState(), s.newestBackup(), time.Local)
	return autoRunAt(next, m.auto.started, m.auto.offset(s.ID))
}

// AutoStatus is a server's schedule and what happened last.
func (m *Manager) AutoStatus(s *Server) AutoStatus {
	a := s.Config().AutoBackup
	a.Keep = a.keep()
	st := s.autoState()
	out := AutoStatus{AutoBackup: a, Last: st.Last, Result: st.Result, Note: st.Note, Running: m.auto.isRunning(s.ID)}
	if next := m.autoNextFor(s); !next.IsZero() {
		out.Next = next.Unix()
	}
	return out
}

// SetAutoBackup changes a server's schedule. Changing when counts from now; only how many to
// keep doesn't move the next one.
func (m *Manager) SetAutoBackup(s *Server, a AutoBackup) (AutoStatus, error) {
	a, err := checkAuto(a)
	if err != nil {
		return AutoStatus{}, err
	}
	old := s.Config().AutoBackup
	a.Since = old.Since
	if a.Every != old.Every || a.Hours != old.Hours || a.At != old.At {
		a.Since = time.Now().Unix()
	}
	if err := s.update(func(c *Config) { c.AutoBackup = a }); err != nil {
		return AutoStatus{}, err
	}
	switch a.Every {
	case "":
		if old.Every != "" {
			s.Note("Automatic backups are off.")
		}
	default:
		s.Note("Automatic backups %s, keeping the newest %d.", a.EveryWords(), a.keep())
	}
	s.pruneBackups()
	return m.AutoStatus(s), nil
}

// autoLoop is the daemon's scheduler.
func (m *Manager) autoLoop() {
	for {
		m.autoTick(time.Now())
		time.Sleep(20 * time.Second)
	}
}

func (m *Manager) autoTick(now time.Time) {
	for _, s := range m.List() {
		next := m.autoNextFor(s)
		if next.IsZero() || now.Before(next) || m.auto.isRunning(s.ID) {
			continue
		}
		if st := s.State(); st == Starting || st == Stopping {
			continue // when it's settled
		}
		m.autoBackup(s) // busy with something else: the next tick tries again
	}
}

// autoBackup starts an automatic backup in a job, unless the server's files are busy.
func (m *Manager) autoBackup(s *Server) (*Job, error) {
	if err := s.setBusy("an automatic backup", false); err != nil {
		return nil, err
	}
	m.auto.mu.Lock()
	m.auto.running[s.ID] = true
	m.auto.mu.Unlock()
	return m.Jobs.start("Automatic backup of "+s.Config().Name, s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer func() {
			m.auto.mu.Lock()
			delete(m.auto.running, s.ID)
			m.auto.mu.Unlock()
		}()
		defer s.setBusy("", false)
		return s.autoBackupLocked(ctx, j, diskFree)
	}), nil
}

// autoBackupLocked makes the backup, or skips it with a note saying why. Every run is written
// down, made, skipped or failed, so the next one counts from it.
func (s *Server) autoBackupLocked(ctx context.Context, j *Job, free func(string) (int64, error)) (*Backup, error) {
	st := s.autoState()
	record := func(result, note string) {
		st.Last, st.Result, st.Note = time.Now().Unix(), result, note
		writeJSON(s.autoStatePath(), st, 0o644)
	}
	if !s.WorldExists() {
		record("skipped", "there's no world yet")
		s.Note("Skipped the automatic backup: there's no world yet.")
		return nil, nil
	}
	fp := s.worldPrint()
	if st.Print != "" && fp == st.Print && !s.joinedSince(time.Unix(st.Made, 0)) {
		record("skipped", "nothing had changed since the last one")
		s.Note("Skipped the automatic backup: nobody has joined and the world hasn't changed since the last one.")
		return nil, nil
	}
	estimate := s.worldSize()
	if bs := s.Backups(); len(bs) > 0 {
		estimate = max(estimate/2, bs[0].Size)
	}
	os.MkdirAll(s.backupDir(), 0o755)
	if n, err := free(s.backupDir()); err == nil && diskTooFull(n, estimate) {
		note := fmt.Sprintf("the disk is nearly full (%s free)", sizeWords(n))
		record("skipped", note)
		s.Note("Skipped the automatic backup: the disk is nearly full (%s free). Free some space, or keep fewer backups.", sizeWords(n))
		return nil, nil
	}
	b, err := s.backupLocked(ctx, autoWhy, j)
	if err != nil {
		record("failed", err.Error())
		s.Note("The automatic backup didn't work: %v. MineNest tries again within an hour.", err)
		return nil, err
	}
	st.Made, st.Print = time.Now().Unix(), b.print
	record("made", "")
	return b, nil
}

// sizeWords says a size the way people do.
func sizeWords(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}
