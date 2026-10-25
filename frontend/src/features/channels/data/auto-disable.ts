import { apiKeyAutoDisableRuleFormSchema } from './schema';
import type {
  APIKeyAutoDisableMode,
  APIKeyAutoDisableRule,
  BulkAutoDisableAction,
  BulkUpdateChannelAutoDisableInput,
  CapabilityPolicy,
} from './schema';

export type { BulkAutoDisableAction, BulkUpdateChannelAutoDisableInput };

export type AutoDisablePoliciesInput = {
  apiKeyAutoDisableMode?: APIKeyAutoDisableMode | null;
  apiKeyAutoDisableRules?: APIKeyAutoDisableRule[] | null;
};

export type ApiKeyAutoDisableRuleFormValue = {
  statusCodes?: number[] | null;
  keywordPatterns?: string[] | null;
  times: number;
  action: APIKeyAutoDisableRule['action'];
  disableDurationMinutes?: number | null;
  disableUntilCron?: string | null;
  disableUntilTimezone?: string | null;
  windowMinutes?: number | null;
  freezeModel?: boolean | null;
};

export const RECOMMENDED_GLOBAL_AUTO_DISABLE_RULES: APIKeyAutoDisableRule[] = [
  { statusCodes: [401], times: 3, action: 'temporary_disable', disableDurationMinutes: 30, windowMinutes: 10, freezeModel: true },
  { statusCodes: [429], times: 3, action: 'temporary_disable', disableDurationMinutes: 30, windowMinutes: 10, freezeModel: true },
];

// Upgrade only legacy absent/zero timing. Keep other invalid numeric values
// visible so live form validation can reject them rather than silently clamp.
function canonicalAutoDisableFields(rule: ApiKeyAutoDisableRuleFormValue) {
  return {
    action: 'temporary_disable' as const,
    disableDurationMinutes: rule.disableDurationMinutes == null || rule.disableDurationMinutes === 0 ? 30 : rule.disableDurationMinutes,
    disableUntilCron: null,
    disableUntilTimezone: null,
    windowMinutes: rule.windowMinutes == null || rule.windowMinutes === 0 ? 10 : rule.windowMinutes,
    freezeModel: true,
  };
}

export function effectiveAutoDisableMode(policies?: AutoDisablePoliciesInput | null): APIKeyAutoDisableMode {
  const mode = policies?.apiKeyAutoDisableMode;
  const hasRules = (policies?.apiKeyAutoDisableRules?.length ?? 0) > 0;
  if (mode === 'off') return 'off';
  if (mode === 'custom') return hasRules ? 'custom' : 'inherit';
  if (mode === 'inherit') return 'inherit';
  return hasRules ? 'custom' : 'inherit';
}

export function toApiKeyAutoDisableRuleFormValues(rules?: APIKeyAutoDisableRule[] | null): ApiKeyAutoDisableRuleFormValue[] {
  return (
    rules?.map((rule) => ({
      statusCodes: rule.statusCodes ?? [],
      keywordPatterns: rule.keywordPatterns ?? [],
      times: rule.times,
      ...canonicalAutoDisableFields(rule),
    })) ?? []
  );
}

export function serializeApiKeyAutoDisableRules(rules: ApiKeyAutoDisableRuleFormValue[]): APIKeyAutoDisableRule[] {
  return rules.map((rule) =>
    apiKeyAutoDisableRuleFormSchema.parse({
      statusCodes: rule.statusCodes?.filter((code): code is number => code != null) ?? [],
      keywordPatterns: rule.keywordPatterns?.map((pattern) => pattern.trim()).filter(Boolean) ?? [],
      times: rule.times,
      ...canonicalAutoDisableFields(rule),
    })
  );
}

export function availabilityPoliciesPayload(input: {
  stream?: CapabilityPolicy;
  mode: APIKeyAutoDisableMode;
  rules: ApiKeyAutoDisableRuleFormValue[];
}): {
  stream?: CapabilityPolicy;
  apiKeyAutoDisableMode: APIKeyAutoDisableMode;
  apiKeyAutoDisableRules: APIKeyAutoDisableRule[] | null;
  emptiedCustom: boolean;
} {
  let savedMode = input.mode;
  const serialized = savedMode === 'inherit' ? [] : serializeApiKeyAutoDisableRules(input.rules);
  let emptiedCustom = false;
  if (savedMode === 'custom' && serialized.length === 0) {
    savedMode = 'inherit';
    emptiedCustom = true;
  }

  return {
    stream: input.stream,
    apiKeyAutoDisableMode: savedMode,
    apiKeyAutoDisableRules: savedMode === 'inherit' ? null : serialized,
    emptiedCustom,
  };
}

export function buildBulkAutoDisableInput(input: {
  channelIDs: string[];
  action: BulkAutoDisableAction;
  rules: ApiKeyAutoDisableRuleFormValue[];
}): { ok: true; input: BulkUpdateChannelAutoDisableInput } | { ok: false; error: 'empty_rules' } {
  if (input.action === 'write_rules') {
    const rules = serializeApiKeyAutoDisableRules(input.rules);
    if (rules.length === 0) {
      return { ok: false, error: 'empty_rules' };
    }
    return { ok: true, input: { channelIDs: input.channelIDs, action: 'write_rules', rules } };
  }

  return { ok: true, input: { channelIDs: input.channelIDs, action: input.action } };
}
