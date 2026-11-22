# MineNest

Your own Minecraft server in a couple of clicks, on Windows and Linux. MineNest downloads the
server and the right Java for you, installs mods and plugins from Modrinth with everything they
need, keeps backups, and gives friends on the internet a way in, all from one window or the
command line.

- **Every kind of server:** Paper, Purpur, Fabric, NeoForge, Forge, or Minecraft's own
  (vanilla), any release. Switch type or version later; the world is backed up first.
- **Mods, plugins and data packs made easy:** search Modrinth from MineNest. Only ones that work
  on your server's loader and Minecraft version show up, and the add-ons they need are installed
  with them. One click updates everything.
- **Modpacks:** pick a Modrinth modpack and MineNest builds the whole server: loader, mods,
  settings.
- **Java handled:** the Java each Minecraft version needs (8 to 25) comes from Eclipse Temurin,
  checked, kept per version.
- **Runs in the background:** close the window and the servers keep going. Start servers with
  MineNest, and MineNest when you sign in. A crashed server starts again by itself, and MineNest
  tells you why it crashed in plain words.
- **Friends on the internet:** playit.gg (free, no router changes), your own VPS (an SSH
  tunnel), or the **supporter relay**: supporters on Patreon get their own address, like
  `brave-otter.mc.owencosta.diy`, with nothing to set up. Each way is checked from outside, so you
  know friends can really get in. If playit.gg's agent has trouble, the Internet page shows what
  it said last, what to do, and a **Try again** button (`minenest internet retry`).
- **Bedrock players** (phones, consoles, Windows) through GeyserMC's Geyser and Floodgate.
- **Players, settings, worlds:** whitelist, operators, bans, kicks; every server.properties
  setting with friendly names; backups that work while the server runs; put one back or start a
  new world. MineNest looks a name up with Mojang itself, tries again when Mojang's servers are
  busy, and the Players page says what really happened. Bedrock players go by their Floodgate
  name, like `.Steve`.
- **Bring the servers you already have:** MineNest finds the ones on your computer, or you point
  it at a folder or upload a zip of one (**New server**, then **Add one you already have**, or
  `minenest import FOLDER`). It's copied in with its world, mods, plugins and settings, and the
  original is left as it was.

## Install

**Windows** (10 or 11), in PowerShell:

```powershell
[Net.ServicePointManager]::SecurityProtocol = 'Tls12'; irm https://github.com/OwenCosta/minenest/releases/latest/download/install.ps1 | iex
```

MineNest goes in your user's programs folder (no administrator needed), gets a Start menu
shortcut, and opens.

**Linux** (x86-64 or ARM64):

```sh
curl -fsSL https://github.com/OwenCosta/minenest/releases/latest/download/install.sh | sh
```

Or download the program for your computer from the
[latest release](https://github.com/OwenCosta/minenest/releases/latest) and run it. It's one
file with nothing else to install. `minenest update` gets newer versions.

## Use it

Open **MineNest** from the Start menu or your apps (or run `minenest gui`), choose
**New server**, pick a type, and start it. The first time, you agree to Minecraft's EULA
yourself; MineNest never agrees for you.

From a terminal:

```sh
minenest create "Survival" -t paper          # or fabric, neoforge, forge, purpur, vanilla
minenest eula                                # read and agree to Minecraft's EULA
minenest start
minenest status                              # where friends join
minenest mods search "simple voice chat"
minenest mods add simple-voice-chat
minenest console                             # watch it and type commands
minenest internet playit                     # let friends on the internet join
minenest create "Cobblemon" --modpack cobblemon-fabric
```

`minenest help` lists everything. With more than one server, add `-s NAME`.

## The supporter relay

Supporters on [Patreon](https://www.patreon.com/) get a name of their own for their server,
which friends type in Minecraft, without playit.gg, a VPS or router changes:

1. On MineNest's **Account** page choose **Sign in with your OCAccount** (or run
   `minenest account login`). Your browser opens your OCAccount at owencosta.diy.
2. On your OCAccount, link your Patreon, and link (or make) your XenithOS profile.
3. Back in MineNest, pick a name, and choose **Supporter relay** on the server's Internet page.

With Paper or Purpur, players' real addresses come through too, so bans by address work. A
XenithOS profile's name and PIN still work instead of an OCAccount (`minenest account login --pin`).

## Use it from another computer

On **Settings and account**, two switches, both off until you turn them on:

- **On your home network:** set a password, turn it on, and open the address MineNest shows
  (like `http://192.168.1.20:47576/`) on any computer or phone at home. Only computers on private
  networks are let in, and only with the password.
- **From anywhere, through the website** (a perk for supporters on Patreon): turn it on, then open
  `https://cloud.owencosta.diy/remote/` anywhere and sign in with the same OCAccount. MineNest keeps
  a connection out to XenithOS Cloud, which passes your browser's requests to it, so nothing has
  to be opened on your router.

Either way you get the whole dashboard, console included. Stopping MineNest itself and these
settings stay on the computer it runs on.

## Updates

When there's a newer MineNest, the sidebar says so: **Update now** downloads it from GitHub,
checks it against the release's checksums, puts it in place, and restarts MineNest, starting the
servers that were running again. `minenest update` does the same from a terminal.

## How it works

`minenest` is a single program. Its background service (`minenest daemon`, started by itself
when needed) runs the servers and answers on `127.0.0.1:47575` only; the window and the command
line both drive it, with a key in MineNest's folder that only your user can read. The window is
a page that service serves, opened in an app window of Microsoft Edge on Windows or a Chromium
browser on Linux.

Everything lives in `%LOCALAPPDATA%\MineNest` on Windows and `~/.local/share/minenest` on Linux
(or `MINENEST_HOME`): each server in `servers/<name>/server`, its backups beside it, Java in
`java/`.

MineNest is part of [XenithOS](https://github.com/OwenCosta/XenithOS), where it's the
Minecraft server app, and works on any Windows or Linux PC on its own.

## Build it

```sh
go build ./cmd/minenest                                  # the program
go build -ldflags "-H windowsgui" ./cmd/minenest-gui     # Windows: the Start menu one, no console
go test ./...
```

Pushing a tag like `v0.1.0` builds every platform and publishes the release (GitHub Actions).

## What it downloads, and from where

MineNest doesn't include Minecraft or anything else it downloads: each comes from its maker,
checked against the checksum they publish where they publish one. Minecraft's server from
Mojang (under [Minecraft's EULA](https://aka.ms/MinecraftEULA)), Paper and Purpur from their
projects, Fabric, Forge and NeoForge from theirs, Java from Eclipse Temurin (Adoptium), mods,
plugins and modpacks from [Modrinth](https://modrinth.com) (made by their authors, under their
own licences), Geyser and Floodgate from GeyserMC, and playit.gg's agent from playit.gg.

## Licence

GNU General Public License v3.0. See [LICENSE](LICENSE).
