/* The right-hand descriptor panel: mono blocks in the system-file idiom. */

import { state } from "./state.js";
import { escapeText } from "./iso.js";

export function renderPanel(plan) {
  const doc = state.doc;
  const panel = document.getElementById("panel");
  const selection = state.selection;
  if (selection && selection.kind === "component" &&
      plan.byName.has(selection.id)) {
    panel.innerHTML = componentPanel(plan.byName.get(selection.id));
    return;
  }
  if (selection && selection.kind === "link") {
    const route = plan.routes.find(r => r.id === selection.id);
    if (route) { panel.innerHTML = linkPanel(route); return; }
  }
  if (selection && selection.kind === "port") {
    const html = portPanel(plan, selection.id);
    if (html) { panel.innerHTML = html; return; }
  }
  if (selection && (selection.kind === "egress" ||
      selection.kind === "publish") && plan.byName.has(selection.id)) {
    panel.innerHTML = externalPanel(
      plan.byName.get(selection.id), selection.kind);
    return;
  }
  panel.innerHTML = systemPanel(doc);
}

function externalPanel(node, kind) {
  const spec = node.spec;
  const lines = [];
  if (kind === "egress") {
    lines.push("egress    " + spec.name);
    lines.push("grants    outbound connections to external destinations");
  } else {
    lines.push("publish   " + spec.name);
    lines.push("grants    inbound connections from each host binding");
    for (const port of node.publishedPorts) {
      lines.push("binding   " + port.protocol + " " + port.host_ip + ":" +
        port.host_port + " -> " + port.container_port);
    }
  }
  const note = kind === "egress" ?
    "OUTBOUND ONLY. NO INBOUND LISTENER,<br>NO INTERNAL LINK." :
    "INBOUND ONLY, THROUGH THESE BINDINGS.<br>NO INTERNAL LINK.";
  return '<h2>' + (kind === "egress" ? "EGRESS" : "PUBLISH") +
    ' · <em>' + escapeText(spec.name.toUpperCase()) + "</em></h2>" +
    '<div class="descriptor">' + escapeText(lines.join("\n")) + "</div>" +
    '<div class="legend">' + note + "</div>";
}

function systemPanel(doc) {
  const counts = (doc.components || []).length + " COMPONENTS · " +
    (doc.links || []).length + " LINKS";
  const stateLine = doc.source === "file" ? "file" :
    doc.operation ? "operation " + doc.operation + " · " + doc.phase :
    doc.desired ?
      (doc.operational ? "running · operational" : "running · degraded") :
      "absent";
  return '<h2>SYSTEM · <em>' + escapeText((doc.name || "").toUpperCase()) +
    "</em></h2>" +
    '<div class="descriptor">' +
    "system    " + escapeText(doc.name || "") + "\n" +
    "state     " + escapeText(stateLine) + "\n" +
    (doc.digest ? "digest    " + escapeText(doc.digest.slice(0, 26)) + "…\n" : "") +
    "</div>" +
    '<div class="legend">' + counts + "</div>" +
    '<div class="rule"></div>' +
    '<div class="legend">' +
    legendGlyph("input") + "INPUT — OPEN SQUARE<br>" +
    legendGlyph("output") + "OUTPUT — FILLED SQUARE<br>" +
    legendGlyph("active-link") + "GREEN LINK — CONNECTED<br>" +
    legendGlyph("link") + "BLACK LINK — DISCONNECTED<br>" +
    legendGlyph("beads") + "BEADS — MEASURED BYTE RATE<br>" +
    legendGlyph("dashed") + "INTERNAL BOUNDARY<br>" +
    legendGlyph("striped") + "HAZARD RING — EXTERNAL REACH<br>" +
    legendGlyph("solid") + "SOLID RULE — PUBLISHED ENTRY" +
    "</div>" +
    '<div class="rule"></div>' +
    '<div class="legend">MODULE COLOUR NAMES MODULES.<br>' +
    "LINK COLOUR SHOWS CONNECTIVITY.</div>";
}

function legendGlyph(kind) {
  if (kind === "input") {
    return '<svg width="14" height="14"><rect x="2" y="2" width="10" ' +
      'height="10" fill="none" stroke="#201A12" stroke-width="2"/></svg>';
  }
  if (kind === "output") {
    return '<svg width="14" height="14"><rect x="2" y="2" width="10" ' +
      'height="10" fill="#2A5CAA"/></svg>';
  }
  if (kind === "link") {
    return '<svg width="14" height="14"><line x1="0" y1="7" x2="14" y2="7" ' +
      'stroke="#201A12" stroke-width="2"/></svg>';
  }
  if (kind === "active-link") {
    return '<svg width="14" height="14"><line x1="0" y1="7" x2="14" y2="7" ' +
      'stroke="#3E7C5A" stroke-width="2"/></svg>';
  }
  if (kind === "beads") {
    return '<svg width="14" height="14"><line x1="0" y1="7" x2="14" y2="7" ' +
      'stroke="#201A12" stroke-width="3" stroke-dasharray="1.5 4"/></svg>';
  }
  if (kind === "dashed") {
    return '<svg width="14" height="14"><line x1="0" y1="7" x2="14" y2="7" ' +
      'stroke="#201A12" stroke-width="2" stroke-dasharray="4 3"/></svg>';
  }
  if (kind === "striped") {
    return '<svg width="14" height="14">' +
      '<rect x="4.5" y="4.5" width="5" height="5" fill="#2A5CAA"/>' +
      '<rect x="1.5" y="1.5" width="11" height="11" fill="none" ' +
      'stroke="#EFB02C" stroke-width="3"/>' +
      '<rect x="1.5" y="1.5" width="11" height="11" fill="none" ' +
      'stroke="#201A12" stroke-width="3" stroke-dasharray="3.5 3.5"/></svg>';
  }
  return '<svg width="14" height="14"><line x1="0" y1="7" x2="14" y2="7" ' +
    'stroke="#201A12" stroke-width="3"/></svg>';
}

function componentPanel(node) {
  const spec = node.spec, doc = state.doc;
  const lines = [];
  lines.push("component " + spec.name);
  lines.push("docker    " + spec.image_ref);
  if (spec.image_id) lines.push("image     " + spec.image_id.slice(0, 19) + "…");
  lines.push("route     " + (spec.egress ? "egress" : "internal"));
  if (spec.status) {
    lines.push("status    " + spec.status.status +
      (spec.status.health ? " · " + spec.status.health : ""));
    if (spec.status.container_id) {
      lines.push("container " + spec.status.container_id.slice(0, 12));
    }
    if (spec.status.status === "exited") {
      lines.push("exit      " + spec.status.exit_code);
    }
  }
  for (const input of spec.inputs || []) {
    const link = (doc.links || []).find(l =>
      l.input.component === spec.name && l.input.endpoint === input.name);
    lines.push("input     " + input.name + "  " + input.service +
      (link ? "  <- " + link.output.component + "." + link.output.endpoint : ""));
  }
  for (const output of spec.outputs || []) {
    lines.push("output    " + output.name + "  " + output.service);
  }
  for (const bind of spec.binds || []) {
    lines.push("bind      " + (bind.source ? bind.source + " " : "") +
      bind.target + (bind.read_only ? " ro" : " rw"));
  }
  for (const volume of spec.volumes || []) {
    lines.push("volume    " + volume.name + " " + volume.target +
      (volume.read_only ? " ro" : " rw"));
  }
  if ((spec.args || []).length) lines.push("args      " + spec.args.join(" "));
  for (const port of node.publishedPorts) {
    lines.push("publish   " + port.protocol + " " + port.host_ip + ":" +
      port.host_port + " -> " + port.container_port);
  }
  let problem = "";
  if (spec.status && spec.status.problem) {
    problem = '<div class="descriptor"><span class="accent">problem</span>   ' +
      escapeText(spec.status.problem) + "</div>";
  }
  return '<h2>COMPONENT · <em>' + escapeText(spec.name.toUpperCase()) +
    "</em></h2>" +
    '<div class="descriptor">' + escapeText(lines.join("\n")) + "</div>" +
    problem;
}

function linkPanel(route) {
  const link = route.link;
  const lines = [
    "input     " + link.input.component + "." + link.input.endpoint,
    "output    " + link.output.component + "." + link.output.endpoint,
    "service   " + link.service,
  ];
  if (typeof link.active === "boolean") {
    lines.push("active    " + (link.active ? "yes" : "no"));
  }
  if (link.active_connections !== undefined) {
    lines.push("streams   " + link.active_connections);
  }
  if (link.activity) {
    lines.push("in -> out " + link.activity.bytes_input_to_output + " bytes");
    lines.push("out -> in " + link.activity.bytes_output_to_input + " bytes");
    const observed = state.linkActivity.get(route.id);
    if (observed) {
      lines.push("rate      " + Math.round(observed.bytesPerSecond) + " B/s");
    }
  }
  return '<h2>LINK</h2>' +
    '<div class="descriptor">' + escapeText(lines.join("\n")) + "</div>" +
    '<div class="legend">ONE PRIVATE CHANNEL.<br>NOTHING ELSE IS REACHABLE.</div>';
}

function portPanel(plan, key) {
  const [direction, componentName, portName] = key.split(":");
  const node = plan.byName.get(componentName);
  if (!node) return "";
  const endpoints = direction === "in" ?
    node.spec.inputs || [] : node.spec.outputs || [];
  const endpoint = endpoints.find(e => e.name === portName);
  if (!endpoint) return "";
  const lines = [];
  lines.push("interface " + endpoint.name);
  lines.push("component " + componentName);
  lines.push("direction " + (direction === "in" ? "input" : "output"));
  lines.push("service   " + endpoint.service);
  const links = (state.doc.links || []).filter(l =>
    direction === "in" ?
      l.input.component === componentName && l.input.endpoint === portName :
      l.output.component === componentName && l.output.endpoint === portName);
  if (!links.length) {
    lines.push("link      unlinked");
  }
  for (const link of links) {
    lines.push(direction === "in" ?
      "link      <- " + link.output.component + "." + link.output.endpoint :
      "link      -> " + link.input.component + "." + link.input.endpoint);
  }
  const note = direction === "in" ?
    "AN INPUT CONSUMES EXACTLY ONE LINKED OUTPUT." :
    (links.length ?
      "AN OUTPUT MAY FAN OUT TO SEVERAL INPUTS." :
      "DECLARED, NOT WIRED — NOTHING REACHES IT.");
  return '<h2>INTERFACE · <em>' + escapeText(portName.toUpperCase()) +
    "</em></h2>" +
    '<div class="descriptor">' + escapeText(lines.join("\n")) + "</div>" +
    '<div class="legend">' + note + "</div>";
}
