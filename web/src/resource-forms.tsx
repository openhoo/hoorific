import { useEffect, useId, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import type { components } from './generated/api';
import { Alert, AlertDescription } from './components/ui/alert';
import { Button } from './components/ui/button';
import { Input } from './components/ui/input';
import { NativeSelect } from './components/ui/select';
import { Textarea } from './components/ui/textarea';
import { Lock, Plus, Trash2 } from 'lucide-react';

type DTO = components['schemas'];
type TenantData = DTO['TenantData'];
type OperatorData = DTO['OperatorData'];
export type ResourceDataByKind = {
  tenants: TenantData;
  operators: OperatorData;
  role_bindings: DTO['RoleBindingData'];
  connections: DTO['ConnectionData'];
  account_pools: DTO['AccountPoolData'];
  models: DTO['ModelData'];
  model_aliases: DTO['AliasData'];
  route_policies: DTO['RoutePolicyData'];
  policy_limits: DTO['PolicyLimitData'];
};
export type WritableResourceKind = keyof ResourceDataByKind;
export type ResourceData = ResourceDataByKind[WritableResourceKind];
export const writableResourceKinds: readonly WritableResourceKind[] = [
  'tenants', 'operators', 'role_bindings', 'connections',
  'account_pools', 'models', 'model_aliases', 'route_policies', 'policy_limits',
];
export function isWritableResourceKind(kind: string): kind is WritableResourceKind {
  return writableResourceKinds.some(candidate => candidate === kind);
}

// These aliases intentionally require regenerated TenantData and OperatorData
// from internal/admin/resources.go; do not replace stale generated contracts.
export function initialResourceData<K extends WritableResourceKind>(kind: K, tenantID: string): ResourceDataByKind[K];
export function initialResourceData(kind: string, tenantID: string): ResourceData | undefined;
export function initialResourceData(kind: string, tenantID: string): ResourceData | undefined {
  const defaults: ResourceDataByKind = {
    tenants: { name: '', enabled: true, allowed_origins: [], max_body_bytes: 0, max_event_bytes: 0 },
    operators: { subject: '', issuer: '', identity_subject: '', display_name: '', enabled: true },
    role_bindings: { subject: '', tenant_id: tenantID, role: 'viewer' },
    connections: { connector: '', account_id: '', base_url: '', region: '', project: '', dedicated: false, enabled: true, settings: {} },
    account_pools: { provider: '', account_ids: [] },
    models: { connection_id: '', upstream_id: '', operations: [], input_modalities: [], output_modalities: [], features: {}, enabled: true },
    model_aliases: { model_ids: [], description: '', enabled: true },
    route_policies: { alias: '', targets: [], residency: [], fallback: false, affinity: false },
    policy_limits: { scope: 'tenant', scope_id: tenantID, requests_per_minute: 0, tokens_per_minute: 0, max_cost: 0, concurrency: 0, cost_window: 'total', outstanding_jobs: 0 },
  };
  return isWritableResourceKind(kind) ? defaults[kind] : undefined;
}

type FieldProps<T> = { label: string; value: T; onChange: (value: T) => void };

type FieldShellProps = {
  label: string;
  htmlFor: string;
  required?: boolean;
  readOnly?: boolean;
  description?: ReactNode;
  error?: string;
  children: ReactNode;
  className?: string;
};
function FieldShell({ label, htmlFor, required = false, readOnly = false, description, error, children, className }: FieldShellProps) {
  const descriptionID = `${htmlFor}-description`;
  const errorID = `${htmlFor}-error`;
  return <div className={`field min-w-0${className ? ` ${className}` : ''}`}>
    <div className="flex min-w-0 items-center gap-1 text-sm font-medium leading-snug text-foreground">
      <label htmlFor={htmlFor} className="min-w-0 break-words">{label}</label>
      {required && <span aria-hidden="true" title="Required" className="text-destructive">*</span>}
      {readOnly && <span aria-hidden="true" title="Read-only" className="inline-flex shrink-0 text-muted-foreground"><Lock className="h-3 w-3" /></span>}
    </div>
    {children}
    {description && <p id={descriptionID} className="text-xs leading-relaxed text-muted-foreground">{description}</p>}
    {error && <p id={errorID} role="alert" className="text-xs font-medium leading-relaxed text-destructive">{error}</p>}
  </div>;
}
type TextFieldProps = FieldProps<string | undefined> & { required?: boolean; readOnly?: boolean; description?: ReactNode };
function TextField({ label, value, onChange, required = false, readOnly = false, description }: TextFieldProps) {
  const id = useId();
  return <FieldShell label={label} htmlFor={id} required={required} readOnly={readOnly} description={description}>
    <Input id={id} value={value ?? ''} required={required} readOnly={readOnly} aria-readonly={readOnly || undefined} aria-describedby={description ? `${id}-description` : undefined} onInput={event => onChange(event.currentTarget.value)} />
  </FieldShell>;
}
function BooleanField({ label, value, onChange, disabled = false, description }: FieldProps<boolean> & { disabled?: boolean; description?: ReactNode }) {
  const id = useId();
  return <div className="field field-check min-w-0">
    <div className="flex min-w-0 items-center gap-3">
      <Input id={id} type="checkbox" checked={value} disabled={disabled} aria-describedby={description ? `${id}-description` : undefined} className="h-4 w-4 shrink-0 rounded border-input p-0 accent-primary" onChange={event => onChange(event.currentTarget.checked)} />
      <label htmlFor={id} className={`min-w-0 break-words text-sm font-medium leading-snug text-foreground${disabled ? '' : ' cursor-pointer'}`}>{label}</label>
    </div>
    {description && <p id={`${id}-description`} className="text-xs leading-relaxed text-muted-foreground">{description}</p>}
  </div>;
}
type SelectFieldProps = FieldProps<string> & { options: readonly string[]; required?: boolean; description?: ReactNode };
function SelectField({ label, value, onChange, options, required = true, description }: SelectFieldProps) {
  const id = useId();
  return <FieldShell label={label} htmlFor={id} required={required} description={description}>
    <NativeSelect id={id} value={value} required={required} aria-describedby={description ? `${id}-description` : undefined} onChange={event => onChange(event.currentTarget.value)}>
      {!required && <option value="">Server default (total)</option>}
      {!options.includes(value) && (value !== '' || required) && <option value={value} disabled>{value || 'Select a value'}</option>}
      {options.map(option => <option key={option} value={option}>{option}</option>)}
    </NativeSelect>
  </FieldShell>;
}

export function IntegerField({ label, value, onChange, optional = false, minimum = 0 }: FieldProps<number | undefined> & { optional?: boolean; minimum?: number }) {
  const [invalid, setInvalid] = useState<string>();
  const inputRef = useRef<HTMLInputElement | null>(null);
  const maxSafeInteger = Number.MAX_SAFE_INTEGER;
  const message = minimum > 0
    ? `Enter a safe integer from ${minimum} to ${maxSafeInteger}.`
    : `Enter a nonnegative safe integer (at most ${maxSafeInteger}).`;
  const text = invalid ?? (value === undefined ? '' : String(value));
  const textIsValid = text === ''
    ? optional
    : /^\d+$/.test(text) && Number.isSafeInteger(Number(text)) && Number(text) >= minimum;
  const error = textIsValid ? '' : message;
  useEffect(() => {
    inputRef.current?.setCustomValidity(error);
  }, [error]);
  const id = useId();
  const handleInput = (event: FormEvent<HTMLInputElement>) => {
    const raw = event.currentTarget.value;
    const isValid = raw === ''
      ? optional
      : /^\d+$/.test(raw) && Number.isSafeInteger(Number(raw)) && Number(raw) >= minimum;
    event.currentTarget.setCustomValidity(isValid ? '' : message);
    setInvalid(isValid ? undefined : raw);
    if (isValid) onChange(raw === '' ? undefined : Number(raw));
  };
  return <FieldShell label={label} htmlFor={id} required={!optional} error={error}>
    <Input id={id} ref={inputRef} type="text" inputMode="numeric" value={text} required={!optional} aria-invalid={Boolean(error)} aria-errormessage={error ? `${id}-error` : undefined} onInput={handleInput} />
  </FieldShell>;
}

function validateReference(value: string): string | undefined {
  if (!value) return 'Enter a value.';
  if (value.length > 512) return 'Use 512 characters or fewer.';
  if (value.trim() !== value) return 'Do not use leading or trailing spaces.';
  if (/[\u0000-\u001f\u007f-\u009f]/.test(value)) return 'Control characters are not allowed.';
  return undefined;
}
function validateName(value: string): string | undefined {
  return value === '*' ? 'Wildcard entries are not allowed here.' : undefined;
}
function validateOperation(value: string): string | undefined {
  return /[\/\\\s]/.test(value) ? 'Do not use spaces, slashes, or backslashes.' : undefined;
}
function validateOrigin(value: string): string | undefined {
  try {
    const origin = new URL(value);
    const authority = value.slice(value.indexOf('://') + 3);
    if (!['http:', 'https:'].includes(origin.protocol) || !origin.hostname || origin.username || origin.password || /[/?#]/.test(authority)) {
      return 'Use an http(s) origin without a path, query, credentials, or fragment.';
    }
  } catch {
    return 'Use an http(s) origin without a path, query, credentials, or fragment.';
  }
  return undefined;
}

type StringListOptions = {
  description?: ReactNode;
  itemLabel?: string;
  minItems?: number;
  unique?: boolean;
  validateItem?: (value: string) => string | undefined;
};
function StringList({ label, value, onChange, description, itemLabel, minItems = 0, unique = false, validateItem }: FieldProps<string[] | null | undefined> & StringListOptions) {
  const rows = value ?? [];
  const itemName = itemLabel ?? label.toLowerCase();
  const id = useId();
  const descriptionID = `${id}-description`;
  const errorID = `${id}-error`;
  const inputRefs = useRef<Array<HTMLInputElement | null>>([]);
  const itemErrors = rows.map((row, index) => {
    const referenceError = validateReference(row);
    if (referenceError) return referenceError;
    const itemError = validateItem?.(row);
    if (itemError) return itemError;
    return unique && rows.some((item, otherIndex) => otherIndex !== index && item === row)
      ? `Duplicate ${itemName} entries are not allowed.`
      : undefined;
  });
  const listError = minItems > 0 && rows.length < minItems
    ? `At least ${minItems === 1 ? `one ${itemName}` : `${minItems} ${itemName} entries`} is required.`
    : undefined;
  const firstErrorIndex = itemErrors.findIndex(Boolean);
  const error = listError ?? (firstErrorIndex >= 0 ? `${itemName} ${firstErrorIndex + 1}: ${itemErrors[firstErrorIndex]}` : undefined);
  const describedBy = [description ? descriptionID : undefined, error ? errorID : undefined].filter(Boolean).join(' ') || undefined;
  useEffect(() => {
    inputRefs.current.length = rows.length;
    inputRefs.current.forEach((input, index) => input?.setCustomValidity(itemErrors[index] ?? ''));
  }, [itemErrors]);
  return <fieldset className="field resource-form-list min-w-0 @min-[28rem]:col-span-2" aria-describedby={describedBy} aria-invalid={Boolean(error)} aria-required={minItems > 0 || undefined}>
    <legend className="resource-form-list-legend">
      <span>{label}</span>
      {minItems > 0 && <span aria-hidden="true" title="At least one entry required" className="ml-1 text-destructive">*</span>}
    </legend>
    {description && <p id={descriptionID} className="text-xs leading-relaxed text-muted-foreground">{description}</p>}
    {error && <p id={errorID} role="alert" className="text-xs font-medium leading-relaxed text-destructive">{error}</p>}
    {rows.length === 0 ? <p className="resource-form-empty">No {itemName} yet. Add an entry below.</p> :
      <div className="resource-form-list-rows">
        {rows.map((row, index) => <div key={index} className="resource-form-list-row">
          <span className="resource-form-list-index" aria-hidden="true">{index + 1}</span>
          <Input ref={element => { inputRefs.current[index] = element; }} aria-label={`${label} ${index + 1}`} value={row} required aria-describedby={describedBy} aria-invalid={Boolean(itemErrors[index])} aria-errormessage={itemErrors[index] ? errorID : undefined} onInput={event => onChange(rows.map((item, i) => i === index ? event.currentTarget.value : item))} />
          <Button type="button" variant="outline" size="sm" className="shrink-0 whitespace-normal" aria-label={`Remove ${itemName} entry ${index + 1}`} onClick={() => onChange(rows.filter((_, i) => i !== index))}><Trash2 aria-hidden="true" />Remove</Button>
        </div>)}
      </div>}
    <Button type="button" variant="outline" size="sm" className="resource-form-add h-auto min-h-10 w-full justify-start whitespace-normal text-left @min-[28rem]:w-auto" aria-invalid={Boolean(listError)} aria-errormessage={listError ? errorID : undefined} onClick={() => onChange([...rows, ''])}><Plus aria-hidden="true" />Add {itemName} entry</Button>
  </fieldset>;
}
function StringMap({ label, value, onChange, features = false }: FieldProps<Record<string, string> | undefined> & { features?: boolean }) {
  const [draft, setDraft] = useState<string>();
  const [error, setError] = useState('');
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const id = useId();
  const description = features ? 'Feature values must be supported, unsupported, or unknown.' : 'Use {} to clear this map.';
  useEffect(() => {
    textareaRef.current?.setCustomValidity(error);
  }, [error]);
  const handleInput = (event: FormEvent<HTMLTextAreaElement>) => {
    const raw = event.currentTarget.value;
    setDraft(raw);
    try {
      const parsed: unknown = JSON.parse(raw);
      if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('Enter an object of string values.');
      const checked: Record<string, string> = Object.create(null);
      for (const [key, item] of Object.entries(parsed)) {
        if (!key.trim() || typeof item !== 'string') throw new Error('Keys must be nonempty and values must be strings.');
        if (features && !['supported', 'unsupported', 'unknown'].includes(item)) throw new Error('Feature values must be supported, unsupported, or unknown.');
        checked[key] = item;
      }
      setError('');
      event.currentTarget.setCustomValidity('');
      onChange(checked);
    } catch (failure) {
      const message = failure instanceof Error ? failure.message : 'Invalid string map.';
      setError(message);
      event.currentTarget.setCustomValidity(message);
    }
  };
  return <FieldShell label={`${label} (JSON object of string values)`} htmlFor={id} error={error} description={description} className="@min-[28rem]:col-span-2">
    <Textarea id={id} ref={textareaRef} value={draft ?? JSON.stringify(value ?? {}, null, 2)} aria-invalid={Boolean(error)} aria-errormessage={error ? `${id}-error` : undefined} aria-describedby={`${id}-description`} onInput={handleInput} className="min-h-32 resize-y font-mono text-sm" />
  </FieldShell>;
}

// Key constraints prevent a descriptor from silently writing the wrong DTO field.
type Keys<T, V> = { [K in keyof T]-?: NonNullable<T[K]> extends V ? K : never }[keyof T];
function fields<T extends object>(value: T, onChange: (value: T) => void) {
  const set = (key: keyof T, next: string | number | boolean | string[] | Record<string, string> | null | undefined) => onChange({ ...value, [key]: next });
  return {
    text: (key: Keys<T, string>, label: string, required = false, readOnly = false, description?: ReactNode) => <TextField key={String(key)} label={label} value={value[key] as string | undefined} required={required} readOnly={readOnly} description={description} onChange={next => set(key, next)} />,
    integer: (key: Keys<T, number>, label: string, optional = false, minimum = 0) => <IntegerField key={String(key)} label={label} value={value[key] as number | undefined} optional={optional} minimum={minimum} onChange={next => set(key, next)} />,
    list: (key: Keys<T, string[]>, label: string, options: StringListOptions = {}) => <StringList key={String(key)} label={label} value={value[key] as string[] | null | undefined} {...options} onChange={next => set(key, next)} />,
    bool: (key: Keys<T, boolean>, label: string, disabled = false, description?: ReactNode) => <BooleanField key={String(key)} label={label} value={Boolean(value[key])} disabled={disabled} description={description} onChange={next => set(key, next)} />,
    map: (key: Keys<T, Record<string, string>>, label: string, features = false) => <StringMap key={String(key)} label={label} value={value[key] as Record<string, string> | undefined} features={features} onChange={next => set(key, next)} />,
    select: (key: Keys<T, string>, label: string, options: readonly string[], required = true, description?: ReactNode) => <SelectField key={String(key)} label={label} value={String(value[key] ?? '')} options={options} required={required} description={description} onChange={next => set(key, next)} />,
  };
}

function FormLayout({ children }: { children: ReactNode }) {
  return <div className="resource-form @container min-w-0">
    <div className="grid min-w-0 gap-x-5 gap-y-4 @min-[28rem]:grid-cols-2">{children}</div>
  </div>;
}
function FormSection({ title, description, children }: { title: string; description?: ReactNode; children: ReactNode }) {
  const headingID = useId();
  const descriptionID = `${headingID}-description`;
  return <section className="resource-form-section min-w-0 @min-[28rem]:col-span-2" aria-labelledby={headingID} aria-describedby={description ? descriptionID : undefined}>
    <div className="resource-form-section-head">
      <h3 id={headingID}>{title}</h3>
      {description && <p id={descriptionID}>{description}</p>}
    </div>
    <div className="grid min-w-0 gap-x-5 gap-y-4 @min-[28rem]:grid-cols-2">{children}</div>
  </section>;
}
function PriceFields({ value, onChange }: FieldProps<DTO['PriceSchedule']>) {
  const f = fields(value, onChange);
  return <FormSection title="Price schedule — integer nanodollars (10⁻⁹ USD)" description="Blank rates mean unknown, not free. Enter zero only for a known zero price. A maximum cost applies only when Unit operation is set.">
    {f.text('version', 'Price version', true)}
    {f.integer('input_per_million', 'Input per million tokens', true)}
    {f.integer('output_per_million', 'Output per million tokens', true)}
    {f.integer('cached_input_per_million', 'Cache-read per million tokens', true)}
    {f.integer('cache_write_input_per_million', 'Cache-write per million tokens', true)}
    {f.integer('cache_write_5m_per_million', '5-minute cache-write per million tokens', true)}
    {f.integer('cache_write_1h_per_million', '1-hour cache-write per million tokens', true)}
    {f.integer('maximum_unit_cost', 'Maximum cost per operation unit', true)}
    {f.text('unit_operation', 'Unit operation')}
  </FormSection>;
}
function ModelFields({ value, onChange }: FieldProps<DTO['ModelData']>) {
  const f = fields(value, onChange);
  const hasPrice = value.price !== undefined;
  return <FormLayout>
    <FormSection title="Model identity">
      {f.text('connection_id', 'Connection ID', true)}
      {f.text('upstream_id', 'Upstream model ID', true)}
    </FormSection>
    <FormSection title="Capabilities" description="Add at least one operation. Other lists are optional; entries must be unique and cannot contain surrounding whitespace.">
      {f.list('operations', 'Operations', { itemLabel: 'operation', minItems: 1, unique: true, validateItem: validateOperation })}
      {f.list('input_modalities', 'Input modalities', { itemLabel: 'input modality', unique: true, validateItem: validateName })}
      {f.list('output_modalities', 'Output modalities', { itemLabel: 'output modality', unique: true, validateItem: validateName })}
      {f.map('features', 'Features', true)}
    </FormSection>
    <FormSection title="Limits and status">
      {f.integer('context_limit', 'Context token limit (blank = unknown)', true)}
      {f.integer('output_limit', 'Output token limit (blank = unknown)', true)}
      {f.text('provenance', 'Provenance')}
      {f.bool('enabled', 'Enabled')}
      <BooleanField label="Provide price schedule" value={hasPrice} description="Unchecking removes the current schedule from this draft." onChange={enabled => onChange({ ...value, price: enabled ? { version: '' } : undefined })} />
    </FormSection>
    {hasPrice && <PriceFields label="Price" value={value.price!} onChange={price => onChange({ ...value, price })} />}
  </FormLayout>;
}
function RouteFields({ value, onChange }: FieldProps<DTO['RoutePolicyData']>) {
  const f = fields(value, onChange);
  const targets = value.targets ?? [];
  const targetKeys = useRef<string[]>([]);
  const nextTargetKey = useRef(0);
  const targetListID = useId();
  const targetErrorID = `${targetListID}-error`;
  while (targetKeys.current.length < targets.length) targetKeys.current.push(`target-${nextTargetKey.current++}`);
  if (targetKeys.current.length > targets.length) targetKeys.current.length = targets.length;
  const duplicateTargetIndex = targets.findIndex((target, index) =>
    Boolean(target.connection_id && target.model_id) && targets.some((other, otherIndex) =>
      otherIndex !== index && other.connection_id === target.connection_id && other.model_id === target.model_id));
  const targetError = targets.length === 0
    ? 'At least one route target is required.'
    : duplicateTargetIndex >= 0
      ? `Route target ${duplicateTargetIndex + 1} duplicates another target.`
      : undefined;
  const removeTarget = (index: number) => {
    targetKeys.current.splice(index, 1);
    onChange({ ...value, targets: targets.filter((_, i) => i !== index) });
  };
  const addTarget = () => {
    targetKeys.current.push(`target-${nextTargetKey.current++}`);
    onChange({ ...value, targets: [...targets, { connection_id: '', model_id: '', priority: 0, weight: 1 }] });
  };
  return <FormLayout>
    <FormSection title="Routing options" description="Add at least one compatible target. Lower priority values run first; weights must be at least 1. Residency and account pools must match the selected targets.">
      {f.text('alias', 'Model alias', true)}
      {f.list('residency', 'Allowed residency regions', { itemLabel: 'residency region', unique: true, validateItem: validateName })}
      {f.bool('fallback', 'Allow fallback')}
      {f.text('account_pool_id', 'Account pool ID')}
      {f.bool('affinity', 'Enable affinity')}
    </FormSection>
    <fieldset className="field resource-form-list resource-form-target-list min-w-0 @min-[28rem]:col-span-2" aria-describedby={targetError ? targetErrorID : undefined} aria-invalid={Boolean(targetError)} aria-required="true">
      <legend className="resource-form-list-legend">
        <span>Route targets</span>
        <span aria-hidden="true" title="At least one entry required" className="ml-1 text-destructive">*</span>
      </legend>
      {targetError && <p id={targetErrorID} role="alert" className="text-xs font-medium leading-relaxed text-destructive">{targetError}</p>}
      {targets.length === 0 ? <p className="resource-form-empty">No route targets yet. Add a target below.</p> :
        <div className="resource-form-list-rows">
          {targets.map((target, index) => {
            const t = fields(target, next => onChange({ ...value, targets: targets.map((item, i) => i === index ? next : item) }));
            return <fieldset key={targetKeys.current[index]} className="resource-form-target min-w-0">
              <legend><span className="resource-form-target-label">Target {index + 1}</span></legend>
              <div className="grid min-w-0 gap-x-5 gap-y-4 @min-[28rem]:grid-cols-2">
                {t.text('connection_id', 'Connection ID', true)}
                {t.text('model_id', 'Model ID', true)}
                {t.integer('priority', 'Priority')}
                {t.integer('weight', 'Weight', false, 1)}
                {t.text('region', 'Region')}
              </div>
              <Button type="button" variant="outline" size="sm" className="shrink-0 whitespace-normal" aria-label={`Remove route target ${index + 1}`} onClick={() => removeTarget(index)}><Trash2 aria-hidden="true" />Remove target</Button>
            </fieldset>;
          })}
        </div>}
      <Button type="button" variant="outline" size="sm" className="resource-form-add h-auto min-h-10 w-full justify-start whitespace-normal text-left @min-[28rem]:w-auto" aria-invalid={Boolean(targetError)} aria-errormessage={targetError ? targetErrorID : undefined} onClick={addTarget}><Plus aria-hidden="true" />Add target</Button>
    </fieldset>
  </FormLayout>;
}
export type ResourceFormProps = { kind: string; value: ResourceData; onChange: (value: ResourceData) => void; readOnlyIdentity?: boolean; activeTenant?: boolean };
// Parent must pair kind with its corresponding DTO and remount on resource ID or
// tenant changes. This component owns fields, not a nested form or submit action.
export function ResourceForm({ kind, value, onChange, readOnlyIdentity = false, activeTenant = false }: ResourceFormProps) {
  switch (kind) {
    case 'tenants': {
      const f = fields(value as DTO['TenantData'], onChange as (value: DTO['TenantData']) => void);
      return <FormLayout>
        <FormSection title="Tenant identity" description="Switch to another enabled tenant before disabling the active tenant. A disabled tenant can be re-enabled from another tenant.">
          {f.text('name', 'Tenant name', true)}
          {f.bool('enabled', 'Enabled', activeTenant, activeTenant ? 'Switch to another enabled tenant first.' : undefined)}
        </FormSection>
        {f.list('allowed_origins', 'Allowed browser origins', { itemLabel: 'origin', unique: true, validateItem: validateOrigin, description: 'Optional. Use an http(s) origin such as https://app.example without a path, query, credentials, or fragment.' })}
        <FormSection title="Request limits" description="Leave either value blank or enter 0 to use the server default.">
          {f.integer('max_body_bytes', 'Maximum request body (bytes)', true)}
          {f.integer('max_event_bytes', 'Maximum stream event (bytes)', true)}
        </FormSection>
      </FormLayout>;
    }
    case 'operators': {
      const f = fields(value as DTO['OperatorData'], onChange as (value: DTO['OperatorData']) => void);
      return <FormLayout>
        <FormSection title="Operator identity" description="The operator ID is globally unique. Identity issuer and identity subject establish its external login mapping and cannot change after creation.">
          {f.text('subject', 'Operator ID (subject)', true, readOnlyIdentity)}
          {f.text('issuer', 'Identity issuer', true, readOnlyIdentity)}
          {f.text('identity_subject', 'Identity subject (issuer sub claim)', true, readOnlyIdentity)}
          {f.text('display_name', 'Display name')}
          {f.bool('enabled', 'Enabled')}
        </FormSection>
      </FormLayout>;
    }
    case 'role_bindings': {
      const f = fields(value as DTO['RoleBindingData'], onChange as (value: DTO['RoleBindingData']) => void);
      return <FormLayout>
        <FormSection title="Role binding" description="This binding grants one existing operator access to the active tenant. The tenant is fixed to the current tenant. Viewer is the least-privileged default; choose a stronger role only when required.">
          {f.text('subject', 'Operator subject (existing operator ID)', true, readOnlyIdentity)}
          {f.text('tenant_id', 'Current tenant binding', true, true)}
          {f.select('role', 'Role', ['viewer', 'operator', 'auditor', 'admin', 'owner'])}
        </FormSection>
      </FormLayout>;
    }
    case 'connections': {
      const f = fields(value as DTO['ConnectionData'], onChange as (value: DTO['ConnectionData']) => void);
      return <FormLayout>
        <FormSection title="Connection identity" description="Account ID and Base URL are optional for connectors that provide them at runtime.">
          {f.text('connector', 'Connector', true)}
          {f.text('account_id', 'Account ID')}
          {f.text('base_url', 'Base URL', false, false, 'Optional. Use an HTTPS URL, or HTTP only where the server private-network policy permits it.')}
          {f.text('region', 'Region')}
          {f.text('project', 'Project')}
        </FormSection>
        <FormSection title="Connection settings">
          {f.bool('dedicated', 'Dedicated')}
          {f.bool('enabled', 'Enabled')}
          {f.map('settings', 'Connection settings')}
        </FormSection>
      </FormLayout>;
    }
    case 'account_pools': {
      const f = fields(value as DTO['AccountPoolData'], onChange as (value: DTO['AccountPoolData']) => void);
      return <FormLayout>
        <FormSection title="Account pool identity" description="The provider should match the connector provider used by each account. Runtime routing requires at least one account ID.">
          {f.text('provider', 'Provider', true)}
        </FormSection>
        {f.list('account_ids', 'Account IDs', { itemLabel: 'account ID', minItems: 1, unique: true, validateItem: validateName })}
      </FormLayout>;
    }
    case 'models': return <ModelFields label="Model" value={value as DTO['ModelData']} onChange={onChange as (value: DTO['ModelData']) => void} />;
    case 'model_aliases': {
      const alias = value as DTO['AliasData'];
      const f = fields(alias, onChange as (value: DTO['AliasData']) => void);
      return <FormLayout>
        <FormSection title="Alias details" description={alias.enabled ? 'Enabled aliases need at least one existing model ID.' : 'Disabled aliases may be left without model IDs.'}>
          {f.text('description', 'Description')}
          {f.bool('enabled', 'Enabled')}
        </FormSection>
        {f.list('model_ids', 'Model IDs', { itemLabel: 'model ID', minItems: alias.enabled ? 1 : 0, unique: true, validateItem: validateName, description: 'Use existing model IDs; duplicates are not allowed.' })}
      </FormLayout>;
    }
    case 'route_policies': return <RouteFields label="Route policy" value={value as DTO['RoutePolicyData']} onChange={onChange as (value: DTO['RoutePolicyData']) => void} />;
    case 'policy_limits': {
      const f = fields(value as DTO['PolicyLimitData'], onChange as (value: DTO['PolicyLimitData']) => void);
      return <FormLayout>
        <FormSection title="Limit scope" description="Scope ID must identify a resource in the selected scope. Tenant scope uses the active tenant; changing scope does not change this ID.">
          {f.select('scope', 'Policy scope', ['tenant', 'key', 'connection', 'account', 'model'])}
          {f.text('scope_id', 'Scope ID', true, false, 'Use the ID from the selected scope.')}
        </FormSection>
        <FormSection title="Policy limits" description="Use 0 or leave a quantity blank for no configured limit. A blank cost window uses the total window.">
          {f.integer('requests_per_minute', 'Requests per minute', true)}
          {f.integer('tokens_per_minute', 'Tokens per minute', true)}
          {f.integer('max_cost', 'Maximum cost (integer nanodollars)', true)}
          {f.select('cost_window', 'Cost window', ['total', 'daily', 'monthly'], false)}
          {f.integer('concurrency', 'Concurrent requests', true)}
          {f.integer('outstanding_jobs', 'Outstanding jobs', true)}
        </FormSection>
      </FormLayout>;
    }
    default:
      return <Alert role="note"><AlertDescription>This resource kind is read-only. No write form is available.</AlertDescription></Alert>;
  }
}
