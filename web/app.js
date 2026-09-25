/* kanshi — single-page live dashboard. No framework, no build step. */
(function () {
  "use strict";

  const $ = (sel) => document.querySelector(sel);

  // Most of a tick's markup is identical to the last one: a stopped container,
  // a meter that did not move. Skipping those writes spares the browser a
  // reparse and relayout every few seconds on a page that may sit open all day.
  function setHTML(el, html) {
    if (el.__html === html) return;
    el.__html = html;
    el.innerHTML = html;
  }
  const KB = 1024;
  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  /* ── formatting ─────────────────────────────────────────────────────── */
  function bytes(n) {
    if (n === null || n === undefined || isNaN(n)) return "—";
    if (n < KB) return n + " B";
    const units = ["KB", "MB", "GB", "TB", "PB"];
    let v = n / KB, i = 0;
    while (v >= KB && i < units.length - 1) { v /= KB; i++; }
    return (v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2)) + " " + units[i];
  }
  function rate(n) {
    if (!n || n < 1) return "0";
    return bytes(n) + "/s";
  }
  function shortRate(n) {
    if (!n || n < KB) return "0";
    const units = ["K", "M", "G"];
    let v = n / KB, i = 0;
    while (v >= KB && i < units.length - 1) { v /= KB; i++; }
    return (v >= 10 ? v.toFixed(0) : v.toFixed(1)) + units[i];
  }
  function duration(secs) {
    if (!secs && secs !== 0) return "—";
    const d = Math.floor(secs / 86400), h = Math.floor((secs % 86400) / 3600), m = Math.floor((secs % 3600) / 60);
    if (d) return d + "d " + h + "h";
    if (h) return h + "h " + m + "m";
    return m + "m";
  }
  function ago(ts) {
    if (!ts) return "never";
    return duration(Math.max(0, Date.now() / 1000 - ts)) + " ago";
  }
  function severity(pct, warn, crit) {
    return pct >= (crit || 90) ? "crit" : pct >= (warn || 75) ? "warn" : "ok";
  }
  function severityWord(cls) {
    return cls === "crit" ? "critical" : cls === "warn" ? "high" : "";
  }

  /* ── processor card ─────────────────────────────────────────────────── */
  const coreEls = [];
  function renderCpu(v) {
    const cpu = v.cpu;
    setHTML($("#cpu-hero"), cpu.percent.toFixed(0) + '<span class="hero-unit">%</span>');
    $("#cpu-count").textContent = cpu.count;
    const hot = cpu.temp !== null && cpu.temp !== undefined;
    $("#cpu-meta").textContent = cpu.count + " cores" + (hot ? " · " + cpu.temp + "°C" : "");

    // Windows keeps no load average, so the row is left out rather than
    // showing three zeros that look like a measurement.
    const aux = [
      cpu.load ? ["Load avg", cpu.load.map((n) => n.toFixed(2)).join("  ")] : null,
      ["Net", "↓" + shortRate(v.network.rx) + "  ↑" + shortRate(v.network.tx)],
      ["Disk", "↓" + shortRate(v.diskio.read) + "  ↑" + shortRate(v.diskio.write)],
    ].filter(Boolean);
    setHTML($("#cpu-aux"), aux.map((r) => "<dt>" + r[0] + "</dt><dd>" + r[1] + "</dd>").join(""));

    const host = $("#cores");
    if (coreEls.length !== cpu.cores.length) {
      host.innerHTML = "";
      coreEls.length = 0;
      cpu.cores.forEach(() => {
        const slot = document.createElement("div");
        const fill = document.createElement("span");
        slot.appendChild(fill);
        host.appendChild(slot);
        coreEls.push(fill);
      });
    }
    // One hue for every core: the bar height already encodes the value, so a
    // per-core colour ramp would double-encode it and say nothing new.
    cpu.cores.forEach((pct, i) => { coreEls[i].style.height = Math.max(2, pct) + "%"; });
    host.setAttribute("aria-label", "Per-core utilisation: " + cpu.cores.map((c) => c.toFixed(0) + "%").join(", "));
  }

  /* ── meters ─────────────────────────────────────────────────────────── */
  function meter(name, pct, detail, warn, crit) {
    const cls = severity(pct, warn, crit);
    const word = severityWord(cls);
    return '<div class="meter ' + cls + '">' +
      '<div class="meter-head"><span class="meter-name">' + name + "</span>" +
      '<span class="meter-val">' + (word ? '<span class="flag">⚠</span>' + word + " · " : "") +
      "<b>" + pct.toFixed(1) + "%</b> · " + detail + "</span></div>" +
      '<div class="meter-track"><div class="meter-fill" style="width:' + Math.min(100, pct) + '%"></div></div>' +
      "</div>";
  }
  function renderMeters(v) {
    const parts = [
      meter("RAM", v.memory.percent, bytes(v.memory.used) + " of " + bytes(v.memory.total), 80, 92),
    ];
    if (v.swap.total > 0) {
      parts.push(meter("Swap", v.swap.percent, bytes(v.swap.used) + " of " + bytes(v.swap.total), 50, 80));
    }
    v.filesystems.forEach((fs) => {
      parts.push(meter(escapeHtml(fs.label), fs.percent, bytes(fs.free) + " free", 80, 92));
    });
    setHTML($("#meters"), parts.join(""));
  }

  /* ── containers ─────────────────────────────────────────────────────── */
  let sortKey = "cpu";
  let lastDocker = null;

  function netTotal(c) { return c.net ? c.net.rx_rate + c.net.tx_rate : -1; }

  /* ── port mappings ──────────────────────────────────────────────────── */
  // A container publishes on the same host that served this page — a Tailscale
  // IP, a LAN name, whatever is in the address bar — so the page's own
  // hostname is what a port link should point at. Hard-coding one would break
  // the moment the dashboard is reached by any other route.
  function hostAddr() {
    const h = location.hostname;
    return h.indexOf(":") >= 0 ? "[" + h + "]" : h; // a bare IPv6 literal needs brackets
  }
  // Docker hands the bind address through verbatim; keep an href to characters
  // an address can actually contain rather than trusting the daemon's string.
  function bindAddr(ip) {
    if (!ip || !/^[0-9a-fA-F.:]+$/.test(ip)) return "";
    return ip.indexOf(":") >= 0 ? "[" + ip + "]" : ip;
  }

  // Past a handful the line stops being glanceable, and the row is one line
  // tall either way — so the overflow is counted rather than wrapped.
  const PORT_LIMIT = 5;

  function proto(p) { return String(p.type || "tcp").replace(/[^a-z]/gi, "").toLowerCase(); }
  // host→internal reads as one token, so the arrow is set tight and muted and
  // the two numbers carry the weight.
  function mapping(pub, priv, suffix) {
    return pub + '<i class="arr">→</i>' + priv + suffix;
  }

  function portLine(c) {
    if (!c.ports || !c.ports.length) return "";
    const published = c.ports.filter((p) => Number(p.public) > 0);
    const internal = c.ports.filter((p) => !Number(p.public));
    const parts = [];

    published.slice(0, PORT_LIMIT).forEach((p) => {
      const pub = Number(p.public), priv = Number(p.private), pr = proto(p);
      const suffix = pr === "tcp" ? "" : '<i class="pr">' + pr + "</i>";
      const label = mapping(pub, priv, suffix);
      // Only a plain TCP publish is something a browser can open; UDP gets the
      // same reading without the dead link.
      if (pr !== "tcp") {
        parts.push('<span title="Host port ' + pub + ' → internal port ' + priv + ', ' + pr +
          ' — not reachable from a browser">' + label + "</span>");
        return;
      }
      // A specific bind address is the authoritative target; a wildcard bind
      // means "wherever you reached this page from".
      const url = "http://" + (bindAddr(p.ip) || hostAddr()) + ":" + pub;
      const hint = "Open " + url + " — host port " + pub + " → internal port " + priv;
      parts.push('<a href="' + url + '" target="_blank" rel="noopener noreferrer" title="' + hint +
        '" aria-label="' + hint + '">' + label + "</a>");
    });
    if (published.length > PORT_LIMIT) {
      parts.push(quiet(published.length - PORT_LIMIT + " more", published.slice(PORT_LIMIT)
        .map((p) => p.public + " → " + p.private).join(", ")));
    }

    if (!published.length) {
      // Nothing published, so the internal ports are all this row has to say.
      internal.slice(0, PORT_LIMIT).forEach((p) => {
        const pr = proto(p), priv = Number(p.private);
        parts.push(quiet(priv + (pr === "tcp" ? "" : '<i class="pr">' + pr + "</i>"),
          "Internal port " + priv + " — exposed inside Docker only, not published to the host"));
      });
      if (internal.length > PORT_LIMIT) parts.push(quiet("+" + (internal.length - PORT_LIMIT), ""));
    } else if (internal.length) {
      // The published ports are what you came here for; the rest fold away so
      // a container like gluetun does not bury them under nine entries.
      parts.push(quiet(internal.length + " internal", "Exposed inside Docker only: " +
        internal.map((p) => p.private + "/" + proto(p)).join(", ")));
    }

    return '<div class="ctr-ports">' + parts.join('<i class="sep">·</i>') + "</div>";
  }

  // Not a link and not meant to compete with one.
  function quiet(label, hint) {
    return '<span class="pq"' + (hint ? ' title="' + hint + '"' : "") + ">" + label + "</span>";
  }

  // One row per container, kept across ticks and updated in place. A tick
  // changes a few numbers per row; rewriting the whole table every five
  // seconds would reparse and relayout thirty rows for that.
  const ctrRows = new Map();   // id → { tr, head, bar, cpu, mem, net }

  function ctrRow() {
    const tr = document.createElement("tr");
    tr.innerHTML = '<td><div class="ctr-head"></div><div class="ctr-bar"><i></i></div></td>' +
      '<td class="num"></td><td class="num"></td><td class="num"></td>';
    return { tr: tr, head: tr.querySelector(".ctr-head"), bar: tr.querySelector(".ctr-bar i"),
      cpu: tr.cells[1], mem: tr.cells[2], net: tr.cells[3] };
  }
  function setText(el, text) { if (el.textContent !== text) el.textContent = text; }

  function fillRow(row, c) {
    const running = c.state === "running";
    const bad = c.health === "unhealthy" || (!running && c.state !== "exited");
    const dot = bad ? "bad" : running ? "run" : "stop";
    // Project first in the sub-line: it is the shared prefix, so it belongs
    // where it can be skimmed past rather than eating the name column.
    const sub = [c.project, running ? c.status : c.state + " · " + c.status].filter(Boolean).join(" · ");
    setHTML(row.head, '<div class="name-cell"><i class="dot ' + dot + '"></i><span class="ctr-name">' +
      escapeHtml(c.name) + '</span></div><div class="ctr-sub">' + escapeHtml(sub) + "</div>" + portLine(c));
    // Bar width is capped at one full core so a 300% spike stays readable.
    const width = Math.min(100, c.cpu) + "%";
    if (row.bar.style.width !== width) row.bar.style.width = width;
    setText(row.cpu, running ? c.cpu.toFixed(1) + "%" : "—");
    setText(row.mem, running ? bytes(c.mem_used) : "—");
    setText(row.net, c.net ? "↓" + shortRate(c.net.rx_rate) + " ↑" + shortRate(c.net.tx_rate) : (running ? "shared" : "—"));
    const cls = running ? "" : "is-stopped";
    if (row.tr.className !== cls) row.tr.className = cls;
    const title = c.full_name || c.name;
    if (row.tr.title !== title) row.tr.title = title;
  }

  function renderContainers(d) {
    lastDocker = d;
    if (!d) return;
    // No daemon at all (a laptop without Docker) is not an error, just
    // nothing to show.
    $("#ctr-empty").hidden = !d.unavailable;
    $(".sortbar").hidden = !!d.unavailable;
    $("#ctr-tbl").hidden = !!d.unavailable;
    if (d.unavailable) {
      $("#ctr-meta").textContent = "not detected";
      return;
    }
    if (d.error) {
      $("#ctr-meta").textContent = "docker: " + d.error;
      return;
    }
    const list = d.containers.slice();
    const cmp = {
      cpu: (a, b) => b.cpu - a.cpu,
      mem: (a, b) => b.mem_used - a.mem_used,
      net: (a, b) => netTotal(b) - netTotal(a),
      name: (a, b) => a.name.localeCompare(b.name),
    }[sortKey];
    list.sort((a, b) => (a.state !== "running") - (b.state !== "running") || cmp(a, b) || a.name.localeCompare(b.name));

    $("#ctr-meta").textContent = d.running + " running · " + d.total + " total";

    const tbody = $("#ctr-tbl").querySelector("tbody");
    const live = new Set();
    list.forEach((c, i) => {
      let row = ctrRows.get(c.id);
      if (!row) { row = ctrRow(); ctrRows.set(c.id, row); }
      live.add(c.id);
      fillRow(row, c);
      // Only rows that actually changed place are moved.
      if (tbody.children[i] !== row.tr) tbody.insertBefore(row.tr, tbody.children[i] || null);
    });
    ctrRows.forEach((row, id) => {
      if (!live.has(id)) { row.tr.remove(); ctrRows.delete(id); }
    });
  }

  document.querySelectorAll(".sortbar .chip").forEach((chip) => {
    chip.addEventListener("click", () => {
      document.querySelectorAll(".sortbar .chip").forEach((c) => c.classList.remove("is-on"));
      chip.classList.add("is-on");
      sortKey = chip.dataset.sort;
      renderContainers(lastDocker);
    });
  });

  /* ── storage ────────────────────────────────────────────────────────── */
  // The page only ever has one directory. /api/storage is a one-line summary
  // per root, and each directory is read from /api/storage/dir as it is
  // opened. Subfolders the server has not sized yet come back pending; it
  // walks them in the background, and the live stream says when one is done.
  const svg = $("#treemap");
  let storage = null;     // walk queue status plus one summary per root
  let rootIndex = 0;
  let segs = [];          // path below the root, one directory name per level
  let listing = null;     // the directory on screen
  let items = [];         // its contents as rendered, largest first
  let navToken = 0;       // drops a slow response that a newer click overtook

  // The table starts short and grows on request. The treemap always covers the
  // whole directory, but slivers too thin to see or tap share one tile.
  const PAGE = 15, MORE = 25;
  const TILE_MIN_SHARE = 0.005, TILE_MAX = 40;
  let shown = PAGE;
  let showFiles = localStorage.getItem("kanshi-files") === "1";

  function plural(n, word) { return n + " " + word + (n === 1 ? "" : "s"); }
  function currentName() {
    return segs.length ? segs[segs.length - 1] : storage.roots[rootIndex].name;
  }

  // Everything in the current directory. Files are either named one by one or
  // collapsed into a single entry, so the parts always add up. Pending folders
  // have no size yet and sort last.
  function viewItems() {
    const l = listing;
    const out = l.dirs.map((d) => ({ name: d.name, size: d.size, kind: "dir", pending: !!d.pending }));
    if (showFiles) {
      let named = 0;
      l.files.forEach((f) => { out.push({ name: f.name, size: f.size, kind: "file" }); named += f.size; });
      const rest = l.file_bytes - named, restCount = l.file_count - l.files.length;
      if (rest > 0 && restCount > 0) out.push({ name: plural(restCount, "smaller file"), size: rest, kind: "rest" });
    } else if (l.file_bytes > 0) {
      out.push({ name: plural(l.file_count, "file"), size: l.file_bytes, kind: "rest", files: true });
    }
    return out.sort((a, b) => (a.pending - b.pending) || (b.size - a.size));
  }

  // Sized items are sorted, so everything below the cut is a suffix of them.
  function tileItems(total) {
    const sized = items.filter((d) => !d.pending);
    const floor = total * TILE_MIN_SHARE;
    let keep = 0;
    while (keep < sized.length && keep < TILE_MAX && sized[keep].size >= floor) keep++;
    if (sized.length - keep <= 1) return sized;
    let folded = 0;
    for (let i = keep; i < sized.length; i++) folded += sized[i].size;
    return sized.slice(0, keep).concat([{
      name: plural(sized.length - keep, "smaller item"), size: folded, kind: "rest", smaller: true,
    }]);
  }

  // What tapping an entry does, shared by the treemap and the table: open a
  // folder, or expand whichever aggregate was tapped.
  function activate(d) {
    if (d.kind === "dir") navigate(rootIndex, segs.concat([d.name]));
    else if (d.files) setShowFiles(true);
    else if (d.smaller) { shown = items.length; renderTable(); $("#stor-tbl").scrollIntoView({ block: "nearest" }); }
  }

  function dirQuery(root, path) {
    return "root=" + root + "&path=" + encodeURIComponent(path.join("/"));
  }

  async function navigate(root, path, keepShown) {
    const token = ++navToken;
    let l;
    try {
      const res = await fetch("/api/storage/dir?" + dirQuery(root, path));
      if (!res.ok) throw new Error("HTTP " + res.status);
      l = await res.json();
    } catch (err) {
      if (token === navToken) $("#tm-focus").textContent = "Could not load that folder — try again.";
      return;
    }
    if (token !== navToken) return;
    if (root !== rootIndex) renderRootBar(root);
    rootIndex = root;
    segs = l.path ? l.path.split("/") : [];
    listing = l;
    if (!keepShown) shown = PAGE;
    drawView();
    $("#tm-focus").textContent = l.partial
      ? "That folder is gone or can't be opened — showing the closest one that is still there."
      : l.pending
        ? "Sizing " + plural(l.pending, "folder") + " — the map fills in as each one is done."
        : "Tap a block to drill in.";
    // The server has just queued those folders. Say so now rather than at the
    // next live frame, which brings the details.
    if (l.pending && storage && !storage.scanning) {
      storage.scanning = true;
      renderScanStatus();
    }
  }

  // Pinned custom paths: bookmarks under a root. The server resolves them the
  // way a walk would, so one can never reach outside the roots.
  const PIN_KEY = "kanshi-pins";
  function loadPins() {
    try { return JSON.parse(localStorage.getItem(PIN_KEY)) || []; } catch (e) { return []; }
  }
  function savePins() { localStorage.setItem(PIN_KEY, JSON.stringify(pins)); }
  let pins = loadPins();

  // Paths are compared the way the server's filesystem does: Windows ones in
  // either slash direction and any case, since that is how Explorer treats them.
  function sep() { return (storage && storage.sep) || "/"; }
  function windowsPaths() { return sep() === "\\"; }
  // A backslash is an ordinary character in a Linux file name, so it is only
  // treated as a separator on Windows.
  function slashes(p) { return windowsPaths() ? p.replace(/\\/g, "/") : p; }
  function normPath(p) {
    const q = slashes(p).replace(/\/+$/, "");
    return windowsPaths() ? q.toLowerCase() : q;
  }
  function absolutePath(p) {
    return windowsPaths() ? /^[a-z]:[\\/]/i.test(p) || p.indexOf("\\\\") === 0 : p.indexOf("/") === 0;
  }

  // Longest matching root label wins, so "/mnt/data/x" resolves against the
  // "/mnt/data" root rather than the "/" root that also technically contains it.
  function bestRootForPath(path) {
    const target = normPath(path);
    let best = null;
    storage.roots.forEach((r, i) => {
      const label = normPath(r.name);   // "/" becomes "", C:\ becomes "c:"
      if (target !== label && target.indexOf(label + "/") !== 0) return;
      if (!best || label.length > best.label.length) {
        // Segments keep the case they were typed in; the server matches them
        // case-insensitively where the filesystem does.
        const rest = slashes(path).replace(/\/+$/, "").slice(label.length + 1);
        best = { rootIndex: i, label: label, segs: rest.split("/").filter(Boolean) };
      }
    });
    return best;
  }

  function goToPin(pin) {
    const idx = storage.roots.findIndex((r) => r.name === pin.root);
    if (idx >= 0) navigate(idx, pin.segs);
  }

  function removePin(pin) {
    pins = pins.filter((p) => !(p.root === pin.root && p.label === pin.label));
    savePins();
    renderRootBar(rootIndex);
  }

  function flashPinError(msg) { $("#pinform-err").textContent = msg; }

  function submitPin(raw) {
    const path = raw.trim();
    if (!absolutePath(path)) { flashPinError("Use a full path, e.g. " + $("#pinform-input").placeholder); return; }
    const match = bestRootForPath(path);
    if (!match) { flashPinError("No storage root covers that path"); return; }
    const pin = { root: storage.roots[match.rootIndex].name, segs: match.segs, label: path };
    if (!pins.some((p) => p.root === pin.root && p.label === pin.label)) {
      pins.push(pin);
      savePins();
    }
    renderRootBar(rootIndex);
    goToPin(pin);
    hidePinForm();
  }

  function showPinForm() {
    $("#pinform-err").textContent = "";
    $("#pinform").hidden = false;
    $("#pinform-input").value = "";
    $("#pinform-input").focus();
  }
  function hidePinForm() { $("#pinform").hidden = true; }

  $("#pinform").addEventListener("submit", (ev) => {
    ev.preventDefault();
    submitPin($("#pinform-input").value);
  });
  $("#pinform-cancel").addEventListener("click", hidePinForm);

  function renderRootBar(active) {
    const rootChips = storage.roots.map((r, i) =>
      '<button role="tab" class="chip' + (i === active ? " is-on" : "") + '" data-root="' + i + '" type="button" aria-selected="' +
      (i === active) + '">' + escapeHtml(r.name) + (r.size == null ? "" : " · " + bytes(r.size)) + "</button>").join("");
    const pinChips = pins.map((p, i) => {
      const label = escapeHtml(p.label);
      return '<span class="chip pin" data-pin="' + i + '">' +
        '<button type="button" class="pin-go" title="' + label + '">' + label + "</button>" +
        '<button type="button" class="pin-x" aria-label="Remove pinned path">×</button></span>';
    }).join("");
    $("#rootbar").innerHTML = rootChips + pinChips +
      '<button class="chip pin-add" data-add="1" type="button">+ Add path</button>';
  }

  $("#rootbar").addEventListener("click", (ev) => {
    const b = ev.target.closest("button");
    if (!b) return;
    if (b.dataset.root) { navigate(+b.dataset.root, []); return; }
    if (b.dataset.add) { showPinForm(); return; }
    const pin = pins[+b.parentElement.dataset.pin];
    if (!pin) return;
    if (b.classList.contains("pin-x")) removePin(pin); else goToPin(pin);
  });

  // The crumbs read as the path itself: C:\Users\you on Windows. A root
  // label's own trailing separator is dropped so it is not shown twice, except
  // for "/", which would otherwise vanish.
  function renderCrumbs() {
    let root = storage.roots[rootIndex].name;
    if (segs.length && root.length > 1 && root.slice(-1) === sep()) root = root.slice(0, -1);
    const names = [root].concat(segs);
    $("#crumbs").innerHTML = names.map((n, i) => {
      const last = i === names.length - 1;
      return '<button type="button" data-depth="' + i + '"' + (last ? " disabled" : "") + ">" + escapeHtml(n) + "</button>" +
        (last ? "" : '<span class="sep">' + escapeHtml(sep()) + "</span>");
    }).join("");
  }

  $("#crumbs").addEventListener("click", (ev) => {
    const b = ev.target.closest("button");
    if (b && !b.disabled) navigate(rootIndex, segs.slice(0, +b.dataset.depth));
  });

  function emptyStorage(message) {
    listing = null;
    items = [];
    $("#rootbar").innerHTML = "";
    $("#crumbs").innerHTML = "";
    svg.textContent = "";
    $("#stor-tbl").querySelector("tbody").innerHTML = "";
    $("#stor-more").hidden = true;
    $("#tm-focus").textContent = message;
    $("#stor-meta").textContent = "";
  }

  // A folder walked for the first time has no previous size to measure
  // against, so the server omits `progress` rather than shipping a
  // meaningless 0%; the bar then only names what is being walked.
  function renderScanStatus() {
    const bar = $("#scan-progress");
    const st = storage || {};
    bar.hidden = !st.scanning;
    if (st.scanning) {
      const p = st.progress;
      $("#scan-progress-fill").style.width = (p ? Math.min(100, p.percent) : 0) + "%";
      $("#scan-progress-label").textContent = "Sizing " + (st.current || "folders") + "…" +
        (p ? " " + p.percent.toFixed(0) + "% · " + bytes(p.bytes_done) + " of " + bytes(p.bytes_total) : "") +
        (st.queued ? " · " + st.queued + " more queued" : "");
    }
    renderMeta();
  }

  function renderMeta() {
    if (!listing) return;
    const parts = [];
    if (listing.pending) parts.push("sizing " + plural(listing.pending, "folder") + "…");
    else if (listing.sized_at) parts.push("sized " + ago(listing.sized_at));
    if (listing.unreadable) parts.push("⚠ " + plural(listing.unreadable, "unreadable dir") + " not counted");
    if (storage && storage.error) parts.push("⚠ " + storage.error);
    $("#stor-meta").textContent = parts.join(" · ");
  }

  function drawTreemap() {
    if (!listing) return;
    const box = svg.parentElement.getBoundingClientRect();
    const width = Math.max(200, Math.round(box.width));
    const height = Math.round(parseFloat(getComputedStyle(svg).height)) || 300;
    svg.setAttribute("height", height);
    const total = listing.size;
    Treemap.render(svg, tileItems(total), {
      width: width, height: height, fmt: bytes,
      onFocus: (d) => {
        const share = total ? (d.size / total * 100).toFixed(1) : "0";
        $("#tm-focus").textContent = d.name + " — " + bytes(d.size) + " (" + share + "% of " + currentName() + ")";
      },
      onSelect: activate,
    });
  }

  // The table twin: every value in the map is reachable without colour or
  // hover, and past the first page, without the treemap folding it away.
  function renderTable() {
    const total = listing.size;
    const count = Math.min(shown, items.length);
    let html = "";
    for (let i = 0; i < count; i++) {
      const d = items[i];
      const share = total ? (d.size / total * 100) : 0;
      const tappable = d.kind === "dir" || d.files;
      const glyph = d.kind === "dir" ? "▸" : d.kind === "file" ? "·" : "⋯";
      html += "<tr" + (tappable ? ' class="tap" data-i="' + i + '"' : "") + ">" +
        '<td><div class="name-cell"><span class="g" aria-hidden="true">' + glyph + "</span><span>" +
        escapeHtml(d.name) + "</span></div></td>" +
        (d.pending
          ? '<td class="num muted">sizing…</td><td class="num muted">—</td></tr>'
          : '<td class="num">' + bytes(d.size) + "</td>" +
            '<td class="num">' + share.toFixed(1) + "%</td></tr>");
    }
    $("#stor-tbl").querySelector("tbody").innerHTML = html || '<tr><td colspan="3" class="muted">Empty</td></tr>';

    const left = items.length - count;
    $("#stor-more").hidden = items.length <= PAGE;
    $("#stor-more-btn").hidden = left <= 0;
    $("#stor-more-btn").textContent = "Show " + Math.min(MORE, left) + " more";
    $("#stor-all-btn").hidden = left <= MORE;
    $("#stor-all-btn").textContent = "Show all " + items.length;
    $("#stor-less-btn").hidden = left > 0;
    $("#stor-count").textContent = count + " of " + items.length;
  }

  $("#stor-tbl").querySelector("tbody").addEventListener("click", (ev) => {
    const tr = ev.target.closest("tr.tap");
    if (tr) activate(items[+tr.dataset.i]);
  });
  $("#stor-more-btn").addEventListener("click", () => { shown += MORE; renderTable(); });
  $("#stor-all-btn").addEventListener("click", () => { shown = items.length; renderTable(); });
  $("#stor-less-btn").addEventListener("click", () => {
    shown = PAGE;
    renderTable();
    $("#stor-tbl").scrollIntoView({ block: "nearest" });
  });

  function renderFilesToggle() {
    const chip = $("#show-files");
    chip.classList.toggle("is-on", showFiles);
    chip.setAttribute("aria-pressed", String(showFiles));
  }
  function setShowFiles(on) {
    showFiles = on;
    localStorage.setItem("kanshi-files", on ? "1" : "0");
    renderFilesToggle();
    if (listing) drawView();
  }
  $("#show-files").addEventListener("click", () => setShowFiles(!showFiles));
  renderFilesToggle();

  function drawView() {
    items = viewItems();
    renderCrumbs();
    drawTreemap();
    renderTable();
    renderMeta();
  }

  async function loadStorage() {
    let snap;
    try { snap = await (await fetch("/api/storage")).json(); } catch (err) { return; }
    storage = snap;
    $("#pinform-input").placeholder = windowsPaths() ? "C:\\Users\\you\\Downloads" : "/mnt/data/media/movies";
    renderScanStatus();
    if (!snap.roots || !snap.roots.length) {
      emptyStorage("No storage roots found.");
      return;
    }
    if (rootIndex >= snap.roots.length) { rootIndex = 0; segs = []; }
    renderRootBar(rootIndex);
    // Re-read the same folder, so a walk finishing in the background fills in
    // its sizes without kicking the user back to the root.
    await navigate(rootIndex, segs, true);
  }

  // Every live frame carries the walk queue's status, so the progress bar
  // moves without any polling, and the folder on screen is only re-read once
  // a walk completes.
  function onScanStatus(st) {
    if (!storage) return;   // the first load is still in flight and will carry this
    const finished = st.updated_at !== storage.updated_at || (st.error || null) !== (storage.error || null);
    ["scanning", "current", "queued", "progress", "updated_at", "error"].forEach((k) => { storage[k] = st[k]; });
    renderScanStatus();
    if (finished) loadStorage();
  }

  // Re-walks the subfolders of the folder on screen. The walks run in the
  // background; the live stream carries them from there.
  $("#rescan").addEventListener("click", async () => {
    try {
      const res = await fetch("/api/storage/rescan?" + dirQuery(rootIndex, segs),
        { method: "POST", headers: { "X-Kanshi": "1" } });
      if (res.ok) onScanStatus(await res.json());
    } catch (err) { /* the next frame will say where things stand */ }
  });

  let resizeTimer;
  new ResizeObserver(() => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(drawTreemap, 120);
  }).observe(svg.parentElement);

  /* ── live stream ────────────────────────────────────────────────────── */
  let source = null, retry = 1000, retryTimer = null;
  function setConn(state, text) {
    const el = $("#conn");
    el.className = "conn " + state;
    el.querySelector("span").textContent = text;
    document.body.classList.toggle("is-stale", state !== "live");
  }

  function disconnect() {
    clearTimeout(retryTimer);
    if (source) { source.close(); source = null; }
  }

  function connect() {
    disconnect();
    if (document.hidden) return;
    source = new EventSource("/api/stream");
    source.onopen = () => { retry = 1000; setConn("live", "live"); };
    source.onmessage = (ev) => {
      let payload;
      try { payload = JSON.parse(ev.data); } catch (e) { return; }
      setConn("live", "live");
      if (payload.vitals) {
        renderCpu(payload.vitals);
        renderMeters(payload.vitals);
        $("#uptime").textContent = "up " + duration(payload.vitals.uptime);
        $("#foot-meta").textContent = (appVersion ? "kanshi " + appVersion + " · " : "") +
          "updated " + new Date().toLocaleTimeString();
      }
      if (payload.docker) renderContainers(payload.docker);
      if (payload.storage) onScanStatus(payload.storage);
    };
    source.onerror = () => {
      setConn("down", "reconnecting");
      disconnect();
      retryTimer = setTimeout(connect, retry);
      retry = Math.min(retry * 2, 15000);   // back off instead of hammering
    };
  }

  // A background tab drops its stream, so once nobody is looking the server's
  // poller goes idle and stops touching /proc and the Docker socket entirely.
  // A phone suspends the page on lock anyway; either way, reconnect and
  // refresh the moment it is visible again.
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) { disconnect(); setConn("down", "paused"); return; }
    connect();
    loadStorage();
  });

  /* ── network access ─────────────────────────────────────────────────── */
  // Who else can open the dashboard. The server decides whether this page may
  // change it (only from this computer, and only when no flag or environment
  // variable pins it); the dialog just shows what it was told.
  let appVersion = "";
  let access = null;
  const KIND = { local: "This computer", lan: "Local network", tailscale: "Tailscale", custom: "Address", other: "Other" };
  const dlg = $("#access-dlg");
  const box = (name) => $("#access-opts").querySelector('input[name="' + name + '"]');

  function renderAccessLine() {
    if (!access || !access.links) return;
    const others = access.links.filter((l) => l.kind !== "local");
    $("#access-line").textContent = others.length
      ? "Also reachable at " + others.map((l) => l.url.replace(/^http:\/\//, "") + " (" + (KIND[l.kind] || l.kind) + ")").join(" · ")
      : "Only reachable from this computer.";
  }

  function renderAccessDialog() {
    const tokens = (access.mode || "local").split(",");
    const all = tokens.indexOf("all") >= 0;
    box("all").checked = all;
    box("lan").checked = all || tokens.indexOf("lan") >= 0;
    box("tailscale").checked = all || tokens.indexOf("tailscale") >= 0;
    syncAccessBoxes();
    // Explicit addresses have no checkbox; they are kept as they are.
    const extra = tokens.filter((t) => ["local", "lan", "tailscale", "all"].indexOf(t) < 0);
    $("#access-extra").hidden = !extra.length;
    $("#access-extra").textContent = extra.length ? "Also listening on " + extra.join(", ") + " (set in the config)." : "";
    $("#access-opts").disabled = !access.editable;
    $("#access-save").hidden = !access.editable;
    $("#access-reason").textContent = access.editable ? "" : access.reason || "";
    $("#access-links").innerHTML = access.links.map((l) =>
      '<li><span class="k">' + escapeHtml(KIND[l.kind] || l.kind) + '</span><a href="' + escapeHtml(l.url) +
      '" target="_blank" rel="noopener">' + escapeHtml(l.url) + "</a></li>").join("");
  }

  // "Every network" already includes the other two, so they are shown ticked
  // and locked while it is on.
  function syncAccessBoxes() {
    const all = box("all").checked;
    ["lan", "tailscale"].forEach((n) => {
      box(n).disabled = all;
      if (all) box(n).checked = true;
    });
  }
  box("all").addEventListener("change", () => {
    if (!box("all").checked) { box("lan").checked = false; box("tailscale").checked = false; }
    syncAccessBoxes();
  });

  async function loadAccess() {
    try {
      access = await (await fetch("/api/access")).json();
    } catch (err) { return; }
    renderAccessLine();
    if (dlg.open) renderAccessDialog();
  }

  $("#access-open").addEventListener("click", async () => {
    $("#access-err").textContent = "";
    await loadAccess();
    if (!access) return;
    renderAccessDialog();
    dlg.showModal();
  });

  $("#access-save").addEventListener("click", async () => {
    const extra = (access.mode || "").split(",").filter((t) => ["local", "lan", "tailscale", "all", ""].indexOf(t) < 0);
    const picked = box("all").checked ? ["all"] : ["lan", "tailscale"].filter((n) => box(n).checked);
    const mode = picked.concat(box("all").checked ? [] : extra).join(",") || "local";
    $("#access-err").textContent = "";
    try {
      const res = await fetch("/api/access", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Kanshi": "1" },
        body: JSON.stringify({ mode: mode }),
      });
      if (!res.ok) throw new Error((await res.text()).trim() || "HTTP " + res.status);
      access = await res.json();
      renderAccessLine();
      renderAccessDialog();
      if (access.reason) $("#access-err").textContent = access.reason;
    } catch (err) {
      $("#access-err").textContent = "Could not change it: " + err.message;
    }
  });

  fetch("/api/config").then((r) => r.json()).then((c) => { appVersion = c.version || ""; }).catch(() => {});
  loadAccess();

  /* ── theme toggle ───────────────────────────────────────────────────── */
  const saved = localStorage.getItem("kanshi-theme");
  if (saved) document.documentElement.dataset.theme = saved;
  $("#theme").addEventListener("click", () => {
    const now = document.documentElement.dataset.theme;
    const prefersDark = matchMedia("(prefers-color-scheme: dark)").matches;
    const next = now === "dark" ? "light" : now === "light" ? "dark" : (prefersDark ? "light" : "dark");
    document.documentElement.dataset.theme = next;
    localStorage.setItem("kanshi-theme", next);
  });

  connect();
  loadStorage();
})();
