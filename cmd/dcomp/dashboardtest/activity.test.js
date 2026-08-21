import assert from "node:assert/strict";
import test from "node:test";

import {
  activityLevel,
  beadAppearance,
  linkID,
  observeLinkActivity,
} from "../dashboard/js/activity.js";

function document(bytes, active = true) {
  return { links: [{
    input: { component: "caller", endpoint: "upstream" },
    output: { component: "service", endpoint: "echo" },
    active,
    activity: {
      bytes_input_to_output: bytes,
      bytes_output_to_input: bytes / 2,
    },
  }] };
}

test("activity is derived from byte deltas, never connection state", () => {
  let observations = observeLinkActivity(new Map(), document(100), 1000);
  const id = linkID(document(0).links[0]);
  assert.equal(observations.get(id).bytesPerSecond, 0);

  observations = observeLinkActivity(observations, document(300), 2000);
  assert.equal(observations.get(id).bytesPerSecond, 300);
  assert.ok(observations.get(id).level > 0);

  const connectedButIdle = observeLinkActivity(
    observations, document(300, true), 9000,
  );
  assert.equal(connectedButIdle.get(id).bytesPerSecond, 0);
  assert.equal(connectedButIdle.get(id).level, 0);
});

test("more measured traffic produces denser, faster beads", () => {
  const cool = beadAppearance(activityLevel(100), 1000);
  const hot = beadAppearance(activityLevel(10000), 1000);
  assert.ok(hot.gap < cool.gap);
  assert.ok(hot.period < cool.period);
  assert.ok(hot.width > cool.width);
  assert.equal(beadAppearance(0), null);
});
