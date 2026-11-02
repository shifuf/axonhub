import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/input';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Channel } from '../data/schema';
import { useSetChannelManualBalance } from '../data/channels';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentRow: Channel;
}

export function ChannelsBalanceDialog({ open, onOpenChange, currentRow }: Props) {
  const { t } = useTranslation();
  const setBalance = useSetChannelManualBalance();
  const [value, setValue] = useState('0');

  useEffect(() => {
    if (open) setValue('0');
  }, [open]);

  const handleConfirm = async () => {
    const balanceUSD = Number(value);
    if (!Number.isFinite(balanceUSD) || balanceUSD < 0) return;
    await setBalance.mutateAsync({ channelID: currentRow.id, balanceUSD });
    onOpenChange(false);
  };

  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('channels.dialogs.manualBalance.title')}
      desc={t('channels.dialogs.manualBalance.description', { name: currentRow.name })}
      confirmText={t('common.buttons.save')}
      cancelBtnText={t('common.buttons.cancel')}
      handleConfirm={handleConfirm}
      disabled={setBalance.isPending || !Number.isFinite(Number(value)) || Number(value) < 0}
      isLoading={setBalance.isPending}
    >
      <div className='space-y-2'>
        <label className='text-sm font-medium' htmlFor='manual-balance-usd'>
          {t('channels.dialogs.manualBalance.amount')}
        </label>
        <Input id='manual-balance-usd' type='number' min='0' step='0.01' value={value} onChange={(event) => setValue(event.target.value)} />
      </div>
    </ConfirmDialog>
  );
}
