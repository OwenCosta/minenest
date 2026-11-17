"use strict";
// MineNest's window. It talks to MineNest's background service on this computer. Everything is
// built with h(), so names and text from servers, players and Modrinth are never read as HTML.

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
const section = (title, aside, ...kids) => h("div", { class: "section" }, h("h2", {}, title, aside ? h("small", {}, aside) : null), kids);
const msg = (kind, tag, ...kids) => h("div", { class: "msg " + kind }, tag ? h("b", { class: "tag" }, tag) : null, kids);

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

function toast(text, bad) {
  const t = h("div", { class: "toast" + (bad ? " bad" : "") }, text);
  $("toasts").append(t);
  setTimeout(() => t.remove(), bad ? 7000 : 3000);
}
async function act(fn, okText) {
  try {
    const r = await fn();
    if (r && r.job) watchJob(r.job);
    if (okText) toast(okText);
    refresh();
    return r || true;
  } catch (e) { toast(e.message, true); return null; }
}
const stateWord = (s) => (s.busy && s.state === "stopped" ? "Busy" : { running: "Running", starting: "Starting", stopping: "Stopping", stopped: "Stopped" }[s.state] || s.state);

// ------------------------------------------------------------------ jobs

function watchJob(id) { S.watching.add(id); pollJobs(); }
async function pollJobs() {
  for (const id of [...S.watching]) {
    let j;
    try { j = await api("GET", "/api/jobs/" + id); } catch (e) { S.watching.delete(id); continue; }
    let el = document.querySelector(`[data-job="${id}"]`);
    if (!el) {
      el = h("div", { class: "job", "data-job": id }, h("div", { class: "t" }), h("div", { class: "s" }), h("div", { class: "bar" }, h("div")));
      $("jobs").append(el);
    }
    el.querySelector(".t").textContent = j.title;
    el.querySelector(".s").textContent = j.error || j.step;
    const bar = el.querySelector(".bar");
    bar.classList.toggle("busy", !j.total && !j.finished);
    bar.firstChild.style.width = j.finished ? "100%" : j.total ? `${Math.round((100 * j.done) / j.total)}%` : "30%";
    if (j.finished) {
      S.watching.delete(id);
      if (j.error) { toast(j.title + ": " + j.error, true); el.remove(); } else setTimeout(() => el.remove(), 1200);
      refresh();
      if (S.view === "server" && ["addons", "world", "settings"].includes(S.tab)) renderTab(true);
    }
  }
}
setInterval(() => { if (S.watching.size) pollJobs(); }, 600);

// ------------------------------------------------------------------ refresh and the sidebar

async function refresh() {
  try { S.servers = await api("GET", "/api/servers"); } catch (e) { return; }
  renderSide();
  if (S.view !== "server") return;
  if (!S.servers.length) { show("empty"); return; }
  if (!S.cur || !S.servers.find((s) => s.id === S.cur)) {
    const saved = localStorage.getItem("mn-cur");
    S.cur = S.servers.find((s) => s.id === saved) ? saved : S.servers[0].id;
  }
  S.status = S.servers.find((s) => s.id === S.cur);
  show("server");
  renderHead();
  if (S.tab === "overview" || S.tab === "internet") renderTab(false);
}
function show(which) { for (const id of ["empty", "server", "create", "account"]) $(id).classList.toggle("hidden", id !== which); }

function renderSide() {
  fill($("server-list"), S.servers.map((s) =>
    h("button", { class: "srv-item" + (s.id === S.cur && S.view === "server" ? " on" : ""), onclick: () => pickServer(s.id) },
      h("div", { class: "t" }, h("span", {}, s.name), h("span", { class: "state " + s.state }, s.state === "stopped" ? "" : stateWord(s))),
      h("div", { class: "s" }, s.installed ? `${s.flavor_name} ${s.minecraft}` : s.busy ? "Setting up" : "Not installed"))));
}
function pickServer(id) {
  S.cur = id; S.view = "server"; localStorage.setItem("mn-cur", id);
  S.seq = 0; S.consoleFor = null; $("console").replaceChildren();
  refresh().then(() => renderTab(true));
}

function renderHead() {
  const s = S.status;
  $("srv-name").textContent = s.name;
  $("srv-sub").textContent = s.installed ? [`${s.flavor_name} ${s.minecraft}`, s.build && `${{ fabric: "loader", forge: "Forge", neoforge: "NeoForge" }[s.flavor] || "build"} ${s.build.replace(s.minecraft + "-", "")}`, s.modpack, `port ${s.port}`, `${(s.ram_mb / 1024).toFixed(1).replace(".0", "")} GB`].filter(Boolean).join("  ·  ") : (s.busy ? "Setting up" : "The server software isn't installed");
  const st = $("srv-state");
  st.textContent = stateWord(s) + (s.state === "running" ? ` · ${s.players.length}/${s.max_players} on` : "");
  st.className = "state " + s.state;
  $("btn-start").disabled = s.state !== "stopped" || !s.installed || !!s.busy;
  $("btn-stop").disabled = s.state === "stopped";
  $("btn-restart").disabled = s.state !== "running";
  $("tab-addons-btn").textContent = { plugin: "Plugins", mod: "Mods", datapack: "Data packs" }[s.addon_kind] || "Mods";
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

const copyBtn = (text) => h("button", { class: "btn small", onclick: () => { navigator.clipboard.writeText(text); toast("Copied " + text); } }, "Copy");
const addrRow = (label, value) => h("div", { class: "addr" }, h("span", { class: "lbl" }, label), h("code", {}, value), copyBtn(value));
function uptime(sec) { const hh = Math.floor(sec / 3600), mm = Math.floor((sec % 3600) / 60); return hh ? `${hh} h ${mm} min` : `${mm} min`; }
const wayNames = { "": "Only at home", playit: "playit.gg", vps: "Your own VPS", relay: "Supporter relay" };

function tabOverview(s) {
  const way = s.way || {};
  const notes = [];
  if (s.busy) notes.push(msg("", "Busy", `MineNest is busy with ${s.busy}. The progress is at the bottom right.`));
  if (s.installed && s.eula_needed) notes.push(msg("warn", "One more step", "Agree to Minecraft's EULA before the first start. ", h("button", { class: "link", onclick: eulaDialog }, "Read it and agree")));
  if (s.whitelist_on && !s.whitelist_count) notes.push(msg("warn", "Whitelist", "Only listed players can join and the list is empty. ", h("button", { class: "link", onclick: () => setTab("players") }, "Add players or turn it off")));
  if (s.why && s.state === "stopped") notes.push(msg("bad", "It stopped by itself:", s.why));
  if (way.claim_url && s.state === "running") notes.push(msg("warn", "playit.gg", "Approve this computer on your playit.gg account (once): ", h("a", { href: way.claim_url, target: "_blank", rel: "noopener" }, "open playit.gg")));
  fill($("tab-overview"), notes,
    h("div", { class: "cols" },
      section("Where to join", null,
        addrRow("This computer", s.addresses.here),
        s.addresses.home ? addrRow("Your home network", s.addresses.home) : null,
        s.addresses.public ? addrRow("The internet", s.addresses.public) :
          h("div", { class: "addr" }, h("span", { class: "lbl" }, "The internet"),
            h("span", { class: "muted" }, way.kind ? (s.state === "running" ? (way.note || "Connecting") : `${wayNames[way.kind]}, once the server is on`) : "Not set up. ",
              way.kind ? null : h("button", { class: "link", onclick: () => setTab("internet") }, "Set it up")), h("span")),
        s.bedrock_on ? h("p", { class: "muted small" }, `Bedrock players use the same address with port 19132${way.bedrock ? `, or ${way.bedrock} from the internet` : ""}.`) : null),
      section("Server", null, h("div", { class: "kv" },
        h("span", {}, "State"), h("span", {}, stateWord(s) + (s.uptime ? `, for ${uptime(s.uptime)}` : "")),
        h("span", {}, "Players"), h("span", {}, s.players.length ? s.players.join(", ") : (s.state === "running" ? "Nobody on" : "None")),
        h("span", {}, "Software"), h("span", {}, s.installed ? `${s.flavor_name} ${s.minecraft}${s.build ? `, build ${s.build}` : ""}` : "Not installed"),
        h("span", {}, "Java"), h("span", {}, s.java ? `Java ${s.java}` : ""),
        h("span", {}, "Memory"), h("span", {}, `${s.ram_mb} MB`),
        h("span", {}, "Internet"), h("span", {}, wayNames[way.kind || ""] + (way.kind && way.state ? ` (${way.state})` : "")),
        h("span", {}, "Folder"), h("span", { class: "small", style: "word-break:break-all" }, s.folder)))));
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
    for (const l of r.lines) { box.append(h("div", { class: l.kind }, l.text)); S.seq = l.seq; }
    while (box.childElementCount > 2000) box.firstChild.remove();
    if (atEnd || r.lines.length > 50) box.scrollTop = box.scrollHeight;
  }
}

// ------------------------------------------------------------------ add-ons

const kindWord = (k) => ({ plugin: "plugin", mod: "mod", datapack: "data pack" }[k] || "mod");
let addonQuery = "";
function hitRow(hit, action) {
  return h("div", { class: "hit" },
    hit.icon ? h("img", { src: hit.icon, alt: "", loading: "lazy" }) : h("div", { class: "noicon" }),
    h("div", { class: "body" }, h("div", { class: "title" }, hit.title, h("span", { class: "muted", style: "font-weight:400" }, "  by " + hit.author)),
      h("div", { class: "desc", title: hit.description }, hit.description),
      h("div", { class: "meta" }, `${fmtCount(hit.downloads)} downloads · `, h("a", { href: `https://modrinth.com/project/${hit.slug}`, target: "_blank", rel: "noopener" }, "on Modrinth"))),
    action);
}
const fmtCount = (n) => (n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? Math.round(n / 1e3) + "k" : String(n));

async function tabAddons(s, full) {
  if (!full) return;
  const el = $("tab-addons");
  if (!s.installed) { fill(el, msg("", null, "Install the server software first.")); return; }
  const kw = kindWord(s.addon_kind);
  const results = h("div", { class: "hits" }, h("p", { class: "muted" }, "Searching Modrinth..."));
  const installed = h("div", {}, h("p", { class: "muted" }, "Loading..."));
  const q = h("input", { placeholder: `Search Modrinth, like "${s.addon_kind === "plugin" ? "LuckPerms" : s.addon_kind === "datapack" ? "Terralith" : "Lithium"}"`, value: addonQuery });
  const search = async () => {
    addonQuery = q.value;
    fill(results, h("p", { class: "muted" }, "Searching Modrinth..."));
    try {
      const r = await api("GET", sp("/addons/search?q=" + encodeURIComponent(q.value)));
      fill(results, r.hits.length ? r.hits.map((hit) => hitRow(hit, hit.installed ? h("span", { class: "muted small" }, "Installed") :
        h("button", { class: "btn small", onclick: (e) => { e.target.disabled = true; e.target.textContent = "Installing"; act(() => api("POST", sp("/addons"), { project: hit.id })); } }, "Install"))) :
        h("p", { class: "muted" }, `Nothing on Modrinth fits that for ${s.flavor_name} ${s.minecraft}.`));
    } catch (e) { fill(results, msg("bad", null, e.message)); }
  };
  q.addEventListener("keydown", (e) => { if (e.key === "Enter") search(); });
  const file = h("input", { type: "file", accept: ".jar,.zip", class: "hidden", onchange: async () => {
    for (const f of file.files) await act(() => api("POST", sp("/addons/upload?name=" + encodeURIComponent(f.name)), f, true), "Added " + f.name);
    tabAddons(S.status, true);
  } });
  fill(el,
    s.state !== "stopped" ? msg("", "Note", `Changes to ${kw}s count after a restart.`) : null,
    section(`Installed ${kw}s`, null, installed,
      h("div", { class: "row mt" },
        h("button", { class: "btn", onclick: async () => { const r = await act(() => api("POST", sp("/addons/check"))); if (Array.isArray(r)) { const n = r.filter((a) => a.update).length; toast(n ? `${n} can be updated.` : "Everything is up to date."); loadInstalled(); } } }, "Check for updates"),
        h("button", { class: "btn", onclick: () => act(() => api("POST", sp("/addons/update"), {})) }, "Update all"),
        h("button", { class: "btn", onclick: () => file.click() }, `Add a ${kw} file...`), file)),
    section(`Find ${kw}s`, `Only ones made for ${s.flavor_name} ${s.minecraft}. What they need comes with them.`,
      h("div", { class: "row" }, h("div", { class: "grow" }, q), h("button", { class: "btn primary", onclick: search }, "Search")),
      results));
  async function loadInstalled() {
    try {
      const l = await api("GET", sp("/addons"));
      const rows = l.addons.map((a) => h("div", { class: "list-row" },
        a.icon ? h("img", { src: a.icon, width: 26, height: 26, style: "border-radius:3px", alt: "" }) : null,
        h("div", { class: "grow" }, h("b", {}, a.title), h("span", { class: "muted" }, `  ${a.version}${a.needed ? ", needed by another" : ""}`)),
        a.update ? h("button", { class: "btn small primary", onclick: () => act(() => api("POST", sp("/addons/update"), { project: a.project })) }, "Update to " + a.update) : null,
        h("button", { class: "btn small danger", onclick: () => act(() => api("DELETE", sp("/addons/" + encodeURIComponent(a.project))), "Removed " + a.title).then(loadInstalled) }, "Remove")));
      const other = l.other.map((n) => h("div", { class: "list-row" }, h("div", { class: "grow" }, n, h("span", { class: "muted" }, "  added by hand or by a modpack")),
        h("button", { class: "btn small danger", onclick: () => confirmDialog(`Remove ${n}?`, "It's deleted from the server's folder.", "Remove", () => act(() => api("DELETE", sp("/addons/" + encodeURIComponent(n))), "Removed " + n).then(loadInstalled)) }, "Remove")));
      fill(installed, rows.length || other.length ? [rows, other] : h("p", { class: "muted" }, `None yet. Search below.`));
    } catch (e) { fill(installed, msg("bad", null, e.message)); }
  }
  loadInstalled();
  search();
}

// ------------------------------------------------------------------ players

function playerAct(action, name, reason) {
  return act(() => api("POST", sp("/players"), { action, name, reason }), "Done.").then(() => { if (S.tab === "players") tabPlayers(S.status, true); });
}
async function tabPlayers(s, full) {
  if (!full) return;
  const el = $("tab-players");
  let p;
  try { p = await api("GET", sp("/players")); } catch (e) { fill(el, msg("bad", null, e.message)); return; }
  const name = h("input", { placeholder: "Minecraft name" });
  const names = (list, remove) => list.length ? h("div", { class: "names" }, list.map((n) => h("span", { class: "name" }, n, remove ? h("button", { title: "Remove", onclick: () => playerAct(remove, n) }, "×") : null))) : h("p", { class: "muted" }, "Nobody.");
  fill(el,
    section("Add a player", null, h("div", { class: "row" }, h("div", { class: "grow", style: "max-width:260px" }, name),
      h("button", { class: "btn", onclick: () => playerAct("allow", name.value) }, "Add to whitelist"),
      h("button", { class: "btn", onclick: () => playerAct("op", name.value) }, "Make operator"),
      h("button", { class: "btn danger", onclick: () => playerAct("ban", name.value) }, "Ban"))),
    section(`Online now`, `${p.online.length} of ${s.max_players}`,
      p.online.length ? p.online.map((n) => h("div", { class: "list-row" }, h("div", { class: "grow" }, h("b", {}, n)),
        h("button", { class: "btn small", onclick: () => playerAct("op", n) }, "Make operator"),
        h("button", { class: "btn small", onclick: () => playerAct("kick", n) }, "Kick"),
        h("button", { class: "btn small danger", onclick: () => playerAct("ban", n) }, "Ban"))) : h("p", { class: "muted" }, s.state === "running" ? "Nobody is on." : "The server isn't running.")),
    h("div", { class: "cols" },
      section("Whitelist", null,
        h("label", { class: "check" }, h("input", { type: "checkbox", checked: p.whitelist_on, onchange: (e) => act(() => api("PUT", sp("/properties"), { "white-list": String(e.target.checked) }), "Saved. It counts after a restart.") }),
          h("span", {}, "Only these players can join")),
        h("div", { class: "mt" }, names(p.whitelist, "disallow"))),
      section("Operators", "can use every command", names(p.ops, "deop")),
      section("Banned", null, names(p.banned, "pardon"))));
}

// ------------------------------------------------------------------ settings and the version pickers

// A Minecraft version and build picker for a server type. onchange gets {version, build}.
function versionPicker(flavor, current, onchange) {
  const ver = h("select", {}), build = h("select", {});
  const buildLabel = h("label", {}, { paper: "Paper build", purpur: "Purpur build", fabric: "Fabric loader", forge: "Forge version", neoforge: "NeoForge version" }[flavor] || "Loader");
  const box = h("div", { class: "form" }, h("label", {}, "Minecraft version"), ver, buildLabel, build);
  const st = { version: "latest", build: "" };
  const emit = () => onchange && onchange({ ...st, buildLabel: build.selectedOptions[0] ? build.selectedOptions[0].textContent : "" });
  const loadBuilds = async () => {
    fill(build, h("option", { value: "" }, "Loading..."));
    try {
      const list = await api("GET", `/api/builds/${flavor}/${encodeURIComponent(ver.value)}`);
      if (!list.length) { fill(build, h("option", { value: "" }, "None needed")); build.disabled = true; }
      else {
        build.disabled = false;
        const rec = list.find((b) => b.recommended);
        fill(build, h("option", { value: "" }, `Recommended (${rec ? rec.label : list[0].label})`),
          list.map((b) => h("option", { value: b.id, selected: current && current.build === b.id && current.version === ver.value }, b.label)));
      }
    } catch (e) { fill(build, h("option", { value: "" }, "Recommended")); }
    st.build = build.value; emit();
  };
  ver.onchange = () => { st.version = ver.value; loadBuilds(); };
  build.onchange = () => { st.build = build.value; emit(); };
  (async () => {
    fill(ver, h("option", { value: "latest" }, "Loading..."));
    try {
      const vs = await api("GET", "/api/versions/" + flavor);
      fill(ver, vs.map((v, i) => h("option", { value: v, selected: current ? current.version === v : i === 0 }, v + (i === 0 ? " (newest)" : ""))));
    } catch (e) { fill(ver, h("option", { value: "latest" }, "Newest")); toast(e.message, true); }
    st.version = ver.value;
    loadBuilds();
  })();
  return { el: box, get: () => st };
}

function typeList(selected, onpick, withModpack) {
  const list = h("div", { class: "types" });
  const draw = () => fill(list, [...S.info.flavors.map((f) => [f.id, f.name, f.about]), ...(withModpack ? [["modpack", "Modpack", "A whole modpack from Modrinth, set up for you."]] : [])].map(([id, n, about]) =>
    h("button", { class: "type" + (selected === id ? " on" : ""), type: "button", onclick: () => { selected = id; draw(); onpick(id); } }, h("span", { class: "radio" }), h("b", {}, n), h("span", {}, about))));
  draw();
  return list;
}

// One setting from server.properties as form cells.
function propField(pi, value, changes) {
  const v = value ?? pi.default;
  if (pi.kind === "bool") return [h("span"), h("label", { class: "check" }, h("input", { type: "checkbox", checked: v === "true", onchange: (e) => (changes[pi.key] = String(e.target.checked)) }), h("span", {}, pi.label, pi.help ? h("div", { class: "h" }, pi.help) : null))];
  const input = pi.kind === "choice"
    ? h("select", { onchange: (e) => (changes[pi.key] = e.target.value) }, pi.choices.map((c) => h("option", { value: c, selected: c === v }, c.replace("minecraft:", "").replace("_", " "))))
    : h("input", { type: pi.kind === "number" ? "number" : "text", value: v, oninput: (e) => (changes[pi.key] = e.target.value) });
  return [h("label", {}, pi.label), input, pi.help ? h("div", { class: "help" }, pi.help) : null];
}

async function tabSettings(s, full) {
  if (!full) return;
  const el = $("tab-settings");
  let props;
  try { props = await api("GET", sp("/properties")); } catch (e) { fill(el, msg("bad", null, e.message)); return; }
  const known = Object.keys(props).length > 3;
  const common = S.info.common_properties.filter((p) => !known || p.key in props);
  const changes = {};
  const save = () => act(() => api("PUT", sp("/properties"), changes), "Saved." + (s.state !== "stopped" ? " It counts after a restart." : ""));
  const mem = h("input", { type: "range", min: 512, max: Math.max(1024, S.info.memory_mb - 512), step: 256, value: s.ram_mb });
  const memLbl = h("span", {}, `${s.ram_mb} MB`);
  mem.oninput = () => (memLbl.textContent = `${mem.value} MB`);
  const jvm = h("input", { value: s.jvm_args, placeholder: "Most people leave this empty" });
  let flavor = s.flavor, pick = null;
  const pickerBox = h("div");
  const drawPicker = () => { pick = versionPicker(flavor, flavor === s.flavor ? { version: s.minecraft, build: s.build } : null); fill(pickerBox, pick.el); };
  drawPicker();
  const all = Object.keys(props).sort().filter((k) => !common.find((c) => c.key === k));
  fill(el,
    section("Game", "server.properties",
      h("div", { class: "form" }, common.filter((p) => p.kind !== "bool").map((p) => propField(p, props[p.key], changes)),
        common.filter((p) => p.kind === "bool").map((p) => propField(p, props[p.key], changes))),
      h("div", { class: "row mt" }, h("button", { class: "btn primary", onclick: save }, "Save"))),
    section("Memory and starting", null,
      h("div", { class: "form" },
        h("label", {}, "Memory"), h("div", { class: "row" }, h("div", { class: "grow" }, mem), memLbl),
        h("div", { class: "help" }, `This computer has ${S.info.memory_mb} MB. 2 to 4 GB suits most servers, big modpacks want 6 to 8.`),
        h("label", {}, "Java options"), jvm,
        h("span"), h("label", { class: "check" }, h("input", { type: "checkbox", checked: s.autostart, onchange: (e) => act(() => api("PATCH", sp(), { autostart: e.target.checked }), "Saved.") }), h("span", {}, "Start it when MineNest starts")),
        h("span"), h("label", { class: "check" }, h("input", { type: "checkbox", checked: s.auto_restart, onchange: (e) => act(() => api("PATCH", sp(), { auto_restart: e.target.checked }), "Saved.") }), h("span", {}, "Start it again if it crashes"))),
      h("div", { class: "row mt" }, h("button", { class: "btn primary", onclick: () => act(() => api("PATCH", sp(), { ram_mb: +mem.value, jvm_args: jvm.value }), "Saved." + (s.state !== "stopped" ? " It counts after a restart." : "")) }, "Save"))),
    section("Server type and version", "the world is backed up before a change",
      typeList(flavor, (id) => { flavor = id; drawPicker(); }),
      h("div", { class: "mt" }, pickerBox),
      h("div", { class: "row mt" },
        h("button", { class: "btn primary", disabled: s.state !== "stopped", onclick: () => { const v = pick.get(); confirmDialog("Change the server software?", `It becomes ${S.info.flavors.find((f) => f.id === flavor).name} ${v.version}. The world is backed up first. Going back to an older Minecraft can damage a world, so keep that backup.`, "Change it", () => act(() => api("POST", sp("/version"), { flavor, version: v.version, build: v.build }))); } }, "Change"),
        h("button", { class: "btn", disabled: s.state !== "stopped", onclick: () => act(() => api("POST", sp("/repair"))) }, "Download it again"),
        s.state !== "stopped" ? h("span", { class: "muted" }, "Stop the server first.") : null)),
    section("Every setting", "as server.properties has them",
      h("table", { class: "props" }, all.map((k) => h("tr", {}, h("td", {}, k), h("td", {}, h("input", { value: props[k], oninput: (e) => (changes[k] = e.target.value) }))))),
      h("div", { class: "row mt" }, h("button", { class: "btn primary", onclick: save }, "Save"))),
    section("Name and removal", null, h("div", { class: "row" }, h("button", { class: "btn", onclick: renameDialog }, "Rename"),
      h("button", { class: "btn danger", disabled: s.state !== "stopped", onclick: () => confirmDialog(`Delete ${s.name}?`, "The server, its world and its backups are deleted. This can't be undone.", "Delete", async () => { await act(() => api("DELETE", sp()), "Deleted."); S.cur = null; refresh(); }) }, "Delete this server"))));
}

// ------------------------------------------------------------------ world

async function tabWorld(s, full) {
  if (!full) return;
  const el = $("tab-world");
  let r;
  try { r = await api("GET", sp("/backups")); } catch (e) { fill(el, msg("bad", null, e.message)); return; }
  const seed = h("input", { placeholder: "Empty for a random one" });
  const size = (n) => (n >= 1048576 ? (n / 1048576).toFixed(1) + " MB" : Math.max(1, Math.round(n / 1024)) + " KB");
  fill(el,
    section("Backups", "MineNest also makes one before anything that could hurt the world",
      h("div", { class: "row" }, h("button", { class: "btn primary", disabled: !r.world, onclick: () => act(() => api("POST", sp("/backups"))) }, "Back up now"),
        !r.world ? h("span", { class: "muted" }, "There's no world yet: start the server once.") : null),
      h("div", { class: "mt" }, r.backups.length ? r.backups.map((b) => h("div", { class: "list-row" },
        h("div", { class: "grow" }, h("b", {}, new Date(b.time * 1000).toLocaleString()), h("span", { class: "muted" }, `  ${size(b.size)}, ${b.why}`)),
        h("button", { class: "btn small", disabled: s.state !== "stopped", onclick: () => confirmDialog("Put this backup back?", "The world as it is now is backed up first, so nothing is lost.", "Put it back", () => act(() => api("POST", sp(`/backups/${encodeURIComponent(b.name)}/restore`)))) }, "Put back"),
        h("button", { class: "btn small danger", onclick: () => confirmDialog("Delete this backup?", "It can't be brought back.", "Delete", () => act(() => api("DELETE", sp(`/backups/${encodeURIComponent(b.name)}`)), "Deleted.").then(() => tabWorld(S.status, true))) }, "Delete"))) : h("p", { class: "muted" }, "No backups yet."))),
    section("A new world", "the old one is backed up, then removed",
      h("div", { class: "form" }, h("label", {}, "Seed"), seed),
      h("div", { class: "row mt" }, h("button", { class: "btn danger", disabled: s.state !== "stopped", onclick: () => confirmDialog("Make a new world?", "The current world is backed up first. The next start makes the new one.", "New world", () => act(() => api("POST", sp("/world/reset"), { seed: seed.value }))) }, "Make a new world"),
        s.state !== "stopped" ? h("span", { class: "muted" }, "Stop the server first.") : null)));
}

// ------------------------------------------------------------------ internet

let vpsKey = "";
async function tabInternet(s, full) {
  const el = $("tab-internet");
  if (!el.firstChild || full) fill(el, h("div", { id: "net-top" }), h("div", { id: "net-extra" }), h("div", { id: "net-bedrock" }));
  const way = s.way || {};
  const kinds = [
    ["", "Only at home", "People on this computer and your home network. Nothing to set up."],
    ["playit", "playit.gg", "Free. Your playit.gg account gives the server an address. Bedrock works too."],
    ["relay", "Supporter relay", "For supporters on Patreon: your own name, like brave-otter.mc.owencosta.diy."],
    ["vps", "Your own VPS", "An SSH tunnel to a server you rent."],
  ];
  if (full && (way.kind === "vps" || S.vpsOpen)) {
    if (!vpsKey) { try { vpsKey = (await api("GET", "/api/sshkey")).key; } catch (e) {} }
    const host = h("input", { placeholder: "203.0.113.7 or vps.example.com" }), user = h("input", { value: "root" });
    const sshp = h("input", { type: "number", value: 22 }), port = h("input", { type: "number", value: 25565 });
    fill($("net-extra"), section("Your VPS", null,
      h("p", {}, "1. Put this line in ~/.ssh/authorized_keys for that user on the VPS:"), h("div", { class: "addr" }, h("span", { class: "lbl" }, "MineNest's key"), h("code", { style: "font-size:11.5px" }, vpsKey), copyBtn(vpsKey)),
      h("p", {}, "2. In /etc/ssh/sshd_config set ", h("code", {}, "GatewayPorts clientspecified"), ", restart sshd, and open the players' port in its firewall."),
      h("p", {}, "3. Its details:"),
      h("div", { class: "form" }, h("label", {}, "Address"), host, h("label", {}, "User"), user, h("label", {}, "SSH port"), sshp, h("label", {}, "Port for players"), port),
      h("div", { class: "row mt" }, h("button", { class: "btn primary", onclick: () => act(() => api("POST", sp("/way"), { kind: "vps", vps: { host: host.value.trim(), user: user.value.trim(), ssh_port: +sshp.value, public_port: +port.value } }), "Your VPS is the way in now.").then((ok) => { if (ok) S.vpsOpen = false; }) }, "Use this VPS"))));
  } else if (full) fill($("net-extra"));
  if (full || !$("net-bedrock").contains(document.activeElement)) {
    fill($("net-bedrock"), ["paper", "purpur"].includes(s.flavor) ? section("Bedrock players", "phones, consoles, Windows",
      h("label", { class: "check" }, h("input", { type: "checkbox", checked: s.bedrock_on, onchange: (e) => act(() => api("POST", sp("/bedrock"), { on: e.target.checked })) }),
        h("span", {}, "Let Bedrock players join", h("div", { class: "h" }, "Adds GeyserMC's Geyser and Floodgate. At home on port 19132, from the internet through playit.gg."))),
      s.bedrock_on ? h("div", { class: "mt" }, h("button", { class: "btn small", onclick: () => act(() => api("POST", sp("/bedrock"), { update: true })) }, "Update Geyser")) : null) : null);
  }
  const status = way.kind ? section("How it's going", null,
    way.address ? addrRow("Friends join at", way.address) : null,
    way.bedrock ? addrRow("Bedrock players", way.bedrock) : null,
    msg(way.up ? (way.check_ok || way.kind === "playit" ? "good" : "warn") : way.state === "problem" ? "bad" : "", way.up ? "Connected." : null,
      way.up ? (way.check || "") : (s.state === "running" ? (way.note || way.state) : "It connects when the server starts.")),
    way.claim_url ? msg("warn", "playit.gg", "Approve this computer on your playit.gg account (once): ", h("a", { href: way.claim_url, target: "_blank", rel: "noopener" }, "open playit.gg")) : null,
    h("div", { class: "row" },
      way.kind !== "playit" && s.state === "running" ? h("button", { class: "btn", onclick: () => act(() => api("POST", sp("/recheck")), "Checking from the internet...") }, "Check again") : null,
      way.kind === "playit" ? h("button", { class: "btn", onclick: () => confirmDialog("Forget playit.gg?", "This computer is disconnected from your playit.gg account. You can connect it again later.", "Forget", () => act(() => api("POST", "/api/playit/forget"), "Forgotten.")) }, "Forget playit.gg") : null,
      way.kind === "vps" ? h("button", { class: "btn", onclick: () => act(() => api("POST", sp("/way/forget-host")), "MineNest will trust the VPS's identity next time.") }, "Trust its new identity") : null)) : null;
  fill($("net-top"),
    section("How friends elsewhere join", null,
      h("div", { class: "types" }, kinds.map(([k, name, about]) => h("button", { class: "type" + ((way.kind || "") === k ? " on" : ""), onclick: () => {
        if (k === "vps") { S.vpsOpen = true; tabInternet(S.status, true); return; }
        if (k === "relay" && !S.info.account) { openAccount(); toast("Sign in with your XenithOS profile first.", true); return; }
        act(() => api("POST", sp("/way"), { kind: k }), k ? `${name} is the way in now.` : "Only people at home can join now.");
      } }, h("span", { class: "radio" }), h("b", {}, name), h("span", {}, about)))),
      way.kind === "relay" && ["paper", "purpur"].includes(s.flavor) ? h("label", { class: "check mt" }, h("input", { type: "checkbox", checked: s.real_ips, onchange: (e) => act(() => api("PATCH", sp(), { real_ips: e.target.checked }), "Saved. It counts from the next start.") }),
        h("span", {}, "Pass on players' real addresses", h("div", { class: "h" }, "So bans by address work through the relay. Not with Bedrock players on."))) : null),
    status);
}

// ------------------------------------------------------------------ dialogs

function modal(...kids) { fill($("modal-box"), kids); $("modal").classList.remove("hidden"); }
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
    h("p", {}, "Running a Minecraft server means agreeing to Minecraft's End User License Agreement. MineNest never agrees for you."),
    h("p", {}, h("a", { href: "https://aka.ms/MinecraftEULA", target: "_blank", rel: "noopener" }, "Read the EULA")),
    h("label", { class: "check" }, box, h("span", {}, "I've read Minecraft's EULA and I agree to it.")),
    h("div", { class: "modal-actions" }, h("button", { class: "btn", onclick: closeModal }, "Not now"), ok));
}
function renameDialog() {
  const name = h("input", { value: S.status.name, maxlength: 40 });
  modal(h("h2", {}, "Rename"), name, h("div", { class: "modal-actions" }, h("button", { class: "btn", onclick: closeModal }, "Cancel"),
    h("button", { class: "btn primary", onclick: () => { closeModal(); act(() => api("PATCH", sp(), { name: name.value }), "Renamed."); } }, "Rename")));
}

// ------------------------------------------------------------------ the new server page

function openCreate() {
  S.view = "create"; renderSide(); show("create");
  const el = $("create");
  const st = { flavor: "paper", modpack: null, packTitle: "" };
  const total = S.info.memory_mb || 4096;
  const name = h("input", { placeholder: "My Server", maxlength: 40 });
  const mem = h("input", { type: "range", min: 1024, max: Math.max(1536, total - 1024), step: 256, value: Math.min(Math.floor(total / 2 / 256) * 256, 2048) });
  const memLbl = h("span", {}, `${mem.value} MB`);
  const eula = h("input", { type: "checkbox" });
  const autostart = h("input", { type: "checkbox" });
  const settings = { "white-list": "false" };
  const sum = {};
  let pick = null;
  const pickerBox = h("div");
  const packArea = h("div", { class: "hidden" });
  const summary = h("div", { class: "kv" });
  const drawSummary = () => {
    const f = S.info.flavors.find((x) => x.id === st.flavor);
    const v = pick ? pick.get() : {};
    fill(summary,
      h("span", {}, "Name"), h("span", {}, name.value || "My Server"),
      h("span", {}, "Type"), h("span", {}, st.flavor === "modpack" ? (st.packTitle || "Choose a modpack") : f.name),
      st.flavor !== "modpack" ? [h("span", {}, "Minecraft"), h("span", {}, v.version || "newest")] : null,
      st.flavor !== "modpack" && st.flavor !== "vanilla" ? [h("span", {}, "Loader"), h("span", {}, v.build ? (sum.buildLabel || v.build) : "recommended")] : null,
      h("span", {}, "Mode"), h("span", {}, settings.gamemode || "survival"),
      h("span", {}, "Difficulty"), h("span", {}, settings.difficulty || "easy"),
      h("span", {}, "Memory"), h("span", {}, `${mem.value} MB`));
  };
  const drawPicker = () => {
    if (st.flavor === "modpack") { fill(pickerBox); pick = null; return; }
    pick = versionPicker(st.flavor, null, (v) => { sum.buildLabel = v.buildLabel; drawSummary(); });
    fill(pickerBox, pick.el);
  };
  mem.oninput = () => { memLbl.textContent = `${mem.value} MB`; drawSummary(); };
  name.oninput = drawSummary;

  const pq = h("input", { placeholder: 'Search modpacks, like "Cobblemon" or "All the Mods"' });
  const presults = h("div", { class: "hits", style: "max-height:360px;overflow:auto" });
  const searchPacks = async () => {
    fill(presults, h("p", { class: "muted" }, "Searching Modrinth..."));
    try {
      const r = await api("GET", "/api/modpacks?q=" + encodeURIComponent(pq.value));
      fill(presults, r.hits.map((hit) => hitRow(hit, h("button", { class: "btn small" + (st.modpack === hit.id ? " primary" : ""), onclick: () => { st.modpack = hit.id; st.packTitle = hit.title; if (!name.value) name.value = hit.title.slice(0, 40); searchPacks(); drawSummary(); } }, st.modpack === hit.id ? "Chosen" : "Choose"))));
    } catch (e) { fill(presults, msg("bad", null, e.message)); }
  };
  pq.addEventListener("keydown", (e) => { if (e.key === "Enter") searchPacks(); });
  fill(packArea, h("div", { class: "row" }, h("div", { class: "grow" }, pq), h("button", { class: "btn", onclick: searchPacks }, "Search")), presults);

  const P = Object.fromEntries(S.info.common_properties.map((p) => [p.key, p]));
  const game = ["gamemode", "difficulty", "max-players", "motd", "level-seed", "level-type", "view-distance", "simulation-distance", "spawn-protection"];
  const toggles = ["hardcore", "white-list", "online-mode", "allow-flight", "generate-structures", "enable-command-block"];
  const track = new Proxy(settings, { set(t, k, v) { t[k] = v; drawSummary(); return true; } });
  const make = async (btn) => {
    if (st.flavor === "modpack" && !st.modpack) { toast("Choose a modpack first.", true); return; }
    btn.disabled = true;
    const body = { name: name.value.trim() || "My Server", ram_mb: +mem.value, agree_eula: eula.checked, settings };
    if (st.flavor === "modpack") body.modpack = st.modpack;
    else { const v = pick.get(); body.flavor = st.flavor; body.version = v.version; body.build = v.build; }
    const r = await act(() => api("POST", "/api/servers", body));
    btn.disabled = false;
    if (r && r.id) {
      if (autostart.checked) api("PATCH", `/api/servers/${encodeURIComponent(r.id)}`, { autostart: true }).catch(() => {});
      S.cur = r.id; S.view = "server"; localStorage.setItem("mn-cur", r.id);
      refresh().then(() => setTab("overview"));
    }
  };
  fill(el,
    createHead("new"),
    h("div", { class: "create mt" },
      h("div", {},
        section(h("span", {}, h("span", { class: "step" }, "1"), "Server type"), "what it runs",
          typeList(st.flavor, (id) => { st.flavor = id; packArea.classList.toggle("hidden", id !== "modpack"); if (id === "modpack") { mem.value = Math.min(6144, total - 1024); memLbl.textContent = `${mem.value} MB`; searchPacks(); } drawPicker(); drawSummary(); }, true)),
        section(h("span", {}, h("span", { class: "step" }, "2"), "Version"), "the newest unless you pick one", pickerBox, packArea),
        section(h("span", {}, h("span", { class: "step" }, "3"), "Name and memory"), null,
          h("div", { class: "form" }, h("label", {}, "Name"), name,
            h("label", {}, "Memory"), h("div", { class: "row" }, h("div", { class: "grow" }, mem), memLbl),
            h("div", { class: "help" }, `This computer has ${total} MB. 2 to 4 GB suits most servers, modpacks want 6 or more.`),
            h("span"), h("label", { class: "check" }, autostart, h("span", {}, "Start it whenever MineNest starts")))),
        section(h("span", {}, h("span", { class: "step" }, "4"), "Game"), "server.properties",
          h("div", { class: "form" }, game.filter((k) => P[k]).map((k) => propField(P[k], undefined, track)), toggles.filter((k) => P[k]).map((k) => propField(P[k], k === "white-list" ? "false" : undefined, track))))),
      h("div", { class: "summary" }, h("h3", {}, "Summary"), summary,
        h("label", { class: "check mt" }, eula, h("span", { class: "small" }, "I've read ", h("a", { href: "https://aka.ms/MinecraftEULA", target: "_blank", rel: "noopener" }, "Minecraft's EULA"), " and agree to it. (Needed before the first start; you can do it later.)")),
        h("button", { class: "btn primary big", onclick: (e) => make(e.target) }, "Make the server"))));
  drawPicker();
  drawSummary();
  name.focus();
}

function createHead(mode) {
  return [h("div", { class: "srv-head" }, h("div", {}, h("h1", {}, mode === "new" ? "New server" : "Add a server you already have"),
      h("div", { class: "sub" }, mode === "new" ? "Everything here can be changed later in the server's Settings." : "MineNest copies it, so the original folder stays as it was.")),
      h("button", { class: "link", onclick: () => { S.view = "server"; refresh().then(() => renderTab(true)); } }, "Cancel")),
    h("div", { class: "tabs" },
      h("button", { class: mode === "new" ? "on" : "", onclick: openCreate }, "Make a new one"),
      h("button", { class: mode === "add" ? "on" : "", onclick: openImport }, "Add one you already have"))];
}

// Bringing in a server that's already on this computer: found by looking, typed, or a zip.
function openImport() {
  S.view = "create"; renderSide(); show("create");
  const el = $("create");
  const found = h("div", {}, h("p", { class: "muted" }, "Looking through this computer's usual folders..."));
  const path = h("input", { placeholder: S.info.os === "windows" ? "C:\\Users\\you\\Desktop\\My Server" : "/home/you/minecraft-server" });
  const name = h("input", { placeholder: "Taken from the folder's name", maxlength: 40 });
  const flavor = h("select", {}, h("option", { value: "" }, "Work it out"), S.info.flavors.map((f) => h("option", { value: f.id }, f.name)));
  const version = h("input", { placeholder: "Work it out (or type one, like 1.21.4)" });
  const zip = h("input", { type: "file", accept: ".zip", class: "hidden" });
  const zipName = h("span", { class: "muted" }, "No zip chosen");
  zip.onchange = () => (zipName.textContent = zip.files[0] ? `${zip.files[0].name}, ${(zip.files[0].size / 1048576).toFixed(0)} MB` : "No zip chosen");
  const done = (r) => { if (r && r.id) { S.cur = r.id; S.view = "server"; localStorage.setItem("mn-cur", r.id); refresh().then(() => setTab("overview")); } };
  const opts = () => ({ name: name.value.trim(), flavor: flavor.value, version: version.value.trim() });
  const add = async (p, extra) => done(await act(() => api("POST", "/api/import", { path: p, ...opts(), ...(extra || {}) })));
  const sendZip = async (btn) => {
    const f = zip.files[0];
    if (!f) { toast("Choose a zip first.", true); return; }
    btn.disabled = true; btn.textContent = "Uploading...";
    const o = opts();
    const q = new URLSearchParams({ filename: f.name, name: o.name, flavor: o.flavor, version: o.version });
    done(await act(() => api("POST", "/api/import/zip?" + q, f, true)));
    btn.disabled = false; btn.textContent = "Upload and add";
  };
  fill(el, createHead("add"),
    section("Found on this computer", "servers in your usual folders", found),
    section("Or say where it is", null,
      h("div", { class: "form" }, h("label", {}, "Its folder"), path, h("div", { class: "help" }, "The folder with server.properties in it.")),
      h("div", { class: "row mt" }, h("button", { class: "btn primary", onclick: () => path.value.trim() ? add(path.value.trim()) : toast("Type the folder first.", true) }, "Add it"))),
    section("Or upload it", "a zip of the server's folder, from anywhere",
      h("div", { class: "row" }, h("button", { class: "btn", onclick: () => zip.click() }, "Choose a zip..."), zipName, zip),
      h("div", { class: "row mt" }, h("button", { class: "btn primary", onclick: (e) => sendZip(e.target) }, "Upload and add"))),
    section("Details", "only if MineNest can't tell by itself",
      h("div", { class: "form" }, h("label", {}, "Name in MineNest"), name, h("label", {}, "Server type"), flavor, h("label", {}, "Minecraft version"), version)));
  (async () => {
    let list;
    try { list = await api("GET", "/api/import/find"); } catch (e) { fill(found, msg("bad", null, e.message)); return; }
    fill(found, list.length ? list.map((f) => h("div", { class: "list-row" },
      h("div", { class: "grow" }, h("b", {}, f.name), h("span", { class: "muted" }, `  ${f.flavor ? S.info.flavors.find((x) => x.id === f.flavor)?.name || f.flavor : "Unknown type"} ${f.version || "(version unknown)"}${f.has_world ? ", has a world" : ""}`),
        h("div", { class: "small muted", style: "word-break:break-all" }, f.path)),
      f.imported ? h("span", { class: "muted small" }, "Already added") : null,
      h("button", { class: "btn small" + (f.imported ? "" : " primary"), onclick: () => {
        if (!f.version && !version.value.trim()) { toast("MineNest can't tell its Minecraft version: type it under Details, then choose Add.", true); version.focus(); return; }
        add(f.path, { name: name.value.trim() || f.name });
      } }, "Add"))) : h("p", { class: "muted" }, "No servers found in the usual places. Type its folder below, or upload a zip."));
  })();
}

// ------------------------------------------------------------------ account and relay

async function openAccount() {
  S.view = "account"; renderSide(); show("account");
  const el = $("account");
  fill(el, h("p", { class: "muted" }, "Loading..."));
  let r;
  try { r = await api("GET", "/api/account"); } catch (e) { fill(el, msg("bad", null, e.message)); return; }
  if (r.signed_in && !r.step) { try { r = await api("POST", "/api/relay/status", {}); } catch (e) { toast(e.message, true); } }
  const head = h("div", { class: "srv-head" }, h("div", {}, h("h1", {}, "Account"), h("div", { class: "sub" }, "Your XenithOS profile and the supporter relay")),
    h("button", { class: "link", onclick: () => { S.view = "server"; refresh().then(() => renderTab(true)); } }, "Back to servers"));
  if (!r.signed_in) {
    const n = h("input", { placeholder: "Profile name" }), pin = h("input", { type: "password", inputmode: "numeric" });
    const go = (create) => act(() => api("POST", "/api/account/login", { name: n.value, pin: pin.value, create }), create ? "Profile made, you're signed in." : "Signed in.").then((a) => { if (a && a.name) { S.info.account = a.name; openAccount(); } });
    fill(el, head, h("div", { class: "mt" }), section("Sign in", null,
      h("p", { class: "muted" }, "MineNest uses the same profile as XenithOS. You only need one for the supporter relay; everything else works without."),
      h("div", { class: "form", style: "max-width:480px" }, h("label", {}, "Name"), n, h("label", {}, "PIN"), pin),
      h("div", { class: "row mt" }, h("button", { class: "btn primary", onclick: () => go(false) }, "Sign in"), h("button", { class: "btn", onclick: () => go(true) }, "Make a new profile"))));
    return;
  }
  const claimName = h("input", { placeholder: r.suggest || "your-server", value: r.name || "" });
  fill(el, head, h("div", { class: "mt" }),
    section("Profile", null, h("div", { class: "row" }, h("span", {}, "Signed in as ", h("b", {}, r.account)),
      h("button", { class: "btn small", onclick: () => act(() => api("POST", "/api/account/logout"), "Signed out.").then(() => { S.info.account = ""; openAccount(); }) }, "Sign out"))),
    section("Supporter relay", "your own address, for supporters on Patreon",
      !r.open ? msg("warn", "Closed", "The relay isn't taking supporters right now.") : null,
      r.message ? msg(r.step === "ok" ? "good" : "", null, r.message) : null,
      r.step === "link" && r.link_url ? h("p", {}, h("a", { href: r.link_url, target: "_blank", rel: "noopener" }, "Link your Patreon and this profile on OCAccounts")) : null,
      (r.step === "patreon" || r.step === "tier") && r.patreon ? h("p", {}, h("a", { href: r.patreon, target: "_blank", rel: "noopener" }, "Become a supporter on Patreon")) : null,
      r.tier ? h("p", { class: "muted" }, "Tier: " + r.tier) : null,
      r.name ? [addrRow("Your address", r.address), h("p", { class: "muted" }, r.used_by ? `${r.used_by} uses it.` : "To use it, choose Supporter relay on a server's Internet tab.")] : null,
      r.step === "ok" ? h("div", { class: "form mt" }, h("label", {}, r.name ? "A different name" : "Pick a name"),
        h("div", { class: "row" }, h("div", { class: "grow" }, claimName), h("span", { class: "muted" }, "." + (r.domain || "mc.owencosta.diy")),
          h("button", { class: "btn primary", onclick: () => act(() => api("POST", "/api/relay/claim", { name: claimName.value || r.suggest }), "It's yours.").then(openAccount) }, r.name ? "Change" : "Claim it")),
        h("div", { class: "help" }, "3 to 20 small letters, numbers and hyphens.")) : null,
      h("div", { class: "row mt" },
        h("button", { class: "btn", onclick: () => act(() => api("POST", "/api/relay/status", {}), "Checked.").then(openAccount) }, "Check my membership"),
        r.name ? h("button", { class: "btn danger", onclick: () => confirmDialog("Give up your name?", "It's kept for you for a week in case you change your mind.", "Give it up", () => act(() => api("POST", "/api/relay/release", {}), "Released.").then(openAccount)) }, "Give up the name") : null)));
}

// ------------------------------------------------------------------ start

async function main() {
  try { S.info = await api("GET", "/api/info"); } catch (e) { document.body.textContent = "MineNest isn't answering. Open it again from its icon."; return; }
  $("version").textContent = `MineNest ${S.info.version}`;
  for (const j of S.info.jobs) if (!j.finished) watchJob(j.id);
  $("new-server").onclick = openCreate;
  $("empty-new").onclick = openCreate;
  $("open-account").onclick = openAccount;
  $("btn-start").onclick = () => {
    if (S.status.eula_needed) { eulaDialog(); return; }
    act(() => api("POST", sp("/start")), "Starting...").then(() => setTab("console"));
  };
  $("btn-stop").onclick = () => act(() => api("POST", sp("/stop")), "Stopping, the world is saved first.");
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
