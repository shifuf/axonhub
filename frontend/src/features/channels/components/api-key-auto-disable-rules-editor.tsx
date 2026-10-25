import { useCallback, useEffect, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react';
import { z } from 'zod';
import { useFieldArray, useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import { Info } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Textarea } from '@/components/ui/textarea';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { toApiKeyAutoDisableRuleFormValues, type ApiKeyAutoDisableRuleFormValue } from '../data/auto-disable';
import { type APIKeyAutoDisableRule, apiKeyAutoDisableRuleFormSchema } from '../data/schema';

const formSchema = z.object({
  rules: z.array(apiKeyAutoDisableRuleFormSchema),
});

const PRESET_DISABLE_DURATIONS = [5, 15, 30, 60, 120, 360, 720, 1440];

function HelpTooltip({ children }: { children: ReactNode }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type='button' className='text-muted-foreground hover:text-foreground inline-flex' aria-hidden='false'>
          <Info className='h-3.5 w-3.5' />
        </button>
      </TooltipTrigger>
      <TooltipContent className='max-w-sm'>{children}</TooltipContent>
    </Tooltip>
  );
}

interface EditorProps {
  rules: ApiKeyAutoDisableRuleFormValue[];
  onChange: (rules: ApiKeyAutoDisableRuleFormValue[]) => void;
}

export function ApiKeyAutoDisableRulesEditor({ rules, onChange }: EditorProps) {
  const { t } = useTranslation();
  const [customDurationModes, setCustomDurationModes] = useState<Record<string, true>>({});

  const form = useForm<z.infer<typeof formSchema>>({
    resolver: zodResolver(formSchema),
    values: { rules },
    mode: 'onChange',
  });

  const {
    fields: ruleFields,
    append: appendRule,
    remove: removeRule,
  } = useFieldArray({
    control: form.control,
    name: 'rules',
  });

  useEffect(() => {
    const subscription = form.watch((value) => {
      onChange((value.rules ?? []) as ApiKeyAutoDisableRuleFormValue[]);
    });
    return () => subscription.unsubscribe();
  }, [form, onChange]);

  const setModeFlag = useCallback((setter: Dispatch<SetStateAction<Record<string, true>>>, fieldID: string, enabled: boolean) => {
    setter((previous) => {
      if (enabled) return previous[fieldID] ? previous : { ...previous, [fieldID]: true };
      if (!previous[fieldID]) return previous;
      const next = { ...previous };
      delete next[fieldID];
      return next;
    });
  }, []);

  return (
    <Form {...form}>
      <div className='space-y-4'>
        <div className='bg-muted/50 text-muted-foreground space-y-1 rounded-md border p-3 text-sm'>
          <p data-testid='auto-disable-model-target'>{t('channels.dialogs.apiKeyRules.policySummary')}</p>
          <p>{t('channels.dialogs.apiKeyRules.retentionHint')}</p>
          <p>{t('channels.dialogs.apiKeyRules.safetyHint')}</p>
        </div>
        {ruleFields.length === 0 && (
          <div className='text-muted-foreground rounded-md border border-dashed p-6 text-center text-sm'>
            {t('channels.dialogs.apiKeyRules.empty')}
          </div>
        )}

        {ruleFields.map((field, index) => {
          const duration = form.watch(`rules.${index}.disableDurationMinutes`);
          const customDuration = !!customDurationModes[field.id] || (duration != null && !PRESET_DISABLE_DURATIONS.includes(duration));
          const durationValue = customDuration ? 'custom' : String(duration ?? 30);

          return (
            <div key={field.id} className='space-y-3 rounded-md border p-4'>
              <div className='flex items-center justify-between'>
                <Badge variant='outline'>{t('channels.dialogs.apiKeyRules.ruleLabel', { index: index + 1 })}</Badge>
                <Button
                  type='button'
                  variant='ghost'
                  size='icon'
                  aria-label={t('common.buttons.delete')}
                  onClick={() => {
                    removeRule(index);
                    setModeFlag(setCustomDurationModes, field.id, false);
                  }}
                >
                  <IconTrash className='h-4 w-4 text-red-500' />
                </Button>
              </div>

              <div className='grid grid-cols-1 gap-3 sm:grid-cols-2'>
                <FormField
                  control={form.control}
                  name={`rules.${index}.statusCodes`}
                  render={({ field: input }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.apiKeyRules.fields.statusCodes')}</FormLabel>
                      <FormControl>
                        <Input
                          key={`status-${field.id}`}
                          defaultValue={input.value?.join(', ') ?? ''}
                          placeholder={t('channels.dialogs.apiKeyRules.fields.statusCodesPlaceholder')}
                          onBlur={(event) => {
                            const codes = event.target.value
                              .split(/[,\s]+/)
                              .map((value) => Number.parseInt(value, 10))
                              .filter((value) => Number.isInteger(value) && value >= 100 && value <= 599);
                            input.onChange(codes);
                          }}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />

                <FormField
                  control={form.control}
                  name={`rules.${index}.times`}
                  render={({ field: input }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.apiKeyRules.fields.times')}</FormLabel>
                      <FormControl>
                        <Input
                          type='number'
                          min={1}
                          step={1}
                          data-testid={`auto-disable-threshold-${index}`}
                          value={input.value ?? ''}
                          onChange={(event) => {
                            const raw = event.target.value;
                            input.onChange(raw === '' ? undefined : event.target.valueAsNumber);
                          }}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </div>

              <div className='grid grid-cols-1 gap-3 sm:grid-cols-2'>
                <FormField
                  control={form.control}
                  name={`rules.${index}.windowMinutes`}
                  render={({ field: input }) => (
                    <FormItem>
                      <FormLabel className='flex items-center gap-2'>
                        {t('channels.dialogs.apiKeyRules.fields.windowMinutes')}
                        <HelpTooltip>
                          <p className='max-w-sm text-xs'>{t('channels.dialogs.apiKeyRules.fields.windowMinutesHelp')}</p>
                        </HelpTooltip>
                      </FormLabel>
                      <FormControl>
                        <Input
                          type='number'
                          min={1}
                          max={10080}
                          step={1}
                          data-testid={`auto-disable-window-${index}`}
                          value={input.value ?? ''}
                          placeholder={t('channels.dialogs.apiKeyRules.fields.windowMinutesPlaceholder')}
                          onChange={(event) => {
                            const raw = event.target.value;
                            input.onChange(raw === '' ? null : event.target.valueAsNumber);
                          }}
                        />
                      </FormControl>
                      <FormDescription>{t('channels.dialogs.apiKeyRules.fields.windowMinutesHint')}</FormDescription>
                      {input.value != null && (!Number.isInteger(input.value) || input.value < 1 || input.value > 10080) ? (
                        <p className='text-destructive text-sm' role='alert'>
                          {t('channels.dialogs.apiKeyRules.validation.windowMinutes')}
                        </p>
                      ) : (
                        <FormMessage />
                      )}
                    </FormItem>
                  )}
                />
              </div>

              <FormField
                control={form.control}
                name={`rules.${index}.keywordPatterns`}
                render={({ field: input }) => (
                  <FormItem>
                    <FormLabel>{t('channels.dialogs.apiKeyRules.fields.keywordPatterns')}</FormLabel>
                    <FormControl>
                      <Textarea
                        key={`patterns-${field.id}`}
                        className='min-h-24 font-mono text-sm'
                        defaultValue={input.value?.join('\n') ?? ''}
                        placeholder={t('channels.dialogs.apiKeyRules.fields.keywordPatternsPlaceholder')}
                        onBlur={(event) =>
                          input.onChange(
                            event.target.value
                              .split(/\r?\n/)
                              .map((value) => value.trim())
                              .filter(Boolean)
                          )
                        }
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />

              <FormField
                control={form.control}
                name={`rules.${index}.disableDurationMinutes`}
                render={({ field: input }) => (
                  <FormItem>
                    <FormLabel>{t('channels.dialogs.apiKeyRules.fields.disableDuration')}</FormLabel>
                    <div className='flex gap-2'>
                      <Select
                        value={durationValue}
                        onValueChange={(value) => {
                          if (value === 'custom') {
                            setModeFlag(setCustomDurationModes, field.id, true);
                            if (!input.value) input.onChange(30);
                          } else {
                            setModeFlag(setCustomDurationModes, field.id, false);
                            input.onChange(Number.parseInt(value, 10));
                          }
                        }}
                      >
                        <SelectTrigger className={customDuration ? 'w-1/2' : undefined}>
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          {PRESET_DISABLE_DURATIONS.map((minutes) => (
                            <SelectItem key={minutes} value={String(minutes)}>
                              {t(`channels.dialogs.apiKeyRules.durations.${minutes}`)}
                            </SelectItem>
                          ))}
                          <SelectItem value='custom'>{t('channels.dialogs.apiKeyRules.fields.disableDurationCustom')}</SelectItem>
                        </SelectContent>
                      </Select>
                      {customDuration && (
                        <Input
                          type='number'
                          min={1}
                          step={1}
                          className='w-1/2'
                          value={input.value ?? ''}
                          placeholder={t('channels.dialogs.apiKeyRules.fields.disableDurationCustomPlaceholder')}
                          onChange={(event) => {
                            input.onChange(event.target.value === '' ? null : event.target.valueAsNumber);
                          }}
                        />
                      )}
                    </div>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>
          );
        })}

        <Button
          type='button'
          variant='outline'
          className='w-full'
          onClick={() =>
            appendRule({
              statusCodes: [],
              keywordPatterns: [],
              times: 3,
              action: 'temporary_disable',
              disableDurationMinutes: 30,
              disableUntilCron: null,
              disableUntilTimezone: null,
              windowMinutes: 10,
              freezeModel: true,
            })
          }
        >
          <IconPlus className='mr-2 h-4 w-4' />
          {t('channels.dialogs.apiKeyRules.addRule')}
        </Button>
      </div>
    </Form>
  );
}

interface PreviewProps {
  rules?: APIKeyAutoDisableRule[] | null;
  emptyLabel?: string;
  inactiveHint?: string;
}

export function ApiKeyAutoDisableRulesPreview({ rules, emptyLabel, inactiveHint }: PreviewProps) {
  const { t } = useTranslation();
  if (!rules || rules.length === 0) {
    return <div className='text-muted-foreground rounded-md border border-dashed p-4 text-sm'>{emptyLabel}</div>;
  }

  return (
    <div className='space-y-2'>
      {inactiveHint && <p className='text-muted-foreground text-sm'>{inactiveHint}</p>}
      <p className='text-muted-foreground text-sm'>{t('channels.dialogs.apiKeyRules.policySummary')}</p>
      {toApiKeyAutoDisableRuleFormValues(rules).map((rule, index) => (
        <div key={index} className='rounded-md border p-3 text-sm'>
          <div className='mb-1 font-medium'>{t('channels.dialogs.apiKeyRules.ruleLabel', { index: index + 1 })}</div>
          <div className='text-muted-foreground space-y-0.5'>
            <div>
              {t('channels.dialogs.apiKeyRules.fields.statusCodes')}: {rule.statusCodes?.length ? rule.statusCodes.join(', ') : '—'}
            </div>
            <div>
              {t('channels.dialogs.apiKeyRules.fields.times')}: {rule.times}
            </div>
            <div>
              {t('channels.dialogs.apiKeyRules.preview.window', { minutes: rule.windowMinutes })}
            </div>
            <div>
              {t('channels.dialogs.apiKeyRules.preview.modelTarget')}
            </div>
            <div>
              {t('channels.dialogs.apiKeyRules.fields.action')}: {t('channels.dialogs.apiKeyRules.actions.temporaryDisable')}
            </div>
            <div>{t('channels.dialogs.apiKeyRules.preview.duration', { minutes: rule.disableDurationMinutes })}</div>
            {rule.keywordPatterns && rule.keywordPatterns.length > 0 && (
              <div>
                {t('channels.dialogs.apiKeyRules.fields.keywordPatterns')}: {rule.keywordPatterns.join(', ')}
              </div>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}
