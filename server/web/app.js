"use strict";

const $ = (s) => document.querySelector(s);
const state = { devices: [], current: null, path: "", parent: "", timer: null, sel: new Map(), entries: [], me: { user: "", role: "" } };

const isAdmin = () => state.me.role === "admin";
const canEdit = () => state.me.role === "admin" || state.me.role === "editor";
const SHEET_EXT = /\.(xlsx|xlsm|xls|csv|ods)$/i;
const isSheet = (name) => SHEET_EXT.test(name || "");

async function api(method, url, body) {
  const opts = { method, headers: { "X-Requested-With": "rf" }, credentials: "same-origin" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(url, opts);
  if (res.status === 401 && url !== "/api/login") {
    showLogin();
    throw new Error("Please sign in again");
  }
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`);
  return data;
}

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v);
  }
  for (const c of children) e.append(c instanceof Node ? c : document.createTextNode(c ?? ""));
  return e;
}

const fmtSize = (n) => {
  if (n < 1024) return `${n} B`;
  const u = ["KB", "MB", "GB", "TB"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return `${n.toFixed(n < 10 ? 1 : 0)} ${u[i]}`;
};
const fmtTime = (t) => (t && !t.startsWith("0001") ? new Date(t).toLocaleString() : "—");
const osLabel = (o) => ({ windows: "Windows", darwin: "macOS", linux: "Linux" }[o] || o);

// ---- auth ----
function showLogin() {
  clearInterval(state.timer);
  $("#app-view").hidden = true;
  $("#login-view").hidden = false;
}

async function showApp() {
  const me = await api("GET", "/api/me");
  state.me = { user: me.user, role: me.role, excel: !!me.excel };
  $("#company").textContent = me.company ? `· ${me.company}` : "";
  $("#login-view").hidden = true;
  $("#app-view").hidden = false;
  applyRole();
  await loadDevices();
  clearInterval(state.timer);
  state.timer = setInterval(() => loadDevices().catch(() => {}), 10000);
}

function applyRole() {
  const badge = $("#me-badge");
  badge.hidden = false;
  badge.textContent = `${state.me.user} · ${state.me.role}`;
  badge.className = "role-badge r-" + state.me.role;
  // admin-only nav tabs
  document.querySelectorAll(".admin-only").forEach((el) => (el.hidden = !isAdmin()));
  // device management (add/rename/revoke) is admin-only
  $("#add-device").style.display = isAdmin() ? "" : "none";
  $("#rename-device").style.display = isAdmin() ? "" : "none";
  $("#revoke-device").style.display = isAdmin() ? "" : "none";
  // editor+ actions
  for (const id of ["#act-upload", "#act-mkdir", "#act-rename", "#act-copy", "#act-move"])
    $(id).style.display = canEdit() ? "" : "none";
  // delete is admin-only
  $("#act-delete").style.display = isAdmin() ? "" : "none";
}

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  $("#login-error").hidden = true;
  try {
    await api("POST", "/api/login", { username: f.get("username"), password: f.get("password") });
    e.target.reset();
    await showApp();
  } catch (err) {
    $("#login-error").textContent = err.message;
    $("#login-error").hidden = false;
  }
});

$("#logout").addEventListener("click", async () => {
  await api("POST", "/api/logout").catch(() => {});
  showLogin();
});

// ---- tabs ----
document.querySelectorAll(".tab").forEach((b) =>
  b.addEventListener("click", () => {
    document.querySelectorAll(".tab").forEach((x) => x.classList.toggle("active", x === b));
    document.querySelectorAll(".tab-panel").forEach((p) => (p.hidden = p.id !== `tab-${b.dataset.tab}`));
    if (b.dataset.tab === "audit") loadAudit();
    if (b.dataset.tab === "users") loadUsers();
  })
);

// ---- devices ----
async function loadDevices() {
  state.devices = await api("GET", "/api/devices");
  const list = $("#device-list");
  list.replaceChildren();
  $("#no-devices").hidden = state.devices.length > 0;
  for (const d of state.devices) {
    const li = el("li", { class: "device" + (state.current === d.id ? " selected" : "") },
      el("span", { class: "dot " + (d.online ? "on" : "off"), title: d.online ? "Online" : "Offline" }),
      el("div", { class: "grow" },
        el("div", { class: "name" }, d.name || d.hostname),
        el("div", { class: "muted small" }, `${osLabel(d.os)} · ${d.username || "?"} · ${d.online ? "online" : "seen " + fmtTime(d.lastSeen)}`)));
    li.addEventListener("click", () => selectDevice(d.id));
    list.append(li);
  }
  if (state.current) renderDeviceHeader();
}

function currentDevice() {
  return state.devices.find((d) => d.id === state.current);
}

function renderDeviceHeader() {
  const d = currentDevice();
  if (!d) return;
  $("#dev-title").textContent = d.name || d.hostname;
  $("#dev-meta").textContent =
    `${d.hostname} · ${osLabel(d.os)} ${d.arch} · user ${d.username} · ${d.online ? "online" : "offline"} · IP ${d.lastIp} · agent ${d.agentVersion}`;
}

async function selectDevice(id) {
  state.current = id;
  state.path = "";
  $("#browser-empty").hidden = true;
  $("#browser").hidden = false;
  $("#file-rows").replaceChildren();
  $("#cur-path").textContent = "";
  document.querySelectorAll(".device").forEach((li, i) =>
    li.classList.toggle("selected", state.devices[i]?.id === id));
  renderDeviceHeader();
  await loadDrives();
}

function showBrowseError(msg) {
  $("#browse-error").textContent = msg || "";
  $("#browse-error").hidden = !msg;
}

async function loadDrives() {
  const box = $("#drives");
  box.replaceChildren(el("span", { class: "muted small" }, "Loading drives…"));
  showBrowseError("");
  try {
    const drives = await api("GET", `/api/devices/${state.current}/drives`);
    box.replaceChildren(...drives.map((d) =>
      el("button", { class: "btn small drive", onclick: () => browse(d.path) }, d.label || d.path)));
    if (drives.length && !state.path) browse(drives[0].path);
  } catch (err) {
    box.replaceChildren();
    showBrowseError(err.message);
  }
}

function clearSelection() {
  state.sel = new Map();
  syncToolbar();
}

function syncToolbar() {
  const n = state.sel ? state.sel.size : 0;
  const only = n === 1 ? [...state.sel.values()][0] : null;
  $("#sel-count").textContent = n === 0 ? "No items selected" : `${n} selected`;
  $("#act-download").disabled = !(n === 1 && only && !only.dir);
  const excelBtn = $("#act-excel");
  const canExcel = state.me.excel && canEdit();
  excelBtn.hidden = !canExcel;
  excelBtn.disabled = !(canExcel && n === 1 && only && !only.dir && isSheet(only.name));
  $("#act-rename").disabled = n !== 1;
  $("#act-copy").disabled = n === 0;
  $("#act-move").disabled = n === 0;
  $("#act-delete").disabled = n === 0;
  const all = $("#sel-all");
  const total = (state.entries || []).length;
  all.checked = total > 0 && n === total;
  all.indeterminate = n > 0 && n < total;
}

async function browse(path) {
  showBrowseError("");
  hideCtx();
  const rows = $("#file-rows");
  rows.replaceChildren(el("tr", {}, el("td", { colspan: "4", class: "muted" }, "Loading…")));
  try {
    const res = await api("GET", `/api/devices/${state.current}/list?path=${encodeURIComponent(path)}`);
    state.path = res.path;
    state.parent = res.parent;
    state.entries = res.entries;
    clearSelection();
    $("#cur-path").textContent = res.path;
    $("#up").disabled = !res.parent;
    $("#truncated").hidden = !res.truncated;
    renderRows();
    if (!res.entries.length) rows.replaceChildren(el("tr", {}, el("td", { colspan: "4", class: "muted" }, "Empty folder")));
  } catch (err) {
    rows.replaceChildren();
    showBrowseError(err.message);
  }
}

function renderRows() {
  const rows = $("#file-rows");
  rows.replaceChildren(...state.entries.map((e) => {
    const cb = el("input", { type: "checkbox", "aria-label": "Select " + e.name });
    cb.checked = state.sel.has(e.path);
    cb.addEventListener("click", (ev) => { ev.stopPropagation(); toggleSel(e, cb.checked); });
    const nameTd = el("td", { class: "fname" }, el("span", { class: "ico" }, e.dir ? "📁" : "📄"), e.name);
    const tr = el("tr", { class: (e.dir ? "dir " : "") + (state.sel.has(e.path) ? "sel" : "") },
      el("td", { class: "chk" }, cb), nameTd,
      el("td", { class: "num" }, e.dir ? "" : fmtSize(e.size)),
      el("td", {}, fmtTime(e.modTime)));
    nameTd.addEventListener("click", () => { if (e.dir) browse(e.path); });
    tr.addEventListener("contextmenu", (ev) => {
      ev.preventDefault();
      if (!state.sel.has(e.path)) { state.sel = new Map([[e.path, e]]); renderRows(); syncToolbar(); }
      showCtx(ev.clientX, ev.clientY, e);
    });
    return tr;
  }));
}

function toggleSel(e, on) {
  if (on) state.sel.set(e.path, e); else state.sel.delete(e.path);
  // toggle row class without full re-render
  for (const tr of $("#file-rows").children) {
    const cb = tr.querySelector("input");
    if (cb && cb.getAttribute("aria-label") === "Select " + e.name) tr.classList.toggle("sel", on);
  }
  syncToolbar();
}

$("#sel-all").addEventListener("change", (ev) => {
  if (ev.target.checked) state.sel = new Map(state.entries.map((e) => [e.path, e]));
  else state.sel = new Map();
  renderRows();
  syncToolbar();
});

$("#up").addEventListener("click", () => state.parent && browse(state.parent));
$("#refresh").addEventListener("click", () => (state.path ? browse(state.path) : loadDrives()));

$("#rename-device").addEventListener("click", async () => {
  const d = currentDevice();
  const name = prompt("Device name", d?.name || d?.hostname || "");
  if (name === null) return;
  try { await api("PATCH", `/api/devices/${state.current}`, { name }); await loadDevices(); }
  catch (err) { alert(err.message); }
});

$("#revoke-device").addEventListener("click", async () => {
  const d = currentDevice();
  if (!confirm(`Revoke access to "${d?.name || d?.hostname}"? The agent on that computer will disconnect and a new invite will be needed.`)) return;
  try {
    await api("DELETE", `/api/devices/${state.current}`);
    state.current = null;
    $("#browser").hidden = true;
    $("#browser-empty").hidden = false;
    await loadDevices();
  } catch (err) { alert(err.message); }
});

// ---- file actions ----
function selectedEntries() { return [...state.sel.values()]; }
function reload() { return state.path ? browse(state.path) : loadDrives(); }

async function opCall(path, body, okMsg) {
  showBrowseError("");
  try {
    await api("POST", `/api/devices/${state.current}/${path}`, body);
    await reload();
  } catch (err) { showBrowseError(err.message); }
}

$("#act-mkdir").addEventListener("click", async () => {
  if (!state.path) return;
  const name = await promptDialog({ title: "New folder", label: "Folder name", value: "New folder" });
  if (name == null) return;
  opCall("mkdir", { dir: state.path, name });
});

$("#act-rename").addEventListener("click", async () => {
  const e = selectedEntries()[0];
  if (!e) return;
  const newName = await promptDialog({ title: "Rename", label: "New name", value: e.name });
  if (newName == null || newName === e.name) return;
  opCall("rename", { path: e.path, newName });
});

$("#act-delete").addEventListener("click", async () => {
  const items = selectedEntries();
  if (!items.length) return;
  const hasDir = items.some((e) => e.dir);
  const word = items.length === 1 ? `"${items[0].name}"` : `${items.length} items`;
  const confirmWord = hasDir ? "DELETE" : null;
  const ok = await promptDialog({
    title: "Delete permanently",
    msg: `This permanently deletes ${word} from the PC. This cannot be undone.` +
         (hasDir ? ` Folders and everything inside them are removed.\n\nType DELETE to confirm.` : ""),
    label: confirmWord ? "Type DELETE" : null,
    confirmText: confirmWord,
    danger: true,
    okText: "Delete",
  });
  if (ok == null) return;
  showBrowseError("");
  let failed = [];
  for (const e of items) {
    try { await api("POST", `/api/devices/${state.current}/delete`, { path: e.path }); }
    catch (err) { failed.push(`${e.name}: ${err.message}`); }
  }
  await reload();
  if (failed.length) showBrowseError("Some items could not be deleted — " + failed.join("; "));
});

$("#act-copy").addEventListener("click", () => transferTo("copy"));
$("#act-move").addEventListener("click", () => transferTo("move"));

async function transferTo(kind) {
  const items = selectedEntries();
  if (!items.length) return;
  const dstDir = await pickDestination(kind === "copy" ? "Copy to folder" : "Move to folder");
  if (dstDir == null) return;
  showBrowseError("");
  let failed = [];
  for (const e of items) {
    try { await api("POST", `/api/devices/${state.current}/${kind}`, { src: e.path, dstDir }); }
    catch (err) { failed.push(`${e.name}: ${err.message}`); }
  }
  await reload();
  if (failed.length) showBrowseError("Some items failed — " + failed.join("; "));
}

// ---- Excel editing ----
function openExcel(e) {
  if (!e || e.dir || !isSheet(e.name)) return;
  const q = `device=${encodeURIComponent(state.current)}&path=${encodeURIComponent(e.path)}&name=${encodeURIComponent(e.name)}`;
  window.open(`/excel/editor?${q}`, "_blank", "noopener");
}
$("#act-excel").addEventListener("click", () => {
  const e = selectedEntries()[0];
  if (e) openExcel(e);
});

// ---- download ----
$("#act-download").addEventListener("click", async () => {
  const e = selectedEntries()[0];
  if (!e || e.dir) return;
  showBrowseError("");
  const row = addXfer(`Downloading ${e.name}…`);
  try {
    const res = await fetch(`/api/devices/${state.current}/download?path=${encodeURIComponent(e.path)}`, {
      headers: { "X-Requested-With": "rf" }, credentials: "same-origin",
    });
    if (!res.ok) {
      let msg = `Download failed (${res.status})`;
      try { msg = (await res.json()).error || msg; } catch {}
      throw new Error(msg);
    }
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url; a.download = e.name; document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
    row.done(`Downloaded ${e.name}`);
  } catch (err) { row.fail(err.message); }
});

// ---- upload ----
$("#act-upload").addEventListener("click", () => { if (state.path) $("#file-input").click(); });
$("#file-input").addEventListener("change", async (ev) => {
  const files = [...ev.target.files];
  ev.target.value = "";
  for (const file of files) await uploadOne(file);
  await reload();
});

function uploadOne(file) {
  return new Promise((resolve) => {
    const row = addXfer(`Uploading ${file.name}…`);
    const q = `dir=${encodeURIComponent(state.path)}&name=${encodeURIComponent(file.name)}`;
    const xhr = new XMLHttpRequest();
    xhr.open("POST", `/api/devices/${state.current}/upload?${q}`);
    xhr.setRequestHeader("X-Requested-With", "rf");
    xhr.upload.onprogress = (e) => { if (e.lengthComputable) row.progress(e.loaded / e.total); };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        let note = "";
        try { if (JSON.parse(xhr.responseText).backup) note = " (old version backed up)"; } catch {}
        row.done(`Uploaded ${file.name}${note}`);
      } else {
        let msg = `Upload failed (${xhr.status})`;
        try { msg = JSON.parse(xhr.responseText).error || msg; } catch {}
        // most common: file open on the PC → offer overwrite retry only on name clash
        if (/already exists/i.test(msg)) return retryOverwrite(file, row, resolve);
        row.fail(msg);
      }
      resolve();
    };
    xhr.onerror = () => { row.fail("Network error"); resolve(); };
    xhr.send(file);
  });
}

async function retryOverwrite(file, row, resolve) {
  const ok = await promptDialog({
    title: "File already exists",
    msg: `"${file.name}" already exists on the PC. Replace it? The old version is backed up first.`,
    okText: "Replace",
  });
  if (ok == null) { row.fail("Skipped — already exists"); return resolve(); }
  const q = `dir=${encodeURIComponent(state.path)}&name=${encodeURIComponent(file.name)}&overwrite=1`;
  const xhr = new XMLHttpRequest();
  xhr.open("POST", `/api/devices/${state.current}/upload?${q}`);
  xhr.setRequestHeader("X-Requested-With", "rf");
  xhr.upload.onprogress = (e) => { if (e.lengthComputable) row.progress(e.loaded / e.total); };
  xhr.onload = () => {
    if (xhr.status >= 200 && xhr.status < 300) row.done(`Replaced ${file.name}`);
    else { let m = `Upload failed (${xhr.status})`; try { m = JSON.parse(xhr.responseText).error || m; } catch {} row.fail(m); }
    resolve();
  };
  xhr.onerror = () => { row.fail("Network error"); resolve(); };
  xhr.send(file);
}

// ---- transfer progress rows ----
function addXfer(label) {
  const bar = el("span", { class: "bar" }, el("span", { class: "fill" }));
  const text = el("span", { class: "grow" }, label);
  const row = el("div", { class: "xfer" }, bar, text);
  $("#xfer-list").prepend(row);
  const remove = (delay) => setTimeout(() => row.remove(), delay);
  return {
    progress(f) { row.querySelector(".fill").style.width = Math.round(f * 100) + "%"; },
    done(msg) { row.classList.add("ok"); row.querySelector(".fill").style.width = "100%"; text.textContent = msg; remove(6000); },
    fail(msg) { row.classList.add("fail"); text.textContent = "⚠ " + msg; remove(12000); },
  };
}

// ---- context menu ----
function hideCtx() { $("#ctx-menu").hidden = true; }
document.addEventListener("click", hideCtx);
document.addEventListener("scroll", hideCtx, true);

function showCtx(x, y, e) {
  const menu = $("#ctx-menu");
  const item = (label, fn, cls) => {
    const li = el("li", { class: cls || "" }, label);
    li.addEventListener("click", (ev) => { ev.stopPropagation(); hideCtx(); fn(); });
    return li;
  };
  const kids = [];
  if (!e.dir && state.me.excel && canEdit() && isSheet(e.name))
    kids.push(item("Edit in Excel", () => openExcel(e)));
  if (!e.dir) kids.push(item("⬇ Download", () => $("#act-download").click()));
  if (canEdit()) {
    kids.push(item("Rename", () => $("#act-rename").click()));
    kids.push(item("Copy to…", () => $("#act-copy").click()));
    kids.push(item("Move to…", () => $("#act-move").click()));
  }
  if (isAdmin()) {
    kids.push(el("li", { class: "div" }));
    kids.push(item("Delete", () => $("#act-delete").click(), "danger"));
  }
  if (!kids.length) return; // viewer on a folder: nothing to show
  menu.replaceChildren(...kids);
  menu.hidden = false;
  const mw = 200, mh = menu.offsetHeight || 220;
  menu.style.left = Math.min(x, window.innerWidth - mw) + "px";
  menu.style.top = Math.min(y, window.innerHeight - mh) + "px";
}

// ---- destination picker ----
const destDlg = $("#dest-dialog");
let destState = { path: "", parent: "", resolve: null };

function pickDestination(title) {
  return new Promise((resolve) => {
    $("#dest-title").textContent = title;
    destState = { path: "", parent: "", resolve };
    $("#dest-error").hidden = true;
    $("#dest-ok").disabled = true;
    $("#dest-path").textContent = "";
    destDlg.showModal();
    destLoadDrives();
  });
}

async function destLoadDrives() {
  const box = $("#dest-drives");
  box.replaceChildren(el("span", { class: "muted small" }, "Loading…"));
  try {
    const drives = await api("GET", `/api/devices/${state.current}/drives`);
    box.replaceChildren(...drives.map((d) =>
      el("button", { class: "btn small", onclick: () => destBrowse(d.path) }, d.label || d.path)));
  } catch (err) { box.replaceChildren(); destErr(err.message); }
}

async function destBrowse(path) {
  $("#dest-error").hidden = true;
  const rows = $("#dest-rows");
  rows.replaceChildren(el("tr", {}, el("td", { class: "muted" }, "Loading…")));
  try {
    const res = await api("GET", `/api/devices/${state.current}/list?path=${encodeURIComponent(path)}`);
    destState.path = res.path; destState.parent = res.parent;
    $("#dest-path").textContent = res.path;
    $("#dest-up").disabled = !res.parent;
    $("#dest-ok").disabled = false;
    const dirs = res.entries.filter((e) => e.dir);
    rows.replaceChildren(...dirs.map((e) => {
      const tr = el("tr", { class: "dir" }, el("td", { class: "fname" }, el("span", { class: "ico" }, "📁"), e.name));
      tr.addEventListener("click", () => destBrowse(e.path));
      return tr;
    }));
    if (!dirs.length) rows.replaceChildren(el("tr", {}, el("td", { class: "muted" }, "No subfolders — you can paste here")));
  } catch (err) { rows.replaceChildren(); destErr(err.message); }
}
function destErr(m) { $("#dest-error").textContent = m; $("#dest-error").hidden = false; }

$("#dest-up").addEventListener("click", () => destState.parent && destBrowse(destState.parent));
$("#dest-cancel").addEventListener("click", () => { destDlg.close(); destState.resolve?.(null); });
$("#dest-ok").addEventListener("click", () => { destDlg.close(); destState.resolve?.(destState.path); });

// ---- generic prompt/confirm dialog ----
const promptDlg = $("#prompt-dialog");
let promptResolve = null;
function promptDialog(opts) {
  return new Promise((resolve) => {
    promptResolve = resolve;
    $("#prompt-title").textContent = opts.title || "";
    const msg = $("#prompt-msg");
    msg.textContent = opts.msg || "";
    msg.hidden = !opts.msg;
    msg.style.whiteSpace = "pre-line";
    const hasInput = opts.label != null;
    $("#prompt-label-wrap").hidden = !hasInput;
    const input = $("#prompt-input");
    if (hasInput) {
      $("#prompt-label-wrap").childNodes[0].textContent = opts.label + " ";
      input.value = opts.value || "";
    }
    $("#prompt-error").hidden = true;
    const ok = $("#prompt-ok");
    ok.textContent = opts.okText || "OK";
    ok.className = "btn " + (opts.danger ? "danger" : "primary");
    promptDlg._opts = opts;
    promptDlg.showModal();
    if (hasInput) { input.focus(); input.select(); }
  });
}
$("#prompt-cancel").addEventListener("click", () => { promptDlg.close(); promptResolve?.(null); });
$("#prompt-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const opts = promptDlg._opts || {};
  const input = $("#prompt-input");
  const val = input.value.trim();
  if (opts.confirmText && val !== opts.confirmText) {
    $("#prompt-error").textContent = `Type ${opts.confirmText} to confirm.`;
    $("#prompt-error").hidden = false;
    return;
  }
  if (opts.label != null && !opts.confirmText && !val) {
    $("#prompt-error").textContent = "Please enter a value.";
    $("#prompt-error").hidden = false;
    return;
  }
  promptDlg.close();
  promptResolve?.(opts.confirmText ? true : (opts.label != null ? val : true));
});

// ---- invites ----
const dlg = $("#invite-dialog");
$("#add-device").addEventListener("click", () => {
  $("#invite-form").reset();
  $("#invite-result").hidden = true;
  $("#invite-create").hidden = false;
  dlg.showModal();
});
$("#invite-close").addEventListener("click", () => dlg.close());
$("#invite-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  try {
    const r = await api("POST", "/api/invites", { note: f.get("note"), hours: Number(f.get("hours")) });
    $("#invite-url").value = r.url;
    $("#invite-exp").textContent = `Expires ${fmtTime(r.expiresAt)}`;
    $("#invite-result").hidden = false;
    $("#invite-create").hidden = true;
  } catch (err) { alert(err.message); }
});
$("#copy-invite").addEventListener("click", async () => {
  const v = $("#invite-url").value;
  try { await navigator.clipboard.writeText(v); $("#copy-invite").textContent = "Copied"; }
  catch { $("#invite-url").select(); }
  setTimeout(() => ($("#copy-invite").textContent = "Copy"), 1500);
});

// ---- audit ----
async function loadAudit() {
  const rows = $("#audit-rows");
  try {
    const items = await api("GET", "/api/audit");
    const names = Object.fromEntries(state.devices.map((d) => [d.id, d.name || d.hostname]));
    rows.replaceChildren(...items.map((a) => el("tr", {},
      el("td", {}, fmtTime(a.time)), el("td", {}, a.actor), el("td", {}, a.ip), el("td", {}, a.action),
      el("td", {}, a.device ? names[a.device] || a.device : ""), el("td", { class: "fname" }, a.path || ""),
      el("td", { class: a.ok ? "ok" : "error" }, a.ok ? "OK" : a.error || "failed"))));
  } catch (err) { rows.replaceChildren(el("tr", {}, el("td", { colspan: "7", class: "error" }, err.message))); }
}
$("#audit-refresh").addEventListener("click", loadAudit);

// ---- users ----
const roleLabel = { admin: "Admin", editor: "Editor", viewer: "Viewer" };

async function loadUsers() {
  const rows = $("#user-rows");
  try {
    const users = await api("GET", "/api/users");
    rows.replaceChildren(...users.map((u) => {
      const actions = el("td", { class: "row gap" });
      if (u.builtin) {
        actions.append(el("span", { class: "muted small" }, "built-in"));
      } else {
        actions.append(
          el("button", { class: "btn small", onclick: () => openUserDialog(u) }, "Edit"),
          el("button", { class: "btn small danger", onclick: () => deleteUser(u) }, "Delete"));
      }
      return el("tr", {},
        el("td", {}, u.username),
        el("td", {}, el("span", { class: "role-badge r-" + u.role }, roleLabel[u.role] || u.role)),
        el("td", { class: u.disabled ? "error" : "ok" }, u.builtin ? "—" : (u.disabled ? "Disabled" : "Active")),
        el("td", { class: "muted small" }, u.builtin ? "" : fmtTime(u.createdAt)),
        actions);
    }));
  } catch (err) {
    rows.replaceChildren(el("tr", {}, el("td", { colspan: "5", class: "error" }, err.message)));
  }
}

const userDlg = $("#user-dialog");
let editingUser = null;

function openUserDialog(u) {
  editingUser = u || null;
  $("#user-dlg-title").textContent = u ? `Edit ${u.username}` : "Add user";
  $("#u-name").value = u ? u.username : "";
  $("#u-name").disabled = !!u;
  $("#u-role").value = u ? u.role : "viewer";
  $("#u-pass").value = "";
  $("#u-pass-hint").textContent = u ? "Leave blank to keep the current password." : "At least 10 characters.";
  $("#user-error").hidden = true;
  userDlg.showModal();
}

$("#add-user").addEventListener("click", () => openUserDialog(null));
$("#user-cancel").addEventListener("click", () => userDlg.close());

$("#user-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("#user-error");
  err.hidden = true;
  const role = $("#u-role").value;
  const pass = $("#u-pass").value;
  try {
    if (editingUser) {
      const body = { role };
      if (pass) body.password = pass;
      await api("PATCH", `/api/users/${encodeURIComponent(editingUser.username)}`, body);
    } else {
      await api("POST", "/api/users", { username: $("#u-name").value.trim(), role, password: pass });
    }
    userDlg.close();
    await loadUsers();
  } catch (e2) {
    err.textContent = e2.message;
    err.hidden = false;
  }
});

async function deleteUser(u) {
  const ok = await promptDialog({
    title: "Delete user",
    msg: `Delete "${u.username}"? They will be signed out and can no longer log in.`,
    okText: "Delete", danger: true,
  });
  if (ok == null) return;
  try { await api("DELETE", `/api/users/${encodeURIComponent(u.username)}`); await loadUsers(); }
  catch (err) { alert(err.message); }
}

// ---- boot ----
showApp().catch(() => showLogin());
