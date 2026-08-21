/* The stage: floor lattice, enclosure, routed traces, prisms, ports.
   Status is line work and small-caps labels — never colour. */

import { state } from "./state.js";
import {
  iso, pts, line, surfaceMatrix, surfaceQuad, escapeText, escapeAttribute,
  INK, CARTA, ROSSO, GIALLO, VERDE,
} from "./iso.js";
import { layout } from "./layout.js";
import { tracePoints } from "./router.js";
import { renderPanel } from "./panel.js";
import { beadAppearance } from "./activity.js";

const PORT_FACES = Object.freeze({
  south: Object.freeze({
    pad: "south", label: "top", labelTurns: 0, labelDown: -6,
  }),
  east: Object.freeze({
    pad: "east", label: "top", labelTurns: 1, labelDown: -6,
  }),
});

function surfaceText(surface, at, text, options = {}) {
  const matrix = surfaceMatrix(
    surface, at, options.along || 0, options.down || 0, options.turns || 0,
  );
  const attributes = [
    'text-anchor="' + (options.anchor || "start") + '"',
    'font-size="' + (options.size || 9) + '"',
  ];
  if (options.weight) attributes.push('font-weight="' + options.weight + '"');
  if (options.spacing) {
    attributes.push('letter-spacing="' + options.spacing + '"');
  }
  if (options.opacity !== undefined) {
    attributes.push('opacity="' + options.opacity + '"');
  }
  if (options.mono) {
    attributes.push('font-family="ui-monospace, Menlo, monospace"');
  }
  return '<text transform="matrix(' +
    matrix.map(value => value.toFixed(4)).join(",") + ')" ' +
    attributes.join(" ") + ">" + escapeText(text) + "</text>";
}

function portLabel(face, at, text) {
  return surfaceText(face.label, at, text, {
    anchor: "middle",
    down: face.labelDown,
    turns: face.labelTurns,
    opacity: 0.85,
    mono: true,
  });
}

export function render() {
  const doc = state.doc;
  if (!doc) return;
  const plan = layout(doc);
  const svg = [];

  renderFloor(svg, plan.bounds);
  renderEnclosure(svg, plan);
  for (const external of plan.external) renderExternalTrace(svg, external);
  for (const route of plan.routes) renderRoute(svg, plan, route);
  /* Painter order along the camera diagonal. */
  const nodes = [...plan.byName.values()]
    .sort((a, b) => (a.x + a.y) - (b.x + b.y));
  for (const node of nodes) renderNode(svg, node);
  for (const route of plan.routes) renderDrops(svg, plan, route);
  for (const node of nodes) renderNodePorts(svg, plan, node);
  for (const external of plan.external) renderExternalPort(svg, external);

  const stage = document.getElementById("stage");
  stage.innerHTML = svg.join("");
  if (!state.viewBox) fitView(plan.bounds);
  applyViewBox();
  bindStage();
  renderPanel(plan);
  renderBanner();
  document.getElementById("feet").textContent =
    "DCOMP · SYSTEM VIEW · " + (doc.name || "").toUpperCase() +
    (doc.digest ? " · " + doc.digest.slice(7, 19).toUpperCase() : "");
}

export function applyViewBox() {
  document.getElementById("stage")
    .setAttribute("viewBox", state.viewBox.join(" "));
}

function fitView(bounds) {
  const corners = [
    iso(bounds.minX, bounds.minY, 1), iso(bounds.maxX, bounds.minY, 0),
    iso(bounds.maxX, bounds.maxY, 0), iso(bounds.minX, bounds.maxY, 0),
  ];
  const xs = corners.map(p => p[0]), ys = corners.map(p => p[1]);
  const minX = Math.min(...xs) - 90, maxX = Math.max(...xs) + 200;
  const minY = Math.min(...ys) - 110, maxY = Math.max(...ys) + 80;
  state.viewBox = [minX, minY, maxX - minX, maxY - minY];
}

function bindStage() {
  const stage = document.getElementById("stage");
  stage.onclick = event => {
    if (state.dragged) { state.dragged = false; return; }
    const hit = event.target.closest("[data-kind]");
    state.selection = hit ? { kind: hit.dataset.kind, id: hit.dataset.id } : null;
    render();
  };
  stage.classList.toggle("lost", state.lost);
}

function renderBanner() {
  const doc = state.doc;
  const banner = document.getElementById("opbanner");
  banner.textContent = doc && doc.operation ?
    "OPERATION " + doc.operation.toUpperCase() +
    " · PHASE " + (doc.phase || "").toUpperCase() : "";
}

function statusOf(component) {
  const status = component.status;
  if (!status) {
    return { word: "", solid: true, faint: false, blink: false, up: false };
  }
  if (status.status === "running" && status.health === "healthy" &&
      !status.problem) {
    return { word: "", solid: true, faint: false, blink: false, up: true };
  }
  if (status.status === "running" && status.health === "starting") {
    return { word: "STARTING", solid: false, faint: false, blink: false, up: true };
  }
  if (status.status === "running") {
    return { word: status.problem ? "PROBLEM" : "UNHEALTHY",
      solid: false, faint: false, blink: true, up: true };
  }
  if (status.status === "missing") {
    return { word: "MISSING", solid: true, faint: true, blink: false, up: false };
  }
  if (status.status === "exited") {
    return { word: "EXITED " + status.exit_code,
      solid: true, faint: true, blink: false, up: false };
  }
  return { word: (status.status || "UNKNOWN").toUpperCase(),
    solid: false, faint: true, blink: true, up: false };
}

function renderFloor(svg, bounds) {
  const x0 = Math.floor(bounds.minX) - 4, x1 = Math.ceil(bounds.maxX) + 4;
  const y0 = Math.floor(bounds.minY) - 4, y1 = Math.ceil(bounds.maxY) + 4;
  for (let x = x0; x <= x1; x++) {
    svg.push(line(iso(x, y0, 0), iso(x, y1, 0), "rgba(32,26,18,0.10)", 1));
  }
  for (let y = y0; y <= y1; y++) {
    svg.push(line(iso(x0, y, 0), iso(x1, y, 0), "rgba(32,26,18,0.10)", 1));
  }
  /* Quiet ink marks at sparse intersections. The floor must never compete
     with ports — squares with meaning live on the tiles, not the ground. */
  for (let x = x0; x <= x1; x++) {
    for (let y = y0; y <= y1; y++) {
      if (((x % 4) + 4) % 4 !== 0 || ((y % 4) + 4) % 4 !== 0) continue;
      const [cx, cy] = iso(x, y, 0);
      svg.push('<rect x="' + (cx - 1.5) + '" y="' + (cy - 1.5) +
        '" width="3" height="3" fill="' + INK + '" opacity="0.14"/>');
    }
  }
}

function renderEnclosure(svg, plan) {
  const b = plan.bounds;
  const corners = [
    iso(b.minX, b.minY, 0), iso(b.maxX, b.minY, 0),
    iso(b.maxX, b.maxY, 0), iso(b.minX, b.maxY, 0),
  ];
  svg.push('<polygon points="' + pts(corners) +
    '" fill="none" stroke="' + INK +
    '" stroke-width="2" stroke-dasharray="7 5" opacity="0.55"/>');
  const boundaryLabel = iso(b.minX + 0.35, b.minY - 0.35, 0);
  svg.push(surfaceText("top", boundaryLabel, "INTERNAL", {
    size: 10, weight: 700, spacing: "0.18em", opacity: 0.55,
  }));

  /* Deliberately routed out: a solid segment on the boundary and a filled
     square outside it per egress component. The trace itself is routed by
     the same rules as every link. */
  for (const external of plan.external) {
    const edge = external.west ? b.minX : b.maxX;
    const parts = ['<g class="hit" data-kind="' + external.type +
      '" data-id="' + escapeAttribute(external.node.spec.name) + '">'];
    parts.push(line(
      iso(edge, external.markY - 1, 0),
      iso(edge, external.markY + 1, 0), INK, 3));
    const [sx, sy] = iso(external.outside, external.markY, 0);
    /* An open square in the link vocabulary: the published entry is where
       the world gets to connect in. */
    parts.push('<polygon points="' +
      pts(surfaceQuad("top", [sx, sy], -4, 4, -4, 4)) +
      '" fill="' + CARTA + '" stroke="' + INK + '" stroke-width="2"/>');
    const text = (external.node.spec.published_ports || []).map(p =>
      p.host_ip + ":" + p.host_port + "→" + p.container_port +
      "/" + p.protocol).join(" · ");
    parts.push(surfaceText("top", [sx, sy], text, {
      along: external.west ? -12 : 12,
      down: 4,
      anchor: external.west ? "end" : "start",
      size: 10,
      weight: 700,
      spacing: "0.18em",
      opacity: 0.8,
      mono: true,
    }));
    parts.push("</g>");
    svg.push(parts.join(""));
  }
}

function externalSelected(external) {
  return state.selection && state.selection.kind === external.type &&
    state.selection.id === external.node.spec.name;
}

/* An external net: routed floor line from the component's port to its
   boundary mark, quieter than a link, never animated, and clickable like
   any other trace. */
function renderExternalTrace(svg, external) {
  const selected = externalSelected(external);
  const points = tracePoints(external);
  const d = "M" + points.map(p => iso(p[0], p[1], p[2]).join(" ")).join(" L");
  svg.push('<path d="' + d + '" fill="none" stroke="' +
    (selected ? ROSSO : INK) + '" stroke-width="' + (selected ? 3 : 2) +
    '" opacity="' + (selected ? 1 : 0.45) + '" stroke-linejoin="round"/>');
  svg.push('<path d="' + d +
    '" fill="none" stroke="rgba(0,0,0,0)" stroke-width="14" class="hit" ' +
    'data-kind="' + external.type + '" data-id="' +
    escapeAttribute(external.node.spec.name) + '"/>');
}

/* The tile-side publish port, with its via. Untyped, so it takes no hue:
   an open ink square through which the component serves its bindings. */
function renderExternalPort(svg, external) {
  const node = external.node;
  const port = node.publishPort;
  const face = PORT_FACES.east;
  const selected = externalSelected(external);
  svg.push(line(iso(port.x, port.y, 0), iso(port.x, port.y, 1),
    "rgba(32,26,18,0.7)", 2));
  const [x, y] = iso(port.x, port.y, 1);
  svg.push('<g class="hit" data-kind="' + external.type + '" data-id="' +
    escapeAttribute(node.spec.name) + '">' +
    '<polygon points="' +
    pts(surfaceQuad(face.pad, [x, y], -5, 5, 0, 7)) +
    '" fill="' + CARTA + '" stroke="' + INK + '" stroke-width="2"/>' +
    (selected ?
      '<polygon points="' +
      pts(surfaceQuad(face.pad, [x, y], -9, 9, -3, 11)) +
      '" fill="none" stroke="' + INK + '" stroke-width="2"/>' : "") +
    portLabel(face, [x, y], "publish") +
    "</g>");
}

function routeSelected(plan, route) {
  const selection = state.selection;
  if (!selection) return false;
  if (selection.kind === "link") return selection.id === route.id;
  if (selection.kind === "port") {
    return (plan.portRoutes.get(selection.id) || []).includes(route.id);
  }
  return false;
}

function renderRoute(svg, plan, route) {
  const selected = routeSelected(plan, route);
  const points = tracePoints(route);
  const d = "M" + points.map(p => iso(p[0], p[1], p[2]).join(" ")).join(" L");
  const connected = route.link.active === true;
  const stroke = selected ? ROSSO : (connected ? VERDE : INK);
  const width = selected ? 3 : 2;
  svg.push('<path d="' + d + '" fill="none" stroke="' + stroke +
    '" stroke-width="' + width + '" opacity="' + (selected ? 1 : 0.9) +
    '" stroke-linejoin="round"/>');
  /* The base colour reports connectivity. Beads are separate: their density
     and speed come only from the measured byte delta between observations. */
  const activity = state.linkActivity.get(route.id);
  const beads = beadAppearance(activity ? activity.level : 0);
  if (beads && !selected) {
    svg.push('<path d="' + d + '" fill="none" stroke="' + INK +
      '" stroke-width="' + beads.width.toFixed(2) +
      '" stroke-dasharray="1.5 ' + beads.gap.toFixed(2) +
      '" class="march" style="--bead-period:' + beads.period.toFixed(3) +
      's;--bead-delay:' + beads.delay.toFixed(3) + 's"/>');
  }
  svg.push('<path d="' + d +
    '" fill="none" stroke="rgba(0,0,0,0)" stroke-width="14" class="hit" ' +
    'data-kind="link" data-id="' + escapeAttribute(route.id) + '"/>');
}

/* A linked port meets its floor route through a vertical drop — the via. */
function renderDrops(svg, plan, route) {
  const producer = plan.byName.get(route.link.output.component);
  const consumer = plan.byName.get(route.link.input.component);
  const from = producer.outputs.get(route.link.output.endpoint);
  const to = consumer.inputs.get(route.link.input.endpoint);
  const selected = routeSelected(plan, route);
  const stroke = selected ? ROSSO : (route.link.active === true ? VERDE : INK);
  for (const port of [from, to]) {
    svg.push(line(iso(port.x, port.y, 0), iso(port.x, port.y, 1),
      stroke, 2));
  }
}

function renderNode(svg, node) {
  const status = statusOf(node.spec);
  const x = node.x, y = node.y, w = node.w, h = node.h;
  const alpha = status.faint ? 0.35 : 1;
  const dash = status.solid ? "" : ' stroke-dasharray="6 4"';
  const stroke = 'stroke="' + INK + '" stroke-width="2"' + dash;
  const top = [
    iso(x, y, 1), iso(x + w, y, 1), iso(x + w, y + h, 1), iso(x, y + h, 1),
  ];
  const group = ['<g opacity="' + alpha +
    '" class="hit" data-kind="component" data-id="' +
    escapeAttribute(node.spec.name) + '">'];

  /* Wireframe prism: opaque carta faces, ink edges, no shading anywhere. */
  const south = [
    iso(x, y + h, 1), iso(x + w, y + h, 1),
    iso(x + w, y + h, 0), iso(x, y + h, 0),
  ];
  const east = [
    iso(x + w, y, 1), iso(x + w, y + h, 1),
    iso(x + w, y + h, 0), iso(x + w, y, 0),
  ];
  group.push('<polygon points="' + pts(south) + '" fill="' + CARTA + '" ' + stroke + '/>');
  group.push('<polygon points="' + pts(east) + '" fill="' + CARTA + '" ' + stroke + '/>');
  group.push('<polygon points="' + pts(top) + '" fill="' + CARTA + '" ' + stroke + '/>');

  /* The module: a flat hue shape preserving the block's aspect ratio on the
     top face. Hollow when not running — the hue names the module, the fill
     state is line work. */
  const moduleX = w * 0.3;
  const moduleY = h * 0.3;
  const cx = x + w / 2, cy = y + h / 2;
  const module_ = [
    iso(cx - moduleX, cy - moduleY, 1),
    iso(cx + moduleX, cy - moduleY, 1),
    iso(cx + moduleX, cy + moduleY, 1),
    iso(cx - moduleX, cy + moduleY, 1),
  ];
  if (status.up || !node.spec.status) {
    group.push('<polygon points="' + pts(module_) + '" fill="' + node.hue + '"/>');
  } else {
    group.push('<polygon points="' + pts(module_) + '" fill="none" stroke="' +
      node.hue + '" stroke-width="3"/>');
  }
  /* External reach is hazard-taped: a ring of alternating giallo and ink
     blocks close around the coloured module. Keeping the tape away from the
     block edge leaves a dedicated, quiet band for interface names.
     Outbound egress and inbound publish both count as reach. */
  if (node.spec.egress || (node.spec.published_ports || []).length) {
    const hazardGap = 0.2;
    hazardRing(group,
      cx - moduleX - hazardGap,
      cy - moduleY - hazardGap,
      cx + moduleX + hazardGap,
      cy + moduleY + hazardGap,
    );
  }

  const selected = state.selection &&
    state.selection.kind === "component" &&
    state.selection.id === node.spec.name;
  if (selected) {
    const halo = [
      iso(x - 0.25, y - 0.25, 1), iso(x + w + 0.25, y - 0.25, 1),
      iso(x + w + 0.25, y + h + 0.25, 1), iso(x - 0.25, y + h + 0.25, 1),
    ];
    group.push('<polygon points="' + pts(halo) + '" fill="none" stroke="' +
      INK + '" stroke-width="3"/>');
  }

  /* The name is silkscreened into the block's top-left inset. Its baseline
     follows the top face; layout reserves enough edge length for the text.
     The status word stays a billboard flag at the apex. */
  const taped = node.spec.egress || (node.spec.published_ports || []).length;
  const [ax, ay] = iso(x + 0.38, y + 0.38, 1);
  const size = Math.min(13, Math.max(9,
    Math.round(((h / 2 - moduleY) - (taped ? 0.35 : 0.1)) * 40)));
  group.push(surfaceText("top", [ax, ay], node.spec.name, {
    size, weight: 700, spacing: "-0.02em",
  }));
  if (status.word) {
    const [nx, ny] = iso(x, y, 1);
    group.push('<text x="' + nx + '" y="' + (ny - 8) +
      '" text-anchor="middle" font-size="9" font-weight="700" ' +
      'letter-spacing="0.2em"' + (status.blink ? ' class="blink"' : "") + ">" +
      escapeText(status.word) + "</text>");
  }
  group.push("</g>");
  svg.push(group.join(""));
}

/* Hazard tape: alternating giallo and ink blocks laid as flat world-space
   quads along the rectangle (x0,y0)-(x1,y1). Block length stays
   near-constant in world units, so the tape reads identically on any tile
   size. The horizontal runs extend past the corners to close the ring. */
function hazardRing(group, x0, y0, x1, y1) {
  const half = 0.11;
  const sides = [
    { from: [x0 - half, y0], to: [x1 + half, y0], axis: "x" },
    { from: [x0 - half, y1], to: [x1 + half, y1], axis: "x" },
    { from: [x0, y0 + half], to: [x0, y1 - half], axis: "y" },
    { from: [x1, y0 + half], to: [x1, y1 - half], axis: "y" },
  ];
  for (const side of sides) {
    const length = side.axis === "x" ?
      side.to[0] - side.from[0] : side.to[1] - side.from[1];
    const blocks = Math.max(4, 2 * Math.round(length / 0.8));
    for (let block = 0; block < blocks; block++) {
      const a = block / blocks, b = (block + 1) / blocks;
      let quad;
      if (side.axis === "x") {
        const ax = side.from[0] + length * a;
        const bx = side.from[0] + length * b;
        const y = side.from[1];
        quad = [iso(ax, y - half, 1), iso(bx, y - half, 1),
          iso(bx, y + half, 1), iso(ax, y + half, 1)];
      } else {
        const ay = side.from[1] + length * a;
        const by = side.from[1] + length * b;
        const x = side.from[0];
        quad = [iso(x - half, ay, 1), iso(x - half, by, 1),
          iso(x + half, by, 1), iso(x + half, ay, 1)];
      }
      group.push('<polygon points="' + pts(quad) + '" fill="' +
        (block % 2 ? INK : GIALLO) + '"/>');
    }
  }
}

/* Every declared endpoint is drawn, linked or not: open square input, filled
   square output, on the visible faces, named in mono, clickable. An unlinked
   port is drawn faded — declared but not wired. */
function renderNodePorts(svg, plan, node) {
  const geometry = {
    in: PORT_FACES.south,
    out: PORT_FACES.east,
  };
  const draw = (direction, ports) => {
    for (const [name, port] of ports) {
      const key = direction + ":" + node.spec.name + ":" + name;
      const linked = plan.portRoutes.has(key);
      const selected = state.selection &&
        state.selection.kind === "port" && state.selection.id === key;
      const [x, y] = iso(port.x, port.y, 1);
      const face = geometry[direction];
      const opacity = linked || selected ? 1 : 0.45;
      const parts = ['<g class="hit" data-kind="port" data-id="' +
        escapeAttribute(key) + '" opacity="' + opacity + '">'];
      if (direction === "out") {
        parts.push('<polygon points="' +
          pts(surfaceQuad(face.pad, [x, y], -5, 5, 0, 7)) +
          '" fill="' + node.hue + '" stroke="' + INK +
          '" stroke-width="1.5"/>');
      } else {
        parts.push('<polygon points="' +
          pts(surfaceQuad(face.pad, [x, y], -5, 5, 0, 7)) +
          '" fill="' + CARTA + '" stroke="' + INK +
          '" stroke-width="2"/>');
      }
      if (selected) {
        parts.push('<polygon points="' +
          pts(surfaceQuad(face.pad, [x, y], -9, 9, -3, 11)) +
          '" fill="none" stroke="' + INK + '" stroke-width="2"/>');
      }
      /* The socket is on the vertical face; its name is printed immediately
         inside the matching top edge and centered on the socket. Turning the
         top-face frame keeps both baselines parallel to their own edge. */
      parts.push(portLabel(face, [x, y], name));
      parts.push("</g>");
      svg.push(parts.join(""));
    }
  };
  draw("out", node.outputs);
  draw("in", node.inputs);
}
