import { useTranslation } from 'react-i18next';
import { IconSnowflake } from '@tabler/icons-react';
import { Badge } from '@/components/ui/badge';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import type { ModelCooldown } from '../data/schema';
import { CooldownDetails, isActiveCooldown } from './cooldown-details';

// Both lists use their existing shared timer; badges never fetch per-row data.
export function ModelFreezeBadges({
  cooldowns,
  channelName,
  now,
}: {
  cooldowns: ModelCooldown[];
  channelName?: string;
  now: number;
}) {
  const { t } = useTranslation();
  const active = cooldowns.filter((cooldown) => isActiveCooldown(cooldown, now));
  if (active.length === 0) return null;

  return (
    <div className='flex max-w-full flex-wrap gap-1' data-testid='model-freeze-badges'>
      {active.map((cooldown) => (
        <Tooltip key={cooldown.model}>
          <TooltipTrigger asChild>
            <Badge variant='outline' tabIndex={0} className='max-w-full gap-1 whitespace-normal text-cyan-700 dark:text-cyan-400'>
              <IconSnowflake className='h-3 w-3 shrink-0' aria-hidden='true' />
              <span className='min-w-0 break-words'>
                {channelName !== undefined
                  ? t('models.cooldown.frozenOnChannel', { channel: channelName })
                  : t('channels.cooldown.modelFrozen', { model: cooldown.model })}
              </span>
            </Badge>
          </TooltipTrigger>
          <TooltipContent className='max-w-sm'>
            <p className='break-all font-mono text-xs'>{cooldown.model}</p>
            <CooldownDetails {...cooldown} now={now} />
          </TooltipContent>
        </Tooltip>
      ))}
    </div>
  );
}
