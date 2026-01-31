/**
 * Kiro quota display helpers.
 */

type KiroUsageInput = {
  current?: number;
  limit?: number;
  percentage?: number;
};

export type KiroRemainingDisplay = {
  remainingPercent: number | null;
  remainingAmount: number | null;
};

export function computeKiroRemaining(input: KiroUsageInput): KiroRemainingDisplay {
  const current = typeof input.current === 'number' && Number.isFinite(input.current)
    ? input.current
    : null;
  const limit = typeof input.limit === 'number' && Number.isFinite(input.limit)
    ? input.limit
    : null;
  const percentage = typeof input.percentage === 'number' && Number.isFinite(input.percentage)
    ? input.percentage
    : null;

  const usedPercent = percentage !== null
    ? percentage
    : (current !== null && limit !== null && limit > 0)
      ? (current / limit) * 100
      : null;

  const clampedUsed = usedPercent === null ? null : Math.max(0, Math.min(100, usedPercent));
  const remainingPercent = clampedUsed === null ? null : Math.max(0, 100 - clampedUsed);
  const remainingAmount = current !== null && limit !== null
    ? Math.max(0, limit - current)
    : null;

  return { remainingPercent, remainingAmount };
}
