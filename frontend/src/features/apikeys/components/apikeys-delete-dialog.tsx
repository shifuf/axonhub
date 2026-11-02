import { IconAlertTriangle } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { useApiKeysContext } from '../context/apikeys-context';
import { useDeleteApiKey } from '../data/apikeys';

export function ApiKeysDeleteDialog() {
  const { t } = useTranslation();
  const { isDialogOpen, closeDialog, selectedApiKey, resetRowSelection } = useApiKeysContext();
  const deleteApiKey = useDeleteApiKey();
  if (!selectedApiKey) return null;

  const handleDelete = async () => {
    await deleteApiKey.mutateAsync(selectedApiKey.id);
    closeDialog('delete');
    resetRowSelection();
  };

  return (
    <ConfirmDialog
      open={isDialogOpen.delete}
      onOpenChange={() => closeDialog('delete')}
      handleConfirm={handleDelete}
      disabled={deleteApiKey.isPending}
      title={
        <span className='text-destructive'>
          <IconAlertTriangle className='stroke-destructive mr-1 inline-block' size={18} />
          {t('apikeys.dialogs.delete.title')}
        </span>
      }
      desc={t('apikeys.dialogs.delete.description', { name: selectedApiKey.name })}
      confirmText={t('common.buttons.delete')}
      cancelBtnText={t('common.buttons.cancel')}
    />
  );
}
