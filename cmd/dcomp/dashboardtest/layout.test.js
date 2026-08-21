import assert from "node:assert/strict";
import test from "node:test";

import { layout } from "../dashboard/js/layout.js";

function endpoint(name) {
  return { name, service: "example.v1.Stream" };
}

test("fan-out shares its producer trace without dropping a connection", () => {
  const document = {
    components: [
      { name: "source", inputs: [], outputs: [endpoint("events")] },
      { name: "first", inputs: [endpoint("events")], outputs: [] },
      { name: "second", inputs: [endpoint("events")], outputs: [] },
    ],
    links: ["first", "second"].map(component => ({
      input: { component, endpoint: "events" },
      output: { component: "source", endpoint: "events" },
    })),
  };

  const plan = layout(document);

  assert.equal(plan.routes.length, document.links.length);
  assert.equal(plan.portRoutes.get("out:source:events").length, 2);
});

test("spaced interface slots remain on the router half-tile grid", () => {
  const document = {
    components: [{
      name: "hub",
      inputs: [endpoint("one"), endpoint("two")],
      outputs: [endpoint("alpha"), endpoint("beta"), endpoint("gamma")],
      published_ports: [{ protocol: "tcp" }],
    }],
    links: [],
  };

  const node = layout(document).byName.get("hub");
  const ports = [...node.inputs.values(), ...node.outputs.values(),
    node.publishPort];

  for (const port of ports) {
    assert.equal(Number.isInteger(port.x * 2), true);
    assert.equal(Number.isInteger(port.y * 2), true);
  }
});

test("component names reserve enough top-edge width", () => {
  const document = {
    components: [
      { name: "a", inputs: [], outputs: [] },
      { name: "a-very-long-component-name", inputs: [], outputs: [] },
    ],
    links: [],
  };

  const plan = layout(document);
  assert.ok(
    plan.byName.get("a-very-long-component-name").w >
      plan.byName.get("a").w,
  );
});
