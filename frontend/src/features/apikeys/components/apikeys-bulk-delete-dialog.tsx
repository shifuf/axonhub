import { IconAlertTriangle } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { useApiKeysContext } from '../context/apikeys-context';
import { useBulkDeleteApiKeys } from '../data/apikeys';

export function ApiKeysBulkDeleteDialog() {
  const { t } = useTranslation();
  const { isDialogOpen, closeDialog, selectedApiKeys, resetRowSelection, setSelectedApiKeys } = useApiKeysContext();
  const bulkDeleteApiKeys = useBulkDeleteApiKeys();
  if (!selectedApiKeys.length) return null;

  const handleDelete = async () => {
    await bulkDeleteApiKeys.mutateAsync(selectedApiKeys.map((apiKey) => apiKey.id));
    resetRowSelection();
    setSelectedApiKeys([]);
    closeDialog('bulkDelete');
  };

  return (
    <ConfirmDialog
      open={isDialogOpen.bulkDelete}
      onOpenChange={() => closeDialog('bulkDelete')}
      handleConfirm={handleDelete}
      disabled={bulkDeleteApiKeys.isPending}
      title={<span className='text-destructive'><IconAlertTriangle className='stroke-destructive mr-1 inline-block' size={18} />{t('apikeys.dialogs.bulkDelete.title')}</span>}
      desc={t('apikeys.dialogs.bulkDelete.description', { count: selectedApiKeys.length })}
      confirmText={t('common.buttons.delete')}
      cancelBtnText={t('common.buttons.cancel')}
    />
  );
}
