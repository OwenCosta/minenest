package nest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What playit's agents print, as their source has it (playit-cloud/playit-agent).
var (
	// playitd 2.0.2 on Windows, given a file path for its control channel (what MineNest did):
	// it exits with status 1 at once.
	agentPipeFailure = []string{
		`2026-11-20T18:02:11.412093Z  INFO playitd::daemon: Starting playitd socket_path=Some("C:\\Users\\Tippy\\AppData\\Local\\MineNest\\playit\\playitd.sock") secret_path=Some("C:\\Users\\Tippy\\AppData\\Local\\MineNest\\playit\\playit.toml") version=2.0.2`,
		`playitd error: IPC error: Failed to bind to socket: not a named pipe path`,
	}
	// playitd 2.0.2 starting well.
	agentConnected = []string{
		`2026-11-20T18:05:40.100200Z  INFO playitd::daemon: Starting playitd socket_path=Some("\\\\.\\pipe\\minenest-playitd-1a2b3c4d") secret_path=Some("C:\\x\\playit.toml") version=2.0.2`,
		`2026-11-20T18:05:41.730010Z  INFO playitd::daemon: playit connected; tunnels loaded agent_id=6f1d2c3b-0a9e-4f8d-8c7b-6a5f4e3d2c1b tunnel_count=1 pending_tunnel_count=0 disabled_tunnel_count=0 account_status=Verified`,
	}
	// playit 0.17's program (the same Linux file name before 1.0) given playitd's options.
	agentTooOld = []string{
		`error: unexpected argument '--secret-path' found`,
		``,
		`  tip: a similar argument exists: '--secret_path'`,
		``,
		`Usage: playit --secret_path <PATH> [COMMAND]`,
	}
	agentNoNetwork = []string{
		`2026-11-20T18:07:02.000001Z ERROR playitd::daemon: failed to start playit agent error=FailedToConnect message=Could not connect to playit tunnel servers. Check your internet connection, firewall, VPN, or DNS settings, then restart playit. retry_in_secs=5`,
	}
	agentBadSecret = []string{
		`2026-11-20T18:08:00.000001Z  WARN playitd::daemon: configured agent secret is no longer valid error=ApiError(Auth(InvalidAgentKey))`,
		`2026-11-20T18:08:00.000002Z  INFO playitd::daemon: Waiting for frontend secret provisioning over IPC secret_path=C:\x\playit.toml`,
	}
	agentOverLimit = []string{
		`2026-11-20T18:09:00.000001Z  WARN playitd::daemon: agent disabled because the account is over the agent limit error=ApiFail("\"AgentDisabledOverLimit\"")`,
	}
)

func TestPlayitDiagnosis(t *testing.T) {
	for name, c := range map[string]struct {
		lines []string
		want  string
	}{
		"pipe": {agentPipeFailure, "pipe"}, "old": {agentTooOld, "oldagent"}, "network": {agentNoNetwork, "network"},
		"secret": {agentBadSecret, "secret"}, "limit": {agentOverLimit, "limit"},
	} {
		got := diagnosePlayit(c.lines, nil)
		if got != playitProblems[c.want] {
			t.Errorf("%s: %+v", name, got)
		}
	}
	if agentEvent(agentConnected[1]) != "connected" || agentEvent(agentConnected[0]) != "" {
		t.Fatal("connected line")
	}
	if !diagnosePlayit(agentTooOld, nil).Redownload {
		t.Fatal("an old agent should be fetched again")
	}
	d := diagnosePlayit([]string{"something else"}, fmt.Errorf("exit status 3"))
	if !strings.Contains(d.Note, "exit status 3") || d.Fix == "" {
		t.Fatal(d)
	}
	if d := diagnoseStart(fmt.Errorf("fork/exec playitd.exe: Operation did not complete successfully because the file contains a virus or potentially unwanted software."), `C:\x`); !strings.Contains(d.Note, "antivirus") || !d.Redownload {
		t.Fatal(d)
	}
}

func TestCleanAgentLine(t *testing.T) {
	key := strings.Repeat("ab12", 16)
	for _, in := range []string{"secret_key = \"" + key + "\"", "Authorization: Agent-Key " + key, "got " + key + " back"} {
		if out := cleanAgentLine(in); strings.Contains(out, key) || strings.Contains(out, "ab12ab12") {
			t.Fatalf("%q kept the key", out)
		}
	}
	if cleanAgentLine("\x1b[32m INFO\x1b[0m hello ") != "INFO hello" {
		t.Fatal(cleanAgentLine("\x1b[32m INFO\x1b[0m hello "))
	}
	// An agent's UUID isn't a secret and stays readable.
	if !strings.Contains(cleanAgentLine(agentConnected[1]), "6f1d2c3b-0a9e") {
		t.Fatal("agent id hidden")
	}
}

func TestPlayitSocket(t *testing.T) {
	w := playitSocket("windows", `C:\Users\Tippy\AppData\Local\MineNest\playit`)
	if !strings.HasPrefix(w, `\\.\pipe\minenest-playitd-`) {
		t.Fatal(w)
	}
	if playitSocket("windows", `C:\other`) == w {
		t.Fatal("two homes share a pipe")
	}
	if l := playitSocket("linux", "/home/tippy/.local/share/minenest/playit"); l != "/home/tippy/.local/share/minenest/playit/playitd.sock" && l != filepath.Join("/home/tippy/.local/share/minenest/playit", "playitd.sock") {
		t.Fatal(l)
	}
	if l := playitSocket("linux", "/"+strings.Repeat("x", 120)); !strings.HasPrefix(l, "@minenest-playitd-") {
		t.Fatal(l)
	}
}

func TestPickPlayitRelease(t *testing.T) {
	sum := "sha256:" + strings.Repeat("a", 64)
	raw := `[
	 {"tag_name":"v2.1.0","prerelease":true,"assets":[{"name":"playitd-windows-x86_64-signed.exe","browser_download_url":"u0","digest":"` + sum + `"}]},
	 {"tag_name":"v2.0.3","assets":[{"name":"playitd-windows-x86_64.exe","browser_download_url":"u1","digest":"` + sum + `"}]},
	 {"tag_name":"v2.0.2","assets":[{"name":"playitd-windows-x86_64-signed.exe","browser_download_url":"u2","digest":"` + sum + `"},{"name":"playit-linux-amd64","browser_download_url":"u3","digest":""}]},
	 {"tag_name":"v1.2.0","assets":[{"name":"playit-linux-amd64","browser_download_url":"u4","digest":"` + sum + `"}]},
	 {"tag_name":"v0.17.1","assets":[{"name":"playit-linux-aarch64","browser_download_url":"u5","digest":"` + sum + `"}]}
	]`
	var rels []playitGHRelease
	if err := json.Unmarshal([]byte(raw), &rels); err != nil {
		t.Fatal(err)
	}
	// The signed Windows agent isn't in 2.0.3 yet: 2.0.2's is used.
	if r, err := pickPlayitRelease(rels, playitAsset("windows", "amd64")); err != nil || r.Tag != "v2.0.2" || r.URL != "u2" || r.SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("%+v %v", r, err)
	}
	// No checksum in 2.0.2 for Linux: 1.2.0's.
	if r, err := pickPlayitRelease(rels, playitAsset("linux", "amd64")); err != nil || r.Tag != "v1.2.0" {
		t.Fatalf("%+v %v", r, err)
	}
	// Before 1.0 the same name was another program: never that.
	if _, err := pickPlayitRelease(rels, playitAsset("linux", "arm64")); err == nil {
		t.Fatal("took 0.17")
	}
	if playitAsset("darwin", "arm64") != "" {
		t.Fatal("no agent for macOS here")
	}
}

func TestPlayitTarget(t *testing.T) {
	srv := func(host string) (string, int, error) {
		if host == "cool-name.gl.joinmc.link" {
			return "147.185.221.20", 52311, nil
		}
		return "", 0, fmt.Errorf("none")
	}
	if h, p := playitTarget("cool-name.gl.joinmc.link", srv); h != "147.185.221.20" || p != 52311 {
		t.Fatal(h, p)
	}
	if h, p := playitTarget("147.185.221.20:40001", srv); h != "147.185.221.20" || p != 40001 {
		t.Fatal(h, p)
	}
	if h, p := playitTarget("plain.example", srv); h != "plain.example" || p != DefaultPort {
		t.Fatal(h, p)
	}
}

func TestParseRunData(t *testing.T) {
	raw := `{"agent_id":"6f1d2c3b-0a9e-4f8d-8c7b-6a5f4e3d2c1b",
	 "tunnels":[{"id":"x","internal_id":1,"name":"MineNest Survival","display_address":"cool-name.gl.joinmc.link","port_type":"tcp","port_count":1,
	   "tunnel_type":"minecraft-java","tunnel_type_display":"Minecraft Java","agent_config":{"fields":[{"name":"local_ip","value":"127.0.0.1"},{"name":"local_port","value":"25570"}]},"disabled_reason":null},
	  {"id":"y","internal_id":2,"name":"MineNest Survival","display_address":"147.185.221.20:19132","port_type":"udp","port_count":1,
	   "tunnel_type":"minecraft-bedrock","tunnel_type_display":"Minecraft Bedrock","agent_config":{"fields":[{"name":"local_port","value":"19132"}]},"disabled_reason":"over limit"}],
	 "pending":[{"id":"z","name":"MineNest Creative","tunnel_type":"minecraft-java","tunnel_type_display":"Minecraft Java","port_type":"tcp","port_count":1,"status_msg":"allocating"}],
	 "notices":[{"priority":"High","message":"Verify your email","resolve_link":"https://playit.gg/account"},{"priority":"Low","message":"tip","resolve_link":null}],
	 "permissions":{"is_self_managed":false,"has_premium":false,"account_status":"email-not-verified"}}`
	rd := parseRunData(json.RawMessage(raw))
	if len(rd.tunnels) != 2 || rd.tunnels[0].Port != 25570 || rd.tunnels[0].Address != "cool-name.gl.joinmc.link" || rd.tunnels[1].Off != "over limit" || rd.tunnels[1].Port != 19132 {
		t.Fatalf("%+v", rd.tunnels)
	}
	if len(rd.pending) != 1 || rd.pending[0] != "MineNest Creative" || len(rd.notices) != 1 || !strings.Contains(rd.notices[0], "https://playit.gg/account") {
		t.Fatalf("%+v", rd)
	}
}

// A stand-in for playitd: the test program itself, started with MINENEST_FAKE_PLAYITD saying
// which of the samples above to print.
func TestMain(m *testing.M) {
	if which := os.Getenv("MINENEST_FAKE_PLAYITD"); which != "" {
		lines := map[string][]string{"pipe": agentPipeFailure, "connected": agentConnected, "network": agentNoNetwork}[which]
		for _, l := range lines {
			fmt.Fprintln(os.Stderr, l)
		}
		if which == "pipe" {
			os.Exit(1)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPlayitAgentStates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MINENEST_HOME", home)
	os.MkdirAll(filepath.Join(home, "playit"), 0o755)
	exe, _ := os.Executable()
	p := newPlayit(nil)
	s := &Server{ID: "test", Dir: t.TempDir(), state: Running}
	p.users[s] = true
	p.state = "on"
	p.tunnels = []playitTunnel{{Type: "minecraft-java", Address: "cool-name.gl.joinmc.link", Port: DefaultPort}}
	wait := func(cond func() bool) {
		for i := 0; i < 100 && !cond(); i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}

	// It stops at once: the page says why, with its last lines and what to do, not "Connected".
	t.Setenv("MINENEST_FAKE_PLAYITD", "pipe")
	if _, err := p.startAgent(exe); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.fails > 0 })
	st := p.statusFor(s)
	if st.Up || st.State != "problem" || st.Agent != "stopped" || st.Note != playitProblems["pipe"].Note || st.Fix == "" || len(st.Log) != 2 {
		t.Fatalf("%+v", st)
	}
	said := ""
	for _, l := range s.Lines(0) {
		said += l.Text + "\n"
	}
	if !strings.Contains(said, "playit.gg's agent stopped: ") || !strings.Contains(said, "not a named pipe path") {
		t.Fatalf("console: %s", said)
	}
	// It waits before starting again, and Try again clears that.
	if wait, err := p.ensureAgent(t.Context()); err == nil || wait <= 0 {
		t.Fatal("started again at once", err)
	}
	p.restart()
	if st := p.statusFor(s); st.State == "problem" && st.Note == playitProblems["pipe"].Note {
		t.Fatalf("Try again didn't clear the problem: %+v", st)
	}

	// Running but not reaching playit.gg: a problem while it keeps trying.
	t.Setenv("MINENEST_FAKE_PLAYITD", "network")
	if _, err := p.startAgent(exe); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.diag != nil })
	if st := p.statusFor(s); st.State != "problem" || st.Agent != "running" || st.Note != playitProblems["network"].Note {
		t.Fatalf("%+v", st)
	}
	p.stopAgent()

	// Connected: up, with the address.
	t.Setenv("MINENEST_FAKE_PLAYITD", "connected")
	if _, err := p.startAgent(exe); err != nil {
		t.Fatal(err)
	}
	defer p.stopAgent()
	if st := p.statusFor(s); st.Up || st.State != "connecting" {
		t.Fatalf("up before connecting: %+v", st)
	}
	wait(func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.connected })
	if st := p.statusFor(s); !st.Up || st.Agent != "connected" || st.Address != "cool-name.gl.joinmc.link" || st.CheckOK {
		t.Fatalf("%+v", st)
	}
}
