/* Turn cumulative proxy byte counters into a recent rate for presentation.
   The counters remain the source of truth; smoothing exists only in the
   viewer so the data plane never invents a visualization policy. */

const COOLING_MILLISECONDS = 1800;

export function linkID(link) {
  return link.input.component + "." + link.input.endpoint +
    "<-" + link.output.component + "." + link.output.endpoint;
}

export function activityLevel(bytesPerSecond) {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return 0;
  return Math.min(1, Math.log2(1 + bytesPerSecond / 32) / 8);
}

export function observeLinkActivity(previous, document, observedAt) {
  const next = new Map();
  for (const link of document.links || []) {
    if (!link.activity) continue;
    const id = linkID(link);
    const total = Number(link.activity.bytes_input_to_output || 0) +
      Number(link.activity.bytes_output_to_input || 0);
    const before = previous.get(id);
    let bytesPerSecond = 0;
    let level = 0;
    if (before && observedAt > before.observedAt && total >= before.total) {
      const elapsed = observedAt - before.observedAt;
      bytesPerSecond = (total - before.total) * 1000 / elapsed;
      const measured = activityLevel(bytesPerSecond);
      const cooled = before.level *
        Math.exp(-elapsed / COOLING_MILLISECONDS);
      level = Math.max(measured, cooled);
      if (level < 0.02) level = 0;
    }
    next.set(id, { total, observedAt, bytesPerSecond, level });
  }
  return next;
}

export function beadAppearance(level, now = Date.now()) {
  const amount = Math.max(0, Math.min(1, Number(level) || 0));
  if (amount === 0) return null;
  const gap = 24 - amount * 18;
  const period = 2.4 - amount * 1.95;
  return {
    gap,
    period,
    delay: -((now / 1000) % period),
    width: 2.5 + amount * 1.5,
  };
}
