import { useState } from 'preact/hooks';
import type { components } from './generated/api';

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
function TextField({ label, value, onChange, required = false, readOnly = false }: FieldProps<string | undefined> & { required?: boolean; readOnly?: boolean }) {
  return <label class="field"><span>{label}</span><input value={value ?? ''} required={required} readOnly={readOnly} onInput={e => onChange(e.currentTarget.value)} /></label>;
}
function BooleanField({ label, value, onChange }: FieldProps<boolean>) {
  return <label class="field"><span><input type="checkbox" checked={value} onChange={e => onChange(e.currentTarget.checked)} /> {label}</span></label>;
}
function SelectField({ label, value, onChange, options }: FieldProps<string> & { options: readonly string[] }) {
  return <label class="field"><span>{label}</span><select value={value} required onChange={e => onChange(e.currentTarget.value)}>
    {!options.includes(value) && <option value={value} disabled>{value || 'Select a value'}</option>}
    {options.map(option => <option key={option} value={option}>{option}</option>)}
  </select></label>;
}
export function IntegerField({ label, value, onChange, optional = false }: FieldProps<number | undefined> & { optional?: boolean }) {
  const [invalid, setInvalid] = useState<string>();
  const message = 'Enter a nonnegative safe integer (at most 9007199254740991).';
  const text = invalid ?? (value === undefined ? '' : String(value));
  const error = (text === '' && optional) || (/^\d+$/.test(text) && Number.isSafeInteger(Number(text))) ? '' : message;
  return <label class="field"><span>{label}</span><input type="text" inputMode="numeric" value={text} required={!optional}
    aria-invalid={Boolean(error)} ref={element => element?.setCustomValidity(error)} onInput={e => {
      const raw = e.currentTarget.value;
      const valid = raw === '' ? optional : /^\d+$/.test(raw) && Number.isSafeInteger(Number(raw));
      e.currentTarget.setCustomValidity(valid ? '' : message);
      setInvalid(valid ? undefined : raw);
      if (valid) onChange(raw === '' ? undefined : Number(raw));
    }} />{error && <small role="alert">{error}</small>}</label>;
}
function StringList({ label, value, onChange }: FieldProps<string[] | null | undefined>) {
  const rows = value ?? [];
  return <fieldset><legend>{label}</legend>{rows.map((row, index) => <div key={index} class="field">
    <input aria-label={`${label} ${index + 1}`} value={row} required onInput={e => onChange(rows.map((item, i) => i === index ? e.currentTarget.value : item))} />
    <button type="button" onClick={() => onChange(rows.filter((_, i) => i !== index))}>Remove</button>
  </div>)}<button type="button" onClick={() => onChange([...rows, ''])}>Add {label.toLowerCase()} entry</button></fieldset>;
}
function StringMap({ label, value, onChange, features = false }: FieldProps<Record<string, string> | undefined> & { features?: boolean }) {
  const [draft, setDraft] = useState<string>();
  const [error, setError] = useState('');
  return <label class="field"><span>{label} (JSON object of string values)</span><textarea value={draft ?? JSON.stringify(value ?? {}, null, 2)}
    aria-invalid={Boolean(error)} ref={element => element?.setCustomValidity(error)} onInput={e => {
      const raw = e.currentTarget.value;
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
        setError(''); e.currentTarget.setCustomValidity(''); onChange(checked);
      } catch (failure) {
        const message = failure instanceof Error ? failure.message : 'Invalid string map.';
        setError(message); e.currentTarget.setCustomValidity(message);
      }
    }} />{error && <small role="alert">{error}</small>}</label>;
}

// Key constraints prevent a descriptor from silently writing the wrong DTO field.
type Keys<T, V> = { [K in keyof T]-?: NonNullable<T[K]> extends V ? K : never }[keyof T];
function fields<T extends object>(value: T, onChange: (value: T) => void) {
  const set = (key: keyof T, next: string | number | boolean | string[] | Record<string, string> | null | undefined) => onChange({ ...value, [key]: next });
  return {
    text: (key: Keys<T, string>, label: string, required = false, readOnly = false) => <TextField key={String(key)} label={label} value={value[key] as string | undefined} required={required} readOnly={readOnly} onChange={next => set(key, next)} />,
    bool: (key: Keys<T, boolean>, label: string) => <BooleanField key={String(key)} label={label} value={Boolean(value[key])} onChange={next => set(key, next)} />,
    integer: (key: Keys<T, number>, label: string, optional = false) => <IntegerField key={String(key)} label={label} value={value[key] as number | undefined} optional={optional} onChange={next => set(key, next)} />,
    list: (key: Keys<T, string[]>, label: string) => <StringList key={String(key)} label={label} value={value[key] as string[] | null | undefined} onChange={next => set(key, next)} />,
    map: (key: Keys<T, Record<string, string>>, label: string, features = false) => <StringMap key={String(key)} label={label} value={value[key] as Record<string, string> | undefined} features={features} onChange={next => set(key, next)} />,
    select: (key: Keys<T, string>, label: string, options: readonly string[]) => <SelectField key={String(key)} label={label} value={String(value[key] ?? '')} options={options} onChange={next => set(key, next)} />,
  };
}
function PriceFields({ value, onChange }: FieldProps<DTO['PriceSchedule']>) {
  const f = fields(value, onChange);
  return <fieldset><legend>Price schedule — integer nanodollars (10⁻⁹ USD)</legend>
    <p class="muted">Blank rates mean unknown, not free. Enter zero only for a known zero price.</p>
    {f.text('version', 'Price version', true)}
    {f.integer('input_per_million', 'Input nanodollars per million tokens', true)}
    {f.integer('output_per_million', 'Output nanodollars per million tokens', true)}
    {f.integer('maximum_unit_cost', 'Maximum nanodollars per operation unit', true)}
    {f.text('unit_operation', 'Unit operation')}
  </fieldset>;
}
function ModelFields({ value, onChange }: FieldProps<DTO['ModelData']>) {
  const f = fields(value, onChange);
  return <>{f.text('connection_id', 'Connection ID', true)}{f.text('upstream_id', 'Upstream model ID', true)}
    {f.list('operations', 'Operations')}{f.list('input_modalities', 'Input modalities')}{f.list('output_modalities', 'Output modalities')}
    {f.map('features', 'Features', true)}{f.integer('context_limit', 'Context token limit (blank = unknown)', true)}{f.integer('output_limit', 'Output token limit (blank = unknown)', true)}
    {f.text('provenance', 'Provenance')}{f.bool('enabled', 'Enabled')}
    <BooleanField label="Provide price schedule" value={value.price !== undefined} onChange={enabled => onChange({ ...value, price: enabled ? { version: '' } : undefined })} />
    {value.price && <PriceFields label="Price" value={value.price} onChange={price => onChange({ ...value, price })} />}
  </>;
}
function RouteFields({ value, onChange }: FieldProps<DTO['RoutePolicyData']>) {
  const f = fields(value, onChange);
  const targets = value.targets ?? [];
  return <>{f.text('alias', 'Model alias', true)}<fieldset><legend>Route targets</legend>
    {targets.map((target, index) => {
      const t = fields(target, next => onChange({ ...value, targets: targets.map((item, i) => i === index ? next : item) }));
      return <fieldset key={index}><legend>Target {index + 1}</legend>{t.text('connection_id', 'Connection ID', true)}{t.text('model_id', 'Model ID', true)}
        {t.integer('priority', 'Priority')}{t.integer('weight', 'Weight')}{t.text('region', 'Region')}
        <button type="button" onClick={() => onChange({ ...value, targets: targets.filter((_, i) => i !== index) })}>Remove target</button>
      </fieldset>;
    })}<button type="button" onClick={() => onChange({ ...value, targets: [...targets, { connection_id: '', model_id: '', priority: 0, weight: 1 }] })}>Add target</button>
    </fieldset>{f.list('residency', 'Allowed residency regions')}{f.bool('fallback', 'Allow fallback')}{f.text('account_pool_id', 'Account pool ID')}{f.bool('affinity', 'Enable affinity')}</>;
}

export type ResourceFormProps = { kind: string; value: ResourceData; onChange: (value: ResourceData) => void };
// Parent must pair kind with its corresponding DTO and remount on resource ID or
// tenant changes. This component owns fields, not a nested form or submit action.
export function ResourceForm({ kind, value, onChange }: ResourceFormProps) {
  switch (kind) {
    case 'tenants': {
      const f = fields(value as DTO['TenantData'], onChange as (value: DTO['TenantData']) => void);
      return <>{f.text('name', 'Tenant name', true)}{f.bool('enabled', 'Enabled')}{f.list('allowed_origins', 'Allowed browser origins')}
        {f.integer('max_body_bytes', 'Maximum request body bytes (0 = default)', true)}{f.integer('max_event_bytes', 'Maximum stream event bytes (0 = default)', true)}</>;
    }
    case 'operators': {
      const f = fields(value as DTO['OperatorData'], onChange as (value: DTO['OperatorData']) => void);
      return <>{f.text('subject', 'Operator subject / ID', true)}{f.text('issuer', 'Identity issuer', true)}{f.text('identity_subject', 'Identity subject (issuer sub claim)', true)}
        {f.text('display_name', 'Display name', true)}{f.bool('enabled', 'Enabled')}</>;
    }
    case 'role_bindings': {
      const f = fields(value as DTO['RoleBindingData'], onChange as (value: DTO['RoleBindingData']) => void);
      return <>{f.text('subject', 'Operator subject', true)}{f.text('tenant_id', 'Current tenant binding', true, true)}{f.select('role', 'Role', ['owner', 'admin', 'operator', 'auditor', 'viewer'])}</>;
    }
    case 'connections': {
      const f = fields(value as DTO['ConnectionData'], onChange as (value: DTO['ConnectionData']) => void);
      return <>{f.text('connector', 'Connector', true)}{f.text('account_id', 'Account ID', true)}{f.text('base_url', 'Base URL', true)}{f.text('region', 'Region')}{f.text('project', 'Project')}
        {f.bool('dedicated', 'Dedicated')}{f.bool('enabled', 'Enabled')}{f.map('settings', 'Connection settings')}</>;
    }
    case 'account_pools': {
      const f = fields(value as DTO['AccountPoolData'], onChange as (value: DTO['AccountPoolData']) => void);
      return <>{f.text('provider', 'Provider', true)}{f.list('account_ids', 'Account IDs')}</>;
    }
    case 'models': return <ModelFields label="Model" value={value as DTO['ModelData']} onChange={onChange as (value: DTO['ModelData']) => void} />;
    case 'model_aliases': {
      const f = fields(value as DTO['AliasData'], onChange as (value: DTO['AliasData']) => void);
      return <>{f.list('model_ids', 'Model IDs')}{f.text('description', 'Description')}{f.bool('enabled', 'Enabled')}</>;
    }
    case 'route_policies': return <RouteFields label="Route policy" value={value as DTO['RoutePolicyData']} onChange={onChange as (value: DTO['RoutePolicyData']) => void} />;
    case 'policy_limits': {
      const f = fields(value as DTO['PolicyLimitData'], onChange as (value: DTO['PolicyLimitData']) => void);
      return <>{f.select('scope', 'Policy scope', ['tenant', 'key', 'connection', 'account', 'model'])}{f.text('scope_id', 'Scope ID', true)}
        <p class="muted">Zero means no configured limit for these policy quantities.</p>
        {f.integer('requests_per_minute', 'Requests per minute', true)}{f.integer('tokens_per_minute', 'Tokens per minute', true)}
        {f.integer('max_cost', 'Maximum cost (integer nanodollars)', true)}{f.select('cost_window', 'Cost window', ['total', 'daily', 'monthly'])}
        {f.integer('concurrency', 'Concurrent requests', true)}{f.integer('outstanding_jobs', 'Outstanding jobs', true)}</>;
    }
    default: return <p role="note">This resource kind is read-only. No write form is available.</p>;
  }
}
