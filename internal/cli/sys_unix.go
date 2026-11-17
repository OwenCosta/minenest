//go:build !windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func detachCmd(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// openApp opens the GUI in a window of its own where a Chromium-based browser can (Brave,
// Chromium, Chrome, Edge), or in the default browser.
func openApp(url string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return fmt.Errorf("there's no desktop here to open a window on")
	}
	for _, b := range []string{"brave-browser", "brave", "chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "microsoft-edge"} {
		if p, err := exec.LookPath(b); err == nil {
			if exec.Command(p, "--app="+url, "--window-size=1200,820").Start() == nil {
				return nil
			}
		}
	}
	return exec.Command("xdg-open", url).Start()
}

func unitPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		h, _ := os.UserHomeDir()
		dir = filepath.Join(h, ".config")
	}
	return filepath.Join(dir, "systemd", "user", "minenest.service")
}

// setAutostart installs (or removes) a systemd user service that runs MineNest's daemon.
func setAutostart(on bool) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("this computer has no systemd; start MineNest with 'minenest daemon' from your own startup script")
	}
	if !on {
		exec.Command("systemctl", "--user", "disable", "--now", "minenest.service").Run()
		os.Remove(unitPath())
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=MineNest (Minecraft servers)
After=network-online.target

[Service]
ExecStart=%s daemon
Restart=on-failure
TimeoutStopSec=150

[Install]
WantedBy=default.target
`, exe)
	if err := os.MkdirAll(filepath.Dir(unitPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath(), []byte(unit), 0o644); err != nil {
		return err
	}
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "--user", "enable", "minenest.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl: %s", out)
	}
	return nil
}

func autostartOn() bool { return fileExists(unitPath()) }

func showError(msg string) { fmt.Fprintln(os.Stderr, msg) }

// openBrowser opens a page in the default web browser, if there's a desktop.
func openBrowser(url string) {
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		exec.Command("xdg-open", url).Start()
	}
}
