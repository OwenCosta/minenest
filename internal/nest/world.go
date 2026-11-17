package nest

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backups of the world: zips in servers/<id>/backups, with a line saying why each was made.

type Backup struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Time int64  `json:"time"`
	Why  string `json:"why"`
}

const keepBackups = 20

func (s *Server) backupDir() string { return filepath.Join(s.Dir, "backups") }

// worldFolders are the world's folders: level-name, and Bukkit's _nether and _the_end beside it.
func (s *Server) worldFolders() []string {
	level := s.props().GetDefault("level-name", "world")
	var out []string
	for _, suffix := range []string{"", "_nether", "_the_end"} {
		name := level + suffix
		if st, err := os.Stat(filepath.Join(s.Root(), name)); err == nil && st.IsDir() {
			out = append(out, name)
		}
	}
	return out
}

func (s *Server) WorldExists() bool { return len(s.worldFolders()) > 0 }

// Backup makes a backup in a job. While the server runs, saving is paused so the files are whole.
func (m *Manager) Backup(s *Server, why string) (*Job, error) {
	if err := s.setBusy("a backup", false); err != nil {
		return nil, err
	}
	return m.Jobs.start("Backing up "+s.Config().Name, s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		return s.backupLocked(ctx, why, j)
	}), nil
}

func (s *Server) backupLocked(ctx context.Context, why string, j *Job) (*Backup, error) {
	folders := s.worldFolders()
	if len(folders) == 0 {
		return nil, fmt.Errorf("there's no world yet: start the server once to make one")
	}
	if why == "" {
		why = "Made by hand"
	}
	if s.State() == Running {
		j.Say("Asking the server to save")
		s.Command("save-off")
		defer s.Command("save-on")
		if err := s.saveAll(ctx, true); err != nil {
			return nil, err
		}
	}
	name := time.Now().Format("2006-01-02_15-04-05") + ".zip"
	dest := filepath.Join(s.backupDir(), name)
	if err := os.MkdirAll(s.backupDir(), 0o755); err != nil {
		return nil, err
	}
	var total int64
	for _, f := range folders {
		filepath.WalkDir(filepath.Join(s.Root(), f), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, e := d.Info(); e == nil {
					total += info.Size()
				}
			}
			return nil
		})
	}
	part := dest + ".part"
	out, err := os.Create(part)
	if err != nil {
		return nil, err
	}
	zw := zip.NewWriter(out)
	var done int64
	for _, f := range folders {
		err = filepath.WalkDir(filepath.Join(s.Root(), f), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rel, _ := filepath.Rel(s.Root(), p)
			if d.IsDir() || d.Name() == "session.lock" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			h, _ := zip.FileInfoHeader(info)
			h.Name = filepath.ToSlash(rel)
			h.Method = zip.Deflate
			w, err := zw.CreateHeader(h)
			if err != nil {
				return err
			}
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			n, err := io.Copy(w, in)
			in.Close()
			done += n
			j.Progress("Zipping the world", done, total)
			return err
		})
		if err != nil {
			break
		}
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return nil, fmt.Errorf("the backup failed: %v", err)
	}
	os.Rename(part, dest)
	os.WriteFile(dest+".txt", []byte(why+"\n"), 0o644)
	s.Note("Backed up the world (%s): %s", why, name)
	s.pruneBackups()
	st, _ := os.Stat(dest)
	b := &Backup{Name: name, Why: why, Time: time.Now().Unix()}
	if st != nil {
		b.Size = st.Size()
	}
	return b, nil
}

func (s *Server) Backups() []Backup {
	entries, _ := os.ReadDir(s.backupDir())
	out := []Backup{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		why, _ := os.ReadFile(filepath.Join(s.backupDir(), e.Name()+".txt"))
		out = append(out, Backup{e.Name(), info.Size(), info.ModTime().Unix(), strings.TrimSpace(string(why))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// pruneBackups keeps the newest ones MineNest made by itself; ones made by hand are kept.
func (s *Server) pruneBackups() {
	n := 0
	for _, b := range s.Backups() {
		if b.Why == "Made by hand" {
			continue
		}
		n++
		if n > keepBackups {
			s.DeleteBackup(b.Name)
		}
	}
}

func (s *Server) backupPath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".zip") {
		return "", fmt.Errorf("there's no backup called %q", name)
	}
	p := filepath.Join(s.backupDir(), name)
	if !exists(p) {
		return "", fmt.Errorf("there's no backup called %q", name)
	}
	return p, nil
}

func (s *Server) DeleteBackup(name string) error {
	p, err := s.backupPath(name)
	if err != nil {
		return err
	}
	os.Remove(p + ".txt")
	return os.Remove(p)
}

// Restore puts a backup back. The world as it is now is backed up first, so nothing is lost.
func (m *Manager) Restore(s *Server, name string) (*Job, error) {
	p, err := s.backupPath(name)
	if err != nil {
		return nil, err
	}
	if err := s.setBusy("putting a backup back", true); err != nil {
		return nil, err
	}
	return m.Jobs.start("Putting back "+name, s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		if s.WorldExists() {
			if _, err := s.backupLocked(ctx, "The world before "+name+" was put back", j); err != nil {
				return nil, err
			}
		}
		j.Say("Unpacking %s", name)
		tmp := filepath.Join(s.Dir, "restoring")
		os.RemoveAll(tmp)
		if err := unzip(p, tmp, 0); err != nil {
			os.RemoveAll(tmp)
			return nil, err
		}
		entries, _ := os.ReadDir(tmp)
		for _, e := range entries {
			target := filepath.Join(s.Root(), e.Name())
			os.RemoveAll(target)
			if err := os.Rename(filepath.Join(tmp, e.Name()), target); err != nil {
				return nil, err
			}
		}
		os.RemoveAll(tmp)
		s.Note("Put back the backup %s.", name)
		return nil, nil
	}), nil
}

// ResetWorld backs the world up and removes it, so the next start makes a new one.
func (m *Manager) ResetWorld(s *Server, seed string) (*Job, error) {
	if err := s.setBusy("making a new world", true); err != nil {
		return nil, err
	}
	return m.Jobs.start("New world for "+s.Config().Name, s.ID, func(ctx context.Context, j *Job) (any, error) {
		defer s.setBusy("", false)
		if s.WorldExists() {
			if _, err := s.backupLocked(ctx, "The world before a new one was made", j); err != nil {
				return nil, err
			}
		}
		for _, f := range s.worldFolders() {
			if err := os.RemoveAll(filepath.Join(s.Root(), f)); err != nil {
				return nil, err
			}
		}
		p := s.props()
		p.Set("level-seed", seed)
		p.Save()
		s.Note("The old world was backed up and removed. The next start makes a new one.")
		return nil, nil
	}), nil
}
