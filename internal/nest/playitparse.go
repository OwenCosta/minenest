package nest

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Reading playit's agent. playitd (playit 1.0 and newer, 2.0.2 when this was written) prints
// tracing lines on its error output, like
//
//	2026-11-20T18:02:13.004211Z  INFO playitd::daemon: playit connected; tunnels loaded agent_id=... tunnel_count=1
//
// and, when it can't start at all, one plain line before it exits with status 1, like
//
//	playitd error: IPC error: Failed to bind to socket: not a named pipe path
//
// The playit program before 1.0 (0.15 to 0.17) was a different program under the same Linux
// file names, with a window of its own and other options; given playitd's options it stops with
// "error: unexpected argument '--secret-path' found". MineNest never needs to read a claim link
// from the agent: it asks playit.gg for the claim itself.

type playitDiag struct {
	Note       string // what's wrong, in plain words
	Fix        string // what to do next
	Redownload bool   // the agent file is wrong or broken: fetch it again
}

var playitProblems = map[string]playitDiag{
	"pipe": {
		Note: "playit.gg's agent couldn't open its control channel, so it stopped as soon as it started.",
		Fix:  "Choose Try again. If it keeps happening, update MineNest.",
	},
	"already": {
		Note: "Another copy of playit.gg's agent from MineNest is still running.",
		Fix:  "Restart the computer (or end playitd in Task Manager), then choose Try again.",
	},
	"oldagent": {
		Note:       "The playit.gg agent MineNest had is an older kind that works differently.",
		Fix:        "Choose Try again: MineNest downloads the current agent.",
		Redownload: true,
	},
	"secret": {
		Note: "playit.gg doesn't accept this computer's key any more.",
		Fix:  "Choose Forget playit.gg, then start the server again to connect this computer to your playit.gg account.",
	},
	"limit": {
		Note: "Your playit.gg account has as many agents as it may have.",
		Fix:  "Delete an agent you don't use any more on playit.gg, then choose Try again.",
	},
	"network": {
		Note: "playit.gg's agent can't reach playit.gg's tunnel servers.",
		Fix:  "Check that this computer is online. A firewall, VPN or school or work network may block it: playitd needs to send UDP to playit.gg. Then choose Try again.",
	},
	"api": {
		Note: "playit.gg's agent can't reach playit.gg.",
		Fix:  "Check that this computer is online, then choose Try again.",
	},
	"rejected": {
		Note: "playit.gg turned the agent away when it started.",
		Fix:  "Look at your account on playit.gg (a notice there says why), then choose Try again.",
	},
}

var playitEvents = []struct {
	event string
	re    *regexp.Regexp
}{
	{"connected", regexp.MustCompile(`playit connected; tunnels loaded|playit tunnel state updated`)},
	{"pipe", regexp.MustCompile(`(?i)not a named pipe path|Failed to bind to socket`)},
	{"already", regexp.MustCompile(`Another instance is already running`)},
	{"oldagent", regexp.MustCompile(`(?i)unexpected argument|Found argument '.*' which wasn't expected|unrecognized option|Usage: playit`)},
	{"secret", regexp.MustCompile(`(?i)secret is no longer valid|Invalid secret file|Waiting for frontend secret provisioning|InvalidAgentKey|Secret error`)},
	{"limit", regexp.MustCompile(`(?i)over the agent limit`)},
	{"network", regexp.MustCompile(`Could not connect to playit tunnel servers|FailedToConnect|failed to ping tunnel server`)},
	{"api", regexp.MustCompile(`Could not reach the playit API`)},
	{"rejected", regexp.MustCompile(`rejected the agent`)},
}

// agentEvent says what one of the agent's lines means: "connected", one of playitProblems'
// names, or "" for nothing in particular.
func agentEvent(line string) string {
	for _, e := range playitEvents {
		if e.re.MatchString(line) {
			return e.event
		}
	}
	return ""
}

var (
	longHexRe   = regexp.MustCompile(`[0-9a-fA-F]{32,}`)
	secretKeyRe = regexp.MustCompile(`(?i)(secret[_ -]?key\s*[=:]\s*|Agent-Key\s+)\S+`)
)

// cleanAgentLine makes one of the agent's lines fit to keep and show: no colours, no secrets.
func cleanAgentLine(s string) string {
	s = strings.TrimSpace(ansiRe.ReplaceAllString(s, ""))
	s = secretKeyRe.ReplaceAllString(s, "${1}(hidden)")
	s = longHexRe.ReplaceAllString(s, "(hidden)")
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

// diagnosePlayit says why the agent stopped, from its last lines and how it exited.
func diagnosePlayit(tail []string, err error) playitDiag {
	for i := len(tail) - 1; i >= 0; i-- {
		if d, ok := playitProblems[agentEvent(tail[i])]; ok {
			return d
		}
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	switch uint32(code) {
	case 0xC0000135, 0xC0000142: // a DLL it needs is missing, or it couldn't start up
		return playitDiag{Note: "Windows couldn't start playit.gg's agent.", Fix: "Choose Try again. If it keeps happening, restart the computer.", Redownload: true}
	case 0xC0000005, 0xC0000409: // it crashed
		return playitDiag{Note: "playit.gg's agent crashed.", Fix: "Choose Try again: MineNest downloads it again first.", Redownload: true}
	}
	note := "playit.gg's agent stopped"
	if err != nil {
		note += " (" + err.Error() + ")"
	}
	return playitDiag{Note: note + ".", Fix: "Choose Try again. If it keeps stopping, the lines below say what it said last."}
}

// diagnoseStart says why the agent couldn't be started at all.
func diagnoseStart(err error, dir string) playitDiag {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "virus") || strings.Contains(s, "potentially unwanted"):
		return playitDiag{Note: "Your antivirus stopped playit.gg's agent from running.",
			Fix: fmt.Sprintf("Allow playitd in your antivirus (it's in %s), then choose Try again.", dir), Redownload: true}
	case strings.Contains(s, "access is denied") || strings.Contains(s, "permission denied"):
		return playitDiag{Note: "playit.gg's agent wasn't allowed to run (" + err.Error() + ").",
			Fix: fmt.Sprintf("An antivirus may be holding it back: allow playitd (in %s), then choose Try again.", dir)}
	case strings.Contains(s, "not a valid win32") || strings.Contains(s, "exec format error") || strings.Contains(s, "not compatible"):
		return playitDiag{Note: "playit.gg's agent file is broken or for another kind of computer.", Fix: "Choose Try again: MineNest downloads it again.", Redownload: true}
	case strings.Contains(s, "cannot find") || strings.Contains(s, "no such file") || strings.Contains(s, "not exist"):
		return playitDiag{Note: "playit.gg's agent has gone missing (an antivirus may have taken it away).", Fix: "Choose Try again: MineNest downloads it again. If it goes again, allow playitd in your antivirus.", Redownload: true}
	}
	return playitDiag{Note: "playit.gg's agent didn't start: " + err.Error(), Fix: "Choose Try again."}
}
