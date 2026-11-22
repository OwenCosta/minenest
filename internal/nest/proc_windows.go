//go:build windows

package nest

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// hideWindow keeps a console program (java, an installer) from opening a window of its own.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// Servers and agents go in a job object that ends them if MineNest's daemon ever goes away, so
// a server is never left running with nothing to stop it.
var (
	jobOnce   sync.Once
	jobHandle windows.Handle
)

func killOnExitJob() windows.Handle {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		_, err = windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
		if err != nil {
			windows.CloseHandle(h)
			return
		}
		jobHandle = h
	})
	return jobHandle
}

func prepareChild(cmd *exec.Cmd) {
	hideWindow(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// adoptChild puts a started child in the job.
func adoptChild(cmd *exec.Cmd) {
	job := killOnExitJob()
	if job == 0 || cmd.Process == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	windows.AssignProcessToJobObject(job, h)
	windows.CloseHandle(h)
}

// detach starts the daemon on its own: no console window, not ended with the terminal.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB
}

type memoryStatusEx struct {
	Length, MemoryLoad                      uint32
	TotalPhys, AvailPhys                    uint64
	TotalPageFile, AvailPageFile            uint64
	TotalVirtual, AvailVirtual, AvailExtend uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func totalMemoryMB() int {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return int(m.TotalPhys >> 20)
}

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// diskFree is how many bytes this user can still write on the disk that holds dir.
func diskFree(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, all uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &all); err != nil {
		return 0, err
	}
	return int64(free), nil
}
