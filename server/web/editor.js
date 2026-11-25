"use strict";

// Host page for the OnlyOffice spreadsheet editor. It asks our server for a
// signed editor config, then loads the Document Server's api.js and mounts the
// editor. The server relays the file to/from the agent on the PC.

(function () {
  const params = new URLSearchParams(location.search);
  const device = params.get("device");
  const path = params.get("path");
  const title = params.get("name") || (path ? path.split(/[\\/]/).pop() : "Spreadsheet");
  document.getElementById("editor-title").textContent = title;
  document.title = title + " — WPS";

  function fail(msg) {
    const box = document.getElementById("editor-error");
    box.textContent = msg;
    box.hidden = false;
    document.getElementById("editor-status").textContent = "";
  }

  if (!device || !path) {
    fail("Missing file information.");
    return;
  }

  document.getElementById("editor-status").textContent = "Opening…";

  // Live indicator: is this file currently open on the PC? Poll and update the
  // hint bar so the admin knows whether saves apply now or are held until the
  // file is closed on the PC.
  (function watchLock() {
    const hint = document.getElementById("editor-hint");
    if (!hint) return;
    let last = null;
    async function poll() {
      try {
        const res = await fetch(
          `/api/devices/${encodeURIComponent(device)}/lockstate?path=${encodeURIComponent(path)}`,
          { credentials: "same-origin" }
        );
        if (!res.ok) return;
        const { open } = await res.json();
        if (open === last) return;
        last = open;
        if (open) {
          hint.textContent =
            "⚠ This file is currently open on the PC. Your changes are saved and will be written to the file as soon as it is closed there.";
          hint.classList.add("warn");
          hint.classList.remove("ok");
        } else {
          hint.textContent = "✓ The file is closed on the PC — your changes save straight to it.";
          hint.classList.add("ok");
          hint.classList.remove("warn");
        }
      } catch {}
    }
    poll();
    setInterval(poll, 6000);
  })();

  fetch(`/api/devices/${encodeURIComponent(device)}/excel/open?path=${encodeURIComponent(path)}`, {
    method: "POST",
    headers: { "X-Requested-With": "rf" },
    credentials: "same-origin",
  })
    .then(async (res) => {
      if (!res.ok) {
        let m = `Could not open the file (${res.status})`;
        try { m = (await res.json()).error || m; } catch {}
        throw new Error(m);
      }
      return res.json();
    })
    .then(({ apiJs, config }) => {
      // Reload the folder behind us when the editor closes.
      config.events = {
        onError: (e) => fail("Editor error: " + (e && e.data ? e.data : "unknown")),
        onRequestClose: () => { window.location.href = "/"; },
      };
      const s = document.createElement("script");
      s.src = apiJs;
      s.onload = () => {
        if (!window.DocsAPI) { fail("The document server did not load."); return; }
        document.getElementById("editor-status").textContent = "";
        try {
          // eslint-disable-next-line no-new
          new window.DocsAPI.DocEditor("ph", config);
        } catch (e) {
          fail("Could not start the editor: " + e.message);
        }
      };
      s.onerror = () => fail("Could not reach the document server. Check that OnlyOffice is running.");
      document.head.appendChild(s);
    })
    .catch((e) => fail(e.message));
})();
