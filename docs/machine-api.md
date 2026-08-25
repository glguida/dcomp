# Machine API 2

DComp 0.2 emits newline-terminated JSON documents for commands that explicitly
accept `--json`. Every document contains `"api_version": 2`, except that the
version document places the same field alongside the semantic version.

Consumers must reject an unsupported `api_version`. Field names, types, and
meanings documented here are stable for API 2. JSON object member order is not
semantic. Optional fields are identified below; all other fields are present,
including empty strings, zero values, and empty arrays.

Diagnostics and lifecycle progress are written to standard error, never mixed
into the JSON line on standard output.

## Common exit behavior

- Exit `0` means the command completed successfully.
- Exit `1` means an operational error occurred.
- Exit `2` means command-line usage was invalid.
- Exit `130` means the command was cancelled by `SIGINT` or `SIGTERM`.

`status --json` is special: it emits a valid status document and exits `0` when
`operational` is true or `1` when `operational` is false. Callers must accept
both codes and then validate that the code agrees with the field.

On errors other than a successfully observed non-operational status, callers
must not expect a JSON document.

## Version document

```text
dcomp version --json
```

```json
{"version":"0.2.1","api_version":2}
```

- `version` is the DComp semantic version string.
- `api_version` is the integer machine-contract version.

The version command does not require Docker or lifecycle state and is the
compatibility probe clients should run first.

## Status document

```text
dcomp [GLOBAL_OPTIONS] status --json NAME
```

Representative operational response:

```json
{
  "api_version": 2,
  "name": "demo",
  "desired": true,
  "operational": true,
  "digest": "sha256:...",
  "operation": "",
  "phase": "",
  "proxy": {
    "instance_id": "...",
    "digest": "sha256:...",
    "pid": 1234,
    "ready": true,
    "inputs": 2,
    "outputs": 2,
    "active_connections": 2,
    "links": [
      {
        "input_component": "worker",
        "input_endpoint": "requests",
        "output_component": "source",
        "output_endpoint": "requests",
        "active_connections": 1,
        "bytes_input_to_output": 2048,
        "bytes_output_to_input": 8192
      }
    ],
    "problem": ""
  },
  "networks": [],
  "components": [
    {
      "name": "worker",
      "container_id": "...",
      "status": "running",
      "health": "healthy",
      "exit_code": 0,
      "problem": "",
      "published_ports": []
    }
  ]
}
```

Top-level fields:

- `name` is the requested system name.
- `desired` is true only when a committed desired deployment exists and no
  operation is being reported.
- `operational` is true only when the deployment is committed, no operation is
  pending, the proxy is ready without a problem, every network verifies, and
  every component is running and healthy without a problem.
- `digest` is the target digest during an operation, the committed system
  digest otherwise, or an empty string for an absent system.
- `operation` is empty or one of `apply`, `down`, or `restart`.
- `phase` is empty or one of `retire`, `networks`, `resync`, `create`, `attach`,
  `start`, `commit`, `restart`, `down`, or `abort`.
- `proxy` is always an object. Its identity strings and PID are empty/zero when
  no proxy is recorded. `proxy.digest` is the wiring digest observed from the
  live proxy, not part of process identity; it is omitted while resync is not
  converged. `active_connections` counts active proxy stream pairs, not idle
  producer connections. When available, `links` contains one item per full
  link identity with its active-pair gauge and cumulative directional bytes.
  Byte counters reset when the proxy process is replaced or a removed link is
  recreated.
- `networks` contains the target or committed component egress networks.
- `components` contains the target or committed component records, sorted by
  component name.

Each network contains:

- `key`: the DComp topology key, currently `component/INSTANCE`;
- `id`: the immutable Docker network ID when recorded;
- `internal`: the observed DComp network policy; 0.2 egress networks are
  non-internal; and
- `problem`: an empty string or an identity/policy/inspection diagnostic.

Each component contains:

- `name`: system-local component name;
- `container_id`: immutable Docker ID, or empty when not created;
- `status`: Docker status such as `created`, `running`, or `exited`, plus
  DComp observation states `missing` and `unknown`;
- `health`: `none`, `starting`, `healthy`, `unhealthy`, or empty when no live
  container was observed;
- `exit_code`: observed container exit code;
- `problem`: empty when identity and policy verify, otherwise a diagnostic;
  and
- `published_ports`: effective Docker host bindings, including dynamically
  allocated host ports.

A published-port object has `protocol` (`tcp` or `udp`), `host_ip`,
`host_port`, and `container_port`.

During an apply, two optional arrays may be present:

```json
{
  "retiring_networks": [],
  "retiring_components": []
}
```

They are omitted when empty and otherwise contain previous-generation objects
with the same schemas as `networks` and `components`. A missing retiring
container may still be included when its Docker network endpoint awaits
cleanup.

An absent system is a successful observation with a non-operational result:

```json
{
  "api_version": 2,
  "name": "absent",
  "desired": false,
  "operational": false,
  "digest": "",
  "operation": "",
  "phase": "",
  "proxy": {
    "instance_id": "",
    "pid": 0,
    "ready": false,
    "inputs": 0,
    "outputs": 0,
    "active_connections": 0,
    "problem": ""
  },
  "networks": [],
  "components": []
}
```

The command exits 1 for this document because `operational` is false.

## View document

```text
dcomp [GLOBAL_OPTIONS] view --json FILE|NAME
```

A view document describes declared topology and, for a recorded system, joins
it with current observations. Representative state response:

```json
{
  "api_version": 2,
  "source": "state",
  "name": "demo",
  "digest": "sha256:...",
  "desired": true,
  "operational": true,
  "components": [
    {
      "name": "worker",
      "image_ref": "example/worker:1",
      "image_id": "sha256:...",
      "inputs": [{"name":"requests","service":"example.v1.Requests"}],
      "outputs": [{"name":"responses","service":"example.v1.Requests"}],
      "egress": false,
      "binds": [],
      "volumes": [],
      "args": [],
      "published_ports": [],
      "status": {
        "name": "worker",
        "container_id": "...",
        "status": "running",
        "health": "healthy",
        "exit_code": 0,
        "problem": "",
        "published_ports": []
      }
    },
    {
      "name": "source",
      "image_ref": "example/source:1",
      "image_id": "sha256:...",
      "inputs": [],
      "outputs": [{"name":"requests","service":"example.v1.Requests"}],
      "egress": false,
      "binds": [],
      "volumes": [],
      "args": [],
      "published_ports": [],
      "status": {
        "name": "source",
        "container_id": "...",
        "status": "running",
        "health": "healthy",
        "exit_code": 0,
        "problem": "",
        "published_ports": []
      }
    }
  ],
  "links": [
    {
      "service": "example.v1.Requests",
      "input": {"component":"worker","endpoint":"requests"},
      "output": {"component":"source","endpoint":"requests"},
      "active": true,
      "active_connections": 1,
      "activity": {
        "bytes_input_to_output": 2048,
        "bytes_output_to_input": 8192
      }
    }
  ],
  "proxy": {
    "ready": true,
    "inputs": 1,
    "outputs": 1,
    "active_connections": 1,
    "problem": ""
  }
}
```

- `source` is `file` for a parsed description file or `state` for a named
  system observed from durable state.
- `name`, `desired`, `operational`, and the optional `digest`, `operation`, and
  `phase` have the same lifecycle meanings as in a status document. File views
  are neither desired nor operational.
- `components` is sorted by component name. Every item contains its authored
  image reference, sorted endpoint declarations, egress policy, binds,
  volumes, arguments, and declared port publications. `image_id` and `status`
  are present only in state views with a resolved component and observation.
- `links` is sorted by input reference. A live proxy observation adds
  `active`, `active_connections`, and `activity`. `active` is connection state;
  `activity` contains cumulative successfully forwarded bytes in each
  direction. These observations are absent for file views or unavailable
  proxy metrics; absence means unknown, not zero.
- `networks` may be present for a recorded topology with egress networks, and
  `proxy` is present for a recorded topology. Network and component status
  objects use the status-document schemas. The proxy object reports readiness,
  endpoint counts, system-wide active stream pairs, and a diagnostic problem;
  link metrics live on the links themselves, and the proxy object intentionally
  omits process identity.

`digest`, `operation`, `phase`, `networks`, and `proxy` are optional. Within a
component, `image_id` and `status` are optional. Within a link, `active`,
`active_connections`, and `activity` are optional as one observation group.
Arrays that are part of a component or the top-level topology are otherwise
present even when empty.

An absent named system emits a successful `source=state` document with empty
component and link arrays, `desired=false`, and `operational=false`. Unlike
`status --json`, `view --json` exits 0 after that observation.

The complete topology semantics and human viewer behavior are documented in
[System view](view.md).

## Dashboard HTTP documents

```text
dcomp [GLOBAL_OPTIONS] dashboard [--listen ADDRESS] [FILE|NAME...]
```

The dashboard serves two read-only JSON endpoints:

```text
GET /api/v2/systems
GET /api/v2/view/NAME
```

The systems response is exactly:

```json
{"api_version":2,"systems":["demo"]}
```

`systems` is sorted and contains each served system name once. The view
response is exactly the API 2 view document described above. Successful JSON
responses use `Content-Type: application/json` and `Cache-Control: no-store`.
Unknown or unselected names return 404, unsupported methods return 405, and
observation failures return 500 with diagnostic text rather than JSON.

## Processes document

```text
dcomp [GLOBAL_OPTIONS] ps [-a|--all] --json [NAME]
```

```json
{
  "api_version": 2,
  "components": [
    {
      "system": "demo",
      "component": "worker",
      "container_id": "...",
      "status": "running",
      "health": "healthy",
      "exit_code": 0,
      "problem": "",
      "operation": "",
      "phase": "",
      "published_ports": []
    }
  ]
}
```

The component status and published-port fields have the same meanings as in a
status document. `operation` and `phase` describe the containing system.
Without `--all`, only records whose observed status is `running` are included.
Supplying `NAME` limits the response to that system. An empty result is
`"components": []` and exits 0.

## Volume document

```text
dcomp [GLOBAL_OPTIONS] volume --json SYSTEM COMPONENT LOGICAL
```

```json
{
  "api_version": 2,
  "system": "demo",
  "component": "worker",
  "logical_name": "state",
  "name": "dcomp.0123456789abcdef0123456789abcdef.demo.volume.worker.state"
}
```

The first three identity fields echo the requested coordinates. `name` is the
verified Docker volume name; the 32 hexadecimal characters after `dcomp.` are
the namespace derived from the selected state root. DComp emits the document
only after the existing volume's driver and complete ownership labels match
those coordinates and that namespace.

## Attachment readiness protocol

`attach` has no JSON mode, but `--ready-fd` is a machine-facing protocol:

```text
dcomp [GLOBAL_OPTIONS] attach --ready-fd FD SYSTEM COMPONENT
```

The caller creates a pipe or equivalent inherited descriptor numbered 3 or
higher and passes its write end as `FD`. After DComp has verified the system,
proxy, and running container and Docker has accepted the standard-I/O
attachment, DComp writes exactly one byte `0x01` and closes `FD`.

EOF before that byte means attachment setup failed; the caller should collect
the process exit status and standard error. After readiness, the DComp process
remains the bidirectional standard-I/O relay until detachment, cancellation, or
container exit.

## Compatibility guidance

A machine client should:

1. run `dcomp version --json` without lifecycle flags;
2. require a supported `api_version` and semantic-version range;
3. decode the complete required shape for that API version;
4. accept the documented optional retiring arrays;
5. treat `problem` strings as diagnostics for humans, not stable identifiers;
   and
6. use immutable IDs and structured fields rather than parsing text output.

Text output, progress messages, and error prose are not part of Machine API 2.
