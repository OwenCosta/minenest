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
- **Automatic backups:** on the World page (or `minenest backup auto every 6`, or
  `daily 03:30`), every few hours or once a day, while the server runs too. MineNest keeps the
  newest ten automatic ones unless you say otherwise, and never deletes backups you made by hand
  or marked to keep (`minenest backup keep NAME`). It skips one, and says why, when nobody has
  played since the last one or the disk is nearly full. A backup missed while the computer was
  off is made soon after MineNest starts, and the server always goes back to saving its world
  afterwards, even when a backup fails. A daily one stays at its time of day whatever backups you
  make by hand, and a server can't be stopped by hand in the middle of a backup (stopping
  MineNest cancels the backups that are running first).
- **How much memory is left:** MineNest says how much of the computer's memory is still free
  for servers and what size of server fits ("enough for one more 4 GB server, or two 2 GB
  ones"), in the sidebar, on the New server page, in a server's Settings and in `minenest
  memory`. It counts what running servers are given and really use, leaves room for the
  operating system, and warns, without stopping you, before a server that wouldn't fit is made
  or started. Where a number can't be read, it says so.
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
minenest memory                              # how much memory is left for servers
minenest mods search "simple voice chat"
minenest mods add simple-voice-chat
minenest console                             # watch it and type commands
minenest internet playit                     # let friends on the internet join
minenest backup auto daily 03:30             # a backup every night, the newest ten kept
minenest files ls plugins                    # a server's own files: look, upload, download
minenest files put MyPlugin.jar --to plugins
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

Either way you get the whole dashboard, console included. Stopping or updating MineNest, these
settings, signing the account in or out, forgetting the playit.gg account and changing a
server's Java arguments stay on the computer it runs on. Through the website, files bigger than
16 MB can't be sent either way (the whole answer travels inside one message); on the home
network downloads have no limit.

Files are the exception to "the whole dashboard". Someone who can put a plugin in a server's
folder can run programs on your computer, so from another computer the Files page is **look
only**: browsing, reading, downloading. Uploading, editing, moving, deleting and unpacking, and
uploading a plugin or mod from the Mods page, only work from another computer after you turn on
**Let other computers change files** in the same settings (off unless you do). That includes
installing or updating a mod or plugin from Modrinth and bringing in a server from a zip. MineNest
checks this itself, not just the page. Looking for servers on the computer's own folders,
bringing one in by its path and Java's own arguments stay on the computer MineNest runs on even
with the switch on, because they can load a program too.

Reading is open to whoever is signed in, so from another computer someone can read a server's
`server.properties` (with its RCON password), plugin settings with tokens in them, and download a
whole server folder as a zip. Only sign in from computers you trust.

## Files

Each server has a **Files** page for its own folder (`servers/<name>/server`), and
`minenest files` does the same from a terminal:

```sh
minenest files ls plugins
minenest files cat server.properties
minenest files put Essentials.jar --to plugins     # then restart the server to load it
minenest files get world                            # a folder comes as a zip
minenest files mv plugins/old.yml plugins/new.yml
minenest files rm plugins/Old.jar                   # to the trash, not gone
minenest files trash                                # what rm moved; trash restore ID, trash empty
minenest files unzip packs/pack.zip
```

- Browse folders with their sizes and dates. Text files (properties, yml, yaml, json, toml, txt,
  conf, cfg, mcfunction, js, sk and md, up to 2 MB) open in an editor; saving keeps the file's
  line endings and encoding, writes through a temporary file so a crash never leaves half a file,
  and says so if something else changed the file since you opened it. Logs open read only, showing
  the newest lines, and can be downloaded whole. Other files can be downloaded, and a folder as
  a zip.
- Upload by dragging files onto the list or with the Upload button (up to 1 GB each, with a
  progress bar). A plugin or mod `.jar` in `plugins` or `mods` is fine, and the page tells you the
  server needs a restart to load it. Programs and scripts for Windows (`.exe`, `.bat`, `.cmd`,
  `.js`, `.url` and the like) are refused, whether they are uploaded, unpacked, copied or made by
  renaming something. `server.properties` can't be replaced by an upload, a move, a copy or an
  unzip: open it in the editor, where MineNest checks it. The Mods page's "Add a file" goes
  through the same upload, so it asks before replacing a file and waits while the server's files
  are busy.
- Rename, move and copy never replace something without asking. **Delete** moves things to a
  trash MineNest keeps for each server, outside the server's folder: you can put them back, and
  **Empty the trash** deletes for good. Anything that was replaced also goes there. The trash
  doesn't grow for ever: what is older than 30 days goes, and when it holds more than 5 GB the
  oldest go first (the Delete box and the World page say how much it holds). When some of
  several things can't be moved because the server is using them, the rest still go and the
  page lists the ones that stayed.
- **Unzip** unpacks a zip where it is, after checking all of it: anything that would land outside
  the folder, links, odd names, more than 50,000 files or more than 4 GB unpacked stops it before
  a single file is written.
- While the server is running, its world's region files, `session.lock`, `level.dat` and its own
  program can't be changed, and the page says why. `server.properties` is saved through the same
  checks as the Settings page, so the port can't be broken, and while players' real addresses
  are on, the port and address MineNest set are kept.
- It only reaches the server's own folder. MineNest's own files (its settings, keys, backups and
  the trash) are out of reach. Names are refused if they use `..`, a full path, a drive letter,
  a network path, `name:stream`, a Windows device name (`CON`, `NUL`, `COM1`...), a trailing dot
  or space, or odd control characters. A link or junction in the folder is shown as a link and
  never followed, for reading, writing, copying, zipping or deleting. Leftovers of an upload or
  an unzip that was cut off are cleaned up after an hour. Downloads are always sent
  as attachments, so an uploaded web page can't run from MineNest's address.

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
