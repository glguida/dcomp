/* The camera and the ink. World space is an orthogonal tile grid; only the
   projection is isometric. All routes and boundaries turn at right angles in
   world space; labels printed on the world follow the face they belong to. */

export const HUES = ["#2A5CAA", "#D8402E", "#EFB02C", "#3E7C5A"];
export const INK = "#201A12";
export const CARTA = "#EFE7D6";
export const ROSSO = "#D8402E";
export const GIALLO = "#EFB02C";
export const VERDE = "#3E7C5A";

export const TX = 36, TY = 18, TZ = 26;

const EDGE_LENGTH = Math.hypot(TX, TY);
const SURFACES = Object.freeze({
  /* Local u/v axes projected into screen space. East uses world -y so its
     labels read left-to-right while remaining in the east-face plane. */
  top: Object.freeze({
    u: Object.freeze([TX / EDGE_LENGTH, TY / EDGE_LENGTH]),
    v: Object.freeze([-TX / EDGE_LENGTH, TY / EDGE_LENGTH]),
  }),
  south: Object.freeze({
    u: Object.freeze([TX / EDGE_LENGTH, TY / EDGE_LENGTH]),
    v: Object.freeze([0, 1]),
  }),
  east: Object.freeze({
    u: Object.freeze([TX / EDGE_LENGTH, -TY / EDGE_LENGTH]),
    v: Object.freeze([0, 1]),
  }),
});

export function iso(x, y, z) {
  return [(x - y) * TX, (x + y) * TY - (z || 0) * TZ];
}

export function pts(list) {
  return list.map(p => p.join(",")).join(" ");
}

export function line(from, to, stroke, width) {
  return '<line x1="' + from[0] + '" y1="' + from[1] + '" x2="' + to[0] +
    '" y2="' + to[1] + '" stroke="' + stroke + '" stroke-width="' + width + '"/>';
}

export function surfaceBasis(surface, turns = 0) {
  const basis = SURFACES[surface];
  if (!basis) throw new Error("unknown isometric surface " + surface);
  const turn = ((turns % 4) + 4) % 4;
  if (turn === 0) return basis;
  if (turn === 1) {
    return { u: [-basis.v[0], -basis.v[1]], v: basis.u };
  }
  if (turn === 2) {
    return {
      u: [-basis.u[0], -basis.u[1]],
      v: [-basis.v[0], -basis.v[1]],
    };
  }
  return { u: basis.v, v: [-basis.u[0], -basis.u[1]] };
}

export function surfacePoint(surface, origin, u, v, turns = 0) {
  const basis = surfaceBasis(surface, turns);
  return [
    origin[0] + basis.u[0] * u + basis.v[0] * v,
    origin[1] + basis.u[1] * u + basis.v[1] * v,
  ];
}

export function surfaceMatrix(surface, origin, u, v, turns = 0) {
  const basis = surfaceBasis(surface, turns);
  const at = surfacePoint(surface, origin, u, v, turns);
  return [basis.u[0], basis.u[1], basis.v[0], basis.v[1], at[0], at[1]];
}

export function surfaceQuad(surface, origin, u0, u1, v0, v1, turns = 0) {
  return [
    surfacePoint(surface, origin, u0, v0, turns),
    surfacePoint(surface, origin, u1, v0, turns),
    surfacePoint(surface, origin, u1, v1, turns),
    surfacePoint(surface, origin, u0, v1, turns),
  ];
}

export function escapeText(value) {
  return String(value)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

export function escapeAttribute(value) {
  return escapeText(value).replace(/"/g, "&quot;");
}
