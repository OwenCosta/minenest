package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/OwenCosta/minenest/internal/nest"
)

// selfUpdate gets the newest MineNest. With the background service running, the service does it
// (and starts again, with the servers that were running); otherwise it's done here.
func selfUpdate(checkOnly bool) error {
	u := nest.CheckUpdate(context.Background(), true)
	if u.Managed != "" {
		fmt.Println(u.Managed)
		return nil
	}
	if u.Error != "" {
		return fmt.Errorf("%s", u.Error)
	}
	if !u.Newer {
		fmt.Printf("MineNest %s is the newest.\n", nest.Version)
		return nil
	}
	fmt.Printf("MineNest %s is out (this is %s).\n", u.Latest, nest.Version)
	if checkOnly {
		return nil
	}
	if c, err := newClient(); err == nil && c.alive() {
		var j jobResp
		if err := c.post("/api/update", nil, &j); err != nil {
			return err
		}
		if err := c.wait(j.Job); err != nil {
			return err
		}
		fmt.Printf("Updated to MineNest %s. It's starting again, with the servers that were running.\n", u.Latest)
		return nil
	}
	if _, err := nest.ApplyUpdate(context.Background(), nil); err != nil {
		return err
	}
	fmt.Printf("Updated to MineNest %s.\n", u.Latest)
	return nil
}

// startDaemonExe starts exe's background service on its own (after an update replaced it).
func startDaemonExe(exe string) error {
	if gui := filepath.Join(filepath.Dir(exe), "minenest-gui.exe"); fileExists(gui) {
		exe = gui
	}
	log, err := os.OpenFile(filepath.Join(nest.Home(), "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Dir = nest.Home()
	detachCmd(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
