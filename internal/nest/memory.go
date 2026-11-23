package nest

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// How much of the computer's memory is still free for servers, and what size of server fits.
//
// The numbers come from the system (readSysMemory in proc_windows.go and proc_unix.go: the
// computer's total and what's available right now, under a container's memory limit when there
// is one) and from each running server's Java process (processMemoryMB). A server given R MB
// takes about javaFootprintMB(R) once it's busy, so what's left is worked out in footprints and
// turned back into a memory setting at the end.

// sysMemory is what the system says, in MB; 0 means it couldn't be read.
type sysMemory struct {
	TotalMB, AvailMB int
	Limited          bool // a memory limit on MineNest (a cgroup) is below the computer's memory
}

// memoryCacheFor is how long the numbers are reused: the window asks every couple of seconds.
var memoryCacheFor = 3 * time.Second

var sysCache struct {
	sync.Mutex
	at  time.Time
	mem sysMemory
}

func cachedSysMemory() sysMemory {
	sysCache.Lock()
	defer sysCache.Unlock()
	if sysCache.at.IsZero() || time.Since(sysCache.at) > memoryCacheFor {
		sysCache.mem, sysCache.at = readSysMemory(), time.Now()
	}
	return sysCache.mem
}

// totalMemoryMB is the memory MineNest's servers can use in all (the computer's, or a lower
// container limit), or 0 when it can't be read.
func totalMemoryMB() int { return cachedSysMemory().TotalMB }

// javaFootprintMB is about what a server given ram MB (Java's -Xmx) takes from the computer once
// it's busy: the heap, plus Java's own share for its code, threads and buffers.
func javaFootprintMB(ram int) int {
	if ram <= 0 {
		return 0
	}
	return ram + ram/8 + 256
}

// ramFor is the reverse: the biggest memory setting, in steps of 512 MB, whose footprint fits in
// room MB.
func ramFor(room int) int {
	if room <= 256 {
		return 0
	}
	return (room - 256) * 8 / 9 / 512 * 512
}

// osReserveMB is what's left alone for the operating system and the programs people use beside
// their servers.
func osReserveMB(total int, goos string) int {
	low := 1024
	if goos == "windows" {
		low = 2048 // Windows itself and a browser want more than Linux does
	}
	r := min(max(total/8, low), 4096)
	return min(r, total/2)
}

// smallestServerMB is the least memory MineNest suggests for a server.
const smallestServerMB = 1024

// ServerMemory is one server's part.
type ServerMemory struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"`
	RAMMB   int    `json:"ram_mb"`  // what it's given
	UsedMB  int    `json:"used_mb"` // what its Java uses now (0 when stopped or it couldn't be read)
	Running bool   `json:"running"`
	// CanHaveMB is the most memory it could be given and still fit: for a stopped server if it
	// started now, for a running one at its next start (its own share counts as free).
	CanHaveMB int  `json:"can_have_mb"`
	Fits      bool `json:"fits"` // its memory setting is within CanHaveMB
}

// MemorySummary is GET /api/memory, "minenest memory", and the lines in the window.
type MemorySummary struct {
	Known     bool           `json:"known"`      // the computer's memory could be read
	TotalMB   int            `json:"total_mb"`   // the computer's (or a container limit's)
	FreeMB    int            `json:"free_mb"`    // free right now, as the system says (0: unknown)
	UsedMB    int            `json:"used_mb"`    // in use right now (0: unknown)
	Limited   bool           `json:"limited"`    // TotalMB is a limit on MineNest, not the computer
	ReserveMB int            `json:"reserve_mb"` // kept for the operating system
	RunningMB int            `json:"running_mb"` // given to servers that are running
	StoppedMB int            `json:"stopped_mb"` // given to servers that are stopped
	RealMB    int            `json:"real_mb"`    // what the running servers really use now
	RealKnown bool           `json:"real_known"` // RealMB counts every running server
	RoomMB    int            `json:"room_mb"`    // room for another server, Java's share included
	GiveMB    int            `json:"give_mb"`    // the most memory another server could be given
	Servers   []ServerMemory `json:"servers"`
	Words     string         `json:"words"`   // "You can still give about 5.5 GB to another server."
	Fits      string         `json:"fits"`    // "That's enough for one more 4 GB server, or two 2 GB ones."
	Stopped   string         `json:"stopped"` // whether the stopped servers would all fit too
	Notes     []string       `json:"notes"`   // what couldn't be read, and what that means
	Short     string         `json:"short"`   // a few words for the sidebar
}

// planMemory works the summary out. It's a pure function of what was read, for the tests.
func planMemory(sys sysMemory, goos string, servers []ServerMemory) MemorySummary {
	sum := MemorySummary{TotalMB: sys.TotalMB, FreeMB: sys.AvailMB, Limited: sys.Limited, Servers: servers, Notes: []string{}}
	if sum.Servers == nil {
		sum.Servers = []ServerMemory{}
	}
	sum.RealKnown = true
	for _, s := range servers {
		if s.Running {
			sum.RunningMB += s.RAMMB
			sum.RealMB += s.UsedMB
			if s.UsedMB <= 0 {
				sum.RealKnown = false
			}
		} else {
			sum.StoppedMB += s.RAMMB
		}
	}
	if sys.TotalMB <= 0 {
		sum.Words = "MineNest couldn't read how much memory this computer has, so it can't say how much is left for servers."
		for i := range sum.Servers {
			sum.Servers[i].Fits = true
		}
		return sum
	}
	sum.Known = true
	if sys.AvailMB > 0 {
		sum.UsedMB = max(0, sys.TotalMB-sys.AvailMB)
	}
	sum.ReserveMB = osReserveMB(sys.TotalMB, goos)
	sum.RoomMB = memoryRoom(sys, sum.ReserveMB, servers, -1)
	sum.GiveMB = ramFor(sum.RoomMB)
	for i := range sum.Servers {
		s := &sum.Servers[i]
		room := sum.RoomMB
		if s.Running {
			room = memoryRoom(sys, sum.ReserveMB, servers, i)
		}
		s.CanHaveMB = ramFor(room)
		s.Fits = javaFootprintMB(s.RAMMB) <= room
	}

	if sum.GiveMB >= smallestServerMB {
		sum.Words = fmt.Sprintf("You can still give about %s to another server.", mbWords(sum.GiveMB))
		sum.Fits = fitWords(sum.RoomMB)
		sum.Short = fmt.Sprintf("About %s left for servers", mbWords(sum.GiveMB))
	} else {
		if sum.RunningMB > 0 {
			sum.Words = "There isn't room for another server right now. Stop one, or give a running one less memory in its Settings."
		} else {
			sum.Words = "This computer doesn't have enough memory free for another server right now. Closing other programs helps."
		}
		sum.Short = "No room for another server"
	}
	if sum.StoppedMB > 0 {
		need := 0
		stopped := 0
		for _, s := range servers {
			if !s.Running {
				need += javaFootprintMB(s.RAMMB)
				stopped++
			}
		}
		switch {
		case need <= sum.RoomMB && stopped == 1:
			sum.Stopped = fmt.Sprintf("Your stopped server (given %s) fits too.", mbWords(sum.StoppedMB))
		case need <= sum.RoomMB:
			sum.Stopped = fmt.Sprintf("Your stopped servers (given %s between them) would all fit too.", mbWords(sum.StoppedMB))
		case stopped == 1:
			sum.Stopped = fmt.Sprintf("Your stopped server is given %s, more than is free now.", mbWords(sum.StoppedMB))
		default:
			sum.Stopped = fmt.Sprintf("Your stopped servers are given %s between them, more than is free, so they can't all run at once.", mbWords(sum.StoppedMB))
		}
	}
	if sys.Limited {
		sum.Notes = append(sum.Notes, fmt.Sprintf("MineNest has a memory limit of %s here, so that counts instead of the whole computer's memory.", mbWords(sys.TotalMB)))
	}
	if sys.AvailMB <= 0 {
		sum.Notes = append(sum.Notes, "MineNest couldn't read how much memory is free right now, so this goes by what the servers are given and leaves other programs out.")
	}
	if sum.RunningMB > 0 && !sum.RealKnown {
		sum.Notes = append(sum.Notes, "MineNest couldn't read what every running server really uses, so it counts what they're given.")
	}
	return sum
}

// memoryRoom is the room for one more server's footprint, in MB, with server skip (-1: none)
// left out as if it were stopped. It's the smaller of two answers: the computer's memory less
// the reserve and what the running servers take (or will once busy), and what's free right now
// less a margin and what the running servers will still grow by. The second one counts every
// other program on the computer.
func memoryRoom(sys sysMemory, reserve int, servers []ServerMemory, skip int) int {
	taken, growth := 0, 0
	for i, s := range servers {
		if !s.Running {
			continue
		}
		foot := javaFootprintMB(s.RAMMB)
		if i == skip {
			// Its memory comes back when it stops: what it uses now is free again.
			growth -= s.UsedMB
			continue
		}
		taken += max(foot, s.UsedMB)
		if s.UsedMB > 0 {
			growth += max(0, foot-s.UsedMB)
		} else {
			growth += foot / 2 // unknown: guess it's half way there
		}
	}
	room := sys.TotalMB - reserve - taken
	if sys.AvailMB > 0 {
		margin := max(512, reserve/2)
		room = min(room, sys.AvailMB-margin-growth)
	}
	return max(0, room)
}

// exampleSizes are the server sizes fitWords suggests, biggest first.
var exampleSizes = []int{32768, 24576, 16384, 12288, 8192, 6144, 4096, 3072, 2048, 1024}

// fitWords names server sizes that fit in room MB: "That's enough for one more 4 GB server, or
// two 2 GB ones." It's "" when not even the smallest fits.
func fitWords(room int) string {
	first, n := 0, 0
	for _, s := range exampleSizes {
		if k := room / javaFootprintMB(s); k >= 1 {
			first, n = s, k
			break
		}
	}
	if first == 0 {
		return ""
	}
	out := "That's enough for " + countWords(n) + " more " + mbWords(first) + " server"
	if n > 1 {
		out += "s"
	}
	for _, s := range exampleSizes {
		if s >= first {
			continue
		}
		if k := room / javaFootprintMB(s); k > n && k >= 2 {
			out += ", or " + countWords(k) + " " + mbWords(s) + " ones"
			break
		}
	}
	return out + "."
}

func countWords(n int) string {
	words := []string{"no", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	return strconv.Itoa(n)
}

// mbWords is a memory size in plain words: "512 MB", "4 GB", "5.5 GB".
func mbWords(mb int) string {
	if mb < 1024 {
		return fmt.Sprintf("%d MB", max(0, mb))
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(mb)/1024), ".0") + " GB"
}

// FitWarning is what to say before creating or starting a server given ram MB when only canHave
// MB fits, or "" when it fits.
func FitWarning(ram, canHave int, known bool) string {
	if !known || ram <= canHave {
		return ""
	}
	if canHave < smallestServerMB {
		return fmt.Sprintf("This computer doesn't have %s free for it right now, so it may run slowly or stop with an out of memory error. Stopping another server or closing other programs helps.", mbWords(ram))
	}
	return fmt.Sprintf("Only about %s is free for it right now, less than the %s it's given, so it may run slowly or stop with an out of memory error. Giving it %s or less is safer.", mbWords(canHave), mbWords(ram), mbWords(canHave))
}

// ------------------------------------------------------------------ the daemon's side

type memCache struct {
	mu  sync.Mutex
	at  time.Time
	key string // the servers' states and memory then: a start, stop or change reads again
	sum MemorySummary
}

// Memory is the summary, worked out again every few seconds at most, and straight away when a
// server starts, stops or is given other memory.
func (m *Manager) Memory() MemorySummary {
	servers, pids := m.serverMemory()
	key := ""
	for _, s := range servers {
		key += fmt.Sprintf("%s/%s/%d/%d;", s.ID, s.State, s.RAMMB, pids[s.ID])
	}
	m.mem.mu.Lock()
	if !m.mem.at.IsZero() && time.Since(m.mem.at) <= memoryCacheFor && m.mem.key == key {
		sum := m.mem.sum
		m.mem.mu.Unlock()
		return sum
	}
	m.mem.mu.Unlock()
	for i := range servers {
		if pid := pids[servers[i].ID]; pid > 0 {
			servers[i].UsedMB = processMemoryMB(pid)
		}
	}
	sum := planMemory(cachedSysMemory(), runtime.GOOS, servers)
	m.mem.mu.Lock()
	m.mem.sum, m.mem.at, m.mem.key = sum, time.Now(), key
	m.mem.mu.Unlock()
	return sum
}

// serverMemory is each server's part so far, and the process of each running one.
func (m *Manager) serverMemory() ([]ServerMemory, map[string]int) {
	out, pids := []ServerMemory{}, map[string]int{}
	for _, s := range m.List() {
		s.mu.Lock()
		sm := ServerMemory{ID: s.ID, Name: s.cfg.Name, State: s.state, RAMMB: s.cfg.RAM, Running: s.state != Stopped}
		if sm.Running && s.cmd != nil && s.cmd.Process != nil {
			pids[s.ID] = s.cmd.Process.Pid
		}
		s.mu.Unlock()
		out = append(out, sm)
	}
	return out, pids
}

// server is one server's part of the summary.
func (sum MemorySummary) server(id string) (ServerMemory, bool) {
	for _, s := range sum.Servers {
		if s.ID == id {
			return s, true
		}
	}
	return ServerMemory{}, false
}

// ------------------------------------------------------------------ reading the system's files

// parseMeminfo reads /proc/meminfo: the total and what's available, in MB. Kernels before 3.14
// have no MemAvailable, so free, buffers and cache stand in for it.
func parseMeminfo(text string) (total, avail int) {
	kb := map[string]int{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			n, err := strconv.Atoi(f[1])
			if err == nil {
				kb[strings.TrimSuffix(f[0], ":")] = n
			}
		}
	}
	total = kb["MemTotal"] / 1024
	if a, ok := kb["MemAvailable"]; ok {
		avail = a / 1024
	} else if f, ok := kb["MemFree"]; ok {
		avail = (f + kb["Buffers"] + kb["Cached"]) / 1024
	}
	return total, avail
}

// parseCgroupLimit reads a cgroup's memory.max (v2) or memory.limit_in_bytes (v1), in bytes.
// "max", or v1's huge number for no limit, is no limit (false).
func parseCgroupLimit(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	if text == "" || text == "max" {
		return 0, false
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n <= 0 || n >= 1<<60 {
		return 0, false
	}
	return n, true
}

// statValue is one number from a cgroup's memory.stat.
func statValue(text, key string) int64 {
	for _, line := range strings.Split(text, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == key {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	return 0
}

// withLimit applies a cgroup's limit and what it uses now (both in bytes; used < 0: unknown).
func withLimit(m sysMemory, limit, used int64) sysMemory {
	lim := int(limit >> 20)
	if lim <= 0 || (m.TotalMB > 0 && lim >= m.TotalMB) {
		return m
	}
	m.TotalMB, m.Limited = lim, true
	if used >= 0 {
		free := max(0, lim-int(used>>20))
		if m.AvailMB <= 0 || free < m.AvailMB {
			m.AvailMB = free
		}
	} else if m.AvailMB > lim {
		m.AvailMB = lim
	}
	return m
}

// parseStatm reads /proc/PID/statm: the second number is the pages in memory.
func parseStatm(text string, pageSize int) int {
	f := strings.Fields(text)
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return int(pages * int64(pageSize) >> 20)
}
