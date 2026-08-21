import assert from "node:assert/strict";
import test from "node:test";

import {
  surfaceBasis,
  surfaceMatrix,
  surfaceQuad,
  TX,
  TY,
} from "../dashboard/js/iso.js";

function close(actual, expected, message) {
  assert.ok(Math.abs(actual - expected) < 1e-12,
    `${message}: got ${actual}, want ${expected}`);
}

test("surface text and pads share the projected face axes", () => {
  const edge = Math.hypot(TX, TY);
  const expected = {
    top: {
      u: [TX / edge, TY / edge],
      v: [-TX / edge, TY / edge],
    },
    south: {
      u: [TX / edge, TY / edge],
      v: [0, 1],
    },
    east: {
      u: [TX / edge, -TY / edge],
      v: [0, 1],
    },
  };

  for (const [surface, want] of Object.entries(expected)) {
    const basis = surfaceBasis(surface);
    close(basis.u[0], want.u[0], `${surface} u.x`);
    close(basis.u[1], want.u[1], `${surface} u.y`);
    close(basis.v[0], want.v[0], `${surface} v.x`);
    close(basis.v[1], want.v[1], `${surface} v.y`);

    const matrix = surfaceMatrix(surface, [20, 30], 0, 0);
    assert.deepEqual(matrix, [
      basis.u[0], basis.u[1], basis.v[0], basis.v[1], 20, 30,
    ]);

    const quad = surfaceQuad(surface, [20, 30], 0, 8, 0, 6);
    close(quad[1][0] - quad[0][0], basis.u[0] * 8,
      `${surface} pad u.x`);
    close(quad[1][1] - quad[0][1], basis.u[1] * 8,
      `${surface} pad u.y`);
    close(quad[3][0] - quad[0][0], basis.v[0] * 6,
      `${surface} pad v.x`);
    close(quad[3][1] - quad[0][1], basis.v[1] * 6,
      `${surface} pad v.y`);
  }
});

test("unknown surfaces fail instead of silently falling back to flat text", () => {
  assert.throws(() => surfaceBasis("screen"), /unknown isometric surface/);
});

test("a quarter-turn follows the adjacent edge without mirroring text", () => {
  const top = surfaceBasis("top");
  const turned = surfaceBasis("top", 1);

  assert.deepEqual(turned.u, [-top.v[0], -top.v[1]]);
  assert.deepEqual(turned.v, top.u);
  assert.ok(
    turned.u[0] * turned.v[1] - turned.u[1] * turned.v[0] > 0,
    "the turned text frame must preserve orientation",
  );
});
