import { useEffect, useState } from 'react';
import { IconSnowflake, IconSnowflakeOff } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { ScrollArea } from '@/components/ui/scroll-area';
import { useChannels } from '../context/channels-context';
import { useChannelModelCooldowns, useClearChannelModelCooldown } from '../data/channels';
import { CooldownDetails } from './cooldown-details';

interface ChannelsModelCooldownsDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function ChannelsModelCooldownsDialog({ open, onOpenChange }: ChannelsModelCooldownsDialogProps) {
  const { t } = useTranslation();
  const { currentRow, setOpen } = useChannels();
  const [confirmModel, setConfirmModel] = useState<string | null>(null);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!open) return;

    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, [open]);

  const channelId = currentRow?.id || '';
  const { data: cooldowns = [], isLoading } = useChannelModelCooldowns(channelId, {
    enabled: open && !!channelId,
  });

  const clearCooldown = useClearChannelModelCooldown();

  const handleClose = () => {
    setOpen(null);
    onOpenChange(false);
    setConfirmModel(null);
  };

  const handleClear = async (model: string) => {
    if (!currentRow) return;
    try {
      if (!(await clearCooldown.mutateAsync({ channelID: currentRow.id, model }))) return;
      setConfirmModel(null);
      toast.success(t('channels.dialogs.modelCooldowns.messages.cleared'));
    } catch {
      // Error is handled by the hook.
    }
  };

  return (
    <Dialog open={open} onOpenChange={handleClose}>
      <DialogContent className='sm:max-w-lg'>
        <DialogHeader className='text-left'>
          <DialogTitle className='flex items-center gap-2'>
            <IconSnowflake className='h-5 w-5' />
            {t('channels.dialogs.modelCooldowns.title')}
          </DialogTitle>
          <DialogDescription className='text-muted-foreground text-sm'>
            {t('channels.dialogs.modelCooldowns.description', { channel: currentRow?.name ?? '' })}
          </DialogDescription>
        </DialogHeader>

        <ScrollArea className='max-h-80'>
          <div className='space-y-2 pr-3'>
            {isLoading ? (
              <p className='text-muted-foreground py-6 text-center text-sm'>{t('common.loading', 'Loading')}...</p>
            ) : cooldowns.length === 0 ? (
              <p className='text-muted-foreground py-6 text-center text-sm'>
                {t('channels.dialogs.modelCooldowns.empty')}
              </p>
            ) : (
              cooldowns.map((cooldown) => (
                <div key={cooldown.model} className='rounded-lg border p-3' data-testid='channel-model-cooldown'>
                  <div className='flex items-start justify-between gap-3'>
                    <div className='min-w-0 space-y-1'>
                      <div className='flex flex-wrap items-center gap-1.5'>
                        <span className='truncate text-sm font-medium'>{cooldown.model}</span>
                      </div>
                      <CooldownDetails {...cooldown} now={now} />
                    </div>
                    <Button
                      size='sm'
                      variant='outline'
                      disabled={clearCooldown.isPending}
                      onClick={() => setConfirmModel(cooldown.model)}
                    >
                      <IconSnowflakeOff size={14} className='mr-1' />
                      {t('channels.dialogs.modelCooldowns.unfreeze')}
                    </Button>
                  </div>
                </div>
              ))
            )}
          </div>
        </ScrollArea>

        <DialogFooter>
          <Button variant='outline' onClick={handleClose}>
            {t('common.buttons.close', 'Close')}
          </Button>
        </DialogFooter>

        {confirmModel && (
          <Dialog open onOpenChange={(isOpen) => !isOpen && setConfirmModel(null)}>
            <DialogContent className='sm:max-w-sm'>
              <DialogHeader className='text-left'>
                <DialogTitle>{t('channels.dialogs.modelCooldowns.confirmTitle')}</DialogTitle>
                <DialogDescription className='text-muted-foreground text-sm'>
                  {t('channels.dialogs.modelCooldowns.confirmDescription', { model: confirmModel })}
                </DialogDescription>
              </DialogHeader>
              <DialogFooter>
                <Button variant='outline' onClick={() => setConfirmModel(null)}>
                  {t('common.buttons.cancel')}
                </Button>
                <Button onClick={() => handleClear(confirmModel)}>{t('channels.dialogs.modelCooldowns.unfreeze')}</Button>
              </DialogFooter>
            </DialogContent>
          </Dialog>
        )}
      </DialogContent>
    </Dialog>
  );
}
