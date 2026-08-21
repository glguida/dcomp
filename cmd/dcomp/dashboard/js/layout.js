/* Placement: layered like a schematic, compacted like a board. Back edges
   are cut so the forward graph is acyclic; longest-path layering is then
   tightened so every module presses against its earliest consumer; rows are
   ordered by barycenter and columns slide toward what they are wired to. */

import { HUES } from "./iso.js";
import { routeAll } from "./router.js";

export function layout(doc) {
  const components = (doc.components || []).slice()
    .sort((a, b) => a.name < b.name ? -1 : 1);
  const byName = new Map();
  /* Inputs live on the south edge and outputs on the east edge — the two
     faces the camera can see. Width follows inputs, depth follows outputs,
     and every tile is large enough that no port sits near a corner. */
  components.forEach((component, index) => {
    const eastSlots = (component.outputs || []).length +
      ((component.published_ports || []).length ? 1 : 0);
    /* A hazard-taped box needs face room between tape and module for its
       silkscreened name, so external reach raises the minimum size. */
    const minimum = component.egress ||
      (component.published_ports || []).length ? 4 : 3;
    byName.set(component.name, {
      spec: component,
      hue: HUES[index % 4],
      layer: 0,
      w: Math.max(minimum, (component.inputs || []).length + 2),
      h: Math.max(minimum, eastSlots + 2),
      x: 0,
      y: 0,
    });
  });
  const links = doc.links || [];

  const adjacency = new Map();
  for (const component of components) adjacency.set(component.name, []);
  const edges = links
    .map(l => ({ from: l.output.component, to: l.input.component }))
    .filter(e => e.from !== e.to && byName.has(e.from) && byName.has(e.to))
    .sort((a, b) => (a.from + " " + a.to) < (b.from + " " + b.to) ? -1 : 1);
  for (const edge of edges) adjacency.get(edge.from).push(edge);

  /* Pure producers make the DFS roots, so the dropped cycle edge is the
     feedback edge rather than an arbitrary one along the forward flow. */
  const color = new Map();
  const forward = [];
  const roots = components.slice().sort((a, b) => {
    const aSource = (a.inputs || []).length === 0 ? 0 : 1;
    const bSource = (b.inputs || []).length === 0 ? 0 : 1;
    if (aSource !== bSource) return aSource - bSource;
    return a.name < b.name ? -1 : 1;
  });
  for (const component of roots) {
    if (color.get(component.name)) continue;
    color.set(component.name, 1);
    const stack = [{ name: component.name, index: 0 }];
    while (stack.length) {
      const frame = stack[stack.length - 1];
      const out = adjacency.get(frame.name);
      if (frame.index < out.length) {
        const edge = out[frame.index++];
        const seen = color.get(edge.to) || 0;
        if (seen === 1) continue;
        forward.push(edge);
        if (seen === 0) {
          color.set(edge.to, 1);
          stack.push({ name: edge.to, index: 0 });
        }
      } else {
        color.set(frame.name, 2);
        stack.pop();
      }
    }
  }
  for (let pass = 0; pass < components.length; pass++) {
    let moved = false;
    for (const edge of forward) {
      const producer = byName.get(edge.from);
      const consumer = byName.get(edge.to);
      if (consumer.layer < producer.layer + 1) {
        consumer.layer = producer.layer + 1;
        moved = true;
      }
    }
    if (!moved) break;
  }
  /* Slack tightening: a module drifts right until it presses against its
     earliest consumer, so nothing sits needlessly far from what it feeds. */
  const consumersOf = new Map();
  for (const edge of forward) {
    if (!consumersOf.has(edge.from)) consumersOf.set(edge.from, []);
    consumersOf.get(edge.from).push(edge.to);
  }
  const descending = [...byName.entries()]
    .sort((a, b) => b[1].layer - a[1].layer);
  for (const [name, node] of descending) {
    const consumers = consumersOf.get(name);
    if (!consumers || !consumers.length) continue;
    node.layer = Math.min(...consumers.map(c => byName.get(c).layer)) - 1;
  }
  const used = [...new Set([...byName.values()].map(n => n.layer))]
    .sort((a, b) => a - b);
  const compact = new Map(used.map((layer, index) => [layer, index]));
  for (const node of byName.values()) node.layer = compact.get(node.layer);

  /* Rows: barycenter sweeps pull linked modules toward each other so traces
     cross less, then tiles stack with routing gutters between them. */
  const layers = new Map();
  for (const node of byName.values()) {
    if (!layers.has(node.layer)) layers.set(node.layer, []);
    layers.get(node.layer).push(node);
  }
  const neighbors = new Map();
  for (const node of byName.values()) neighbors.set(node.spec.name, []);
  for (const link of links) {
    const producer = byName.get(link.output.component);
    const consumer = byName.get(link.input.component);
    if (!producer || !consumer || producer === consumer) continue;
    neighbors.get(producer.spec.name).push(consumer);
    neighbors.get(consumer.spec.name).push(producer);
  }
  const order = [...layers.keys()].sort((a, b) => a - b);
  const layerX = new Map();
  let xCursor = 0;
  for (const layer of order) {
    layerX.set(layer, xCursor);
    const width = Math.max(...layers.get(layer).map(n => n.w));
    xCursor += width + 3;
  }
  const place = nodes => {
    let cursor = 0;
    for (const node of nodes) {
      node.x = layerX.get(node.layer);
      node.y = cursor;
      cursor += node.h + 2;
    }
  };
  for (const layer of order) {
    layers.get(layer).sort((a, b) => a.spec.name < b.spec.name ? -1 : 1);
    place(layers.get(layer));
  }
  for (let sweep = 0; sweep < 3; sweep++) {
    for (const layer of order) {
      const nodes = layers.get(layer);
      const center = node => {
        const near = neighbors.get(node.spec.name);
        if (!near.length) return node.y + node.h / 2;
        return near.reduce((sum, n) => sum + n.y + n.h / 2, 0) / near.length;
      };
      nodes.sort((a, b) => {
        const left = center(a), right = center(b);
        if (left !== right) return left - right;
        return a.spec.name < b.spec.name ? -1 : 1;
      });
      place(nodes);
    }
  }
  /* Vertical alignment: each column slides toward the centre of what it is
     wired to instead of stacking from the top, then snaps to the grid. */
  for (let pass = 0; pass < 4; pass++) {
    for (const layer of order) {
      const nodes = layers.get(layer);
      let sum = 0, count = 0;
      for (const node of nodes) {
        for (const near of neighbors.get(node.spec.name)) {
          sum += near.y + near.h / 2 - (node.y + node.h / 2);
          count++;
        }
      }
      if (!count) continue;
      const shift = sum / count;
      for (const node of nodes) node.y += shift;
    }
  }
  let minRow = Infinity, maxRow = -Infinity, maxCol = 0;
  for (const node of byName.values()) {
    node.y = Math.round(node.y);
    minRow = Math.min(minRow, node.y);
    maxRow = Math.max(maxRow, node.y + node.h);
    maxCol = Math.max(maxCol, node.x + node.w);
  }
  if (!isFinite(minRow)) { minRow = 0; maxRow = 0; }

  /* Port slots on the half-tile grid, on the visible faces, inset from every
     corner: inputs across the south edge, outputs down the east edge. An
     egress component gets one untyped outbound slot below its outputs. */
  for (const node of byName.values()) {
    node.inputs = new Map();
    node.outputs = new Map();
    const inputs = node.spec.inputs || [];
    const inputStart = (node.w - (inputs.length - 1)) / 2;
    inputs.forEach((endpoint, index) => {
      node.inputs.set(endpoint.name,
        { x: node.x + inputStart + index, y: node.y + node.h });
    });
    const outputs = node.spec.outputs || [];
    const published = (node.spec.published_ports || []).length !== 0;
    const eastSlots = outputs.length + (published ? 1 : 0);
    const eastStart = (node.h - (eastSlots - 1)) / 2;
    outputs.forEach((endpoint, index) => {
      node.outputs.set(endpoint.name,
        { x: node.x + node.w, y: node.y + eastStart + index });
    });
    if (published) {
      node.publishPort = {
        x: node.x + node.w,
        y: node.y + eastStart + outputs.length,
      };
    }
  }

  const bounds = {
    minX: -2, minY: minRow - 2, maxX: maxCol + 2, maxY: maxRow + 2,
  };

  /* Published entries are the only external nets drawn: outbound egress is
     already carried by the hazard ring and adds no route information. Marks
     take the nearest west or east boundary, staggered so neighbours never
     overlap; the trace to each mark is routed like any other. */
  const externalRequests = [];
  for (const node of byName.values()) {
    if (!node.publishPort) continue;
    externalRequests.push({
      node, type: "publish",
      west: (node.x + node.w / 2 - bounds.minX) <=
        (bounds.maxX - node.x - node.w / 2),
      markY: Math.round(node.publishPort.y),
    });
  }
  for (const side of [true, false]) {
    let previous = -Infinity;
    for (const request of externalRequests.filter(r => r.west === side)
      .sort((l, r) => l.markY - r.markY ||
        (l.node.spec.name < r.node.spec.name ? -1 : 1))) {
      request.markY = Math.max(request.markY, previous + 3);
      previous = request.markY;
    }
  }
  for (const request of externalRequests) {
    request.outside = request.west ? bounds.minX - 1 : bounds.maxX + 1;
  }

  const { routes, external } =
    routeAll(byName, links, bounds, externalRequests);

  /* Every port knows its routes, so selecting an interface can light the
     channels it participates in and the panel can say where it is wired. */
  const portRoutes = new Map();
  for (const route of routes) {
    for (const key of [
      "in:" + route.link.input.component + ":" + route.link.input.endpoint,
      "out:" + route.link.output.component + ":" + route.link.output.endpoint,
    ]) {
      if (!portRoutes.has(key)) portRoutes.set(key, []);
      portRoutes.get(key).push(route.id);
    }
  }

  return { byName, routes, external, portRoutes, bounds };
}
