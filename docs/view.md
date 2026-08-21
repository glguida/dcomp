# System View

DComp exposes one machine-readable description of a system — the view
document — and ships one consumer of it, the dash viewer. The document is the
stable contract; the viewer is replaceable. Anyone can build their own viewer
against the same two read-only HTTP endpoints or against `dcomp view --json`.

## The view document

A view document describes one system: its wiring topology and, when observed
from durable state, the live status of its resources.

```text
dcomp view --json FILE   # source=file  — parsed topology, no runtime facts
dcomp view --json NAME   # source=state — topology plus observed status
```

Top-level fields:

| Field | Meaning |
| --- | --- |
| `api_version` | Machine API version of this document. |
| `source` | `file` or `state`. |
| `name` | System name. |
| `digest` | Resolved system digest (`state` only). |
| `desired`, `operational` | Committed and healthy, per `dcomp status` semantics. |
| `operation`, `phase` | Present while a lifecycle operation is recorded. |
| `components` | Declared components, sorted by name. |
| `links` | Declared links, sorted by input reference. |
| `networks` | Observed network diagnostics (`state` only). |
| `proxy` | Observed proxy readiness, endpoint counts, active stream pairs, and any problem (`state` only). |

Each component carries its declared interface and bounded runtime policy —
`inputs` and `outputs` (local name plus nominal service), `egress`, `binds`,
`volumes`, `args`, `published_ports` — and, in `state` mode, a `status`
object with `container_id`, `status`, `health`, `exit_code`, and `problem`.
Each link carries its `input` and `output` endpoint references and the linked
`service`. A live proxy observation additionally carries `active`, the exact
`active_connections` gauge, and cumulative directional counters under
`activity.bytes_input_to_output` and `activity.bytes_output_to_input`.
`active` means at least one stream pair is currently forwarding; it says
nothing about whether bytes are moving. The counters measure successfully
forwarded opaque transport bytes and reset when the per-system proxy restarts.
These fields are absent in file views or when proxy metrics are unavailable;
absence means unknown, not idle. A view is an observation: reading it never
repairs, starts, or stops anything.

## The dash server

```text
dcomp dash [--listen ADDRESS] [FILE|NAME...]
```

`dash` binds `127.0.0.1:8199` by default and serves until interrupted:

```text
GET /                    the bundled viewer
GET /api/v2/systems      {"api_version":2,"systems":[...]}
GET /api/v2/view/NAME    one view document
```

Arguments restrict the served set. A system file supplies parsed topology
without runtime facts; a name selects a system from durable state. Observations
are cached for one second per system to bound Docker load under concurrent
viewers. The server is read-only and unauthenticated: it is a local observation
surface, and the default bind is deliberately loopback. Exposing it beyond the
host is the operator's decision and responsibility.

## The viewer

The bundled page draws the system as an isometric wireframe on the project's
graphic language: warm paper, ink line work, and the four module hues in fixed
rotation. The projection is the camera's; the drawn world stays orthogonal —
links are routed at right angles, and no arrowheads exist anywhere.

- A component is a wireframe prism carrying its flat hue square. Colour names
  the module, never its state. Inputs sit on the south face, outputs on the
  east face, and the tile grows to hold them.
- An open square is an input, a filled square an output; each link is one
  channel routed like a board trace — around tiles, at a distance from
  unrelated modules, jumping another trace with a squared hop where it must
  cross. A black channel is disconnected and a green channel has a live
  stream pair. The selected link is drawn in rosso.
- Beads mean measured traffic, not connectivity. The viewer derives a recent
  byte rate from successive cumulative counter observations; more traffic
  produces denser, faster beads, and an idle connected link remains solid
  green. The visual rate cools over a few seconds between observations.
- The dashed enclosure is the internal boundary. Each egress component has an
  untyped ink outbound port, a trace routed by the same rules to the nearest
  boundary, a solid segment on the rule, and a filled square outside it,
  labelled with its host bindings when ports are published.
- Component state is carried by line work and small-caps labels — dashed
  wireframes, faded ink, `STARTING`, `UNHEALTHY`, `EXITED n`, `MISSING` —
  never by recolouring modules. Link colour reports connection state only.

Click a component or link for its descriptor; drag to pan, scroll to zoom,
Escape to return to the system summary. The page polls the view API every
1.5 seconds and marks the whole stage `UNREACHABLE` when observation stops.
