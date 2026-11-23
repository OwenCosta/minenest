package nest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestFootprintAndBack(t *testing.T) {
	for _, ram := range []int{1024, 2048, 3072, 4096, 6144, 8192, 16384} {
		if got := ramFor(javaFootprintMB(ram)); got != ram {
			t.Errorf("ramFor(javaFootprintMB(%d)) = %d", ram, got)
		}
		if got := ramFor(javaFootprintMB(ram) - 1); got >= ram {
			t.Errorf("a little less room than %d MB needs still gives %d", ram, got)
		}
	}
	if ramFor(200) != 0 || javaFootprintMB(0) != 0 || javaFootprintMB(4096) != 4864 {
		t.Fatal(ramFor(200), javaFootprintMB(4096))
	}
}

func TestOSReserve(t *testing.T) {
	for _, c := range []struct {
		total int
		goos  string
		want  int
	}{
		{16384, "windows", 2048}, {16384, "linux", 2048}, {8192, "linux", 1024}, {8192, "windows", 2048},
		{65536, "linux", 4096}, {4096, "windows", 2048}, {2048, "linux", 1024}, {32768, "windows", 4096},
	} {
		if got := osReserveMB(c.total, c.goos); got != c.want {
			t.Errorf("osReserveMB(%d, %s) = %d, want %d", c.total, c.goos, got, c.want)
		}
	}
}

func TestMBWordsAndCounts(t *testing.T) {
	for mb, want := range map[int]string{512: "512 MB", 1024: "1 GB", 5632: "5.5 GB", 4096: "4 GB", 0: "0 MB", -5: "0 MB"} {
		if got := mbWords(mb); got != want {
			t.Errorf("mbWords(%d) = %q, want %q", mb, got, want)
		}
	}
	if countWords(2) != "two" || countWords(12) != "12" {
		t.Fatal(countWords(2), countWords(12))
	}
}

func TestFitWords(t *testing.T) {
	for _, c := range []struct {
		room int
		want string
	}{
		{javaFootprintMB(4096), "That's enough for one more 4 GB server, or three 1 GB ones."},
		{2 * javaFootprintMB(4096), "That's enough for one more 8 GB server, or two 4 GB ones."},
		{javaFootprintMB(6144), "That's enough for one more 6 GB server, or two 2 GB ones."},
		{javaFootprintMB(2048) * 2, "That's enough for one more 4 GB server, or two 2 GB ones."},
		{javaFootprintMB(1024), "That's enough for one more 1 GB server."},
		{javaFootprintMB(1024) * 3, "That's enough for one more 3 GB server, or three 1 GB ones."},
		{javaFootprintMB(1024) - 1, ""},
		{0, ""},
	} {
		if got := fitWords(c.room); got != c.want {
			t.Errorf("fitWords(%d) = %q, want %q", c.room, got, c.want)
		}
	}
}

func TestPlanMemoryEmptyComputer(t *testing.T) {
	// 16 GB, 12 GB free, nothing running: the reserve is 2 GB, the margin on what's free 1 GB.
	sum := planMemory(sysMemory{TotalMB: 16384, AvailMB: 12288}, "windows", nil)
	if !sum.Known || sum.ReserveMB != 2048 || sum.UsedMB != 4096 || sum.RoomMB != 12288-1024 {
		t.Fatalf("%+v", sum)
	}
	if sum.GiveMB != 9728 || sum.Words != "You can still give about 9.5 GB to another server." {
		t.Fatal(sum.GiveMB, sum.Words)
	}
	if sum.Fits != "That's enough for one more 8 GB server, or two 4 GB ones." || sum.Short != "About 9.5 GB left for servers" {
		t.Fatal(sum.Fits, sum.Short)
	}
	if len(sum.Notes) != 0 || sum.Stopped != "" || len(sum.Servers) != 0 {
		t.Fatal(sum.Notes, sum.Stopped)
	}
}

func TestPlanMemoryWithServers(t *testing.T) {
	servers := []ServerMemory{
		{ID: "a", RAMMB: 4096, Running: true, UsedMB: 3000},
		{ID: "b", RAMMB: 2048, Running: false},
		{ID: "c", RAMMB: 8192, Running: false},
	}
	sum := planMemory(sysMemory{TotalMB: 16384, AvailMB: 9000}, "linux", servers)
	// From the total: 16384 - 2048 - 4864 = 9472. From what's free: 9000 - 1024 - (4864-3000) = 6112.
	if sum.RoomMB != 6112 || sum.RunningMB != 4096 || sum.StoppedMB != 10240 || sum.RealMB != 3000 || !sum.RealKnown {
		t.Fatalf("%+v", sum)
	}
	if sum.GiveMB != 5120 || sum.Words != "You can still give about 5 GB to another server." {
		t.Fatal(sum.GiveMB, sum.Words)
	}
	if sum.Stopped != "Your stopped servers are given 10 GB between them, more than is free, so they can't all run at once." {
		t.Fatal(sum.Stopped)
	}
	b, _ := sum.server("b")
	c, _ := sum.server("c")
	a, _ := sum.server("a")
	if !b.Fits || c.Fits || b.CanHaveMB != 5120 {
		t.Fatal(b, c)
	}
	// The running one could have its own memory back at its next start: 9000 + 3000 free, less the margin.
	if !a.Fits || a.CanHaveMB != ramFor(10976) {
		t.Fatal(a, ramFor(10976))
	}
	if w := FitWarning(c.RAMMB, c.CanHaveMB, true); !strings.Contains(w, "Only about 5 GB is free for it right now, less than the 8 GB it's given") {
		t.Fatal(w)
	}
	if FitWarning(2048, 5120, true) != "" || FitWarning(8192, 0, false) != "" {
		t.Fatal("a warning when it fits, or when nothing is known")
	}
}

func TestPlanMemoryNothingFits(t *testing.T) {
	servers := []ServerMemory{{ID: "a", RAMMB: 6144, Running: true, UsedMB: 6500}}
	sum := planMemory(sysMemory{TotalMB: 8192, AvailMB: 1200}, "windows", servers)
	if sum.GiveMB >= smallestServerMB || sum.Fits != "" || sum.Short != "No room for another server" {
		t.Fatalf("%+v", sum)
	}
	if sum.Words != "There isn't room for another server right now. Stop one, or give a running one less memory in its Settings." {
		t.Fatal(sum.Words)
	}
	sum = planMemory(sysMemory{TotalMB: 4096, AvailMB: 900}, "windows", nil)
	if sum.Words != "This computer doesn't have enough memory free for another server right now. Closing other programs helps." {
		t.Fatal(sum.Words)
	}
	if w := FitWarning(2048, 0, true); !strings.HasPrefix(w, "This computer doesn't have 2 GB free for it right now") {
		t.Fatal(w)
	}
}

func TestPlanMemoryHonestWhenUnread(t *testing.T) {
	sum := planMemory(sysMemory{}, "linux", []ServerMemory{{ID: "a", RAMMB: 2048}})
	if sum.Known || !strings.Contains(sum.Words, "couldn't read how much memory this computer has") || !sum.Servers[0].Fits || sum.Short != "" {
		t.Fatalf("%+v", sum)
	}
	// The total but not what's free, and a running server whose use can't be read.
	sum = planMemory(sysMemory{TotalMB: 16384}, "linux", []ServerMemory{{ID: "a", RAMMB: 4096, Running: true}})
	if !sum.Known || sum.UsedMB != 0 || sum.RoomMB != 16384-2048-4864 || len(sum.Notes) != 2 || sum.RealKnown {
		t.Fatalf("%+v", sum)
	}
	sum = planMemory(sysMemory{TotalMB: 4096, AvailMB: 3000, Limited: true}, "linux", nil)
	if len(sum.Notes) != 1 || !strings.Contains(sum.Notes[0], "memory limit of 4 GB") {
		t.Fatal(sum.Notes)
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail := parseMeminfo("MemTotal:       16303660 kB\nMemFree:          812344 kB\nMemAvailable:    9453624 kB\nBuffers:          201400 kB\nCached:          7000000 kB\n")
	if total != 15921 || avail != 9232 {
		t.Fatal(total, avail)
	}
	// Before Linux 3.14 there's no MemAvailable.
	total, avail = parseMeminfo("MemTotal: 2048000 kB\nMemFree: 102400 kB\nBuffers: 102400 kB\nCached: 204800 kB\n")
	if total != 2000 || avail != 400 {
		t.Fatal(total, avail)
	}
	if total, avail = parseMeminfo(""); total != 0 || avail != 0 {
		t.Fatal(total, avail)
	}
}

func TestCgroupLimit(t *testing.T) {
	if _, ok := parseCgroupLimit("max\n"); ok {
		t.Fatal("max is no limit")
	}
	if _, ok := parseCgroupLimit("9223372036854771712\n"); ok {
		t.Fatal("cgroup v1's no limit")
	}
	if n, ok := parseCgroupLimit("4294967296\n"); !ok || n != 4<<30 {
		t.Fatal(n, ok)
	}
	if statValue("anon 100\ninactive_file 2048\nactive_file 5\n", "inactive_file") != 2048 {
		t.Fatal("statValue")
	}
	// A 4 GB container on a 16 GB computer that uses 1 GB of it.
	m := withLimit(sysMemory{TotalMB: 16384, AvailMB: 10000}, 4<<30, 1<<30)
	if m.TotalMB != 4096 || m.AvailMB != 3072 || !m.Limited {
		t.Fatal(m)
	}
	// A limit above the computer's memory changes nothing.
	if m := withLimit(sysMemory{TotalMB: 8192, AvailMB: 4000}, 64<<30, 0); m.TotalMB != 8192 || m.Limited {
		t.Fatal(m)
	}
	// The computer has less free than the container would allow.
	if m := withLimit(sysMemory{TotalMB: 16384, AvailMB: 1000}, 4<<30, 1<<30); m.AvailMB != 1000 {
		t.Fatal(m)
	}
	if parseStatm("250000 131072 2000 1 0 90000 0\n", 4096) != 512 || parseStatm("", 4096) != 0 {
		t.Fatal("parseStatm")
	}
}

// The real numbers on the computer the tests run on: these must be readable on Windows and Linux.
func TestReadsThisComputer(t *testing.T) {
	m := readSysMemory()
	if m.TotalMB <= 0 || m.AvailMB <= 0 || m.AvailMB > m.TotalMB {
		t.Fatalf("%+v", m)
	}
	if got := processMemoryMB(os.Getpid()); got <= 0 || got > m.TotalMB {
		t.Fatalf("this test's own memory: %d MB", got)
	}
	if processMemoryMB(-1) != 0 {
		t.Fatal("a process that isn't there")
	}
}

func TestManagerMemory(t *testing.T) {
	t.Setenv("MINENEST_HOME", t.TempDir())
	m, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{ID: "x", Dir: t.TempDir(), m: m, state: Stopped, players: map[string]time.Time{}}
	s.cfg = Config{Name: "X", RAM: 1 << 30} // far more than any test computer has
	m.servers[s.ID] = s
	sum := m.Memory()
	if !sum.Known || len(sum.Servers) != 1 || sum.Servers[0].Fits || sum.StoppedMB != 1<<30 {
		t.Fatalf("%+v", sum)
	}
	st := m.Status(s)
	if st.MemFits || !strings.Contains(st.MemWarning, "free for it right now") {
		t.Fatal(st.MemFits, st.MemWarning)
	}
	// A change to its memory counts at once, not after the cache runs out.
	s.update(func(c *Config) { c.RAM = 512 })
	if sum := m.Memory(); sum.StoppedMB != 512 {
		t.Fatal(sum.StoppedMB)
	}
}

// A running server's process is read: the test binary stands in for Java (see TestMain).
func TestManagerMemoryReadsARunningServer(t *testing.T) {
	t.Setenv("MINENEST_HOME", t.TempDir())
	m, err := NewManager()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "MINENEST_FAKE_PLAYITD=connected")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	s := &Server{ID: "x", Dir: t.TempDir(), m: m, state: Running, cmd: cmd, players: map[string]time.Time{}}
	s.cfg = Config{Name: "X", RAM: 2048}
	m.servers[s.ID] = s
	var sum MemorySummary
	for i := 0; i < 50; i++ {
		if sum = m.Memory(); sum.RealMB > 0 {
			break
		}
		m.mem.mu.Lock()
		m.mem.at = time.Time{}
		m.mem.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	if !sum.RealKnown || sum.RealMB <= 0 || sum.RunningMB != 2048 || !sum.Servers[0].Running {
		t.Fatalf("%+v", sum)
	}
	if st := m.Status(s); st.MemUsedMB != sum.RealMB {
		t.Fatal(st.MemUsedMB, sum.RealMB)
	}
}
