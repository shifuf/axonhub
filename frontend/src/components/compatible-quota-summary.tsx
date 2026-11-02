import { format } from 'date-fns';
import { useTranslation } from 'react-i18next';
import { parseCompatibleQuotaDisplay } from '@/features/system/data/compatible-quota-display';

const SOURCE_LABEL_KEYS: Record<string, string> = {
  new_api: 'quota.label.compatible_source.new_api',
  sub2api: 'quota.label.compatible_source.sub2api',
  openai_billing: 'quota.label.compatible_source.openai_billing',
  deepseek: 'quota.label.compatible_source.deepseek',
};

export function CompatibleQuotaSummary({ value, testId }: { value: unknown; testId?: string }) {
  const { t, i18n } = useTranslation();
  const summary = parseCompatibleQuotaDisplay(value);
  if (!summary) return null;

  const formatCurrency = (amount: number, currency = summary.currency) => {
    try {
      return t('currencies.format', {
        val: amount,
        currency,
        locale: i18n.language.startsWith('zh') ? 'zh-CN' : 'en-US',
        minimumFractionDigits: 2,
        maximumFractionDigits: 6,
      });
    } catch {
      return `${currency} ${amount.toFixed(2)}`;
    }
  };

  const sourceLabel = summary.source
    ? SOURCE_LABEL_KEYS[summary.source]
      ? t(SOURCE_LABEL_KEYS[summary.source])
      : summary.source
    : undefined;
  const remainingPercent = summary.usedRatio === undefined ? undefined : Math.round((1 - summary.usedRatio) * 100);
  const resetAt =
    summary.expiresAt && !Number.isNaN(Date.parse(summary.expiresAt)) ? format(new Date(summary.expiresAt), 'yyyy-MM-dd HH:mm') : undefined;

  const rows: Array<{ key: string; label: string; value: string }> = [];
  if (sourceLabel) rows.push({ key: 'source', label: t('quota.label.compatible_source'), value: sourceLabel });
  if (summary.unlimited) rows.push({ key: 'unlimited', label: t('quota.label.available_balance'), value: t('quota.label.unlimited') });

  if (summary.balances.length > 0) {
    summary.balances.forEach((balance, index) => {
      rows.push({
        key: `balance-${index}`,
        label: `${t('quota.label.available_balance')} · ${balance.currency}`,
        value: formatCurrency(balance.amount, balance.currency),
      });
      if (balance.grantedAmount !== undefined) {
        rows.push({
          key: `granted-${index}`,
          label: `${t('quota.label.granted_balance')} · ${balance.currency}`,
          value: formatCurrency(balance.grantedAmount, balance.currency),
        });
      }
      if (balance.toppedUpAmount !== undefined) {
        rows.push({
          key: `topped-up-${index}`,
          label: `${t('quota.label.topped_up_balance')} · ${balance.currency}`,
          value: formatCurrency(balance.toppedUpAmount, balance.currency),
        });
      }
    });
  } else if (!summary.unlimited && summary.available !== undefined) {
    rows.push({
      key: 'available',
      label: t('quota.label.available_balance'),
      value: formatCurrency(summary.available),
    });
  }

  if (summary.total !== undefined) {
    rows.push({ key: 'total', label: t('quota.label.total_quota'), value: formatCurrency(summary.total) });
  }
  if (summary.used !== undefined) {
    rows.push({ key: 'used', label: t('quota.label.total_used'), value: formatCurrency(summary.used) });
  }
  if (remainingPercent !== undefined) {
    rows.push({ key: 'remaining-percent', label: t('quota.label.remaining_percent'), value: `${remainingPercent}%` });
  }
  if (summary.todayCost !== undefined) {
    rows.push({ key: 'today-cost', label: t('quota.label.today_cost'), value: formatCurrency(summary.todayCost) });
  }
  if (summary.todayRequests !== undefined) {
    rows.push({ key: 'today-requests', label: t('quota.label.today_requests'), value: String(summary.todayRequests) });
  }
  if (resetAt) {
    rows.push({ key: 'expires-at', label: t('quota.label.expires_at'), value: resetAt });
  }

  return (
    <div className='space-y-2 text-xs' data-testid={testId}>
      {rows.map((row) => (
        <div key={row.key} className='flex items-center justify-between gap-3'>
          <span className='text-muted-foreground'>{row.label}</span>
          <span className='text-foreground text-right font-medium'>{row.value}</span>
        </div>
      ))}
      {summary.usedRatio !== undefined && (
        <div className='bg-muted h-1.5 overflow-hidden rounded-full' aria-label={t('quota.label.remaining_percent')}>
          <div
            className={summary.usedRatio >= 0.8 ? 'h-full rounded-full bg-amber-500' : 'h-full rounded-full bg-emerald-500'}
            style={{ width: `${Math.round(summary.usedRatio * 100)}%` }}
          />
        </div>
      )}
    </div>
  );
}
