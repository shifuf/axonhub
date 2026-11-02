export type CompatibleQuotaBalance = {
  amount: number;
  currency: string;
  grantedAmount?: number;
  toppedUpAmount?: number;
};

export type CompatibleQuotaDisplay = {
  source?: string;
  currency: string;
  available?: number;
  total?: number;
  used?: number;
  usedRatio?: number;
  todayCost?: number;
  todayRequests?: number;
  unlimited: boolean;
  expiresAt?: string;
  balances: CompatibleQuotaBalance[];
};

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined;
}

function readNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function firstNumber(record: Record<string, unknown>, ...keys: string[]): number | undefined {
  for (const key of keys) {
    const value = readNumber(record[key]);
    if (value !== undefined) return value;
  }
  return undefined;
}

function readString(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() !== '' ? value.trim() : undefined;
}

function parseBalances(value: unknown): CompatibleQuotaBalance[] {
  if (!Array.isArray(value)) return [];

  return value.flatMap((entry) => {
    const row = asRecord(entry);
    if (!row) return [];
    const amount = readNumber(row.amount);
    if (amount === undefined) return [];

    return [
      {
        amount,
        currency: readString(row.currency) ?? 'CNY',
        grantedAmount: readNumber(row.granted_amount),
        toppedUpAmount: readNumber(row.topped_up_amount),
      },
    ];
  });
}

/**
 * Reads both live TestQuota payloads (`raw_data` + `limits`) and persisted
 * channel quota data (flattened raw fields + `_limits`).
 */
export function parseCompatibleQuotaDisplay(value: unknown): CompatibleQuotaDisplay | null {
  const root = asRecord(value);
  if (!root) return null;

  const raw = asRecord(root.raw_data) ?? root;
  const balances = parseBalances(raw.balances);
  const source = readString(raw.source);
  const unlimited = raw.unlimited_quota === true;
  const currency = readString(raw.currency) ?? balances[0]?.currency ?? 'USD';

  let available = firstNumber(raw, 'total_available_usd', 'balance_usd', 'remaining');
  let total = readNumber(raw.total_granted_usd);
  let used = readNumber(raw.total_used_usd);

  if (available === undefined && balances.length > 0) {
    available = balances[0].amount;
  }
  if (used === undefined && total !== undefined && available !== undefined) {
    used = Math.max(0, total - available);
  }
  if (available === undefined && total !== undefined && used !== undefined) {
    available = Math.max(0, total - used);
  }

  const rawLimits = root._limits ?? root.limits;
  const limits = Array.isArray(rawLimits) ? rawLimits.flatMap((entry) => (asRecord(entry) ? [asRecord(entry)!] : [])) : [];
  const creditsLimit = limits.find((limit) => readString(limit.window) === 'credits');
  let usedRatio = readNumber(creditsLimit?.usageRatio);
  if (usedRatio === undefined && total !== undefined && total > 0 && used !== undefined) {
    usedRatio = used / total;
  }
  if (usedRatio === undefined && total !== undefined && total > 0 && available !== undefined) {
    usedRatio = 1 - available / total;
  }
  if (usedRatio !== undefined) {
    usedRatio = Math.min(1, Math.max(0, usedRatio));
  }

  const todayCost = readNumber(raw.today_cost_usd);
  const todayRequests = readNumber(raw.today_requests);
  const expiresAt = readString(raw.expires_at);
  const hasData =
    source !== undefined ||
    unlimited ||
    balances.length > 0 ||
    available !== undefined ||
    total !== undefined ||
    used !== undefined ||
    todayCost !== undefined ||
    todayRequests !== undefined;

  if (!hasData) return null;

  return {
    source,
    currency,
    available,
    total,
    used,
    usedRatio,
    todayCost,
    todayRequests,
    unlimited,
    expiresAt,
    balances,
  };
}
