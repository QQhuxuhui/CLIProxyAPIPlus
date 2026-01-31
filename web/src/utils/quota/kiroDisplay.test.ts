import { describe, it, expect } from 'vitest';
import { computeKiroRemaining } from './kiroDisplay';

describe('computeKiroRemaining', () => {
  it('uses percentage to compute remaining percent and amount', () => {
    const result = computeKiroRemaining({ current: 80, limit: 100, percentage: 80 });

    expect(result.remainingPercent).toBe(20);
    expect(result.remainingAmount).toBe(20);
  });

  it('falls back to current/limit when percentage is missing', () => {
    const result = computeKiroRemaining({ current: 25, limit: 50 });

    expect(result.remainingPercent).toBe(50);
    expect(result.remainingAmount).toBe(25);
  });
});
