import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

export function isActiveCooldown(cooldown: { expiresAt?: string | null }, now: number): boolean {
  return !cooldown.expiresAt || new Date(cooldown.expiresAt).getTime() > now;
}

export function useCooldownNow(enabled = true): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!enabled) return;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, [enabled]);
  return now;
}

// Shared by channel status, model cooldowns and model association/expanded rows.
// A missing expiry is permanent, not expired: only a manual recovery can clear it.
export function CooldownDetails({
  reason,
  expiresAt,
  now = Date.now(),
  showRemaining = true,
}: {
  reason?: string | null;
  expiresAt?: string | null;
  now?: number;
  showRemaining?: boolean;
}) {
  const { t, i18n } = useTranslation();
  const expiry = expiresAt ? new Date(expiresAt) : null;
  const validExpiry = expiry && Number.isFinite(expiry.getTime());
  const remainingMinutes = validExpiry ? Math.max(0, Math.ceil((expiry.getTime() - now) / 60_000)) : 0;

  return (
    <div className='text-muted-foreground space-y-1 text-xs' data-testid='cooldown-details'>
      {reason && <p className='break-words whitespace-pre-wrap'>{reason}</p>}
      {!expiresAt ? (
        <p>{t('channels.cooldown.permanent')}</p>
      ) : !validExpiry ? (
        <p>{t('channels.cooldown.unknownExpiry')}</p>
      ) : (
        <>
          {showRemaining && (
            <p>
              {remainingMinutes > 0
                ? t('channels.cooldown.remaining', { minutes: remainingMinutes })
                : t('channels.dialogs.modelCooldowns.expired')}
            </p>
          )}
          <p>
            {t('channels.cooldown.until')}{' '}
            <time dateTime={expiresAt}>
              {expiry.toLocaleString(i18n.language.startsWith('zh') ? 'zh-CN' : 'en-US')}
            </time>
          </p>
        </>
      )}
    </div>
  );
}
