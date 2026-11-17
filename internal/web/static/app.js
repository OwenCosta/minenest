"use strict";
// MineNest's GUI. It talks to the daemon's API on this computer; everything is built with h()
// so text from servers, players and Modrinth is never read as HTML.

const $ = (id) => document.getElementById(id);
const S = { info: null, servers: [], cur: null, tab: "overview", seq: 0, consoleFor: null, watching: new Set(), status: null, view: "server" };

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "value") el.value = v;
    else if (k === "checked") el.checked = !!v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}
const fill = (el, ...kids) => { el.replaceChildren(...kids.flat(Infinity).filter((k) => k != null && k !== false)); return el; };

async function api(method, path, body, raw) {
  const opt = { method, headers: { "X-MineNest": "1" } };
  if (body !== undefined) {
    if (raw) opt.body = body;
    else { opt.body = JSON.stringify(body); opt.headers["Content-Type"] = "application/json"; }
  }
  const r = await fetch(path, opt);
  let data = null;
  try { data = await r.json(); } catch (e) {}
  if (!r.ok) throw new Error((data && data.error) || `MineNest answered ${r.status}`);
  return data;
}
const sp = (p = "") => `/api/servers/${encodeURIComponent(S.cur)}${p}`;

function toast(msg, bad) {
  const t = h("div", { class: "toast" + (bad ? " bad" : "") }, msg);
  $("toasts").append(t);
  setTimeout(() => t.remove(), bad ? 7000 : 3500);
}
async function act(fn, okMsg) {
  try {
    const r = await fn();
    if (r && r.job) watchJob(r.job);
    if (okMsg) toast(okMsg);
    refresh();
    return r;
  } catch (e) { toast(e.message, true); return null; }
}

// ------------------------------------------------------------------ jobs

function watchJob(id) { S.watching.add(id); pollJobs(); }
async function pollJobs() {
  const box = $("jobs");
  for (const id of [...S.watching]) {
    let j;
    try { j = await api("GET", "/api/jobs/" + id); } catch (e) { S.watching.delete(id); continue; }
    let el = document.querySelector(`[data-job="${id}"]`);
    if (!el) {
      el = h("div", { class: "job", "data-job": id }, h("div", { class: "t" }), h("div", { class: "s" }), h("div", { class: "bar" }, h("div")));
      box.append(el);
    }
    el.querySelector(".t").textContent = j.title;
    el.querySelector(".s").textContent = j.error || j.step;
    const bar = el.querySelector(".bar");
    bar.classList.toggle("busy", !j.total && !j.finished);
    bar.firstChild.style.width = j.finished ? "100%" : j.total ? `${Math.round((100 * j.done) / j.total)}%` : "35%";
    if (j.finished) {
      S.watching.delete(id);
      if (j.error) { toast(j.title + ": " + j.error, true); el.remove(); }
      else { setTimeout(() => el.remove(), 1500); }
      refresh();
    }
  }
}
setInterval(() => { if (S.watching.size) pollJobs(); }, 600);

// ------------------------------------------------------------------ refresh

async function refresh() {
  try {
    S.servers = await api("GET", "/api/servers");
  } catch (e) { return; }
  renderSide();
  if (S.view === "account") return;
  if (!S.servers.length) { show("empty"); return; }
  if (!S.cur || !S.servers.find((s) => s.id === S.cur)) {
    S.cur = (localStorage.getItem("mn-cur") && S.servers.find((s) => s.id === localStorage.getItem("mn-cur"))) ? localStorage.getItem("mn-cur") : S.servers[0].id;
  }
  S.status = S.servers.find((s) => s.id === S.cur);
  show("server");
  renderHead();
  if (["overview", "internet"].includes(S.tab)) renderTab(false);
}
function show(which) {
  for (const id of ["empty", "server", "account"]) $(id).classList.toggle("hidden", id !== which);
}

function renderSide() {
  fill($("server-list"), S.servers.map((s) =>
    h("button", { class: "srv-item" + (s.id === S.cur && S.view === "server" ? " on" : ""), onclick: () => pickServer(s.id) },
      h("span", { class: "dot " + s.state }),
      h("span", {}, h("div", { class: "t" }, s.name), h("div", { class: "s" }, s.installed ? `${s.flavor_name} ${s.minecraft}` : s.busy ? "Setting up..." : "Not installed"))
    )));
}
function pickServer(id) {
  S.cur = id; S.view = "server"; localStorage.setItem("mn-cur", id);
  S.seq = 0; S.consoleFor = null; $("console").replaceChildren();
  refresh().then(() => renderTab(true));
}

function renderHead() {
  const s = S.status;
  $("srv-name").textContent = s.name;
  $("srv-sub").textContent = s.installed ? `${s.flavor_name} ${s.minecraft}${s.modpack ? " · " + s.modpack : ""} · port ${s.port} · ${s.ram_mb} MB` : (s.busy ? "Setting up..." : "The server software isn't installed");
  const st = $("srv-state");
  st.textContent = s.busy && s.state === "stopped" ? "busy" : s.state;
  st.className = "pill " + s.state;
  $("btn-start").disabled = s.state !== "stopped" || !s.installed || !!s.busy;
  $("btn-stop").disabled = s.state === "stopped";
  $("btn-restart").disabled = s.state !== "running";
  const kind = { plugin: "Plugins", mod: "Mods", datapack: "Data packs" }[s.addon_kind] || "Mods";
  $("tab-addons-btn").textContent = kind;
}

// ------------------------------------------------------------------ tabs

function setTab(t) {
  S.tab = t;
  for (const b of $("tabs").children) b.classList.toggle("on", b.dataset.tab === t);
  for (const sec of document.querySelectorAll(".tab")) sec.classList.toggle("hidden", sec.id !== "tab-" + t);
  renderTab(true);
}
function renderTab(full) {
  const s = S.status;
  if (!s) return;
  ({ overview: tabOverview, console: tabConsole, addons: tabAddons, players: tabPlayers, settings: tabSettings, world: tabWorld, internet: tabInternet })[S.tab](s, full);
}

function copyBtn(text) {
  return h("button", { class: "btn small", onclick: () => { navigator.clipboard.writeText(text); toast("Copied " + text); } }, "Copy");
}
function addrRow(label, value) {
  return h("div", { class: "addr" }, h("span", { class: "lbl" }, label), h("code", {}, value), copyBtn(value));
}
function uptime(sec) {
  const hh = Math.floor(sec / 3600), mm = Math.floor((sec % 3600) / 60);
  return hh ? `${hh} h ${mm} min` : `${mm} min`;
}

function tabOverview(s) {
  const el = $("tab-overview");
  const notices = [];
  if (s.busy) notices.push(h("div", { class: "notice" }, `MineNest is busy with ${s.busy}. Progress shows at the bottom right.`));
  if (s.installed && s.eula_needed) notices.push(h("div", { class: "notice warn" }, "Before the server can start, you need to agree to Minecraft's EULA. ", h("button", { class: "link", onclick: eulaDialog }, "Read and agree")));
  if (s.whitelist_on && !s.whitelist_count) notices.push(h("div", { class: "notice warn" }, "Only players on the whitelist can join, and it's empty. ", h("button", { class: "link", onclick: () => setTab("players") }, "Add players"), " or turn the whitelist off there."));
  if (s.why && s.state === "stopped") notices.push(h("div", { class: "notice bad" }, "Last time it stopped by itself: " + s.why));
  const way = s.way || {};
  const wayName = { "": "Only at home", playit: "playit.gg", vps: "Your VPS", relay: "Supporter relay" }[way.kind || ""];
  fill(el,
    notices,
    h("div", { class: "grid" },
      h("div", { class: "card" }, h("h2", {}, "Where to join"),
        addrRow("On this computer", s.addresses.here),
        s.addresses.home ? addrRow("At home", s.addresses.home) : null,
        s.addresses.public ? addrRow("On the internet", s.addresses.public) : h("div", { class: "muted", style: "margin-top:8px" },
          way.kind ? (s.state === "running" ? (way.note || "Connecting...") : `${wayName} connects when the server starts.`) : "Friends elsewhere can't join yet. ",
          way.kind ? null : h("button", { class: "link", onclick: () => setTab("internet") }, "Let people on the internet join")),
        s.bedrock_on ? h("div", { class: "muted", style: "margin-top:8px" }, `Bedrock players: the same address, port 19132${way.bedrock ? ` (internet: ${way.bedrock})` : ""}.`) : null,
        way.claim_url && s.state === "running" ? h("div", { class: "notice warn" }, "Connect this computer to your playit.gg account: ", h("a", { href: way.claim_url, target: "_blank", rel: "noopener" }, way.claim_url)) : null),
      h("div", { class: "card" }, h("h2", {}, `Players (${s.players.length}/${s.max_players})`),
        s.players.length ? h("div", { class: "chips" }, s.players.map((p) => h("span", { class: "chip" }, p, h("button", { title: "Kick", onclick: () => playerAct("kick", p) }, "×"))))
          : h("div", { class: "muted" }, s.state === "running" ? "Nobody is on right now." : "The server isn't running."),
        h("h3", {}, "Server"),
        h("div", { class: "kv" },
          h("div", {}, "State"), h("div", {}, s.state + (s.uptime ? ` for ${uptime(s.uptime)}` : "")),
          h("div", {}, "Software"), h("div", {}, s.installed ? `${s.flavor_name} ${s.minecraft}${s.build ? " (build " + s.build + ")" : ""}, Java ${s.java}` : "Not installed"),
          h("div", {}, "Memory"), h("div", {}, `${s.ram_mb} MB`),
          h("div", {}, "Internet"), h("div", {}, wayName + (way.state && way.kind ? ` (${way.state})` : "")),
          h("div", {}, "Folder"), h("div", { style: "word-break:break-all" }, s.folder)))
    ));
}

// ------------------------------------------------------------------ console

let consoleLoop = 0;
async function tabConsole(s, full) {
  if (!full) return;
  if (S.consoleFor !== s.id) { S.consoleFor = s.id; S.seq = 0; $("console").replaceChildren(); }
  const my = ++consoleLoop;
  $("console-cmd").focus();
  let first = true;
  while (my === consoleLoop && S.tab === "console" && S.view === "server") {
    let r;
    try { r = await api("GET", sp(`/console?since=${S.seq}&wait=${first ? 0 : 1}`)); } catch (e) { await new Promise((ok) => setTimeout(ok, 2000)); continue; }
    first = false;
    if (my !== consoleLoop) return;
    const box = $("console");
    const atEnd = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
    for (const l of r.lines) {
      box.append(h("div", { class: l.kind }, l.text));
      S.seq = l.seq;
    }
    while (box.childElementCount > 2000) box.firstChild.remove();
    if (atEnd || r.lines.length > 50) box.scrollTop = box.scrollHeight;
  }
}

// ------------------------------------------------------------------ add-ons

const kindWord = (k) => ({ plugin: "plugin", mod: "mod", datapack: "data pack" }[k] || "mod");
let addonQuery = "";
async function tabAddons(s, full) {
  if (!full) return;
  const el = $("tab-addons");
  if (!s.installed) { fill(el, h("div", { class: "notice" }, "Install the server software first.")); return; }
  const kw = kindWord(s.addon_kind);
  const results = h("div", { class: "hits" }, h("div", { class: "muted" }, "Searching Modrinth..."));
  const installed = h("div", {}, h("div", { class: "muted" }, "Loading..."));
  const q = h("input", { placeholder: `Search ${kw}s on Modrinth, like "${s.addon_kind === "plugin" ? "EssentialsX" : s.addon_kind === "datapack" ? "terralith" : "sodium"}"`, value: addonQuery });
  const search = async () => {
    addonQuery = q.value;
    fill(results, h("div", { class: "muted" }, "Searching Modrinth..."));
    try {
      const r = await api("GET", sp("/addons/search?q=" + encodeURIComponent(q.value)));
      fill(results, r.hits.length ? r.hits.map((hit) => hitRow(hit, hit.installed ? h("span", { class: "muted" }, "Installed") :
        h("button", { class: "btn primary small", onclick: async (e) => { e.target.disabled = true; const j = await act(() => api("POST", sp("/addons"), { project: hit.id })); if (j) setTimeout(() => tabAddons(S.status, true), 2500); } }, "Install"))) :
        h("div", { class: "muted" }, `Nothing on Modrinth fits that search for ${s.flavor_name} ${s.minecraft}.`));
    } catch (e) { fill(results, h("div", { class: "notice bad" }, e.message)); }
  };
  q.addEventListener("keydown", (e) => { if (e.key === "Enter") search(); });
  const file = h("input", { type: "file", accept: ".jar,.zip", class: "hidden", onchange: async () => {
    for (const f of file.files) await act(() => api("POST", sp("/addons/upload?name=" + encodeURIComponent(f.name)), f, true), "Added " + f.name);
    tabAddons(S.status, true);
  } });
  fill(el,
    s.state !== "stopped" ? h("div", { class: "notice" }, `Changes to ${kw}s count once the server restarts.`) : null,
    h("div", { class: "card" }, h("h2", {}, `Installed ${kw}s`), installed,
      h("div", { class: "row", style: "margin-top:10px" },
        h("button", { class: "btn", onclick: async () => { const r = await act(() => api("POST", sp("/addons/check"))); if (r) { const n = r.filter((a) => a.update).length; toast(n ? `${n} can be updated.` : "Everything is up to date."); loadInstalled(); } } }, "Check for updates"),
        h("button", { class: "btn", onclick: () => act(() => api("POST", sp("/addons/update"), {})).then(() => setTimeout(loadInstalled, 3000)) }, "Update all"),
        h("button", { class: "btn", onclick: () => file.click() }, "Add a file..."), file)),
    h("div", { class: "card" }, h("h2", {}, `Find ${kw}s`),
      h("div", { class: "row" }, h("div", { class: "grow" }, q), h("button", { class: "btn primary", onclick: search }, "Search")),
      h("div", { class: "muted", style: "margin:6px 0 10px" }, `Only ${kw}s that work on ${s.flavor_name} ${s.minecraft} show up. What they need is installed with them.`),
      results));
  async function loadInstalled() {
    try {
      const l = await api("GET", sp("/addons"));
      const rows = l.addons.map((a) => h("div", { class: "list-row" },
        a.icon ? h("img", { src: a.icon, width: 28, height: 28, style: "border-radius:6px", alt: "" }) : null,
        h("div", { class: "grow" }, h("b", {}, a.title), " ", h("span", { class: "muted" }, a.version + (a.needed ? " · needed by another" : ""))),
        a.update ? h("button", { class: "btn small primary", onclick: () => act(() => api("POST", sp("/addons/update"), { project: a.project })).then(() => setTimeout(loadInstalled, 3000)) }, "Update to " + a.update) : null,
        h("button", { class: "btn small danger", onclick: () => act(() => api("DELETE", sp("/addons/" + encodeURIComponent(a.project))), "Removed " + a.title).then(loadInstalled) }, "Remove")));
      const other = l.other.map((n) => h("div", { class: "list-row" }, h("div", { class: "grow" }, n, " ", h("span", { class: "muted" }, "· added by hand or by a modpack")),
        h("button", { class: "btn small danger", onclick: () => confirmDialog(`Remove ${n}?`, "It's deleted from the server's folder.", "Remove", () => act(() => api("DELETE", sp("/addons/" + encodeURIComponent(n))), "Removed " + n).then(loadInstalled)) }, "Remove")));
      fill(installed, rows.length || other.length ? [rows, other] : h("div", { class: "muted" }, `No ${kw}s yet. Find some below.`));
    } catch (e) { fill(installed, h("div", { class: "notice bad" }, e.message)); }
  }
  loadInstalled();
  search();
}
function hitRow(hit, action) {
  return h("div", { class: "hit" },
    hit.icon ? h("img", { src: hit.icon, alt: "", loading: "lazy" }) : h("div", { class: "noicon" }),
    h("div", { class: "body" }, h("div", { class: "title" }, hit.title, " ", h("span", { class: "muted" }, "by " + hit.author)),
      h("div", { class: "desc", title: hit.description }, hit.description),
      h("div", { class: "muted", style: "font-size:12px" }, `${fmtCount(hit.downloads)} downloads · `, h("a", { href: `https://modrinth.com/project/${hit.slug}`, target: "_blank", rel: "noopener" }, "Modrinth page"))),
    action);
}
function fmtCount(n) { return n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? Math.round(n / 1e3) + "k" : String(n); }

// ------------------------------------------------------------------ players

function playerAct(action, name, reason) {
  return act(() => api("POST", sp("/players"), { action, name, reason }), "Done.").then(() => { if (S.tab === "players") tabPlayers(S.status, true); });
}
async function tabPlayers(s, full) {
  if (!full) return;
  const el = $("tab-players");
  let p;
  try { p = await api("GET", sp("/players")); } catch (e) { fill(el, h("div", { class: "notice bad" }, e.message)); return; }
  const name = h("input", { placeholder: "A player's Minecraft name" });
  const list = (title, names, remove, help) => h("div", { class: "card" }, h("h2", {}, title), help ? h("div", { class: "muted", style: "margin-bottom:8px" }, help) : null,
    names.length ? h("div", { class: "chips" }, names.map((n) => h("span", { class: "chip" }, n, remove ? h("button", { title: "Remove", onclick: () => playerAct(remove, n) }, "×") : null))) : h("div", { class: "muted" }, "Nobody."));
  fill(el,
    h("div", { class: "card" }, h("h2", {}, "Add a player"),
      h("div", { class: "row" }, h("div", { class: "grow" }, name),
        h("button", { class: "btn", onclick: () => playerAct("allow", name.value) }, "Allow on whitelist"),
        h("button", { class: "btn", onclick: () => playerAct("op", name.value) }, "Make operator"),
        h("button", { class: "btn danger", onclick: () => playerAct("ban", name.value) }, "Ban"))),
    h("div", { class: "card" }, h("h2", {}, `Online (${p.online.length})`),
      p.online.length ? p.online.map((n) => h("div", { class: "list-row" }, h("div", { class: "grow" }, h("b", {}, n)),
        h("button", { class: "btn small", onclick: () => playerAct("op", n) }, "Operator"),
        h("button", { class: "btn small", onclick: () => playerAct("kick", n) }, "Kick"),
        h("button", { class: "btn small danger", onclick: () => playerAct("ban", n) }, "Ban"))) : h("div", { class: "muted" }, s.state === "running" ? "Nobody is on." : "The server isn't running.")),
    h("div", { class: "card" }, h("label", { class: "switch" },
      h("input", { type: "checkbox", checked: p.whitelist_on, onchange: (e) => act(() => api("PUT", sp("/properties"), { "white-list": String(e.target.checked) }), e.target.checked ? "Only listed players can join (after a restart)." : "Anyone can join (after a restart).") }),
      h("span", {}, h("b", {}, "Only players on the whitelist can join"))),
      h("div", { class: "muted" }, "Good for a server just for friends.")),
    list("Whitelist", p.whitelist, "disallow"),
    list("Operators", p.ops, "deop", "Operators can use every command."),
    list("Banned", p.banned, "pardon"));
}

// ------------------------------------------------------------------ settings

async function tabSettings(s, full) {
  if (!full) return;
  const el = $("tab-settings");
  let props;
  try { props = await api("GET", sp("/properties")); } catch (e) { fill(el, h("div", { class: "notice bad" }, e.message)); return; }
  // Only what this Minecraft version has (newer ones dropped some, like pvp), unless it hasn't
  // written server.properties yet.
  const known = Object.keys(props).length > 3;
  const common = S.info.common_properties.filter((p) => !known || p.key in props);
  const changes = {};
  const field = (pi) => {
    const v = props[pi.key] ?? pi.default;
    let input;
    if (pi.kind === "bool") input = h("input", { type: "checkbox", checked: v === "true", onchange: (e) => (changes[pi.key] = String(e.target.checked)) });
    else if (pi.kind === "choice") input = h("select", { onchange: (e) => (changes[pi.key] = e.target.value) }, pi.choices.map((c) => h("option", { value: c, selected: c === v }, c.replace("minecraft:", ""))));
    else input = h("input", { type: pi.kind === "number" ? "number" : "text", value: v, oninput: (e) => (changes[pi.key] = e.target.value) });
    if (pi.kind === "bool") return h("label", { class: "switch" }, input, h("span", {}, h("b", {}, pi.label), pi.help ? h("div", { class: "muted", style: "font-size:12px" }, pi.help) : null));
    return h("div", { class: "field" }, h("label", {}, pi.label), input, pi.help ? h("div", { class: "help" }, pi.help) : null);
  };
  const mem = h("input", { type: "range", min: 512, max: Math.max(1024, S.info.memory_mb - 512), step: 256, value: s.ram_mb });
  const memLbl = h("b", {}, `${s.ram_mb} MB`);
  mem.oninput = () => (memLbl.textContent = `${mem.value} MB`);
  const jvm = h("input", { value: s.jvm_args, placeholder: "Extra Java options (most people leave this empty)" });
  const flavorSel = h("select", {}, S.info.flavors.map((f) => h("option", { value: f.id, selected: f.id === s.flavor }, f.name)));
  const verSel = h("select", {}, h("option", { value: "latest" }, "Newest"));
  const loadVers = async () => {
    fill(verSel, h("option", { value: "latest" }, "Newest"));
    try { (await api("GET", "/api/versions/" + flavorSel.value)).forEach((v) => verSel.append(h("option", { value: v, selected: v === s.minecraft && flavorSel.value === s.flavor }, v))); } catch (e) {}
  };
  flavorSel.onchange = loadVers;
  loadVers();
  const all = Object.keys(props).sort().filter((k) => !common.find((c) => c.key === k));
  fill(el,
    h("div", { class: "card" }, h("h2", {}, "Game"), h("div", { class: "grid" }, h("div", {}, common.filter((p) => p.kind !== "bool").map(field)), h("div", {}, common.filter((p) => p.kind === "bool").map(field))),
      h("div", { class: "row" }, h("button", { class: "btn primary", onclick: () => act(() => api("PUT", sp("/properties"), changes), "Saved." + (s.state !== "stopped" ? " Restart the server for it to count." : "")).then(() => tabSettings(S.status, true)) }, "Save"))),
    h("div", { class: "card" }, h("h2", {}, "MineNest"),
      h("div", { class: "field" }, h("label", {}, "Memory: ", memLbl), mem, h("div", { class: "help" }, `This computer has ${S.info.memory_mb} MB. 2 to 4 GB suits most servers; big modpacks want 6 to 8.`)),
      h("label", { class: "switch" }, h("input", { type: "checkbox", checked: s.autostart, onchange: (e) => act(() => api("PATCH", sp(), { autostart: e.target.checked }), "Saved.") }), h("span", {}, h("b", {}, "Start with MineNest"), h("div", { class: "muted", style: "font-size:12px" }, "Turn on 'minenest autostart on' too, so MineNest starts when you sign in."))),
      h("label", { class: "switch" }, h("input", { type: "checkbox", checked: s.auto_restart, onchange: (e) => act(() => api("PATCH", sp(), { auto_restart: e.target.checked }), "Saved.") }), h("span", {}, h("b", {}, "Start again if it crashes"))),
      h("div", { class: "field" }, h("label", {}, "Java options"), jvm),
      h("button", { class: "btn primary", onclick: () => act(() => api("PATCH", sp(), { ram_mb: +mem.value, jvm_args: jvm.value }), "Saved." + (s.state !== "stopped" ? " Restart the server for it to count." : "")) }, "Save")),
    h("div", { class: "card" }, h("h2", {}, "Server type and Minecraft version"),
      h("div", { class: "muted" }, "Changing it backs the world up first. Going back to an older Minecraft version can damage a world, so keep the backup."),
      h("div", { class: "row", style: "margin-top:10px" }, h("div", { class: "grow" }, flavorSel), h("div", { class: "grow" }, verSel),
        h("button", { class: "btn primary", disabled: s.state !== "stopped", onclick: () => confirmDialog("Change the server?", `It becomes ${flavorSel.selectedOptions[0].textContent} ${verSel.value === "latest" ? "(newest)" : verSel.value}. The world is backed up first.`, "Change", () => act(() => api("POST", sp("/version"), { flavor: flavorSel.value, version: verSel.value }))) }, "Change")),
      h("div", { class: "row", style: "margin-top:10px" }, h("button", { class: "btn", disabled: s.state !== "stopped", onclick: () => act(() => api("POST", sp("/repair"))) }, "Repair (download it again)")),
      s.state !== "stopped" ? h("div", { class: "muted", style: "margin-top:6px" }, "Stop the server to change it.") : null),
    h("div", { class: "card" }, h("h2", {}, "All settings (server.properties)"),
      h("table", { class: "props" }, all.map((k) => h("tr", {}, h("td", {}, k), h("td", {}, h("input", { value: props[k], oninput: (e) => (changes[k] = e.target.value) }))))),
      h("button", { class: "btn primary", style: "margin-top:10px", onclick: () => act(() => api("PUT", sp("/properties"), changes), "Saved.") }, "Save")),
    h("div", { class: "card" }, h("h2", {}, "Rename or delete"),
      h("div", { class: "row" }, h("button", { class: "btn", onclick: renameDialog }, "Rename"),
        h("button", { class: "btn danger", disabled: s.state !== "stopped", onclick: () => confirmDialog(`Delete ${s.name}?`, "The server, its world and its backups are deleted. This can't be undone.", "Delete", async () => { await act(() => api("DELETE", sp()), "Deleted."); S.cur = null; refresh(); }) }, "Delete this server"))));
}

// ------------------------------------------------------------------ world

async function tabWorld(s, full) {
  if (!full) return;
  const el = $("tab-world");
  let r;
  try { r = await api("GET", sp("/backups")); } catch (e) { fill(el, h("div", { class: "notice bad" }, e.message)); return; }
  const seed = h("input", { placeholder: "Seed (empty for a random world)" });
  fill(el,
    h("div", { class: "card" }, h("h2", {}, "Backups"),
      h("div", { class: "muted", style: "margin-bottom:10px" }, "A backup is a copy of the world. MineNest also makes one before every change that could hurt it."),
      h("button", { class: "btn primary", disabled: !r.world, onclick: () => act(() => api("POST", sp("/backups"))).then(() => setTimeout(() => tabWorld(S.status, true), 2500)) }, "Back up now"),
      !r.world ? h("div", { class: "muted", style: "margin-top:8px" }, "There's no world yet: start the server once to make one.") : null,
      h("div", { style: "margin-top:12px" }, r.backups.length ? r.backups.map((b) => h("div", { class: "list-row" },
        h("div", { class: "grow" }, h("b", {}, new Date(b.time * 1000).toLocaleString()), " ", h("span", { class: "muted" }, `${(b.size / 1048576).toFixed(1)} MB · ${b.why}`)),
        h("button", { class: "btn small", disabled: s.state !== "stopped", onclick: () => confirmDialog("Put this backup back?", "The world as it is now is backed up first, so nothing is lost.", "Put it back", () => act(() => api("POST", sp(`/backups/${encodeURIComponent(b.name)}/restore`)))) }, "Put back"),
        h("button", { class: "btn small danger", onclick: () => confirmDialog("Delete this backup?", "It can't be brought back.", "Delete", () => act(() => api("DELETE", sp(`/backups/${encodeURIComponent(b.name)}`)), "Deleted.").then(() => tabWorld(S.status, true))) }, "Delete"))) : h("div", { class: "muted" }, "No backups yet.")),
      s.state !== "stopped" ? h("div", { class: "muted", style: "margin-top:6px" }, "Stop the server to put a backup back.") : null),
    h("div", { class: "card" }, h("h2", {}, "A new world"),
      h("div", { class: "muted" }, "The world is backed up, then removed. The next start makes a new one."),
      h("div", { class: "row", style: "margin-top:10px" }, h("div", { class: "grow" }, seed),
        h("button", { class: "btn danger", disabled: s.state !== "stopped", onclick: () => confirmDialog("Make a new world?", "The current world is backed up first.", "New world", () => act(() => api("POST", sp("/world/reset"), { seed: seed.value }))) }, "New world"))));
}

// ------------------------------------------------------------------ internet

let vpsKey = "";
async function tabInternet(s, full) {
  const el = $("tab-internet");
  if (!el.firstChild || full) fill(el, h("div", { id: "net-top" }), h("div", { id: "net-extra" }), h("div", { id: "net-bedrock" }));
  const way = s.way || {};
  const kinds = [
    ["", "Only at home", "Players on this computer and your home network join. Nothing else to set up."],
    ["playit", "playit.gg", "Free. A playit.gg account gives the server an address anyone can join. Works with Bedrock too."],
    ["relay", "Supporter relay", "For supporters on Patreon: your own name, like brave-otter.mc.owencosta.diy."],
    ["vps", "Your own VPS", "An SSH tunnel to a server you rent. For people who have one."],
  ];
  const status = way.kind ? h("div", { class: "card" }, h("h2", {}, "Status"),
    way.address ? addrRow("Friends join at", way.address) : null,
    h("div", { class: "notice " + (way.up ? (way.check_ok || way.kind === "playit" ? "good" : "warn") : way.state === "problem" ? "bad" : "") },
      way.up ? (way.check || "Connected.") : (s.state === "running" ? (way.note || way.state) : "Connects when the server starts.")),
    way.claim_url ? h("div", { class: "notice warn" }, "Connect this computer to your playit.gg account (once): ", h("a", { href: way.claim_url, target: "_blank", rel: "noopener" }, "open playit.gg")) : null,
    way.bedrock ? addrRow("Bedrock players", way.bedrock) : null,
    h("div", { class: "row" },
      way.kind !== "playit" && s.state === "running" ? h("button", { class: "btn", onclick: () => act(() => api("POST", sp("/recheck")), "Checking from the internet...") }, "Check again") : null,
      way.kind === "playit" ? h("button", { class: "btn", onclick: () => confirmDialog("Forget playit.gg?", "This computer is disconnected from your playit.gg account. You can connect it again later.", "Forget", () => act(() => api("POST", "/api/playit/forget"), "Forgotten.")) }, "Forget playit.gg") : null,
      way.kind === "vps" ? h("button", { class: "btn", onclick: () => act(() => api("POST", sp("/way/forget-host")), "MineNest will trust the VPS's identity next time.") }, "Trust its new identity") : null)) : null;
  let extra = null;
  if (full) {
    if (way.kind === "vps" || S.vpsOpen) {
      if (!vpsKey) { try { vpsKey = (await api("GET", "/api/sshkey")).key; } catch (e) {} }
      const host = h("input", { placeholder: "Address, like 203.0.113.7 or vps.example.com" }), user = h("input", { placeholder: "User", value: "root" });
      const sshp = h("input", { type: "number", value: 22 }), port = h("input", { type: "number", value: 25565 });
      extra = h("div", { class: "card" }, h("h2", {}, "Your VPS"),
        h("ol", {}, h("li", {}, "Put this line in ~/.ssh/authorized_keys for that user on the VPS:", h("div", { class: "addr" }, h("code", { style: "font-size:12px" }, vpsKey), copyBtn(vpsKey))),
          h("li", {}, "In its /etc/ssh/sshd_config set ", h("code", {}, "GatewayPorts clientspecified"), ", restart sshd, and open the port in its firewall."),
          h("li", {}, "Fill in its details:")),
        h("div", { class: "grid" }, h("div", { class: "field" }, h("label", {}, "Address"), host), h("div", { class: "field" }, h("label", {}, "User"), user),
          h("div", { class: "field" }, h("label", {}, "SSH port"), sshp), h("div", { class: "field" }, h("label", {}, "Port for players"), port)),
        h("button", { class: "btn primary", onclick: () => act(() => api("POST", sp("/way"), { kind: "vps", vps: { host: host.value.trim(), user: user.value.trim(), ssh_port: +sshp.value, public_port: +port.value } }), "Your VPS is the way in.").then(() => { S.vpsOpen = false; }) }, "Use this VPS"));
    }
  }
  const bedrockCard = ["paper", "purpur"].includes(s.flavor) ? h("div", { class: "card" }, h("h2", {}, "Bedrock players"),
    h("label", { class: "switch" }, h("input", { type: "checkbox", checked: s.bedrock_on, onchange: (e) => act(() => api("POST", sp("/bedrock"), { on: e.target.checked })) }),
      h("span", {}, h("b", {}, "Let Bedrock players join (phones, consoles, Windows)"), h("div", { class: "muted", style: "font-size:12px" }, "Adds GeyserMC's Geyser and Floodgate. At home, on port 19132; from the internet, through playit.gg."))),
    s.bedrock_on ? h("button", { class: "btn small", onclick: () => act(() => api("POST", sp("/bedrock"), { update: true })) }, "Update Geyser") : null) : null;
  const realIPs = way.kind === "relay" && ["paper", "purpur"].includes(s.flavor) ? h("label", { class: "switch" }, h("input", { type: "checkbox", checked: s.real_ips, onchange: (e) => act(() => api("PATCH", sp(), { real_ips: e.target.checked }), "Saved. It counts from the next start.") }),
    h("span", {}, h("b", {}, "Show players' real addresses"), h("div", { class: "muted", style: "font-size:12px" }, "So bans by address work through the relay. Off when Bedrock players are on."))) : null;
  if (full) fill($("net-extra"), extra);
  if (!(full || !$("net-bedrock").contains(document.activeElement))) return;
  fill($("net-bedrock"), bedrockCard);
  fill($("net-top"),
    h("div", { class: "card" }, h("h2", {}, "How do friends elsewhere join?"),
      h("div", { class: "flavors" }, kinds.map(([k, name, about]) => h("button", { class: "flavor" + ((way.kind || "") === k ? " on" : ""), onclick: () => {
        if (k === "vps") { S.vpsOpen = true; tabInternet(S.status, true); return; }
        if (k === "relay" && !(S.info.account)) { openAccount(); toast("Sign in with your XenithOS profile first.", true); return; }
        act(() => api("POST", sp("/way"), { kind: k }), k ? `${name} is the way in.` : "Only people at home can join now.");
      } }, h("b", {}, name), h("span", {}, about)))), realIPs),
    status);
}

// ------------------------------------------------------------------ dialogs

function modal(...kids) {
  fill($("modal-box"), kids);
  $("modal").classList.remove("hidden");
}
function closeModal() { $("modal").classList.add("hidden"); }
$("modal").addEventListener("mousedown", (e) => { if (e.target.id === "modal") closeModal(); });
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeModal(); });

function confirmDialog(title, text, okText, fn) {
  modal(h("h2", {}, title), h("p", {}, text), h("div", { class: "modal-actions" },
    h("button", { class: "btn", onclick: closeModal }, "Cancel"),
    h("button", { class: "btn primary", onclick: () => { closeModal(); fn(); } }, okText)));
}

function eulaDialog() {
  const box = h("input", { type: "checkbox" });
  const ok = h("button", { class: "btn primary", disabled: true, onclick: () => { closeModal(); act(() => api("POST", sp("/eula"), { agree: true }), "Agreed. You can start the server now."); } }, "Agree");
  box.onchange = () => (ok.disabled = !box.checked);
  modal(h("h2", {}, "Minecraft's EULA"),
    h("p", {}, "To run a Minecraft server you need to agree to Minecraft's End User License Agreement. MineNest never agrees for you."),
    h("p", {}, h("a", { href: "https://aka.ms/MinecraftEULA", target: "_blank", rel: "noopener" }, "Read the EULA (aka.ms/MinecraftEULA)")),
    h("label", { class: "switch" }, box, h("span", {}, "I have read Minecraft's EULA and I agree to it.")),
    h("div", { class: "modal-actions" }, h("button", { class: "btn", onclick: closeModal }, "Not now"), ok));
}

function renameDialog() {
  const name = h("input", { value: S.status.name, maxlength: 40 });
  modal(h("h2", {}, "Rename"), name, h("div", { class: "modal-actions" }, h("button", { class: "btn", onclick: closeModal }, "Cancel"),
    h("button", { class: "btn primary", onclick: () => { closeModal(); act(() => api("PATCH", sp(), { name: name.value }), "Renamed."); } }, "Rename")));
}

function newServerDialog() {
  const st = { flavor: "paper", version: "latest", modpack: null };
  const name = h("input", { placeholder: "My Server", maxlength: 40 });
  const ver = h("select", {}, h("option", { value: "latest" }, "Newest"));
  const total = S.info.memory_mb || 4096;
  const mem = h("input", { type: "range", min: 1024, max: Math.max(1536, total - 1024), step: 256, value: Math.min(total / 2, 2048) });
  const memLbl = h("b", {}, `${mem.value} MB`);
  mem.oninput = () => (memLbl.textContent = `${mem.value} MB`);
  const eula = h("input", { type: "checkbox" });
  const packArea = h("div", { class: "hidden" });
  const verArea = h("div", { class: "field" }, h("label", {}, "Minecraft version"), ver);
  const loadVers = async () => {
    fill(ver, h("option", { value: "latest" }, "Newest"));
    try { (await api("GET", "/api/versions/" + st.flavor)).forEach((v) => ver.append(h("option", { value: v }, v))); } catch (e) { toast(e.message, true); }
  };
  const cards = h("div", { class: "flavors" });
  const drawCards = () => fill(cards, [...S.info.flavors.map((f) => [f.id, f.name, f.about]), ["modpack", "Modpack", "A whole modpack from Modrinth, ready to play."]].map(([id, n, about]) =>
    h("button", { class: "flavor" + (st.flavor === id ? " on" : ""), onclick: () => {
      st.flavor = id; drawCards();
      packArea.classList.toggle("hidden", id !== "modpack"); verArea.classList.toggle("hidden", id === "modpack");
      if (id === "modpack") { mem.value = Math.min(6144, total - 1024); memLbl.textContent = `${mem.value} MB`; searchPacks(""); } else loadVers();
    } }, h("b", {}, n), h("span", {}, about))));
  const pq = h("input", { placeholder: "Search modpacks, like \"Cobblemon\" or \"All the Mods\"" });
  const presults = h("div", { class: "hits", style: "max-height:300px;overflow:auto;margin-top:8px" });
  const searchPacks = async (q) => {
    fill(presults, h("div", { class: "muted" }, "Searching Modrinth..."));
    try {
      const r = await api("GET", "/api/modpacks?q=" + encodeURIComponent(q));
      fill(presults, r.hits.map((hit) => hitRow(hit, h("button", { class: "btn small" + (st.modpack === hit.id ? " primary" : ""), onclick: () => { st.modpack = hit.id; if (!name.value) name.value = hit.title.slice(0, 40); searchPacks(pq.value); } }, st.modpack === hit.id ? "Chosen" : "Choose"))));
    } catch (e) { fill(presults, h("div", { class: "notice bad" }, e.message)); }
  };
  pq.addEventListener("keydown", (e) => { if (e.key === "Enter") searchPacks(pq.value); });
  fill(packArea, h("div", { class: "row" }, h("div", { class: "grow" }, pq), h("button", { class: "btn", onclick: () => searchPacks(pq.value) }, "Search")), presults);
  drawCards(); loadVers();
  modal(h("h2", {}, "New server"),
    h("div", { class: "field" }, h("label", {}, "Name"), name),
    h("div", { class: "field" }, h("label", {}, "Type"), cards),
    verArea, packArea,
    h("div", { class: "field" }, h("label", {}, "Memory: ", memLbl), mem, h("div", { class: "help" }, `This computer has ${total} MB. You can change it later.`)),
    h("label", { class: "switch" }, eula, h("span", {}, "I have read ", h("a", { href: "https://aka.ms/MinecraftEULA", target: "_blank", rel: "noopener" }, "Minecraft's EULA"), " and I agree to it. (Needed before the server can start; you can do it later.)")),
    h("div", { class: "modal-actions" }, h("button", { class: "btn", onclick: closeModal }, "Cancel"),
      h("button", { class: "btn primary", onclick: async () => {
        if (st.flavor === "modpack" && !st.modpack) { toast("Choose a modpack first.", true); return; }
        const body = { name: name.value || "My Server", ram_mb: +mem.value, agree_eula: eula.checked };
        if (st.flavor === "modpack") body.modpack = st.modpack; else { body.flavor = st.flavor; body.version = ver.value; }
        const r = await act(() => api("POST", "/api/servers", body));
        if (r) { closeModal(); S.cur = r.id; S.view = "server"; refresh().then(() => setTab("overview")); }
      } }, "Make it")));
  name.focus();
}

// ------------------------------------------------------------------ account and relay

async function openAccount() {
  S.view = "account"; renderSide(); show("account");
  const el = $("account");
  fill(el, h("div", { class: "muted" }, "Loading..."));
  let r;
  try { r = await api("GET", "/api/account"); } catch (e) { fill(el, h("div", { class: "notice bad" }, e.message)); return; }
  if (r.signed_in && !r.step) { try { r = await api("POST", "/api/relay/status", {}); } catch (e) { toast(e.message, true); } }
  const back = h("button", { class: "link", onclick: () => { S.view = "server"; refresh().then(() => renderTab(true)); } }, "← Back to servers");
  if (!r.signed_in) {
    const n = h("input", { placeholder: "Profile name" }), pin = h("input", { type: "password", placeholder: "PIN", inputmode: "numeric" });
    const go = (create) => act(() => api("POST", "/api/account/login", { name: n.value, pin: pin.value, create }), create ? "Profile made. You're signed in." : "Signed in.").then((a) => { if (a) { S.info.account = a.name; openAccount(); } });
    fill(el, back, h("h1", {}, "Account"),
      h("div", { class: "card" }, h("h2", {}, "Sign in with your XenithOS profile"),
        h("p", { class: "muted" }, "MineNest uses the same profile as XenithOS. It's what the supporter relay goes by, so your server gets a name of its own when you support on Patreon. The rest of MineNest works without one."),
        h("div", { class: "grid" }, h("div", { class: "field" }, h("label", {}, "Name"), n), h("div", { class: "field" }, h("label", {}, "PIN"), pin)),
        h("div", { class: "row" }, h("button", { class: "btn primary", onclick: () => go(false) }, "Sign in"), h("button", { class: "btn", onclick: () => go(true) }, "Make a new profile"))));
    return;
  }
  const claimName = h("input", { placeholder: r.suggest || "your-server-name", value: r.name || "" });
  const relayCard = h("div", { class: "card" }, h("h2", {}, "Supporter relay"),
    !r.open ? h("div", { class: "notice warn" }, "The supporter relay isn't taking supporters right now.") : null,
    r.message ? h("div", { class: "notice" + (r.step === "ok" ? " good" : "") }, r.message) : null,
    r.step === "link" && r.link_url ? h("p", {}, h("a", { href: r.link_url, target: "_blank", rel: "noopener" }, "Link your Patreon and this profile on OCAccounts")) : null,
    (r.step === "patreon" || r.step === "tier") && r.patreon ? h("p", {}, h("a", { href: r.patreon, target: "_blank", rel: "noopener" }, "Become a supporter on Patreon")) : null,
    r.tier ? h("p", { class: "muted" }, "Tier: " + r.tier) : null,
    r.name ? h("div", {}, addrRow("Your server's address", r.address), h("p", { class: "muted" }, r.used_by ? `Used by ${r.used_by}.` : "Choose Supporter relay on a server's Internet page to use it.")) : null,
    r.step === "ok" ? h("div", { class: "field" }, h("label", {}, r.name ? "Change the name" : "Pick a name"),
      h("div", { class: "row" }, h("div", { class: "grow" }, claimName), h("span", { class: "muted" }, "." + (r.domain || "mc.owencosta.diy")),
        h("button", { class: "btn primary", onclick: () => act(() => api("POST", "/api/relay/claim", { name: claimName.value || r.suggest }), "It's yours.").then(openAccount) }, r.name ? "Change" : "Claim it")),
      h("div", { class: "help" }, "3 to 20 small letters, numbers and hyphens.")) : null,
    h("div", { class: "row", style: "margin-top:10px" },
      h("button", { class: "btn", onclick: () => act(() => api("POST", "/api/relay/status", {}), "Checked.").then(openAccount) }, "Check my membership"),
      r.name ? h("button", { class: "btn danger", onclick: () => confirmDialog("Give up your name?", "It's kept for you for a week in case you change your mind.", "Give it up", () => act(() => api("POST", "/api/relay/release", {}), "Released.").then(openAccount)) }, "Give up the name") : null));
  fill(el, back, h("h1", {}, "Account"),
    h("div", { class: "card" }, h("h2", {}, "Signed in as " + r.account),
      h("button", { class: "btn", onclick: () => act(() => api("POST", "/api/account/logout"), "Signed out.").then(() => { S.info.account = ""; openAccount(); }) }, "Sign out")),
    relayCard);
}

// ------------------------------------------------------------------ start

async function main() {
  try { S.info = await api("GET", "/api/info"); } catch (e) { document.body.textContent = "MineNest isn't answering. Open it again from its icon."; return; }
  $("version").textContent = `MineNest ${S.info.version} · ${S.info.os}`;
  for (const j of S.info.jobs) if (!j.finished) watchJob(j.id);
  $("new-server").onclick = newServerDialog;
  $("empty-new").onclick = newServerDialog;
  $("open-account").onclick = openAccount;
  $("btn-start").onclick = () => {
    if (S.status.eula_needed) { eulaDialog(); return; }
    act(() => api("POST", sp("/start")), "Starting...").then(() => setTab("console"));
  };
  $("btn-stop").onclick = () => act(() => api("POST", sp("/stop")), "Stopping (saving the world first)...");
  $("btn-restart").onclick = () => act(() => api("POST", sp("/restart")));
  for (const b of $("tabs").children) b.onclick = () => setTab(b.dataset.tab);
  $("console-form").onsubmit = (e) => {
    e.preventDefault();
    const c = $("console-cmd");
    if (!c.value.trim()) return;
    act(() => api("POST", sp("/command"), { command: c.value }));
    c.value = "";
  };
  await refresh();
  if (S.status) renderTab(true);
  setInterval(() => { if (!document.hidden) refresh(); }, 2000);
}
main();
