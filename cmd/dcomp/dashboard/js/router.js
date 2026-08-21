/* Traces are routed like a printed board: an A* maze router on the half-tile
   grid, with tiles as obstacles, corners taxed, running on top of another
   trace forbidden, and perpendicular crossings paid for — the later trace
   jumps the earlier one with a squared hop. */

export function routeAll(byName, links, bounds, egressRequests) {
  const scale = 2;
  /* A link is internal by definition: its trace may touch the boundary but
     never leave it. Egress nets may cross, yet the floor beyond the rule is
     taxed so a trace exits only at its own mark, not to shortcut around
     the board. */
  const enclosure = {
    minX: Math.round(bounds.minX * scale) + 2,
    minY: Math.round(bounds.minY * scale) + 2,
    maxX: Math.round(bounds.maxX * scale) - 2,
    maxY: Math.round(bounds.maxY * scale) - 2,
  };
  const extended = {
    minX: Math.round((bounds.minX - 1) * scale),
    minY: Math.round((bounds.minY - 1) * scale),
    maxX: Math.round((bounds.maxX + 1) * scale),
    maxY: Math.round((bounds.maxY + 1) * scale),
  };
  const world = {
    key: (x, y) => x + "," + y,
    blocked: new Set(),
    occupied: new Map(),
    foreignPorts: new Set(),
    range: enclosure,
    beyond: null,
  };
  for (const node of byName.values()) {
    for (let gx = node.x * scale; gx <= (node.x + node.w) * scale; gx++) {
      for (let gy = node.y * scale; gy <= (node.y + node.h) * scale; gy++) {
        world.blocked.add(world.key(gx, gy));
      }
    }
  }
  /* Clearance: floor near any tile is taxed, so traces keep their distance
     from modules they are not wired to — and stay out of the occluded strip
     behind each prism. Port approaches pay the toll briefly and locally. */
  world.penalty = new Map();
  for (const cell of world.blocked) {
    const [bx, by] = cell.split(",").map(Number);
    for (let dx = -3; dx <= 3; dx++) {
      for (let dy = -3; dy <= 3; dy++) {
        if (!dx && !dy) continue;
        const near = world.key(bx + dx, by + dy);
        if (world.blocked.has(near)) continue;
        const tax = Math.max(Math.abs(dx), Math.abs(dy)) <= 2 ? 3 : 1;
        world.penalty.set(near, Math.max(world.penalty.get(near) || 0, tax));
      }
    }
  }
  const portCells = new Set();
  for (const node of byName.values()) {
    const ports = [...node.inputs.values(), ...node.outputs.values()];
    if (node.publishPort) ports.push(node.publishPort);
    for (const port of ports) {
      const cell = world.key(port.x * scale, port.y * scale);
      world.blocked.delete(cell);
      portCells.add(cell);
    }
  }

  const routes = [];
  const ordered = links.slice().sort((a, b) => {
    const left = a.input.component + "." + a.input.endpoint;
    const right = b.input.component + "." + b.input.endpoint;
    return left < right ? -1 : 1;
  });
  for (const link of ordered) {
    const producer = byName.get(link.output.component);
    const consumer = byName.get(link.input.component);
    if (!producer || !consumer) continue;
    const from = producer.outputs.get(link.output.endpoint);
    const to = consumer.inputs.get(link.input.endpoint);
    if (!from || !to) continue;
    const start = { x: from.x * scale, y: from.y * scale };
    const goal = { x: to.x * scale, y: to.y * scale };
    const net = link.output.component + "." + link.output.endpoint;
    world.foreignPorts = new Set(portCells);
    world.foreignPorts.delete(world.key(start.x, start.y));
    world.foreignPorts.delete(world.key(goal.x, goal.y));
    world.range = enclosure;
    world.beyond = null;
    const cells = routeOne(start, goal, world, net);
    if (!cells) continue;
    const id = link.input.component + "." + link.input.endpoint +
      "<-" + link.output.component + "." + link.output.endpoint;
    const crossings = markRoute(world, cells, net);
    routes.push({ link, id, cells, crossings });
  }

  /* External nets obey the same rules as links: same obstacles, same
     clearance, same crossing discipline. Only the destination differs —
     a mark on the boundary instead of another module's port. */
  const external = [];
  for (const request of egressRequests || []) {
    const node = request.node;
    const port = node.publishPort;
    const start = { x: port.x * scale, y: port.y * scale };
    const goal = { x: request.outside * scale, y: request.markY * scale };
    world.foreignPorts = new Set(portCells);
    world.foreignPorts.delete(world.key(start.x, start.y));
    world.range = extended;
    world.beyond = enclosure;
    const id = request.type + ":" + node.spec.name;
    const cells = routeOne(start, goal, world, id);
    if (!cells) continue;
    const crossings = markRoute(world, cells, id);
    external.push({ node, id, cells, crossings, type: request.type,
      west: request.west, markY: request.markY, outside: request.outside });
  }
  return { routes, external };
}

function markRoute(world, cells, net) {
  const crossings = [];
  for (let index = 1; index < cells.length - 1; index++) {
    const before = cells[index - 1];
    const cell = cells[index];
    const after = cells[index + 1];
    const entry = world.occupied.get(world.key(cell.x, cell.y)) || {};
    if (before.y === cell.y && after.y === cell.y &&
        entry.v && entry.v !== net) {
      crossings.push({ x: cell.x, y: cell.y, horizontal: true });
    }
    if (before.x === cell.x && after.x === cell.x &&
        entry.h && entry.h !== net) {
      crossings.push({ x: cell.x, y: cell.y, horizontal: false });
    }
  }
  for (let index = 1; index < cells.length; index++) {
    const before = cells[index - 1];
    const cell = cells[index];
    const orientation = before.y === cell.y ? "h" : "v";
    for (const marked of [before, cell]) {
      const cellKey = world.key(marked.x, marked.y);
      const entry = world.occupied.get(cellKey) || {};
      entry[orientation] = net;
      world.occupied.set(cellKey, entry);
    }
  }
  return crossings;
}

function routeOne(start, goal, world, net) {
  const moves = [
    { dx: 1, dy: 0, o: "h" }, { dx: -1, dy: 0, o: "h" },
    { dx: 0, dy: 1, o: "v" }, { dx: 0, dy: -1, o: "v" },
  ];
  const stateKey = (x, y, o) => x + "," + y + "," + o;
  const heap = new MinHeap();
  const best = new Map();
  const distance = (x, y) => Math.abs(x - goal.x) + Math.abs(y - goal.y);
  const first = { x: start.x, y: start.y, o: "h", cost: 0, parent: null };
  best.set(stateKey(start.x, start.y, "h"), 0);
  heap.push(distance(start.x, start.y), first);
  while (heap.length) {
    const current = heap.pop();
    if (current.x === goal.x && current.y === goal.y) {
      const path = [];
      for (let node = current; node; node = node.parent) {
        path.push({ x: node.x, y: node.y });
      }
      return path.reverse();
    }
    for (const move of moves) {
      const nx = current.x + move.dx;
      const ny = current.y + move.dy;
      if (nx < world.range.minX || ny < world.range.minY ||
          nx > world.range.maxX || ny > world.range.maxY) continue;
      const cellKey = world.key(nx, ny);
      const terminal = nx === goal.x && ny === goal.y;
      if (!terminal && world.blocked.has(cellKey)) continue;
      if (!terminal && world.foreignPorts.has(cellKey)) continue;
      const entry = world.occupied.get(cellKey) || {};
      if (entry[move.o] && entry[move.o] !== net) continue;
      let cost = current.cost + 1 + (world.penalty.get(cellKey) || 0);
      if (world.beyond &&
          (nx < world.beyond.minX || nx > world.beyond.maxX ||
           ny < world.beyond.minY || ny > world.beyond.maxY)) cost += 6;
      if (move.o !== current.o) cost += 3;
      const crossing = entry[move.o === "h" ? "v" : "h"];
      if (crossing && crossing !== net) cost += 9;
      const key = stateKey(nx, ny, move.o);
      if ((best.get(key) ?? Infinity) <= cost) continue;
      best.set(key, cost);
      heap.push(cost + distance(nx, ny),
        { x: nx, y: ny, o: move.o, cost, parent: current });
    }
  }
  return null;
}

class MinHeap {
  constructor() { this.items = []; }
  get length() { return this.items.length; }
  push(priority, value) {
    const items = this.items;
    items.push({ priority, value });
    let index = items.length - 1;
    while (index > 0) {
      const parent = (index - 1) >> 1;
      if (items[parent].priority <= items[index].priority) break;
      [items[parent], items[index]] = [items[index], items[parent]];
      index = parent;
    }
  }
  pop() {
    const items = this.items;
    const top = items[0].value;
    const last = items.pop();
    if (items.length) {
      items[0] = last;
      let index = 0;
      for (;;) {
        const left = 2 * index + 1;
        const right = left + 1;
        let smallest = index;
        if (left < items.length &&
            items[left].priority < items[smallest].priority) smallest = left;
        if (right < items.length &&
            items[right].priority < items[smallest].priority) smallest = right;
        if (smallest === index) break;
        [items[smallest], items[index]] = [items[index], items[smallest]];
        index = smallest;
      }
    }
    return top;
  }
}

/* A routed trace as drawable points: grid cells collapse to corners, and
   each recorded crossing becomes a squared jumper hop over the other trace. */
export function tracePoints(route) {
  const cells = route.cells;
  const corners = [cells[0]];
  for (let index = 1; index < cells.length - 1; index++) {
    const before = cells[index - 1];
    const cell = cells[index];
    const after = cells[index + 1];
    if ((before.x === cell.x && cell.x === after.x) ||
        (before.y === cell.y && cell.y === after.y)) continue;
    corners.push(cell);
  }
  corners.push(cells[cells.length - 1]);
  const world = corners.map(c => [c.x / 2, c.y / 2]);

  const HOP = 0.22, LIFT = 0.5;
  const points = [];
  for (let index = 0; index < world.length - 1; index++) {
    const [ax, ay] = world[index];
    const [bx, by] = world[index + 1];
    points.push([ax, ay, 0]);
    const horizontal = ay === by;
    const along = route.crossings
      .filter(c => c.horizontal === horizontal)
      .map(c => ({ x: c.x / 2, y: c.y / 2 }))
      .filter(c => horizontal ?
        c.y === ay && (c.x - ax) * (c.x - bx) < 0 :
        c.x === ax && (c.y - ay) * (c.y - by) < 0)
      .sort((l, r) => horizontal ?
        (ax < bx ? l.x - r.x : r.x - l.x) :
        (ay < by ? l.y - r.y : r.y - l.y));
    for (const crossing of along) {
      const sign = horizontal ? Math.sign(bx - ax) : Math.sign(by - ay);
      if (horizontal) {
        points.push([crossing.x - sign * HOP, ay, 0]);
        points.push([crossing.x - sign * HOP, ay, LIFT]);
        points.push([crossing.x + sign * HOP, ay, LIFT]);
        points.push([crossing.x + sign * HOP, ay, 0]);
      } else {
        points.push([ax, crossing.y - sign * HOP, 0]);
        points.push([ax, crossing.y - sign * HOP, LIFT]);
        points.push([ax, crossing.y + sign * HOP, LIFT]);
        points.push([ax, crossing.y + sign * HOP, 0]);
      }
    }
  }
  const last = world[world.length - 1];
  points.push([last[0], last[1], 0]);
  return points;
}
