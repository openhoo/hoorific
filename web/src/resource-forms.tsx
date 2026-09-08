import { useEffect, useId, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import type { components } from './generated/api';
import { Alert, AlertDescription } from './components/ui/alert';
import { Button } from './components/ui/button';
import { Input } from './components/ui/input';
import { NativeSelect } from './components/ui/select';
import { Textarea } from './components/ui/textarea';
import { Plus, Trash2 } from 'lucide-react';

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
  description?: ReactNode;
  error?: string;
  children: ReactNode;
  className?: string;
};
function FieldShell({ label, htmlFor, required = false, description, error, children, className }: FieldShellProps) {
  const descriptionID = `${htmlFor}-description`;
  const errorID = `${htmlFor}-error`;
  return <div className={`field min-w-0${className ? ` ${className}` : ''}`}>
    <label htmlFor={htmlFor} className="flex min-w-0 items-center gap-1 text-sm font-medium leading-snug text-foreground">
      <span className="min-w-0 break-words">{label}</span>
    </label>
    {children}
    {description && <p id={descriptionID} className="text-xs leading-relaxed text-muted-foreground">{description}</p>}
    {error && <p id={errorID} role="alert" className="text-xs font-medium leading-relaxed text-destructive">{error}</p>}
  </div>;
}

function TextField({ label, value, onChange, required = false, readOnly = false }: FieldProps<string | undefined> & { required?: boolean; readOnly?: boolean }) {
  const id = useId();
  return <FieldShell label={label} htmlFor={id} required={required}>
    <Input id={id} value={value ?? ''} required={required} readOnly={readOnly} onInput={event => onChange(event.currentTarget.value)} />
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
function SelectField({ label, value, onChange, options }: FieldProps<string> & { options: readonly string[] }) {
  const id = useId();
  return <FieldShell label={label} htmlFor={id} required>
    <NativeSelect id={id} value={value} required onChange={event => onChange(event.currentTarget.value)}>
      {!options.includes(value) && <option value={value} disabled>{value || 'Select a value'}</option>}
      {options.map(option => <option key={option} value={option}>{option}</option>)}
    </NativeSelect>
  </FieldShell>;
}
export function IntegerField({ label, value, onChange, optional = false }: FieldProps<number | undefined> & { optional?: boolean }) {
  const [invalid, setInvalid] = useState<string>();
  const inputRef = useRef<HTMLInputElement | null>(null);
  const message = 'Enter a nonnegative safe integer (at most 9007199254740991).';
  const text = invalid ?? (value === undefined ? '' : String(value));
  const error = (text === '' && optional) || (/^\d+$/.test(text) && Number.isSafeInteger(Number(text))) ? '' : message;
  useEffect(() => {
    inputRef.current?.setCustomValidity(error);
  }, [error]);
  const id = useId();
  const handleInput = (event: FormEvent<HTMLInputElement>) => {
    const raw = event.currentTarget.value;
    const valid = raw === '' ? optional : /^\d+$/.test(raw) && Number.isSafeInteger(Number(raw));
    event.currentTarget.setCustomValidity(valid ? '' : message);
    setInvalid(valid ? undefined : raw);
    if (valid) onChange(raw === '' ? undefined : Number(raw));
  };
  return <FieldShell label={label} htmlFor={id} required={!optional} error={error}>
    <Input id={id} ref={inputRef} type="text" inputMode="numeric" value={text} required={!optional} aria-invalid={Boolean(error)} aria-errormessage={error ? `${id}-error` : undefined} onInput={handleInput} />
  </FieldShell>;
}
function StringList({ label, value, onChange }: FieldProps<string[] | null | undefined>) {
  const rows = value ?? [];
  const itemName = label.toLowerCase();
  return <fieldset className="field resource-form-list min-w-0 @min-[28rem]:col-span-2">
    <legend className="resource-form-list-legend">{label}</legend>
    {rows.length === 0 ? <p className="resource-form-empty">No {itemName} yet. Add an entry below.</p> :
      <div className="resource-form-list-rows">
        {rows.map((row, index) => <div key={index} className="resource-form-list-row">
          <span className="resource-form-list-index" aria-hidden="true">{index + 1}</span>
          <Input aria-label={`${label} ${index + 1}`} value={row} required onInput={event => onChange(rows.map((item, i) => i === index ? event.currentTarget.value : item))} />
          <Button type="button" variant="outline" size="sm" className="shrink-0 whitespace-normal" aria-label={`Remove ${itemName} entry ${index + 1}`} onClick={() => onChange(rows.filter((_, i) => i !== index))}><Trash2 aria-hidden="true" />Remove</Button>
        </div>)}
      </div>}
    <Button type="button" variant="outline" size="sm" className="resource-form-add h-auto min-h-10 w-full justify-start whitespace-normal text-left @min-[28rem]:w-auto" onClick={() => onChange([...rows, ''])}><Plus aria-hidden="true" />Add {itemName} entry</Button>
  </fieldset>;
}
function StringMap({ label, value, onChange, features = false }: FieldProps<Record<string, string> | undefined> & { features?: boolean }) {
  const [draft, setDraft] = useState<string>();
  const [error, setError] = useState('');
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const id = useId();
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
  return <FieldShell label={`${label} (JSON object of string values)`} htmlFor={id} error={error} description={features ? 'Feature values must be supported, unsupported, or unknown.' : undefined} className="@min-[28rem]:col-span-2">
    <Textarea id={id} ref={textareaRef} value={draft ?? JSON.stringify(value ?? {}, null, 2)} aria-invalid={Boolean(error)} aria-errormessage={error ? `${id}-error` : undefined} aria-describedby={features ? `${id}-description` : undefined} onInput={handleInput} className="min-h-32 resize-y font-mono text-sm" />
  </FieldShell>;
}

// Key constraints prevent a descriptor from silently writing the wrong DTO field.
type Keys<T, V> = { [K in keyof T]-?: NonNullable<T[K]> extends V ? K : never }[keyof T];
function fields<T extends object>(value: T, onChange: (value: T) => void) {
  const set = (key: keyof T, next: string | number | boolean | string[] | Record<string, string> | null | undefined) => onChange({ ...value, [key]: next });
  return {
    text: (key: Keys<T, string>, label: string, required = false, readOnly = false) => <TextField key={String(key)} label={label} value={value[key] as string | undefined} required={required} readOnly={readOnly} onChange={next => set(key, next)} />,
    integer: (key: Keys<T, number>, label: string, optional = false) => <IntegerField key={String(key)} label={label} value={value[key] as number | undefined} optional={optional} onChange={next => set(key, next)} />,
    list: (key: Keys<T, string[]>, label: string) => <StringList key={String(key)} label={label} value={value[key] as string[] | null | undefined} onChange={next => set(key, next)} />,
    bool: (key: Keys<T, boolean>, label: string, disabled = false, description?: ReactNode) => <BooleanField key={String(key)} label={label} value={Boolean(value[key])} disabled={disabled} description={description} onChange={next => set(key, next)} />,
    map: (key: Keys<T, Record<string, string>>, label: string, features = false) => <StringMap key={String(key)} label={label} value={value[key] as Record<string, string> | undefined} features={features} onChange={next => set(key, next)} />,
    select: (key: Keys<T, string>, label: string, options: readonly string[]) => <SelectField key={String(key)} label={label} value={String(value[key] ?? '')} options={options} onChange={next => set(key, next)} />,
  };
}

function FormLayout({ children }: { children: ReactNode }) {
  return <div className="resource-form @container min-w-0">
    <div className="grid min-w-0 gap-x-5 gap-y-4 @min-[28rem]:grid-cols-2">{children}</div>
  </div>;
}
function FormSection({ title, description, children }: { title: string; description?: ReactNode; children: ReactNode }) {
  return <section className="resource-form-section min-w-0 @min-[28rem]:col-span-2">
    <div className="resource-form-section-head">
      <h3>{title}</h3>
      {description && <p>{description}</p>}
    </div>
    <div className="grid min-w-0 gap-x-5 gap-y-4 @min-[28rem]:grid-cols-2">{children}</div>
  </section>;
}
function PriceFields({ value, onChange }: FieldProps<DTO['PriceSchedule']>) {
  const f = fields(value, onChange);
  return <FormSection title="Price schedule — integer nanodollars (10⁻⁹ USD)" description="Blank rates mean unknown, not free. Enter zero only for a known zero price.">
    {f.text('version', 'Price version', true)}
    {f.integer('input_per_million', 'Input nanodollars per million tokens', true)}
    {f.integer('output_per_million', 'Output nanodollars per million tokens', true)}
    {f.integer('cached_input_per_million', 'Cache-read nanodollars per million tokens', true)}
    {f.integer('cache_write_input_per_million', 'Cache-write nanodollars per million tokens', true)}
    {f.integer('cache_write_5m_per_million', '5-minute cache-write nanodollars per million tokens', true)}
    {f.integer('cache_write_1h_per_million', '1-hour cache-write nanodollars per million tokens', true)}
    {f.integer('maximum_unit_cost', 'Maximum nanodollars per operation unit', true)}
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
    <FormSection title="Capabilities">
      {f.list('operations', 'Operations')}
      {f.list('input_modalities', 'Input modalities')}
      {f.list('output_modalities', 'Output modalities')}
      {f.map('features', 'Features', true)}
    </FormSection>
    <FormSection title="Limits and status">
      {f.integer('context_limit', 'Context token limit (blank = unknown)', true)}
      {f.integer('output_limit', 'Output token limit (blank = unknown)', true)}
      {f.text('provenance', 'Provenance')}
      {f.bool('enabled', 'Enabled')}
      <BooleanField label="Provide price schedule" value={hasPrice} onChange={enabled => onChange({ ...value, price: enabled ? { version: '' } : undefined })} />
    </FormSection>
    {hasPrice && <PriceFields label="Price" value={value.price!} onChange={price => onChange({ ...value, price })} />}
  </FormLayout>;
}
function RouteFields({ value, onChange }: FieldProps<DTO['RoutePolicyData']>) {
  const f = fields(value, onChange);
  const targets = value.targets ?? [];
  const targetKeys = useRef<string[]>([]);
  const nextTargetKey = useRef(0);
  while (targetKeys.current.length < targets.length) targetKeys.current.push(`target-${nextTargetKey.current++}`);
  if (targetKeys.current.length > targets.length) targetKeys.current.length = targets.length;
  const removeTarget = (index: number) => {
    targetKeys.current.splice(index, 1);
    onChange({ ...value, targets: targets.filter((_, i) => i !== index) });
  };
  const addTarget = () => {
    targetKeys.current.push(`target-${nextTargetKey.current++}`);
    onChange({ ...value, targets: [...targets, { connection_id: '', model_id: '', priority: 0, weight: 1 }] });
  };
  return <FormLayout>
    <FormSection title="Routing options">
      {f.text('alias', 'Model alias', true)}
      {f.list('residency', 'Allowed residency regions')}
      {f.bool('fallback', 'Allow fallback')}
      {f.text('account_pool_id', 'Account pool ID')}
      {f.bool('affinity', 'Enable affinity')}
    </FormSection>
    <fieldset className="field resource-form-list resource-form-target-list min-w-0 @min-[28rem]:col-span-2">
      <legend className="resource-form-list-legend">Route targets</legend>
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
                {t.integer('weight', 'Weight')}
                {t.text('region', 'Region')}
              </div>
              <Button type="button" variant="outline" size="sm" className="shrink-0 whitespace-normal" aria-label={`Remove route target ${index + 1}`} onClick={() => removeTarget(index)}><Trash2 aria-hidden="true" />Remove target</Button>
            </fieldset>;
          })}
        </div>}
      <Button type="button" variant="outline" size="sm" className="resource-form-add h-auto min-h-10 w-full justify-start whitespace-normal text-left @min-[28rem]:w-auto" onClick={addTarget}><Plus aria-hidden="true" />Add target</Button>
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
        <FormSection title="Tenant identity" description="The active tenant cannot be disabled from this session. Switch to another enabled tenant first; re-enable a disabled tenant from another tenant.">
          {f.text('name', 'Tenant name', true)}
          {f.bool('enabled', 'Enabled', activeTenant, activeTenant ? 'The active tenant cannot be disabled from this session. Switch to another enabled tenant first.' : undefined)}
        </FormSection>
        {f.list('allowed_origins', 'Allowed browser origins')}
        <FormSection title="Request limits">
          {f.integer('max_body_bytes', 'Maximum request body bytes (0 = default)', true)}
          {f.integer('max_event_bytes', 'Maximum stream event bytes (0 = default)', true)}
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
          {f.text('display_name', 'Display name', true)}
          {f.bool('enabled', 'Enabled')}
        </FormSection>
      </FormLayout>;
    }
    case 'role_bindings': {
      const f = fields(value as DTO['RoleBindingData'], onChange as (value: DTO['RoleBindingData']) => void);
      return <FormLayout>
        <FormSection title="Role binding" description="This binding grants one existing operator access to the active tenant. Viewer is the least-privileged default; choose a stronger role only when required.">
          {f.text('subject', 'Operator subject (existing operator ID)', true, readOnlyIdentity)}
          {f.text('tenant_id', 'Current tenant binding', true, readOnlyIdentity)}
          {f.select('role', 'Role', ['viewer', 'operator', 'auditor', 'admin', 'owner'])}
        </FormSection>
      </FormLayout>;
    }
    case 'connections': {
      const f = fields(value as DTO['ConnectionData'], onChange as (value: DTO['ConnectionData']) => void);
      return <FormLayout>
        <FormSection title="Connection identity">
          {f.text('connector', 'Connector', true)}
          {f.text('account_id', 'Account ID', true)}
          {f.text('base_url', 'Base URL', true)}
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
        <FormSection title="Account pool identity">
          {f.text('provider', 'Provider', true)}
        </FormSection>
        {f.list('account_ids', 'Account IDs')}
      </FormLayout>;
    }
    case 'models': return <ModelFields label="Model" value={value as DTO['ModelData']} onChange={onChange as (value: DTO['ModelData']) => void} />;
    case 'model_aliases': {
      const f = fields(value as DTO['AliasData'], onChange as (value: DTO['AliasData']) => void);
      return <FormLayout>
        <FormSection title="Alias details">
          {f.text('description', 'Description')}
          {f.bool('enabled', 'Enabled')}
        </FormSection>
        {f.list('model_ids', 'Model IDs')}
      </FormLayout>;
    }
    case 'route_policies': return <RouteFields label="Route policy" value={value as DTO['RoutePolicyData']} onChange={onChange as (value: DTO['RoutePolicyData']) => void} />;
    case 'policy_limits': {
      const f = fields(value as DTO['PolicyLimitData'], onChange as (value: DTO['PolicyLimitData']) => void);
      return <FormLayout>
        <FormSection title="Limit scope">
          {f.select('scope', 'Policy scope', ['tenant', 'key', 'connection', 'account', 'model'])}
          {f.text('scope_id', 'Scope ID', true)}
          <p className="resource-form-note @min-[28rem]:col-span-2">Zero means no configured limit for these policy quantities.</p>
        </FormSection>
        <FormSection title="Policy limits">
          {f.integer('requests_per_minute', 'Requests per minute', true)}
          {f.integer('tokens_per_minute', 'Tokens per minute', true)}
          {f.integer('max_cost', 'Maximum cost (integer nanodollars)', true)}
          {f.select('cost_window', 'Cost window', ['total', 'daily', 'monthly'])}
          {f.integer('concurrency', 'Concurrent requests', true)}
          {f.integer('outstanding_jobs', 'Outstanding jobs', true)}
        </FormSection>
      </FormLayout>;
    }
    default:
      return <Alert role="note"><AlertDescription>This resource kind is read-only. No write form is available.</AlertDescription></Alert>;
  }
}
