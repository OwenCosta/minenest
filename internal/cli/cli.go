package cli

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/OwenCosta/minenest/internal/nest"
	"github.com/OwenCosta/minenest/internal/web"
)

const usage = `MineNest %s: easy Minecraft servers on Windows and Linux.

  minenest gui                          open MineNest's window
  minenest list                         your servers
  minenest create NAME [-t TYPE] [-v VERSION] [-b BUILD] [-m MEMORY] [--modpack SLUG]
                                        make a server (TYPE: paper, vanilla, fabric, neoforge,
                                        forge, purpur; VERSION: latest or e.g. 1.21.4)
  minenest start|stop|restart|kill [SERVER]
  minenest status [SERVER]              how it's doing, and where friends join
  minenest memory                       how much memory is left for servers, and what fits
  minenest console [SERVER]             watch the console and type commands (Ctrl+C leaves)
  minenest cmd [-s SERVER] COMMAND...   send one command, like: minenest cmd say hi
  minenest eula [SERVER]                read and agree to Minecraft's EULA (needed once)

  minenest mods search WORDS [-s SERVER]   find mods, plugins or data packs that fit the server
  minenest mods add NAME... [-s SERVER]    install from Modrinth, with what they need
  minenest mods list|check|update [NAME]|remove NAME [-s SERVER]
  minenest modpacks [WORDS]             find modpacks (then: minenest create NAME --modpack SLUG)

  minenest players [SERVER]             who's on, the whitelist, operators and bans
  minenest op|deop|kick|ban|pardon|allow|disallow PLAYER [-s SERVER]
                                        (allow: the whitelist; a Bedrock player is .NAME)
  minenest set [-s SERVER] KEY=VALUE... change server.properties (minenest get to see them)
  minenest config [-s SERVER] [ram=4G] [autostart=on] [auto-restart=on] [name=NAME]
  minenest switch [-s SERVER] TYPE [VERSION] [-b BUILD]  change the type, version or build
  minenest versions TYPE                which Minecraft versions a type can run
  minenest builds TYPE VERSION          its loader versions or builds (choose one with -b)

  minenest files ls|cat|put|get|mkdir|mv|cp|rm|unzip|trash [-s SERVER]
                                        the server's own files: look, upload, download, change.
                                        rm only moves to the trash, which empties what is over 30 days old
                                        (minenest files for the rest)

  minenest import FOLDER [--name NAME] [-t TYPE] [-v VERSION]
                                        bring in a server you already have (it's copied)

  minenest backup|backups [SERVER]      back the world up, or list the backups
  minenest backup auto [-s SERVER] [off|every HOURS|daily HH:MM] [--keep N]
                                        automatic backups: see or change when they're made,
                                        and how many of them to keep (10 unless you say)
  minenest backup keep|unkeep BACKUP [-s SERVER]  keep one for good, or let it go again
  minenest restore BACKUP [-s SERVER]   put a backup back (the world now is backed up first)
  minenest new-world [-s SERVER] [--seed SEED]

  minenest internet [-s SERVER] off|playit
  minenest internet retry               start playit.gg's agent again after a problem
  minenest internet [-s SERVER] vps --host HOST --user USER [--ssh-port 22] [--port 25565]
  minenest bedrock [-s SERVER] on|off|update   Bedrock players through Geyser (Paper)
  minenest sshkey                       MineNest's key, for a VPS's authorized_keys

  minenest account [login|logout]       sign in with your OCAccount (for the Remote dashboard)
                                        (login --pin: a XenithOS profile's name and PIN instead)

  minenest autostart on|off             start MineNest when you sign in
  minenest delete SERVER                remove a server and all its files
  minenest update                       get the newest MineNest
  minenest daemon                       run the background service in this terminal
  minenest quit                         stop the background service (stops every server)

Everything lives in %s.
`

// Run is the command line. It returns the exit code.
func Run(args []string) int {
	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, "MineNest: "+err.Error())
		return 1
	}
	return 0
}

type flags struct {
	pos   []string
	vals  map[string]string
	bools map[string]bool
}

var valueFlags = map[string]string{"-s": "server", "--server": "server", "-t": "type", "--type": "type", "-v": "version",
	"--version": "version", "-m": "ram", "--memory": "ram", "--ram": "ram", "--modpack": "modpack", "--seed": "seed",
	"--host": "host", "-b": "build", "--build": "build", "--loader": "build", "--user": "user", "--ssh-port": "ssh-port", "--port": "port", "--pin": "pin", "--name": "name", "-n": "lines", "--keep": "keep", "--to": "to", "--into": "into"}

func parse(args []string) flags {
	f := flags{vals: map[string]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.pos = append(f.pos, args[i+1:]...)
			break
		}
		if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "-") {
			if name, known := valueFlags[k]; known {
				f.vals[name] = v
				continue
			}
		}
		if name, ok := valueFlags[a]; ok && i+1 < len(args) {
			f.vals[name] = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			f.bools[strings.TrimPrefix(a, "--")] = true
			continue
		}
		f.pos = append(f.pos, a)
	}
	return f
}

// server is -s, or the first positional when the command takes one.
func (f flags) server(usePos bool) string {
	if s := f.vals["server"]; s != "" {
		return s
	}
	if usePos && len(f.pos) > 0 {
		return f.pos[0]
	}
	return ""
}

func run(args []string) error {
	if len(args) == 0 {
		args = []string{"help"}
	}
	cmd, rest := args[0], args[1:]
	f := parse(rest)
	switch cmd {
	case "help", "-h", "--help":
		fmt.Printf(usage, nest.Version, nest.Home())
		return nil
	case "about", "--version", "-V":
		fmt.Printf("MineNest %s\nhttps://github.com/%s\n", nest.Version, nest.RepoSlug)
		return nil
	case "daemon":
		return daemon()
	case "update":
		return selfUpdate(f.bools["check"])
	case "autostart":
		if len(f.pos) == 0 {
			fmt.Println("Start with your computer:", onOff(autostartOn()))
			return nil
		}
		on := isOn(f.pos[0])
		if err := setAutostart(on); err != nil {
			return err
		}
		if on {
			fmt.Println("MineNest now starts when you sign in. Servers set to start with MineNest start then too (minenest config autostart=on).")
		} else {
			fmt.Println("MineNest no longer starts by itself.")
		}
		return nil
	}

	c, err := connect()
	if err != nil {
		return err
	}
	switch cmd {
	case "gui", "open":
		return openGUI(c)
	case "quit", "stop-daemon":
		fmt.Println("Stopping MineNest (and saving every running server)...")
		c.post("/api/shutdown", nil, nil)
		return nil
	case "list", "ls":
		return list(c)
	case "create", "new":
		return create(c, f)
	case "import":
		if len(f.pos) == 0 {
			return fmt.Errorf("say which folder: minenest import FOLDER")
		}
		ram, err := parseRAM(f.vals["ram"])
		if err != nil {
			return err
		}
		var r jobResp
		if err := c.post("/api/import", nest.ImportOptions{Path: f.pos[0], Name: f.vals["name"], Flavor: f.vals["type"], Version: f.vals["version"], RAM: ram}, &r); err != nil {
			return err
		}
		if err := c.wait(r.Job); err != nil {
			return err
		}
		fmt.Printf("Brought in as %s. Start it with: minenest start %s\n", r.ID, r.ID)
		return nil
	case "status", "info":
		return status(c, f.server(true))
	case "memory", "mem", "ram":
		return memory(c)
	case "start":
		id, err := pick(c, f.server(true))
		if err != nil {
			return err
		}
		var st nest.ServerStatus
		if c.get(sp(id), &st) == nil && st.MemWarning != "" && st.State == nest.Stopped {
			fmt.Println(st.MemWarning)
		}
		if err := c.post(sp(id)+"/start", nil, nil); err != nil {
			if strings.Contains(err.Error(), "EULA") {
				return fmt.Errorf("%v\nRun: minenest eula %s", err, id)
			}
			return err
		}
		fmt.Println("Starting. Watch it with: minenest console " + id)
		return nil
	case "stop", "kill":
		id, err := pick(c, f.server(true))
		if err != nil {
			return err
		}
		if err := c.post(sp(id)+"/"+cmd, nil, nil); err != nil {
			return err
		}
		if cmd == "stop" {
			fmt.Println("Stopping (saving the world first).")
		}
		return nil
	case "restart":
		id, err := pick(c, f.server(true))
		if err != nil {
			return err
		}
		var j jobResp
		if err := c.post(sp(id)+"/restart", nil, &j); err != nil {
			return err
		}
		return c.wait(j.Job)
	case "console", "attach":
		id, err := pick(c, f.server(true))
		if err != nil {
			return err
		}
		return console(c, id)
	case "logs":
		id, err := pick(c, f.server(true))
		if err != nil {
			return err
		}
		n, _ := strconv.Atoi(f.vals["lines"])
		if n <= 0 {
			n = 60
		}
		var r struct{ Lines []nest.Line }
		if err := c.get(sp(id)+"/console?since=0", &r); err != nil {
			return err
		}
		if len(r.Lines) > n {
			r.Lines = r.Lines[len(r.Lines)-n:]
		}
		for _, l := range r.Lines {
			fmt.Println(l.Text)
		}
		return nil
	case "cmd", "command", "say":
		id, err := pick(c, f.server(false))
		if err != nil {
			return err
		}
		line := strings.Join(f.pos, " ")
		if cmd == "say" {
			line = "say " + line
		}
		return c.post(sp(id)+"/command", map[string]string{"command": line}, nil)
	case "eula":
		return eula(c, f)
	case "mods", "mod", "plugins", "plugin", "addons", "datapacks":
		return mods(c, f)
	case "modpacks", "modpack":
		var r nest.AddonSearch
		if err := c.get("/api/modpacks?q="+url.QueryEscape(strings.Join(f.pos, " ")), &r); err != nil {
			return err
		}
		for _, h := range r.Hits {
			fmt.Printf("  %-28s %s\n  %-28s %s, %s downloads\n\n", h.Slug, h.Title, "", cut(h.Description, 70), count(h.Downloads))
		}
		fmt.Println("Make a server from one with: minenest create NAME --modpack SLUG")
		return nil
	case "players":
		return players(c, f.server(true))
	case "op", "deop", "kick", "ban", "pardon", "unban", "allow", "disallow", "whitelist":
		if cmd == "unban" {
			cmd = "pardon"
		}
		if cmd == "whitelist" {
			cmd = "allow"
		}
		if len(f.pos) == 0 {
			return fmt.Errorf("say which player")
		}
		id, err := pick(c, f.server(false))
		if err != nil {
			return err
		}
		reason := strings.Join(f.pos[1:], " ")
		var said struct{ Message string }
		if err := c.post(sp(id)+"/players", map[string]string{"action": cmd, "name": f.pos[0], "reason": reason}, &said); err != nil {
			return err
		}
		if said.Message == "" {
			said.Message = "Done."
		}
		fmt.Println(said.Message)
		return nil
	case "get":
		id, err := pick(c, f.server(false))
		if err != nil {
			return err
		}
		var props map[string]string
		if err := c.get(sp(id)+"/properties", &props); err != nil {
			return err
		}
		keys := make([]string, 0, len(props))
		for k := range props {
			if len(f.pos) == 0 || strings.Contains(k, f.pos[0]) {
				keys = append(keys, k)
			}
		}
		sortStrings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, props[k])
		}
		return nil
	case "set":
		id, err := pick(c, f.server(false))
		if err != nil {
			return err
		}
		set := map[string]string{}
		for _, p := range f.pos {
			k, v, ok := strings.Cut(p, "=")
			if !ok {
				return fmt.Errorf("write settings as key=value, like: minenest set difficulty=hard")
			}
			set[k] = v
		}
		if len(set) == 0 {
			return fmt.Errorf("nothing to set")
		}
		return c.do("PUT", sp(id)+"/properties", set, nil)
	case "config":
		return config(c, f)
	case "switch":
		id, err := pick(c, f.server(false))
		if err != nil {
			return err
		}
		if len(f.pos) == 0 {
			return fmt.Errorf("say which type: paper, vanilla, fabric, neoforge, forge or purpur")
		}
		v := "latest"
		if len(f.pos) > 1 {
			v = f.pos[1]
		}
		var j jobResp
		if err := c.post(sp(id)+"/version", map[string]string{"flavor": f.pos[0], "version": v, "build": f.vals["build"]}, &j); err != nil {
			return err
		}
		return c.wait(j.Job)
	case "builds", "loaders":
		if len(f.pos) < 2 {
			return fmt.Errorf("say the type and Minecraft version: minenest builds fabric 1.21.4")
		}
		var bs []nest.Build
		if err := c.get("/api/builds/"+url.PathEscape(f.pos[0])+"/"+url.PathEscape(f.pos[1]), &bs); err != nil {
			return err
		}
		if len(bs) == 0 {
			fmt.Println("There's only one: Minecraft's own.")
		}
		for i, b := range bs {
			if i == 25 {
				fmt.Printf("  ...and %d older\n", len(bs)-25)
				break
			}
			mark := ""
			if b.Recommended {
				mark = "  (MineNest's pick)"
			}
			fmt.Printf("  %-28s %s%s\n", b.ID, b.Label, mark)
		}
		return nil
	case "versions":
		if len(f.pos) == 0 {
			return fmt.Errorf("say which type: paper, vanilla, fabric, neoforge, forge or purpur")
		}
		var vs []string
		if err := c.get("/api/versions/"+url.PathEscape(f.pos[0]), &vs); err != nil {
			return err
		}
		fmt.Println(strings.Join(vs, "  "))
		return nil
	case "backup":
		if len(f.pos) > 0 && (f.pos[0] == "auto" || f.pos[0] == "keep" || f.pos[0] == "unkeep") {
			return backupMore(c, f)
		}
		id, err := pick(c, f.server(true))
		if err != nil {
			return err
		}
		var j jobResp
		if err := c.post(sp(id)+"/backups", nil, &j); err != nil {
			return err
		}
		if err := c.wait(j.Job); err != nil {
			return err
		}
		fmt.Println("Backed up.")
		return nil
	case "backups":
		id, err := pick(c, f.server(true))
		if err != nil {
			return err
		}
		var r backupList
		if err := c.get(sp(id)+"/backups", &r); err != nil {
			return err
		}
		if len(r.Backups) == 0 {
			fmt.Println("No backups yet. Make one with: minenest backup")
		}
		for _, b := range r.Backups {
			kept := ""
			if b.Keep {
				kept = " (kept)"
			}
			fmt.Printf("  %-26s %8s  %s%s\n", b.Name, size(b.Size), b.Why, kept)
		}
		if len(r.Backups) > 0 {
			fmt.Printf("\nThey use %s.", size(r.Used))
			if r.Free >= 0 {
				fmt.Printf(" %s is free on this disk.", size(r.Free))
			}
			fmt.Println()
		}
		fmt.Println(autoWords(r.Auto))
		return nil
	case "restore":
		id, err := pick(c, f.server(false))
		if err != nil {
			return err
		}
		if len(f.pos) == 0 {
			return fmt.Errorf("say which backup (minenest backups lists them)")
		}
		var j jobResp
		if err := c.post(sp(id)+"/backups/"+url.PathEscape(f.pos[0])+"/restore", nil, &j); err != nil {
			return err
		}
		return c.wait(j.Job)
	case "new-world":
		id, err := pick(c, f.server(true))
		if err != nil {
			return err
		}
		if !f.bools["yes"] && !confirm("The world is backed up, then removed, and the next start makes a new one. Go ahead?") {
			return nil
		}
		var j jobResp
		if err := c.post(sp(id)+"/world/reset", map[string]string{"seed": f.vals["seed"]}, &j); err != nil {
			return err
		}
		return c.wait(j.Job)
	case "files", "file":
		return files(c, f)
	case "internet", "way":
		return internet(c, f)
	case "bedrock":
		id, err := pick(c, f.server(false))
		if err != nil {
			return err
		}
		if len(f.pos) == 0 {
			return fmt.Errorf("say on, off or update")
		}
		var j jobResp
		if err := c.post(sp(id)+"/bedrock", map[string]bool{"on": isOn(f.pos[0]), "update": f.pos[0] == "update"}, &j); err != nil {
			return err
		}
		return c.wait(j.Job)
	case "sshkey":
		var r struct{ Key string }
		if err := c.get("/api/sshkey", &r); err != nil {
			return err
		}
		fmt.Println(r.Key)
		return nil
	case "account":
		return account(c, f)
	case "delete", "remove", "rm":
		id, err := pick(c, f.server(true))
		if err != nil {
			return err
		}
		if !f.bools["yes"] && !confirm(fmt.Sprintf("Delete %s with its world, backups and everything? This can't be undone.", id)) {
			return nil
		}
		if err := c.do("DELETE", sp(id), nil, nil); err != nil {
			return err
		}
		fmt.Println("Deleted.")
		return nil
	}
	return fmt.Errorf("unknown command %q. Run 'minenest help' to see them all", cmd)
}

func daemon() error {
	m, err := nest.NewManager()
	if err != nil {
		return err
	}
	quit := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		select {
		case <-quit:
		default:
			close(quit)
		}
	}()
	fmt.Printf("%s MineNest %s is running (home %s).\n", time.Now().Format(time.DateTime), nest.Version, nest.Home())
	go m.Autostart()
	files := web.Files()
	if dir := os.Getenv("MINENEST_WEB_DIR"); dir != "" { // for working on the GUI
		files = os.DirFS(dir)
	}
	err = nest.Serve(m, files, quit)
	fmt.Printf("%s Stopping every server...\n", time.Now().Format(time.DateTime))
	m.Shutdown()
	if exe := m.RestartExe(); exe != "" { // updated: the new MineNest takes over
		fmt.Printf("%s Starting the updated MineNest\n", time.Now().Format(time.DateTime))
		if serr := startDaemonExe(exe); serr != nil {
			fmt.Println("It didn't start:", serr)
		}
	}
	return err
}

func openGUI(c *client) error {
	u := c.base + "/open?key=" + c.token
	if err := openApp(u); err != nil {
		fmt.Println("Open this in a browser on this computer:\n  " + u)
		return nil
	}
	return nil
}

// pick turns "" into the only server, and checks a name.
func pick(c *client, ref string) (string, error) {
	var list []nest.ServerStatus
	if err := c.get("/api/servers", &list); err != nil {
		return "", err
	}
	if ref == "" {
		switch len(list) {
		case 0:
			return "", fmt.Errorf("there are no servers yet. Make one: minenest create \"My Server\"")
		case 1:
			return list[0].ID, nil
		}
		names := []string{}
		for _, s := range list {
			names = append(names, s.ID)
		}
		return "", fmt.Errorf("say which server (-s NAME): %s", strings.Join(names, ", "))
	}
	for _, s := range list {
		if s.ID == ref || strings.EqualFold(s.Name, ref) {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("there's no server called %q", ref)
}

func list(c *client) error {
	var list []nest.ServerStatus
	if err := c.get("/api/servers", &list); err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("No servers yet. Make one with: minenest create \"My Server\"   (or: minenest gui)")
		return nil
	}
	for _, s := range list {
		ver := s.FlavorName + " " + s.Minecraft
		if !s.Installed {
			ver = "not installed yet"
		}
		fmt.Printf("  %-18s %-9s %-22s %d/%d players  port %d\n", s.ID, s.State, ver, len(s.Players), s.MaxPlayers, s.Port)
	}
	return nil
}

func status(c *client, ref string) error {
	id, err := pick(c, ref)
	if err != nil {
		return err
	}
	var s nest.ServerStatus
	if err := c.get(sp(id), &s); err != nil {
		return err
	}
	fmt.Printf("%s (%s)\n", s.Name, s.ID)
	fmt.Printf("  %-14s %s\n", "State", s.State)
	if s.Why != "" {
		fmt.Printf("  %-14s %s\n", "Stopped", s.Why)
	}
	if s.Installed {
		fmt.Printf("  %-14s %s %s (Java %d)\n", "Software", s.FlavorName, s.Minecraft, s.Java)
	}
	if s.Modpack != "" {
		fmt.Printf("  %-14s %s\n", "Modpack", s.Modpack)
	}
	mem := gb(s.RAM)
	if s.MemUsedMB > 0 {
		mem += ", using " + gb(s.MemUsedMB) + " now"
	}
	fmt.Printf("  %-14s %s\n", "Memory", mem)
	var sum nest.MemorySummary
	if c.get("/api/memory", &sum) == nil && sum.Known {
		fmt.Printf("  %-14s %s\n", "", sum.Words)
		if s.MemWarning != "" && s.State == nest.Stopped {
			fmt.Printf("  %-14s %s\n", "", s.MemWarning)
		}
	}
	fmt.Printf("  %-14s %d/%d %s\n", "Players", len(s.Players), s.MaxPlayers, strings.Join(s.Players, ", "))
	fmt.Printf("  %-14s %s\n", "This computer", s.Addresses.Here)
	if s.Addresses.Home != "" {
		fmt.Printf("  %-14s %s\n", "At home", s.Addresses.Home)
	}
	way := map[string]string{"": "off", "playit": "playit.gg", "vps": "your VPS"}[s.Way.Kind]
	fmt.Printf("  %-14s %s", "Internet", way)
	if s.Way.Address != "" {
		fmt.Printf(": %s", s.Way.Address)
	}
	if s.Way.Kind != "" {
		fmt.Printf(" (%s)", s.Way.State)
	}
	fmt.Println()
	if s.Way.ClaimURL != "" {
		fmt.Printf("  %-14s open %s to connect this computer to your playit.gg account\n", "", s.Way.ClaimURL)
	}
	for _, n := range []string{s.Way.Note, s.Way.Fix, s.Way.Check} {
		if n != "" {
			fmt.Printf("  %-14s %s\n", "", n)
		}
	}
	if len(s.Way.Log) > 0 && !s.Way.Up {
		fmt.Printf("  %-14s %s\n", "", "What playit.gg's agent said last:")
		for _, l := range s.Way.Log {
			fmt.Printf("  %-14s   %s\n", "", l)
		}
	}
	if s.BedrockOn {
		fmt.Printf("  %-14s port %d\n", "Bedrock", nest.BedrockPort)
	}
	if s.EulaNeeded {
		fmt.Printf("\nMinecraft's EULA hasn't been agreed to yet: minenest eula %s\n", s.ID)
	}
	return nil
}

func create(c *client, f flags) error {
	name := f.vals["name"]
	if name == "" && len(f.pos) > 0 {
		name = strings.Join(f.pos, " ")
	}
	if name == "" {
		return fmt.Errorf("give it a name: minenest create \"My Server\"")
	}
	ram, err := parseRAM(f.vals["ram"])
	if err != nil {
		return err
	}
	o := nest.CreateOptions{Name: name, Flavor: f.vals["type"], Version: f.vals["version"], RAM: ram, Modpack: f.vals["modpack"], Build: f.vals["build"]}
	if f.bools["agree-eula"] || f.bools["eula"] {
		o.AgreeEULA = true
	}
	var sum nest.MemorySummary
	if ram > 0 && c.get("/api/memory", &sum) == nil {
		if w := nest.FitWarning(ram, sum.GiveMB, sum.Known); w != "" {
			fmt.Println(w)
		}
	}
	var r jobResp
	if err := c.post("/api/servers", o, &r); err != nil {
		return err
	}
	fmt.Printf("Setting up %s...\n", name)
	if err := c.wait(r.Job); err != nil {
		return fmt.Errorf("%v\n(The server %s is there; try: minenest switch -s %s TYPE VERSION)", err, r.ID, r.ID)
	}
	if o.AgreeEULA {
		fmt.Printf("Ready. Start it with: minenest start %s\n", r.ID)
	} else {
		fmt.Printf("Ready. Next: minenest eula %s, then minenest start %s\n", r.ID, r.ID)
	}
	return nil
}

func eula(c *client, f flags) error {
	id, err := pick(c, f.server(true))
	if err != nil {
		return err
	}
	fmt.Println("To run a Minecraft server you need to agree to Minecraft's End User License Agreement:")
	fmt.Println("  " + nest.EulaURL)
	if !f.bools["yes"] && !confirm("Have you read it, and do you agree to it?") {
		fmt.Println("Not agreed. The server can't start until you do.")
		return nil
	}
	if err := c.post(sp(id)+"/eula", map[string]bool{"agree": true}, nil); err != nil {
		return err
	}
	fmt.Println("Agreed. Start it with: minenest start " + id)
	return nil
}

func console(c *client, id string) error {
	var since int64
	var r struct{ Lines []nest.Line }
	if err := c.get(sp(id)+"/console?since=0", &r); err != nil {
		return err
	}
	if len(r.Lines) > 40 {
		r.Lines = r.Lines[len(r.Lines)-40:]
	}
	for _, l := range r.Lines {
		fmt.Println(colour(l))
		since = l.Seq
	}
	fmt.Println("(Type a command and press Enter. Ctrl+C leaves; the server keeps running.)")
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				if err := c.post(sp(id)+"/command", map[string]string{"command": line}, nil); err != nil {
					fmt.Println("  " + err.Error())
				}
			}
		}
	}()
	long := &client{base: c.base, token: c.token, http: c.http}
	for {
		var r struct{ Lines []nest.Line }
		if err := long.get(fmt.Sprintf("%s/console?since=%d&wait=1", sp(id), since), &r); err != nil {
			return err
		}
		for _, l := range r.Lines {
			fmt.Println(colour(l))
			since = l.Seq
		}
	}
}

func colour(l nest.Line) string {
	if !isTerminal() {
		return l.Text
	}
	code := map[string]string{"warn": "33", "error": "31", "join": "32", "chat": "36", "sent": "35", "nest": "94"}[l.Kind]
	if code == "" {
		return l.Text
	}
	return "\033[" + code + "m" + l.Text + "\033[0m"
}

func mods(c *client, f flags) error {
	sub := "list"
	if len(f.pos) > 0 {
		sub, f.pos = f.pos[0], f.pos[1:]
	}
	id, err := pick(c, f.server(false))
	if err != nil {
		return err
	}
	switch sub {
	case "search", "find":
		var r nest.AddonSearch
		if err := c.get(sp(id)+"/addons/search?q="+url.QueryEscape(strings.Join(f.pos, " ")), &r); err != nil {
			return err
		}
		if len(r.Hits) == 0 {
			fmt.Println("Nothing on Modrinth fits that search for this server.")
		}
		for _, h := range r.Hits {
			mark := ""
			if h.Installed {
				mark = " (installed)"
			}
			fmt.Printf("  %-26s %s%s\n  %-26s %s, %s downloads\n\n", h.Slug, h.Title, mark, "", cut(h.Description, 70), count(h.Downloads))
		}
		fmt.Printf("Install one with: minenest mods add SLUG\n")
		return nil
	case "add", "install":
		if len(f.pos) == 0 {
			return fmt.Errorf("say which (minenest mods search WORDS finds them)")
		}
		for _, ref := range f.pos {
			var j jobResp
			if err := c.post(sp(id)+"/addons", map[string]string{"project": ref}, &j); err != nil {
				return err
			}
			if err := c.wait(j.Job); err != nil {
				return err
			}
			var jr struct{ Result []string }
			c.get("/api/jobs/"+j.Job, &jr)
			fmt.Println("Installed " + strings.Join(jr.Result, ", ") + ".")
		}
		return nil
	case "remove", "rm", "delete":
		if len(f.pos) == 0 {
			return fmt.Errorf("say which")
		}
		var r struct{ Removed string }
		if err := c.do("DELETE", sp(id)+"/addons/"+url.PathEscape(f.pos[0]), nil, &r); err != nil {
			return err
		}
		fmt.Println("Removed " + r.Removed + ".")
		return nil
	case "check":
		var list []nest.Addon
		if err := c.post(sp(id)+"/addons/check", nil, &list); err != nil {
			return err
		}
		n := 0
		for _, a := range list {
			if a.Update != "" {
				fmt.Printf("  %-30s %s -> %s\n", a.Title, a.Version, a.Update)
				n++
			}
		}
		if n == 0 {
			fmt.Println("Everything is up to date.")
		} else {
			fmt.Println("Update them with: minenest mods update")
		}
		return nil
	case "update":
		ref := ""
		if len(f.pos) > 0 {
			ref = f.pos[0]
		}
		var j jobResp
		if err := c.post(sp(id)+"/addons/update", map[string]string{"project": ref}, &j); err != nil {
			return err
		}
		return c.wait(j.Job)
	case "list", "ls":
		var l nest.AddonList
		if err := c.get(sp(id)+"/addons", &l); err != nil {
			return err
		}
		if len(l.Addons) == 0 && len(l.Other) == 0 {
			fmt.Printf("No %ss yet. Find some with: minenest mods search WORDS\n", l.Kind)
		}
		for _, a := range l.Addons {
			extra := ""
			if a.Needed {
				extra = " (needed by another)"
			}
			fmt.Printf("  %-30s %s%s\n", a.Title, a.Version, extra)
		}
		for _, o := range l.Other {
			fmt.Printf("  %-30s (added by hand)\n", o)
		}
		return nil
	}
	return fmt.Errorf("unknown: minenest mods %s (search, add, list, check, update, remove)", sub)
}

func players(c *client, ref string) error {
	id, err := pick(c, ref)
	if err != nil {
		return err
	}
	var p nest.PlayerLists
	if err := c.get(sp(id)+"/players", &p); err != nil {
		return err
	}
	show := func(t string, v []string) {
		if len(v) == 0 {
			fmt.Printf("  %-11s nobody\n", t)
		} else {
			fmt.Printf("  %-11s %s\n", t, strings.Join(v, ", "))
		}
	}
	show("Online", p.Online)
	show("Operators", p.Ops)
	wl := "Whitelist"
	if !p.WhitelistOn {
		wl += " (off)"
	}
	show(wl, p.Whitelist)
	show("Banned", p.Banned)
	return nil
}

func config(c *client, f flags) error {
	id, err := pick(c, f.server(false))
	if err != nil {
		return err
	}
	if len(f.pos) == 0 {
		var s nest.ServerStatus
		if err := c.get(sp(id), &s); err != nil {
			return err
		}
		fmt.Printf("name=%s\nram=%dM\nautostart=%s\nauto-restart=%s\njvm-args=%s\n", s.Name, s.RAM, onOff(s.Autostart), onOff(s.AutoRestart), s.JVMArgs)
		return nil
	}
	patch := map[string]any{}
	for _, p := range f.pos {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return fmt.Errorf("write it as key=value, like: minenest config ram=4G")
		}
		switch k {
		case "ram", "memory":
			n, err := parseRAM(v)
			if err != nil {
				return err
			}
			patch["ram_mb"] = n
		case "autostart":
			patch["autostart"] = isOn(v)
		case "auto-restart":
			patch["auto_restart"] = isOn(v)
		case "name":
			patch["name"] = v
		case "jvm-args":
			patch["jvm_args"] = v
		default:
			return fmt.Errorf("unknown setting %q (ram, autostart, auto-restart, name, jvm-args). For server.properties use: minenest set", k)
		}
	}
	if err := c.do("PATCH", sp(id), patch, nil); err != nil {
		return err
	}
	if _, ok := patch["ram_mb"]; ok {
		var s nest.ServerStatus
		if c.get(sp(id), &s) == nil && s.MemWarning != "" {
			fmt.Println(s.MemWarning)
		}
	}
	return nil
}

// memory is "minenest memory": the computer's memory, the servers' share, and what fits.
func memory(c *client) error {
	var sum nest.MemorySummary
	if err := c.get("/api/memory", &sum); err != nil {
		return err
	}
	if !sum.Known {
		fmt.Println(sum.Words)
		return nil
	}
	row := func(k, v string) { fmt.Printf("  %-22s %s\n", k, v) }
	fmt.Println("Memory on this computer")
	row("In all", gb(sum.TotalMB))
	if sum.FreeMB > 0 {
		row("In use now", fmt.Sprintf("%s (%s free)", gb(sum.UsedMB), gb(sum.FreeMB)))
	} else {
		row("In use now", "couldn't be read")
	}
	row("Left for the computer", gb(sum.ReserveMB))
	running := gb(sum.RunningMB) + " given"
	if sum.RunningMB > 0 && sum.RealMB > 0 {
		running += ", using " + gb(sum.RealMB) + " now"
	}
	row("Running servers", running)
	row("Stopped servers", gb(sum.StoppedMB)+" given")
	if len(sum.Servers) > 0 {
		fmt.Println()
		for _, s := range sum.Servers {
			line := gb(s.RAMMB) + " given"
			switch {
			case s.Running && s.UsedMB > 0:
				line += ", using " + gb(s.UsedMB)
			case !s.Running && s.Fits:
				line += ", fits if it starts now"
			case !s.Running:
				line += ", more than is free now (" + gb(s.CanHaveMB) + " would fit)"
			}
			fmt.Printf("  %-22s %-9s %s\n", cut(s.Name, 22), s.State, line)
		}
	}
	fmt.Println()
	fmt.Println(sum.Words)
	for _, n := range append([]string{sum.Fits, sum.Stopped}, sum.Notes...) {
		if n != "" {
			fmt.Println(n)
		}
	}
	return nil
}

// gb is a size in MB in plain words: 512 MB, 4 GB, 5.5 GB.
func gb(mb int) string {
	if mb < 1024 {
		return fmt.Sprintf("%d MB", max(0, mb))
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(mb)/1024), ".0") + " GB"
}

func internet(c *client, f flags) error {
	id, err := pick(c, f.server(false))
	if err != nil {
		return err
	}
	if len(f.pos) == 0 {
		return status(c, id)
	}
	kind := f.pos[0]
	if kind == "retry" {
		if err := c.post("/api/playit/restart", nil, nil); err != nil {
			return err
		}
		fmt.Println("playit.gg's agent starts again. 'minenest status' shows how it's going.")
		return nil
	}
	body := map[string]any{"kind": kind}
	if kind == "vps" && f.vals["host"] != "" {
		sshPort, _ := strconv.Atoi(f.vals["ssh-port"])
		port, _ := strconv.Atoi(f.vals["port"])
		body["vps"] = nest.TunnelConfig{Host: f.vals["host"], User: f.vals["user"], SSHPort: sshPort, PublicPort: port}
	}
	if err := c.post(sp(id)+"/way", body, nil); err != nil {
		return err
	}
	switch kind {
	case "playit":
		fmt.Println("playit.gg is on. When the server starts, 'minenest status' shows a link to approve this computer on playit.gg (once), then the address.")
	case "vps":
		var k struct{ Key string }
		c.get("/api/sshkey", &k)
		fmt.Println("Your VPS is the way in. Put this line in ~/.ssh/authorized_keys for that user on the VPS:\n  " + k.Key)
		fmt.Println("and set 'GatewayPorts clientspecified' in its sshd_config so friends can reach the port.")
	default:
		fmt.Println("Only people at home (and on this computer) can join now.")
	}
	return nil
}

func account(c *client, f flags) error {
	sub := ""
	if len(f.pos) > 0 {
		sub = f.pos[0]
	}
	switch sub {
	case "", "status":
		var r nest.AccountInfo
		if err := c.get("/api/account", &r); err != nil {
			return err
		}
		if !r.SignedIn {
			fmt.Println("Not signed in. Sign in with your OCAccount: minenest account login")
			fmt.Println("(or with a XenithOS profile's name and PIN: minenest account login --pin)")
		} else if r.OCAccount != "" {
			fmt.Printf("Signed in as %s, with the OCAccount %s.\n", r.Account, r.OCAccount)
		} else {
			fmt.Println("Signed in as " + r.Account + ".")
		}
		return nil
	case "login", "oc", "ocaccount":
		if f.bools["pin"] {
			break
		}
		var st struct{ URL string }
		if err := c.post("/api/account/oc/start", nil, &st); err != nil {
			return err
		}
		fmt.Println("Sign in with your OCAccount in your browser:\n  " + st.URL)
		openBrowser(st.URL)
		for i := 0; i < 300; i++ {
			time.Sleep(2 * time.Second)
			var p nest.OCState
			if err := c.post("/api/account/oc/poll", nil, &p); err != nil {
				return err
			}
			switch p.State {
			case "done":
				fmt.Printf("Signed in as %s (OCAccount %s).\n", p.Name, p.Account)
				return nil
			case "error":
				return fmt.Errorf("%s", p.Error)
			case "none":
				return fmt.Errorf("the sign-in ran out. Try again")
			}
		}
		return fmt.Errorf("the sign-in ran out. Try again")
	}
	switch sub {
	case "login", "create", "pin":
		name := f.vals["name"]
		if name == "" && len(f.pos) > 1 {
			name = f.pos[1]
		}
		if name == "" {
			name = prompt("Profile name: ")
		}
		pin := f.vals["pin"]
		if pin == "" {
			pin = prompt("PIN: ")
		}
		var a nest.Account
		if err := c.post("/api/account/login", map[string]any{"name": name, "pin": pin, "create": sub == "create"}, &a); err != nil {
			return err
		}
		fmt.Println("Signed in as " + a.Name + ".")
		return nil
	case "logout":
		return c.post("/api/account/logout", nil, nil)
	}
	return fmt.Errorf("unknown: minenest account %s (login, login --pin, create, logout)", sub)
}

// ------------------------------------------------------------------ little things

func parseRAM(v string) (int, error) {
	v = strings.ToUpper(strings.TrimSpace(v))
	if v == "" {
		return 0, nil
	}
	mult := 1
	switch {
	case strings.HasSuffix(v, "G") || strings.HasSuffix(v, "GB"):
		mult = 1024
		v = strings.TrimSuffix(strings.TrimSuffix(v, "B"), "G")
	case strings.HasSuffix(v, "M") || strings.HasSuffix(v, "MB"):
		v = strings.TrimSuffix(strings.TrimSuffix(v, "B"), "M")
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("memory is like 4G or 3072M")
	}
	return int(n * float64(mult)), nil
}

type backupList struct {
	Backups []nest.Backup
	Auto    nest.AutoStatus
	Used    int64
	Free    int64
}

// backupMore is "minenest backup auto ..." (the schedule) and "minenest backup keep|unkeep NAME".
func backupMore(c *client, f flags) error {
	id, err := pick(c, f.server(false))
	if err != nil {
		return err
	}
	what, args := f.pos[0], f.pos[1:]
	if what == "keep" || what == "unkeep" {
		if len(args) == 0 {
			return fmt.Errorf("say which backup (minenest backups lists them)")
		}
		if err := c.post(sp(id)+"/backups/"+url.PathEscape(args[0])+"/keep", map[string]bool{"keep": what == "keep"}, nil); err != nil {
			return err
		}
		if what == "keep" {
			fmt.Println("Kept. MineNest won't delete it by itself.")
		} else {
			fmt.Println("It's no longer kept, so it goes when it's among the oldest automatic ones.")
		}
		return nil
	}
	var r backupList
	if err := c.get(sp(id)+"/backups", &r); err != nil {
		return err
	}
	a := r.Auto.AutoBackup
	changed := false
	if len(args) > 0 {
		changed = true
		switch args[0] {
		case "off":
			a.Every = ""
		case "every":
			if len(args) < 2 {
				return fmt.Errorf("say how many hours, like: minenest backup auto every 6")
			}
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(args[1], "h"), "hours"))
			if err != nil {
				return fmt.Errorf("say how many hours, like: minenest backup auto every 6")
			}
			a.Every, a.Hours = "hours", n
		case "daily":
			if len(args) < 2 {
				return fmt.Errorf("say what time, like: minenest backup auto daily 03:30")
			}
			a.Every, a.At = "day", args[1]
		default:
			return fmt.Errorf("say off, every HOURS or daily HH:MM")
		}
	}
	if k := f.vals["keep"]; k != "" {
		n, err := strconv.Atoi(k)
		if err != nil || n < 1 || n > 1000 {
			return fmt.Errorf("--keep wants a number from 1 to 1000, like --keep 10")
		}
		a.Keep, changed = n, true
	}
	st := r.Auto
	if changed {
		if err := c.do("PUT", sp(id)+"/backups/auto", a, &st); err != nil {
			return err
		}
	}
	fmt.Println(autoWords(st))
	return nil
}

// autoWords says how automatic backups are set, when the last was and when the next is.
func autoWords(a nest.AutoStatus) string {
	var b strings.Builder
	if a.Every != "" {
		fmt.Fprintf(&b, "Automatic backups %s, keeping the newest %d.", a.EveryWords(), a.Keep)
	} else {
		b.WriteString("Automatic backups are off. Turn them on with: minenest backup auto every 6 (or daily 03:30)")
	}
	when := func(u int64) string { return time.Unix(u, 0).Format("Mon 2 Jan 15:04") }
	if a.Last > 0 {
		fmt.Fprintf(&b, "\n  %-10s %s, ", "Last", when(a.Last))
		switch a.Result {
		case "made":
			b.WriteString("made a backup")
		case "skipped":
			b.WriteString("skipped: " + a.Note)
		case "failed":
			b.WriteString("didn't work: " + a.Note)
		}
	}
	if a.Running {
		fmt.Fprintf(&b, "\n  %-10s %s", "Now", "making one")
	} else if a.Next > 0 {
		fmt.Fprintf(&b, "\n  %-10s %s", "Next", when(a.Next))
	}
	return b.String()
}

func isOn(v string) bool {
	switch strings.ToLower(v) {
	case "on", "yes", "true", "1", "enable", "enabled":
		return true
	}
	return false
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func confirm(q string) bool {
	return strings.HasPrefix(strings.ToLower(prompt(q+" [y/N] ")), "y")
}

func prompt(q string) string {
	fmt.Print(q)
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

func cut(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func count(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

var _ = filepath.Join

// RunQuiet is Run for minenest-gui, which has no console: errors go to a message box.
func RunQuiet(args []string) int {
	if err := run(args); err != nil {
		showError("MineNest: " + err.Error())
		return 1
	}
	return 0
}
