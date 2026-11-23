"use strict";
// The Files tab: browse and edit a server's own folder. Every name that comes from the server is
// put on the page with h(), as text, never as HTML. The daemon makes the real decisions (what
// may be touched, and from where); this page only shows what it says.

const F = { server: null, path: "", list: null, trash: null, view: "list", open: null, dirty: false, ups: [], note: null };

const fsize = (n) => (n >= 1 << 30 ? (n / (1 << 30)).toFixed(1) + " GB" : n >= 1 << 20 ? (n / (1 << 20)).toFixed(1) + " MB" : n >= 1024 ? Math.round(n / 1024) + " KB" : n + " B");
const fwhen = (t) => (t ? new Date(t * 1000).toLocaleString([], { day: "numeric", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit" }) : "");
const fjoin = (dir, name) => (dir ? dir + "/" + name : name);
const fparent = (p) => (p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "");
const fq = (o) => "?" + Object.entries(o).map(([k, v]) => `${k}=${encodeURIComponent(v)}`).join("&");
const fpath = (p, o) => sp("/files" + p) + (o ? fq(o) : "");
const fhref = (p, o) => fpath(p, o).replace(/^\//, ""); // relative, so it also works from another computer

async function tabFiles(s, full) {
  if (!full) return;
  if (F.server !== S.cur) Object.assign(F, { server: S.cur, path: "", list: null, trash: null, view: "list", open: null, dirty: false, ups: [], note: null });
  if (F.view === "edit" && F.open) { drawFiles(); return; }
  await loadFiles(F.path);
}

async function loadFiles(path) {
  const el = $("tab-files");
  try {
    const [list, trash] = await Promise.all([api("GET", fpath("", { path })), api("GET", fpath("/trash"))]);
    F.list = list; F.trash = trash; F.path = list.path; F.view = F.view === "trash" ? "trash" : "list";
  } catch (e) {
    if (F.list && path !== "") { toast(e.message, true); return; }
    fill(el, msg("bad", null, e.message));
    return;
  }
  drawFiles();
}

function drawFiles() {
  const el = $("tab-files");
  if (F.view === "edit" && F.open) fill(el, editorView());
  else if (F.view === "trash") fill(el, trashView());
  else fill(el, listView());
}

function crumbs(path, last) {
  const parts = path ? path.split("/") : [];
  const out = [h("button", { class: "link", onclick: () => goDir("") }, "Server folder")];
  parts.forEach((p, i) => {
    const to = parts.slice(0, i + 1).join("/");
    out.push(h("span", { class: "sep" }, "/"));
    out.push(i === parts.length - 1 && !last ? h("b", {}, p) : h("button", { class: "link", onclick: () => goDir(to) }, p));
  });
  if (last) { out.push(h("span", { class: "sep" }, "/")); out.push(h("b", {}, last)); }
  return h("div", { class: "crumbs" }, out);
}

async function goDir(path) {
  if (F.dirty && !(await leaveOk())) return;
  F.view = "list"; F.open = null; F.dirty = false;
  await loadFiles(path);
}
function leaveOk() {
  return new Promise((res) => modal(h("h2", {}, "Leave without saving?"), h("p", {}, "The changes you made to this file haven't been saved."),
    h("div", { class: "modal-actions" },
      h("button", { class: "btn", onclick: () => { closeModal(); res(false); } }, "Stay"),
      h("button", { class: "btn primary", onclick: () => { closeModal(); res(true); } }, "Leave"))));
}

// "replace?" asks once when something is already there, then does it again with overwrite set.
async function replacing(what, fn) {
  try { return await fn(false); } catch (e) {
    if (e.status !== 409) { toast(e.message, true); return null; }
    return new Promise((res) => {
      modal(h("h2", {}, "Replace it?"), h("p", {}, e.message + " "), h("p", { class: "muted" }, "What is replaced goes to the trash, so it can come back."),
        h("div", { class: "modal-actions" },
          h("button", { class: "btn", onclick: () => { closeModal(); res(null); } }, "Cancel"),
          h("button", { class: "btn primary", onclick: async () => { closeModal(); try { res(await fn(true)); } catch (e2) { toast(e2.message, true); res(null); } } }, "Replace")));
    });
  }
}

const canChange = () => F.list && F.list.can_change;
const isZip = (n) => /\.zip$/i.test(n);

function listView() {
  const l = F.list;
  const fileInput = h("input", { type: "file", multiple: true, class: "hidden", onchange: (e) => { uploadFiles([...e.target.files]); e.target.value = ""; } });
  const ups = h("div", { id: "files-ups" });
  F.upsEl = ups;
  const box = h("div", { class: "files-box" });
  const rows = l.entries.map(fileRow);
  fill(box, h("div", { class: "frow fhead" }, h("span", {}, "Name"), h("span", { class: "num" }, "Size"), h("span", {}, "Changed"), h("span")),
    l.path ? h("div", { class: "frow" }, h("button", { class: "link", onclick: () => goDir(fparent(l.path)) }, ".."), h("span"), h("span"), h("span")) : null,
    rows, !rows.length ? h("p", { class: "muted pad" }, "This folder is empty.") : null,
    l.truncated ? h("p", { class: "muted pad" }, "Only the first " + l.entries.length + " are shown.") : null,
    l.hidden ? h("p", { class: "muted small pad" }, `${l.hidden} with names MineNest won't handle (odd characters) aren't shown.`) : null);
  if (canChange()) {
    box.addEventListener("dragover", (e) => { if ([...(e.dataTransfer.types || [])].includes("Files")) { e.preventDefault(); box.classList.add("drop"); } });
    box.addEventListener("dragleave", (e) => { if (e.target === box) box.classList.remove("drop"); });
    box.addEventListener("drop", (e) => {
      e.preventDefault(); box.classList.remove("drop");
      const items = [...(e.dataTransfer.items || [])];
      const files = [...e.dataTransfer.files].filter((f, i) => {
        const en = items[i] && items[i].webkitGetAsEntry && items[i].webkitGetAsEntry();
        if (en && en.isDirectory) { toast(`${f.name} is a folder. Put it in a zip, upload that, then unzip it here.`, true); return false; }
        return true;
      });
      uploadFiles(files);
    });
  }
  const trashN = F.trash ? F.trash.items.length : 0;
  setTimeout(drawUps, 0);
  return [
    F.note ? h("div", { class: "msg warn" }, h("b", { class: "tag" }, "Restart needed"), F.note, " ", h("button", { class: "link", onclick: () => { F.note = null; drawFiles(); } }, "Dismiss")) : null,
    !l.can_change ? msg("", "Look only", l.why || "Files can't be changed from here.") : null,
    h("div", { class: "files-top" }, crumbs(l.path),
      h("div", { class: "row" },
        canChange() ? [h("button", { class: "btn primary small", onclick: () => fileInput.click() }, "Upload"), fileInput,
          h("button", { class: "btn small", onclick: newFolderDialog }, "New folder"),
          h("button", { class: "btn small", onclick: newFileDialog }, "New file")] : null,
        h("button", { class: "btn small", onclick: () => loadFiles(F.path) }, "Refresh"),
        h("button", { class: "btn small", onclick: () => { F.view = "trash"; drawFiles(); } }, trashN ? `Trash (${trashN})` : "Trash"))),
    ups, box,
    h("p", { class: "muted small mt" },
      l.running ? "The server is running, so its world and its own program are locked. " : "",
      l.free >= 0 ? `${fsize(l.free)} is free on this disk. ` : "",
      canChange() ? `Files up to ${fsize(l.max_upload)} can be uploaded; drop them on the list.` : "")];
}

function fileRow(e) {
  const p = fjoin(F.list.path, e.name);
  const locked = e.locked ? { disabled: true, title: e.locked } : {};
  const change = canChange();
  const acts = [];
  if (e.dir) acts.push(h("a", { class: "btn small", href: fhref("/zip", { path: p }), download: "" }, "Zip"));
  else if (!e.link) acts.push(h("a", { class: "btn small", href: fhref("/download", { path: p }), download: "" }, "Download"));
  if (change) {
    if (!e.dir && !e.link && isZip(e.name)) acts.push(h("button", { class: "btn small", onclick: () => unzipDialog(p) }, "Unzip"));
    acts.push(h("button", Object.assign({ class: "btn small", onclick: () => moveDialog(p, e, "Rename", true) }, locked), "Rename"));
    acts.push(h("button", Object.assign({ class: "btn small", onclick: () => moveDialog(p, e, "Move", false) }, locked), "Move"));
    if (!e.link) acts.push(h("button", { class: "btn small", onclick: () => copyDialog(p, e) }, "Copy"));
    acts.push(h("button", Object.assign({ class: "btn small danger", onclick: () => deleteDialog(p, e) }, locked), "Delete"));
  }
  const name = e.dir ? h("button", { class: "link fname dir", onclick: () => goDir(p) }, e.name)
    : e.link ? h("span", { class: "fname", title: "A link. MineNest doesn't follow links." }, e.name)
    : e.kind === "text" || e.kind === "log" ? h("button", { class: "link fname", onclick: () => openFile(p) }, e.name)
    : h("span", { class: "fname" }, e.name);
  return h("div", { class: "frow" },
    h("span", { class: "fcell" }, name, e.link ? h("span", { class: "muted small" }, "  link") : null, e.locked ? h("span", { class: "muted small", title: e.locked }, "  locked") : null),
    h("span", { class: "num muted" }, e.dir ? `${e.items} item${e.items === 1 ? "" : "s"}` : e.link ? "" : fsize(e.size)),
    h("span", { class: "muted" }, fwhen(e.mod)),
    h("span", { class: "facts" }, acts));
}

// ------------------------------------------------------------------ dialogs

function askName(title, label, value, okText, fn, help) {
  const inp = h("input", { value, autocomplete: "off", spellcheck: "false" });
  const go = () => { const v = inp.value.trim(); if (!v) return; closeModal(); fn(v); };
  inp.addEventListener("keydown", (e) => { if (e.key === "Enter") go(); });
  modal(h("h2", {}, title), h("div", { class: "form" }, h("label", {}, label), inp, help ? h("div", { class: "help" }, help) : null),
    h("div", { class: "modal-actions" }, h("button", { class: "btn", onclick: closeModal }, "Cancel"), h("button", { class: "btn primary", onclick: go }, okText)));
  inp.focus(); inp.select();
}

function newFolderDialog() {
  askName("New folder", "Name", "", "Make it", async (name) => {
    try { await api("POST", fpath("/mkdir"), { path: fjoin(F.path, name) }); loadFiles(F.path); } catch (e) { toast(e.message, true); }
  });
}
function newFileDialog() {
  askName("New text file", "Name", "", "Make it", async (name) => {
    try { await api("PUT", fpath("/content"), { path: fjoin(F.path, name), text: "", create: true }); await openFile(fjoin(F.path, name)); } catch (e) { toast(e.message, true); }
  }, "A text file: .txt, .yml, .json, .properties and so on.");
}
function moveDialog(p, e, title, nameOnly) {
  const sibling = (v) => (nameOnly ? fjoin(fparent(p), v) : v);
  askName(`${title} ${e.name}`, nameOnly ? "New name" : "New place", nameOnly ? e.name : p, title, async (v) => {
    const to = sibling(v);
    const r = await replacing(to, (ow) => api("POST", fpath("/move"), { from: p, to, overwrite: ow }));
    if (r) { toast(nameOnly ? "Renamed." : "Moved."); loadFiles(F.path); }
  }, nameOnly ? null : "The whole path from the server's folder, like plugins/Essentials/config.yml. Folders it needs have to exist.");
}
function copyDialog(p, e) {
  const dot = e.name.lastIndexOf(".");
  const copyName = dot > 0 && !e.dir ? e.name.slice(0, dot) + " copy" + e.name.slice(dot) : e.name + " copy";
  askName(`Copy ${e.name}`, "Copy to", fjoin(fparent(p), copyName), "Copy", async (to) => {
    const r = await replacing(to, (ow) => api("POST", fpath("/copy"), { from: p, to, overwrite: ow }));
    if (r) { toast(r.skipped ? `Copied. ${r.skipped} links or special files were left out.` : "Copied."); loadFiles(F.path); }
  });
}
function deleteDialog(p, e) {
  confirmDialog(`Delete ${e.name}?`, "It goes to the trash, where you can bring it back or delete it for good.", "Move to trash", async () => {
    try { await api("POST", fpath("/delete"), { paths: [p] }); toast("Moved to the trash."); loadFiles(F.path); } catch (er) { toast(er.message, true); }
  });
}
function unzipDialog(p) {
  const run = async () => {
    const r = await replacing(p, (ow) => api("POST", fpath("/unzip"), { path: p, overwrite: ow }));
    if (!r) return;
    toast(`Unpacked ${r.files} file${r.files === 1 ? "" : "s"}.` + (r.skipped && r.skipped.length ? ` ${r.skipped.length} programs or scripts were left out.` : ""));
    if (r.restart) F.note = "Plugins or mods were added. The server needs a restart to load them.";
    loadFiles(F.path);
  };
  confirmDialog("Unpack this zip here?", "Its files are unpacked into this folder. Anything already there is only replaced if you say so.", "Unpack", run);
}

// ------------------------------------------------------------------ upload

function drawUps() {
  const el = F.upsEl;
  if (!el || !el.isConnected) return;
  fill(el, F.ups.map((u) => h("div", { class: "job upl" }, h("div", { class: "t" }, u.name), h("div", { class: "s" }, u.msg),
    u.state === "error" ? null : h("div", { class: "bar" }, h("div", { style: `width:${u.pct}%` })))));
}

async function uploadFiles(files) {
  if (!files.length) return;
  const dir = F.path;
  for (const f of files) {
    const u = { name: f.name, pct: 0, state: "wait", msg: `Waiting, ${fsize(f.size)}` };
    F.ups.push(u);
    drawUps();
    if (F.list && f.size > F.list.max_upload) { Object.assign(u, { state: "error", msg: `Too big: ${fsize(f.size)} is more than the ${fsize(F.list.max_upload)} limit.` }); drawUps(); continue; }
    try {
      await sendFile(f, dir, u, false);
    } catch (e) {
      if (e.status === 409) {
        const yes = await new Promise((res) => modal(h("h2", {}, "Replace it?"), h("p", {}, `${f.name} is already in this folder.`), h("p", { class: "muted" }, "The old one goes to the trash, so it can come back."),
          h("div", { class: "modal-actions" }, h("button", { class: "btn", onclick: () => { closeModal(); res(false); } }, "Cancel"), h("button", { class: "btn primary", onclick: () => { closeModal(); res(true); } }, "Replace"))));
        if (yes) { try { await sendFile(f, dir, u, true); } catch (e2) { Object.assign(u, { state: "error", msg: e2.message }); } } else Object.assign(u, { state: "error", msg: "Not uploaded." });
      } else Object.assign(u, { state: "error", msg: e.message });
    }
    drawUps();
  }
  if (F.path === dir && F.view === "list") await loadFiles(dir);
  setTimeout(() => { F.ups = F.ups.filter((u) => u.state === "error"); drawUps(); }, 4000);
}

function sendFile(f, dir, u, overwrite) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", fhref("/upload", { dir, name: f.name, overwrite: overwrite ? 1 : 0 }));
    xhr.setRequestHeader("X-MineNest", "1");
    u.state = "up"; u.pct = 0; u.msg = "Uploading";
    xhr.upload.onprogress = (e) => { if (e.lengthComputable) { u.pct = Math.round((100 * e.loaded) / e.total); u.msg = `${fsize(e.loaded)} of ${fsize(e.total)}`; drawUps(); } };
    xhr.onerror = () => reject(new Error("The upload couldn't be sent. Is MineNest still running?"));
    xhr.onload = () => {
      let data = null;
      try { data = JSON.parse(xhr.responseText); } catch (e) {}
      if (xhr.status >= 200 && xhr.status < 300) {
        u.state = "done"; u.pct = 100; u.msg = "Uploaded";
        if (data && data.restart) F.note = data.running ? `${f.name} is in. The server is running, so restart it to load it.` : `${f.name} is in. It loads the next time the server starts.`;
        resolve(data);
      } else { const er = new Error((data && data.error) || `MineNest answered ${xhr.status}`); er.status = xhr.status; reject(er); }
    };
    xhr.send(f);
  });
}

// ------------------------------------------------------------------ the editor

async function openFile(p) {
  if (F.dirty && !(await leaveOk())) return;
  try {
    F.open = await api("GET", fpath("/read", { path: p }));
    F.view = "edit"; F.dirty = false;
    drawFiles();
    const ta = document.getElementById("editor");
    if (ta && F.open.read_only) ta.scrollTop = ta.scrollHeight;
  } catch (e) { toast(e.message, true); }
}

const eolWords = { lf: "Unix line endings", crlf: "Windows line endings", cr: "old Mac line endings" };
const encWords = { "utf-8": "UTF-8", "utf-8-bom": "UTF-8 with a byte order mark", "utf-16le": "UTF-16", "utf-16be": "UTF-16", latin1: "a plain Western encoding (Latin-1)" };

function editorView() {
  const o = F.open;
  const ro = o.read_only || !canChange();
  const ta = h("textarea", { id: "editor", class: "editor", spellcheck: "false", wrap: "off", readonly: ro, "aria-label": o.name });
  ta.value = o.text;
  const save = h("button", { class: "btn primary", disabled: true, onclick: () => saveFile(ta, false) }, "Save");
  ta.addEventListener("input", () => { F.dirty = ta.value !== o.text; save.disabled = !F.dirty; });
  ta.addEventListener("keydown", (e) => { if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") { e.preventDefault(); if (!save.disabled) saveFile(ta, false); } });
  const info = [fsize(o.size), o.kind === "log" ? null : encWords[o.encoding], o.kind === "log" ? null : eolWords[o.eol]].filter(Boolean).join(", ");
  const isProps = o.path.toLowerCase() === "server.properties";
  return [
    h("div", { class: "files-top" }, crumbs(fparent(o.path), o.name),
      h("div", { class: "row" },
        ro ? null : save,
        o.kind === "log" ? h("button", { class: "btn", onclick: () => openFile(o.path) }, "Refresh") : null,
        h("a", { class: "btn", href: fhref("/download", { path: o.path }), download: "" }, o.kind === "log" ? "Download the whole log" : "Download"),
        h("button", { class: "btn", onclick: () => goDir(fparent(o.path)) }, "Close"))),
    o.read_only ? msg("", "Read only", o.why, o.tail ? ` Showing the newest ${o.lines} lines.` : "") : !canChange() ? msg("", "Look only", F.list && F.list.why ? F.list.why : "Files can't be changed from here.") : null,
    o.mixed ? msg("warn", "Line endings", `This file mixes line endings. Saving makes them all ${eolWords[o.eol]}.`) : null,
    h("div", { class: "muted small" }, info),
    ta,
    isProps && !ro ? h("p", { class: "muted small mt" }, "MineNest checks the port when this is saved, and keeps the settings it looks after itself.") : null];
}

async function saveFile(ta, force) {
  const o = F.open;
  try {
    const r = await api("PUT", fpath("/content"), { path: o.path, text: ta.value, encoding: o.encoding, eol: o.eol, version: o.version, force });
    F.open = r; F.dirty = false;
    toast("Saved." + (S.status && S.status.state !== "stopped" ? " It counts after a restart." : ""));
    drawFiles();
  } catch (e) {
    if (e.status === 409) {
      confirmDialog("This file changed", e.message + " Saving now would replace those changes.", "Save anyway", () => saveFile(ta, true));
    } else toast(e.message, true);
  }
}

// ------------------------------------------------------------------ the trash

function trashView() {
  const items = F.trash ? F.trash.items : [];
  const change = canChange();
  return [
    h("div", { class: "files-top" }, h("div", { class: "crumbs" }, h("button", { class: "link", onclick: () => { F.view = "list"; drawFiles(); } }, "Server folder"), h("span", { class: "sep" }, "/"), h("b", {}, "Trash")),
      h("div", { class: "row" }, change && items.length ? h("button", { class: "btn small danger", onclick: emptyTrashDialog }, "Empty the trash") : null,
        h("button", { class: "btn small", onclick: () => { F.view = "list"; drawFiles(); } }, "Back to the files"))),
    h("p", { class: "muted" }, "What you delete in Files waits here, kept by MineNest outside the server's folder. Nothing in it is gone until you empty the trash."
      + (items.length ? ` It holds ${fsize(F.trash.size)}.` : "")),
    h("div", { class: "files-box" }, items.length ? items.map((it) => h("div", { class: "frow trow" },
      h("span", { class: "fcell" }, h("b", {}, it.name), h("span", { class: "muted small" }, `  from ${it.orig ? it.orig : "the server's folder"}`)),
      h("span", { class: "num muted" }, it.dir ? `${it.count} file${it.count === 1 ? "" : "s"}, ${fsize(it.size)}` : fsize(it.size)),
      h("span", { class: "muted" }, fwhen(it.deleted)),
      h("span", { class: "facts" }, change ? [
        h("button", { class: "btn small", onclick: () => restoreItem(it) }, "Put back"),
        h("button", { class: "btn small danger", onclick: () => confirmDialog(`Delete ${it.name} for good?`, "This can't be undone.", "Delete for good", async () => { try { await api("DELETE", fpath("/trash/" + it.id)); loadFiles(F.path); } catch (e) { toast(e.message, true); } }) }, "Delete for good")] : null)))
      : h("p", { class: "muted pad" }, "The trash is empty."))];
}
async function restoreItem(it) {
  const r = await replacing(it.orig, (ow) => api("POST", fpath(`/trash/${it.id}/restore`), { overwrite: ow }));
  if (r) { toast(`Put back as ${r.path}.`); loadFiles(F.path); }
}
function emptyTrashDialog() {
  confirmDialog("Empty the trash?", "Everything in it is deleted for good. This can't be undone.", "Empty the trash", async () => {
    try { await api("POST", fpath("/trash/empty"), { confirm: true }); toast("The trash is empty."); loadFiles(F.path); } catch (e) { toast(e.message, true); }
  });
}
