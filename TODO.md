# TODO

## Type lifecycle journal progress

After the 0.2.1 reconciliation work, make the lifecycle journal distinguish
these concepts in code:

- durable operation intent and irreversible branch decisions;
- identity-bound, monotonic external progress; and
- volatile observations that must be inspected again.

Replace direct, string-keyed `Operation.Completed` access and ambiguous boolean
interpretation with phase-specific helpers or types. A decision such as abort
replacement fallback must select a branch without implying that its proxy and
container cleanup has completed. Observations such as container running state
must not become durable completion markers.

Preserve the existing safety machinery: ordered apply phases, exact resource
identities, pending-create write-ahead records, endpoint-cleanup records,
identity-mismatch refusal, and the retire-before-socket-publication invariant.
Do not turn this cleanup into health supervision or remove checkpoints needed
to recover calls whose external effect may have landed without a response.

Add phase-boundary fault-injection tests before changing the representation.
Treat any durable schema change as a separate design decision; this cleanup
does not by itself require a state-format change.
