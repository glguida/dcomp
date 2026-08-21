/* The camera and the ink. World space is an orthogonal tile grid; only the
   projection is isometric. All routes and boundaries turn at right angles in
   world space, and type never tilts with the world. */

export const HUES = ["#2A5CAA", "#D8402E", "#EFB02C", "#3E7C5A"];
export const INK = "#201A12";
export const CARTA = "#EFE7D6";
export const ROSSO = "#D8402E";
export const GIALLO = "#EFB02C";

export const TX = 36, TY = 18, TZ = 26;

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

export function label(at, dy, text, size, opacity, mono) {
  return '<text x="' + at[0] + '" y="' + (at[1] + dy) + '" font-size="' + size +
    '" font-weight="700" letter-spacing="0.18em" opacity="' + (opacity || 1) +
    '"' + (mono ? ' font-family="ui-monospace, Menlo, monospace"' : "") + ">" +
    escapeText(text) + "</text>";
}

export function escapeText(value) {
  return String(value)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

export function escapeAttribute(value) {
  return escapeText(value).replace(/"/g, "&quot;");
}
