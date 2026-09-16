/* Full-width stacked bars, ~90 lines of SVG. No charting library: the whole
 * point of this app is to have no external dependencies.
 *
 * Each item is a bar spanning the full width, stacked top to bottom, height
 * proportional to its byte share. A squarified 2D treemap packs a dominant
 * item into a near-square block and leaves the rest a sliver whose width is
 * fixed by that item's share — no matter how tall the section grows, a 2%
 * item stays too narrow for its name. Giving every bar the full width means
 * only height is ever scarce, and the section can grow to make room for it. */
(function (global) {
  "use strict";

  const NS = "http://www.w3.org/2000/svg";
  const GAP = 2;        // 2px surface gap between fills — never a stroke border
  const RADIUS = 3;
  const GLYPH = { dir: "▸", file: "·", rest: "⋯" };

  function layoutRows(items, w, h) {
    const out = [];
    const list = items.filter((d) => d.size > 0).slice().sort((a, b) => b.size - a.size);
    let total = 0;
    for (const item of list) total += item.size;
    if (total <= 0) return out;

    let y = 0;
    for (const item of list) {
      const rh = (item.size / total) * h;
      out.push({ item: item, x: 0, y: y, w: w, h: rh });
      y += rh;
    }
    return out;
  }

  function el(name, attrs) {
    const node = document.createElementNS(NS, name);
    for (const key in attrs) node.setAttribute(key, attrs[key]);
    return node;
  }

  /* Trim a label until it truly fits its tile. The character estimate below is
   * only an estimate — proportional type makes "WWW" three times the width of
   * "iii" — so anything still over budget is measured and cut for real. The
   * node has to be in the document before getComputedTextLength() means
   * anything, hence the second pass after every tile is placed. */
  function fitLabel(entry) {
    const node = entry.node;
    if (node.getComputedTextLength() <= entry.room) return;
    // One proportional guess lands within a character or two; the loop closes
    // the rest. Both run on a handful of labels, never the whole tree.
    let chars = Math.max(1, Math.floor(entry.name.length * (entry.room / node.getComputedTextLength())) - 1);
    node.textContent = entry.prefix + entry.name.slice(0, chars) + "…";
    while (chars > 1 && node.getComputedTextLength() > entry.room) {
      chars -= 1;
      node.textContent = entry.prefix + entry.name.slice(0, chars) + "…";
    }
    // Not even one character plus the ellipsis fits: drop the label rather than
    // let it bleed over the tile edge. The table below still carries the name.
    if (node.getComputedTextLength() > entry.room) node.textContent = "";
  }

  /* render(svg, children, { width, height, fmt, onSelect, onFocus }) */
  function render(svg, children, opts) {
    const W = opts.width, H = opts.height;
    svg.setAttribute("viewBox", "0 0 " + W + " " + H);
    while (svg.firstChild) svg.removeChild(svg.firstChild);

    if (!children || !children.length) {
      const note = el("text", { x: W / 2, y: H / 2, "text-anchor": "middle",
        fill: "var(--text-muted)", "font-size": 13 });
      note.textContent = "Nothing to show here";
      svg.appendChild(note);
      return;
    }

    const placed = layoutRows(children, W, H);
    const pending = [];   // labels to measure once they are in the document

    for (const cell of placed) {
      const d = cell.item;
      // Inset by half the gap on every side: neighbours each give up 1px, so
      // the visible channel between two fills is exactly GAP.
      const x = cell.x + GAP / 2, y = cell.y + GAP / 2;
      const w = Math.max(0, cell.w - GAP), h = Math.max(0, cell.h - GAP);
      if (w <= 0 || h <= 0) continue;

      const isRest = d.kind === "rest";
      const group = el("g", { class: "tile", tabindex: 0, role: "button" });
      group.setAttribute("aria-label",
        d.name + ", " + opts.fmt(d.size) + (d.kind === "dir" ? ", folder" : ""));

      group.appendChild(el("rect", {
        x: x, y: y, width: w, height: h,
        rx: Math.min(RADIUS, w / 2, h / 2),
        fill: isRest ? "var(--neutral-mark)" : "var(--tile)",
        "fill-opacity": d.kind === "file" ? 0.72 : 1,
      }));

      const ink = isRest ? "var(--tile-rest-ink)" : "var(--tile-ink)";
      // Only label a tile when the text genuinely fits with padding — a clipped
      // label is worse than none, and the table below carries every value.
      if (w >= 58 && h >= 24) {
        const label = el("text", {
          x: x + 7, y: y + 16, fill: ink, "font-size": 12.5,
          "font-weight": 600, "letter-spacing": "-.005em",
        });
        const glyph = GLYPH[d.kind] || "";
        const prefix = glyph ? glyph + " " : "";
        // The glyph rides in the same line box, so it has to come out of the
        // budget too — otherwise every labelled tile runs two characters long
        // and the name is sliced by the SVG clip instead of ellipsised. 6.9 is
        // a deliberately pessimistic advance width: real names carry capitals
        // and spaces, which run wider than the lowercase average.
        const maxChars = Math.floor((w - 14) / 6.9) - prefix.length;
        let name = d.name;
        if (name.length > maxChars) name = name.slice(0, Math.max(1, maxChars - 1)) + "…";
        label.textContent = prefix + name;
        group.appendChild(label);
        pending.push({ node: label, prefix: prefix, name: d.name, room: w - 14 });

        if (h >= 42) {
          const value = el("text", {
            x: x + 7, y: y + 32, fill: ink, "fill-opacity": 0.85,
            "font-size": 11.5, "font-weight": 500,
          });
          value.textContent = opts.fmt(d.size);
          group.appendChild(value);
          pending.push({ node: value, prefix: "", name: opts.fmt(d.size), room: w - 14 });
        }
      }

      const focus = function () { opts.onFocus && opts.onFocus(d); };
      const activate = function (ev) { ev.preventDefault(); opts.onSelect && opts.onSelect(d); };
      group.addEventListener("click", activate);
      group.addEventListener("mouseenter", focus);
      group.addEventListener("focus", focus);
      group.addEventListener("keydown", function (ev) {
        if (ev.key === "Enter" || ev.key === " ") activate(ev);
      });
      svg.appendChild(group);
    }

    for (const entry of pending) fitLabel(entry);
  }

  global.Treemap = { render: render, layoutRows: layoutRows };
})(window);
