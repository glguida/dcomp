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
{"version":"0.2.0","api_version":2}
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
- `phase` is empty or one of `retire`, `networks`, `proxy`, `create`, `attach`,
  `start`, `commit`, `restart`, `down`, or `abort`.
- `proxy` is always an object. Its identity strings and PID are empty/zero when
  no proxy is recorded. `active_connections` counts active proxy stream pairs,
  not idle producer connections.
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
    "digest": "",
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
  "name": "dcomp.demo.volume.worker.state"
}
```

The first three identity fields echo the requested coordinates. `name` is the
verified Docker volume name. DComp emits the document only after the existing
volume's driver and complete ownership labels match those coordinates.

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
