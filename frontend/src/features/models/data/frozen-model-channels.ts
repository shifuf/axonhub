import { z } from 'zod';
import { useQuery } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { usePermissions } from '@/hooks/usePermissions';
import { modelCooldownSchema } from '@/features/channels/data/schema';

// Keep this selection and parser shared with the single-model detail query.
export const FROZEN_MODEL_CHANNEL_FIELDS = `
  channel { id name type status }
  cooldowns { model reason createdAt expiresAt }
`;
export const frozenModelChannelsSchema = z.array(
  z.object({
    channel: z.object({ id: z.string(), name: z.string(), type: z.string(), status: z.string() }),
    cooldowns: z.array(modelCooldownSchema),
  }),
);
export type FrozenModelChannel = z.infer<typeof frozenModelChannelsSchema>[number];
export type FrozenModelChannelsByModel = Record<string, FrozenModelChannel[]>;

const BATCH_SIZE = 20;

// The model table renders all uncollapsed groups (it has no pagination).
// Exclude hidden groups before batching rather than starting a query per cell.
export function getVisibleFrozenModelIDs(
  groups: ReadonlyMap<string, ReadonlyArray<{ original: { modelID: string } }>>,
  collapsedGroups: ReadonlySet<string>,
): string[] {
  const ids = [...groups].flatMap(([developer, rows]) =>
    collapsedGroups.has(developer) ? [] : rows.map((row) => row.original.modelID),
  );
  return [...new Set(ids.filter(Boolean))].sort();
}

export async function fetchFrozenModelChannelsBatch(modelIDs: string[], signal?: AbortSignal): Promise<FrozenModelChannelsByModel> {
  const ids = [...new Set(modelIDs.filter(Boolean))].sort();
  const result: FrozenModelChannelsByModel = Object.fromEntries(ids.map((id) => [id, []]));
  // Bound both document size and concurrency. Do not approximate associations
  // from channel.supportedModels: this resolver also understands aliases/rules.
  for (let offset = 0; offset < ids.length; offset += BATCH_SIZE) {
    signal?.throwIfAborted();
    const batch = ids.slice(offset, offset + BATCH_SIZE);
    const variables = Object.fromEntries(batch.map((id, i) => [`model${i}`, id]));
    const parameters = batch.map((_, i) => `$model${i}: String!`).join(', ');
    const fields = batch.map((_, i) => `m${i}: queryFrozenModelChannels(modelID: $model${i}) { ${FROZEN_MODEL_CHANNEL_FIELDS} }`).join('\n');
    // Admin-wide query: no selected project's X-Project-ID header.
    const data = await graphqlRequest<Record<string, unknown>>(
      `query QueryFrozenModelChannelsBatch(${parameters}) { ${fields} }`, variables, undefined, { signal },
    );
    batch.forEach((id, i) => {
      result[id] = frozenModelChannelsSchema.parse(data[`m${i}`]);
    });
  }
  return result;
}

export function useFrozenModelChannelsBatch(modelIDs: string[]) {
  const { hasSystemScope, user } = usePermissions();
  const canRead = hasSystemScope('read_channels');
  const ids = [...new Set(modelIDs.filter(Boolean))].sort();
  const query = useQuery({
    queryKey: ['frozenModelChannels', 'list', user?.id, ids],
    queryFn: ({ signal }) => fetchFrozenModelChannelsBatch(ids, signal),
    enabled: canRead && ids.length > 0,
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
  });
  // Disabled queries may still expose cached data from an earlier permission set.
  return { ...query, data: canRead ? query.data : undefined };
}
