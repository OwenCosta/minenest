//go:build !windows

package nest

import (
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
)

func hideWindow(cmd *exec.Cmd) {}

// prepareChild puts a server or agent in a process group of its own (so a Ctrl+C in a terminal
// doesn't reach it) and, on Linux, asks the kernel to end it if the daemon goes away.
func prepareChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	setDeathSignal(cmd.SysProcAttr)
}

func adoptChild(cmd *exec.Cmd) {}

func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// readSysMemory is the computer's memory and what's available now (MemAvailable in
// /proc/meminfo), under the memory limit of MineNest's cgroup when there is one (a container, or
// a systemd service with MemoryMax). Anything it can't read stays 0.
func readSysMemory() sysMemory {
	var m sysMemory
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		m.TotalMB, m.AvailMB = parseMeminfo(string(b))
	}
	if limit, used, ok := cgroupMemory(); ok {
		m = withLimit(m, limit, used)
	}
	return m
}

// cgroupMemory is the tightest memory limit on this process's cgroup or a cgroup above it, and
// what that cgroup uses now less the cache it can drop (-1 when that can't be read).
func cgroupMemory() (limit, used int64, ok bool) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, 0, false
	}
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return string(b)
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		var root, maxFile, curFile, inactive string
		switch {
		case parts[0] == "0" && parts[1] == "": // cgroup v2
			root, maxFile, curFile, inactive = "/sys/fs/cgroup", "memory.max", "memory.current", "inactive_file"
		case strings.Contains(","+parts[1]+",", ",memory,"): // cgroup v1
			root, maxFile, curFile, inactive = "/sys/fs/cgroup/memory", "memory.limit_in_bytes", "memory.usage_in_bytes", "total_inactive_file"
		default:
			continue
		}
		// Inside a container the cgroup's own folder is often the root itself.
		dirs := []string{}
		for p := path.Clean(parts[2]); ; p = path.Dir(p) {
			dirs = append(dirs, root+strings.TrimSuffix(p, "/"))
			if p == "/" || p == "." {
				break
			}
		}
		for _, d := range dirs {
			lim, has := parseCgroupLimit(read(d + "/" + maxFile))
			if !has || (ok && lim >= limit) {
				continue
			}
			limit, used, ok = lim, -1, true
			if cur := strings.TrimSpace(read(d + "/" + curFile)); cur != "" {
				if n, err := strconv.ParseInt(cur, 10, 64); err == nil {
					used = max(0, n-statValue(read(d+"/memory.stat"), inactive))
				}
			}
		}
		if ok {
			return limit, used, true
		}
	}
	return 0, 0, false
}

// processMemoryMB is what a process has in memory now (its resident set), or 0.
func processMemoryMB(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
	if err != nil {
		return 0
	}
	return parseStatm(string(b), os.Getpagesize())
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// diskFree is how many bytes this user can still write on the disk that holds dir.
func diskFree(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}
