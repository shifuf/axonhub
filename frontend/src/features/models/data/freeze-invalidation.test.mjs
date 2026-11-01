import assert from 'node:assert/strict';
import test from 'node:test';
import { QueryClient } from '@tanstack/react-query';
import { loadFunctions } from '../../../../tests/unit-loader.mjs';

const mutations = {
  'features/models/data/models.ts': ['useCreateModel', 'useBulkCreateModels', 'useUpdateModel', 'useDeleteModel', 'useUpdateModelStatus', 'useBulkDisableModels', 'useBulkEnableModels'],
  'features/channels/data/channels.ts': ['useUpdateChannel', 'useUpdateChannelStatus', 'useBulkArchiveChannels', 'useBulkDisableChannels', 'useBulkEnableChannels', 'useBulkRecoverChannels', 'useDeleteChannel', 'useBulkDeleteChannels', 'useBulkManageChannelTags', 'useFreezeChannelModel', 'useClearChannelModelCooldown'],
};

for (const [path, names] of Object.entries(mutations)) {
  for (const name of names) {
    test(`${name} invalidates both list and detail freeze caches after success`, () => {
      const client = new QueryClient();
      const listKey = ['frozenModelChannels', 'list', 'admin-1', ['alias-model']];
      const detailKey = ['frozenModelChannels', 'detail', 'admin-1', 'alias-model'];
      const unrelatedKey = ['quotaRoutingSettings'];
      client.setQueryData(listKey, { 'alias-model': [] });
      client.setQueryData(detailKey, []);
      client.setQueryData(unrelatedKey, { enabled: true });
      const bindings = {
        useQueryClient: () => client,
        useMutation: (options) => options,
        useTranslation: () => ({ t: (key) => key }),
        useErrorHandler: () => ({ handleError() {} }),
        toast: { success() {} },
      };
      const hook = loadFunctions(path, [name], bindings)[name]();
      hook.onSuccess(true, { length: 1, status: 'enabled', ids: ['7'], channelId: '7', channelID: '7' });
      assert.equal(client.getQueryState(listKey).isInvalidated, true);
      assert.equal(client.getQueryState(detailKey).isInvalidated, true);
      assert.equal(client.getQueryState(unrelatedKey).isInvalidated, false);
      client.clear();
    });
  }
}
