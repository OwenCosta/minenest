//go:build windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func detachCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB | 0x08000000}
}

// openApp opens the GUI in its own window: Microsoft Edge's app mode (every Windows 10 and 11
// has it), or the default browser.
func openApp(url string) error {
	for _, edge := range []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
	} {
		if fileExists(edge) {
			cmd := exec.Command(edge, "--app="+url, "--window-size=1200,820")
			if cmd.Start() == nil {
				return nil
			}
		}
	}
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// setAutostart makes MineNest start (in the background) when the user signs in to Windows.
func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		err := k.DeleteValue("MineNest")
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if gui := filepath.Join(filepath.Dir(exe), "minenest-gui.exe"); fileExists(gui) {
		exe = gui
	}
	return k.SetStringValue("MineNest", fmt.Sprintf(`"%s" daemon`, exe))
}

func autostartOn() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue("MineNest")
	return err == nil
}

func showError(msg string) {
	t, _ := windows.UTF16PtrFromString("MineNest")
	m, _ := windows.UTF16PtrFromString(msg)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}
