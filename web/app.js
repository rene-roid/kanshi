/* kanshi — single-page live dashboard. No framework, no build step. */
(function () {
  "use strict";

  const $ = (sel) => document.querySelector(sel);
  const KB = 1024;

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
    $("#cpu-hero").innerHTML = cpu.percent.toFixed(0) + '<span class="hero-unit">%</span>';
    $("#cpu-count").textContent = cpu.count;
    const hot = cpu.temp !== null && cpu.temp !== undefined;
    $("#cpu-meta").textContent = cpu.count + " cores" + (hot ? " · " + cpu.temp + "°C" : "");

    const aux = [
      ["Load avg", cpu.load.map((n) => n.toFixed(2)).join("  ")],
      ["Net", "↓" + shortRate(v.network.rx) + "  ↑" + shortRate(v.network.tx)],
      ["Disk", "↓" + shortRate(v.diskio.read) + "  ↑" + shortRate(v.diskio.write)],
    ];
    $("#cpu-aux").innerHTML = aux.map((r) => "<dt>" + r[0] + "</dt><dd>" + r[1] + "</dd>").join("");

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
      parts.push(meter(fs.label, fs.percent, bytes(fs.free) + " free", 80, 92));
    });
    $("#meters").innerHTML = parts.join("");
  }

  /* ── containers ─────────────────────────────────────────────────────── */
  let sortKey = "cpu";
  let lastDocker = null;

  function netTotal(c) { return c.net ? c.net.rx_rate + c.net.tx_rate : -1; }

  function renderContainers(d) {
    lastDocker = d;
    if (!d) return;
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

    const rows = list.map((c) => {
      const running = c.state === "running";
      const bad = c.health === "unhealthy" || (!running && c.state !== "exited");
      const dot = bad ? "bad" : running ? "run" : "stop";
      // Bar width is capped at one full core so a 300% spike stays readable.
      const barPct = Math.min(100, c.cpu);
      const mem = running ? bytes(c.mem_used) : "—";
      const net = c.net ? "↓" + shortRate(c.net.rx_rate) + " ↑" + shortRate(c.net.tx_rate)
                        : (running ? "shared" : "—");
      // Project first in the sub-line: it is the shared prefix, so it belongs
      // where it can be skimmed past rather than eating the name column.
      const sub = [c.project, running ? c.status : c.state + " · " + c.status]
        .filter(Boolean).join(" · ");
      return '<tr class="' + (running ? "" : "is-stopped") + '" title="' + (c.full_name || c.name) + '">' +
        "<td><div class=\"name-cell\"><i class=\"dot " + dot + '"></i><span class="ctr-name">' + c.name + "</span></div>" +
        '<div class="ctr-sub">' + sub + "</div>" +
        '<div class="ctr-bar"><i style="width:' + barPct + '%"></i></div></td>' +
        '<td class="num">' + (running ? c.cpu.toFixed(1) + "%" : "—") + "</td>" +
        '<td class="num">' + mem + "</td>" +
        '<td class="num">' + net + "</td>" +
        "</tr>";
    });
    $("#ctr-tbl").querySelector("tbody").innerHTML = rows.join("");
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
  const svg = $("#treemap");
  let storage = null;
  let rootIndex = 0;
  let trail = [];       // node stack from the selected root down to the view

  function current() { return trail[trail.length - 1]; }

  // Bars are always full width, so height is the only scarce dimension. Grow
  // the section so even the smallest visible bar clears the label threshold,
  // capped so one tiny outlier can't blow the section up indefinitely.
  function treemapHeight(kids) {
    const MIN_HEIGHT = 300, MAX_HEIGHT = 640, MIN_ROW_HEIGHT = 30;
    if (!kids || !kids.length) return MIN_HEIGHT;
    let total = 0, minSize = Infinity;
    kids.forEach((d) => { total += d.size; if (d.size > 0 && d.size < minSize) minSize = d.size; });
    if (!total || !isFinite(minSize)) return MIN_HEIGHT;
    const needed = Math.ceil((MIN_ROW_HEIGHT * total) / minSize);
    return Math.min(MAX_HEIGHT, Math.max(MIN_HEIGHT, needed));
  }

  // Pinned custom paths: bookmarks into the already-scanned tree, so adding one
  // costs no server round trip and can never reach outside what was walked.
  const PIN_KEY = "kanshi-pins";
  function loadPins() {
    try { return JSON.parse(localStorage.getItem(PIN_KEY)) || []; } catch (e) { return []; }
  }
  function savePins() { localStorage.setItem(PIN_KEY, JSON.stringify(pins)); }
  let pins = loadPins();

  // Longest matching root label wins, so "/mnt/data/x" resolves against the
  // "/mnt/data" root rather than the "/" root that also technically contains it.
  function bestRootForPath(path) {
    let best = null;
    storage.roots.forEach((r, i) => {
      const label = r.name;
      const matches = label === "/" ? path.indexOf("/") === 0 : (path === label || path.indexOf(label + "/") === 0);
      if (!matches) return;
      if (!best || label.length > best.label.length) {
        const rest = label === "/" ? path.slice(1) : path.slice(label.length + 1);
        best = { rootIndex: i, label: label, segs: rest.split("/").filter(Boolean) };
      }
    });
    return best;
  }

  function resolvePin(pin) {
    const idx = storage.roots.findIndex((r) => r.name === pin.root);
    if (idx < 0) return null;
    let node = storage.roots[idx];
    const resTrail = [node];
    for (const seg of pin.segs) {
      const kids2 = node.children || [];
      const hit = kids2.find((k) => k.name === seg);
      if (!hit) break;
      node = hit;
      resTrail.push(node);
    }
    return { rootIndex: idx, trail: resTrail, complete: resTrail.length === pin.segs.length + 1 };
  }

  function goToPin(pin) {
    const res = resolvePin(pin);
    if (!res) return;
    rootIndex = res.rootIndex;
    trail = res.trail;
    drawStorage();
    if (!res.complete) {
      $("#tm-focus").textContent = "Landed as deep as the scan reaches — the rest is below the scan depth or folded away.";
    }
  }

  function removePin(pin) {
    pins = pins.filter((p) => !(p.root === pin.root && p.label === pin.label));
    savePins();
    renderRootBar();
  }

  function flashPinError(msg) { $("#pinform-err").textContent = msg; }

  function submitPin(raw) {
    const path = raw.trim();
    if (path.indexOf("/") !== 0) { flashPinError("Use an absolute path, e.g. /mnt/data/media"); return; }
    const match = bestRootForPath(path);
    if (!match) { flashPinError("No storage root covers that path"); return; }
    const pin = { root: storage.roots[match.rootIndex].name, segs: match.segs, label: path };
    if (!pins.some((p) => p.root === pin.root && p.label === pin.label)) {
      pins.push(pin);
      savePins();
    }
    renderRootBar();
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

  function escapeHtml(s) {
    return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  function renderRootBar() {
    const rootChips = storage.roots.map((r, i) =>
      '<button role="tab" class="chip' + (i === rootIndex ? " is-on" : "") + '" data-i="' + i + '" type="button" aria-selected="' +
      (i === rootIndex) + '">' + escapeHtml(r.name) + " · " + bytes(r.size) + "</button>").join("");
    const pinChips = pins.map((p, i) => {
      const label = escapeHtml(p.label);
      return '<span class="chip pin" data-i="' + i + '">' +
        '<button type="button" class="pin-go" title="' + label + '">' + label + "</button>" +
        '<button type="button" class="pin-x" aria-label="Remove pinned path">×</button></span>';
    }).join("");
    $("#rootbar").innerHTML = rootChips + pinChips +
      '<button class="chip pin-add" id="pin-add" type="button">+ Add path</button>';
    $("#rootbar").querySelectorAll("button.chip[data-i]").forEach((b) => {
      b.addEventListener("click", () => { rootIndex = +b.dataset.i; trail = [storage.roots[rootIndex]]; drawStorage(); });
    });
    $("#rootbar").querySelectorAll(".chip.pin").forEach((span) => {
      const pin = pins[+span.dataset.i];
      span.querySelector(".pin-go").addEventListener("click", () => goToPin(pin));
      span.querySelector(".pin-x").addEventListener("click", () => removePin(pin));
    });
    $("#pin-add").addEventListener("click", showPinForm);
  }

  function renderCrumbs() {
    const html = trail.map((n, i) => {
      const last = i === trail.length - 1;
      return '<button type="button" data-i="' + i + '"' + (last ? " disabled" : "") + ">" + n.name + "</button>" +
        (last ? "" : '<span class="sep">/</span>');
    }).join("");
    $("#crumbs").innerHTML = html;
    $("#crumbs").querySelectorAll("button").forEach((b) => {
      b.addEventListener("click", () => { trail = trail.slice(0, +b.dataset.i + 1); drawStorage(); });
    });
  }

  function emptyStorage(message) {
    // The first walk can take the better part of a minute on a big filesystem.
    // Say so, rather than leaving a blank box that reads as broken.
    $("#rootbar").innerHTML = "";
    $("#crumbs").innerHTML = "";
    $("#treemap").innerHTML = "";
    $("#stor-tbl").querySelector("tbody").innerHTML = "";
    $("#tm-focus").textContent = message;
    $("#stor-meta").textContent = "";
  }

  // There is nothing to compare against on the very first walk ever — no
  // previous byte totals — so the server omits `progress` rather than
  // shipping a meaningless 0%. The bar only appears once there is a real
  // percentage to show.
  function renderScanProgress() {
    const bar = $("#scan-progress");
    const p = storage && storage.progress;
    if (!storage || !storage.scanning || !p) { bar.hidden = true; return; }
    bar.hidden = false;
    $("#scan-progress-fill").style.width = Math.min(100, p.percent) + "%";
    $("#scan-progress-label").textContent =
      "Scanning " + p.root + "… " + p.percent.toFixed(0) + "% · " + bytes(p.bytes_done) + " of " + bytes(p.bytes_total);
  }

  function updateRescanButton() {
    const btn = $("#rescan");
    const scanning = !!(storage && storage.scanning);
    btn.disabled = scanning;
    btn.textContent = scanning ? "Scanning…" : "Rescan";
  }

  function drawStorage() {
    renderScanProgress();
    updateRescanButton();
    if (!storage || !storage.roots || !storage.roots.length) {
      emptyStorage(storage && storage.error
        ? "Scan failed: " + storage.error
        : "Walking the filesystem for the first time — this can take a minute.");
      return;
    }
    const node = current();
    const kids = node.children || [];
    renderCrumbs();

    const box = svg.parentElement.getBoundingClientRect();
    const width = Math.max(200, Math.round(box.width));
    const height = treemapHeight(kids);
    svg.style.height = height + "px";
    svg.setAttribute("height", height);

    Treemap.render(svg, kids, {
      width: width, height: height, fmt: bytes,
      onFocus: (d) => {
        const share = node.size ? (d.size / node.size * 100).toFixed(1) : "0";
        $("#tm-focus").textContent = d.name + " — " + bytes(d.size) + " (" + share + "% of " + node.name + ")";
      },
      onSelect: (d) => {
        if (d.kind === "dir" && d.children && d.children.length) { trail.push(d); drawStorage(); }
      },
    });

    // The table twin: every value in the map is reachable without colour or hover.
    const rows = kids.slice().sort((a, b) => b.size - a.size).map((d) => {
      const share = node.size ? (d.size / node.size * 100) : 0;
      const drillable = d.kind === "dir" && d.children && d.children.length;
      const glyph = d.kind === "dir" ? "▸" : d.kind === "file" ? "·" : "⋯";
      return "<tr" + (drillable ? ' class="tap" data-name="' + encodeURIComponent(d.name) + '"' : "") + ">" +
        '<td><div class="name-cell"><span class="g" aria-hidden="true">' + glyph + "</span><span>" + d.name + "</span></div></td>" +
        '<td class="num">' + bytes(d.size) + "</td>" +
        '<td class="num">' + share.toFixed(1) + "%</td></tr>";
    });
    const tbody = $("#stor-tbl").querySelector("tbody");
    tbody.innerHTML = rows.join("") || '<tr><td colspan="3" class="muted">Empty</td></tr>';
    tbody.querySelectorAll("tr.tap").forEach((tr) => {
      tr.addEventListener("click", () => {
        const name = decodeURIComponent(tr.dataset.name);
        const hit = kids.find((k) => k.name === name);
        if (hit) { trail.push(hit); drawStorage(); }
      });
    });

    const root = storage.roots[rootIndex];
    const warn = root.unreadable
      ? " · ⚠ " + root.unreadable + " unreadable dir" + (root.unreadable === 1 ? "" : "s") + " not counted"
      : "";
    $("#stor-meta").textContent = "walked " + root.root + " in " + root.walk_seconds + "s · scanned " +
      ago(storage.scanned_at) + (storage.scanning ? " · rescanning…" : "") + warn;
  }

  // While a walk is running the server updates its progress counters live, so
  // poll every second instead of the normal 60s cadence — cheap, since /api/storage
  // just reads counters rather than repeating any filesystem work. The chain
  // stops itself the moment a poll comes back with scanning: false.
  let scanPollTimer = null;
  function scheduleScanPoll() {
    clearTimeout(scanPollTimer);
    scanPollTimer = setTimeout(async () => {
      await loadStorage();
      if (storage && storage.scanning) scheduleScanPoll();
    }, 1000);
  }

  async function loadStorage() {
    try {
      const res = await fetch("/api/storage");
      storage = await res.json();
      if (storage.scanning) scheduleScanPoll();
      if (!storage.roots || !storage.roots.length) { drawStorage(); return; }
      if (rootIndex >= storage.roots.length) rootIndex = 0;
      // Re-anchor the current view onto the fresh tree so a background rescan
      // doesn't kick the user back to the root while they're drilling around.
      const names = trail.slice(1).map((n) => n.name);
      trail = [storage.roots[rootIndex]];
      for (const name of names) {
        const kids = current().children || [];
        const hit = kids.find((k) => k.name === name);
        if (!hit) break;
        trail.push(hit);
      }
      renderRootBar();
      drawStorage();
    } catch (err) { /* keep the previous render */ }
  }

  // The rescan endpoint now starts the walk in the background and returns
  // immediately (a full walk can run well over a minute), so the button just
  // kicks it off and lets the 1s poll loop above carry the live percentage —
  // it does not wait for the walk to finish.
  $("#rescan").addEventListener("click", async () => {
    try { await fetch("/api/storage/rescan", { method: "POST" }); } catch (err) { /* next poll retries */ }
    await loadStorage();
  });

  let resizeTimer;
  new ResizeObserver(() => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(drawStorage, 120);
  }).observe(svg.parentElement);

  /* ── live stream ────────────────────────────────────────────────────── */
  let source = null, retry = 1000;
  function setConn(state, text) {
    const el = $("#conn");
    el.className = "conn " + state;
    el.querySelector("span").textContent = text;
    document.body.classList.toggle("is-stale", state !== "live");
  }

  function connect() {
    if (source) source.close();
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
        $("#foot-meta").textContent = "updated " + new Date().toLocaleTimeString();
      }
      if (payload.docker) renderContainers(payload.docker);
    };
    source.onerror = () => {
      setConn("down", "reconnecting");
      source.close();
      setTimeout(connect, retry);
      retry = Math.min(retry * 2, 15000);   // back off instead of hammering
    };
  }

  // A phone suspends the page on lock; reconnect and refresh the moment it returns.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") { connect(); loadStorage(); }
  });

  /* ── theme toggle ───────────────────────────────────────────────────── */
  const saved = localStorage.getItem("kanshi-theme");
  if (saved) document.documentElement.dataset.theme = saved;
  $("#theme").addEventListener("click", () => {
    const now = document.documentElement.dataset.theme;
    const prefersDark = matchMedia("(prefers-color-scheme: dark)").matches;
    const next = now === "dark" ? "light" : now === "light" ? "dark" : (prefersDark ? "light" : "dark");
    document.documentElement.dataset.theme = next;
    localStorage.setItem("kanshi-theme", next);
    drawStorage();
  });

  connect();
  loadStorage();
  setInterval(loadStorage, 60000);   // cheap: the server serves a cached tree
})();
