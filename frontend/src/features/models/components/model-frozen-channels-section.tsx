import { useTranslation } from 'react-i18next';
import { CooldownDetails, isActiveCooldown, useCooldownNow } from '@/features/channels/components/cooldown-details';
import { Badge } from '@/components/ui/badge';
import { useFrozenModelChannels } from '../data/models';

export function ModelFrozenChannelsSection({ modelID }: { modelID: string }) {
  const { t } = useTranslation();
  const { data = [], isLoading, isError } = useFrozenModelChannels(modelID);
  const now = useCooldownNow();
  const channels = data
    .map((entry) => ({ ...entry, cooldowns: entry.cooldowns.filter((cooldown) => isActiveCooldown(cooldown, now)) }))
    .filter((entry) => entry.cooldowns.length > 0);

  return (
    <section className='space-y-3' data-testid='model-frozen-channels'>
      <h4 className='text-sm font-semibold'>{t('models.expandedRow.frozenChannels')}</h4>
      {isLoading ? (
        <p className='text-muted-foreground text-sm'>{t('common.loading')}</p>
      ) : isError ? (
        <p className='text-destructive text-sm'>{t('common.errors.internalServerError')}</p>
      ) : channels.length === 0 ? (
        <p className='text-muted-foreground text-sm'>{t('channels.dialogs.modelCooldowns.empty')}</p>
      ) : (
        <div className='space-y-2'>
          {channels.map(({ channel, cooldowns }) => (
            <div key={channel.id} className='space-y-2 rounded-md border p-3'>
              <div className='flex flex-wrap items-center gap-2 text-sm'>
                <span className='font-medium'>{channel.name}</span>
                <Badge variant='outline'>{t(`channels.status.${channel.status}`)}</Badge>
              </div>
              {cooldowns.map((cooldown) => (
                <div key={cooldown.model} className='space-y-1'>
                  <p className='break-all font-mono text-xs'>{cooldown.model}</p>
                  <CooldownDetails {...cooldown} now={now} />
                </div>
              ))}
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
