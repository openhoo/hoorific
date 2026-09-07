import { useEffect, useState } from 'preact/hooks';
import { LocationProvider, Router, Route } from 'preact-iso';
import { api, APIError, type ActionBody, type APIKeyGrantData, type APIKeyMetadataData, type CredentialActionData, type CredentialActionResult, type Resource, type ResourcePage, type Session } from './api';
import { ResourceForm, initialResourceData, isWritableResourceKind, type ResourceData } from './resource-forms';
import { Playground } from './playground';

const RESOURCE_KINDS = ['tenants','operators','role_bindings','connections','credentials','api_keys','account_pools','oauth_sessions','models','model_aliases','route_policies','policy_limits','usage_ledger','audit_events','upstream_operations','admissions','reconciliations'];
const LABELS: Record<string,string> = {tenants:'Tenants',operators:'Operators',role_bindings:'Role bindings',connections:'Connections',credentials:'Credentials',api_keys:'API keys',account_pools:'Account pools',oauth_sessions:'OAuth sessions',models:'Models',model_aliases:'Aliases',route_policies:'Routes',policy_limits:'Limits',usage_ledger:'Usage',audit_events:'Audit',upstream_operations:'Jobs',admissions:'Admissions',reconciliations:'Reconciliations'};
function requiredPermission(kind: string) { if (kind === 'tenants' || kind === 'operators' || kind === 'role_bindings') return 'tenant:read'; if (kind === 'audit_events') return 'audit:read'; if (kind === 'oauth_sessions') return 'session:read'; if (kind === 'usage_ledger' || kind === 'admissions' || kind === 'reconciliations') return 'usage:read'; if (kind === 'models' || kind === 'model_aliases' || kind === 'account_pools') return 'catalog:read'; if (kind === 'connections' || kind === 'credentials') return 'connection:read'; if (kind === 'api_keys') return 'key:read'; if (kind === 'route_policies') return 'route:read'; if (kind === 'policy_limits') return 'budget:read'; if (kind === 'upstream_operations') return 'job:read'; return 'resource:read'; }
function writePermission(kind: string) { return requiredPermission(kind).replace(':read', ':write'); }
function principalRole(session: Session) { return session.principal.Role; }
function permissionsFor(session: Session) { return session.permissions ?? session.principal.Permissions ?? []; }
function hasPermission(session: Session, permission: string) { const role = principalRole(session); const permissions = permissionsFor(session); return role === 'owner' || role === 'admin' || permissions.includes('*') || permissions.includes(permission); }
function canRead(session: Session, kind: string) { return hasPermission(session, requiredPermission(kind)) || (principalRole(session) === 'operator' && ['connections','models','model_aliases','upstream_operations'].includes(kind)); }
function canWrite(session: Session, kind: string) { return hasPermission(session, writePermission(kind)); }

function ErrorNotice({error, onClose}: {error: unknown; onClose?: () => void}) {
  if (!error) return null;
  const text = error instanceof Error ? error.message : String(error);
  return <div class="notice error" role="alert"><strong>Error:</strong> {text}{onClose && <button class="quiet" onClick={onClose} aria-label="Dismiss error">Dismiss</button>}</div>;
}
function Loading() { return <p class="muted" role="status">Loading…</p>; }
const CREDENTIAL_METADATA_FIELDS = ['connection_id', 'credential_id', 'provider', 'account_id', 'status', 'version', 'rotated_at'] as const;
function safeCredentialMetadata(data: unknown): Record<string, unknown> {
  if (!data || typeof data !== 'object') return {};
  const source = data as Record<string, unknown>;
  return Object.fromEntries(CREDENTIAL_METADATA_FIELDS.filter(field => Object.prototype.hasOwnProperty.call(source, field)).map(field => [field, source[field]]));
}
function summarize(kind: string, data: unknown) {
  if (!data || typeof data !== 'object') return String(data ?? '');
  const obj = kind === 'credentials' ? safeCredentialMetadata(data) : data as Record<string, unknown>;
  return Object.keys(obj).slice(0, 3).map(k => `${k}: ${String(obj[k])}`).join(' · ');
}
function ResourceTable({kind, page, selected, onSelect}: {kind: string; page: ResourcePage; selected?: Resource; onSelect: (r: Resource) => void}) {
  return <div class="table-wrap"><table><caption class="sr-only">{LABELS[kind] ?? kind}</caption><thead><tr><th scope="col">ID</th><th scope="col">Version</th><th scope="col">Summary</th></tr></thead><tbody>{page.items.map(item => <tr key={item.id} class={selected?.id === item.id ? 'selected' : ''}><td><button class="table-link" onClick={() => onSelect(item)}>{item.id}</button></td><td>{item.version}</td><td>{summarize(kind, item.data)}</td></tr>)}</tbody></table>{!page.items.length && <p class="empty">No resources returned.</p>}</div>;
}

type KeyMetadataChange = { id: string; resource?: Resource };
function ResourceView({kind, onSelected, onSession, keyChange}: {kind: string; onSelected?: (r?: Resource) => void; onSession?: (next: Session) => void; keyChange?: KeyMetadataChange}) {
  const [page, setPage] = useState<ResourcePage>(); const [cursor, setCursor] = useState<string>(); const [selected, setSelected] = useState<Resource>();
  const tenantID = api.session?.principal.TenantID ?? '';
  const writable = isWritableResourceKind(kind) && canWrite(api.session!, kind);
  const [draft, setDraft] = useState<ResourceData | undefined>(() => writable ? initialResourceData(kind, tenantID) as ResourceData : undefined);
  const [newID, setNewID] = useState(''); const [error, setError] = useState<unknown>(); const [saving, setSaving] = useState(false); const [notice, setNotice] = useState(''); const [conflict, setConflict] = useState<Resource>();
  const load = async (next?: string, resetSelection = false) => { setError(undefined); try { const result = await api.list(kind, next); setPage(result); setCursor(result.next_cursor); if (resetSelection) { setSelected(undefined); onSelected?.(undefined); } } catch (e) { setError(e); } };
  useEffect(() => { void load(undefined, true); }, [kind, tenantID]);
  useEffect(() => {
    if (kind !== 'api_keys' || !keyChange) return;
    setPage(previous => previous && ({...previous, items: keyChange.resource ? previous.items.map(row => row.id === keyChange.id ? keyChange.resource! : row) : previous.items.filter(row => row.id !== keyChange.id)}));
    setSelected(previous => previous?.id === keyChange.id ? keyChange.resource : previous);
  }, [keyChange]);
  const choose = (r: Resource) => { setSelected(r); if (writable) setDraft(r.data as ResourceData); setNotice(''); onSelected?.(r); };
  const save = async () => {
    if (!writable || !draft) return;
    setSaving(true); setError(undefined);
    try {
      if (selected) { const saved = await api.update(kind, selected, draft); setSelected(saved); onSelected?.(saved); setDraft(saved.data as ResourceData); setConflict(undefined); setNotice('Saved.'); }
      else { if (!newID.trim()) throw new Error('An ID is required.'); const saved = await api.create(kind, newID.trim(), draft); setSelected(saved); onSelected?.(saved); setDraft(saved.data as ResourceData); setNewID(''); setNotice('Created.'); if (kind === 'tenants' && onSession) onSession(await api.loadSession()); }
      await load();
    } catch (e) { setError(e); if (e instanceof APIError && e.status === 412 && selected) { setNotice('This resource changed on the server. Your draft is retained.'); try { setConflict(await api.get(kind, selected.id)); } catch (failure) { setError(failure); } } }
    finally { setSaving(false); }
  };
  const remove = async () => { if (!writable || !selected || !window.confirm(`Delete ${selected.id}?`)) return; setSaving(true); setError(undefined); try { await api.remove(kind, selected); setSelected(undefined); onSelected?.(undefined); setDraft(initialResourceData(kind, tenantID) as ResourceData); await load(); } catch (e) { setError(e); if (e instanceof APIError && e.status === 412) setNotice('Delete rejected because the version is stale; your editor is retained.'); } finally { setSaving(false); } };
  return <section class="resource"><div class="section-head"><div><h2>{LABELS[kind] ?? kind}</h2><p class="muted">Server-owned resources, paginated and versioned.</p></div><button onClick={() => void load()} disabled={!page}>Refresh</button></div><ErrorNotice error={error} /><p class="notice" role="status">{notice}</p><ResourceTable kind={kind} page={page ?? {items: []}} selected={selected} onSelect={choose} />{cursor && <button type="button" class="secondary" onClick={() => void load(cursor)}>Next page</button>}{kind === 'api_keys' && hasPermission(api.session!, 'key:write') && <APIKeyIssuePanel tenantID={tenantID} onIssued={() => void load(undefined, true)} />}{selected && conflict && <div class="conflict notice"><strong>Server version {conflict.version}</strong><div class="conflict-grid"><pre>{JSON.stringify(conflict.data,null,2)}</pre><pre>{JSON.stringify(draft,null,2)}</pre></div><button type="button" class="secondary" onClick={() => { setSelected(conflict); if (writable) setDraft(conflict.data as ResourceData); setConflict(undefined); setNotice('Reloaded server version.'); }}>Reload server version</button></div>}{writable && <form class="editor" onSubmit={e => { e.preventDefault(); void save(); }}><h3>{selected ? `Edit ${selected.id}` : `New ${LABELS[kind] ?? kind}`}</h3>{selected && <p class="muted">Version {selected.version}. Writes use an If-Match version check.</p>}{!selected && <label class="field"><span>ID</span><input value={newID} required onInput={e => setNewID(e.currentTarget.value)} /></label>}{draft && <ResourceForm kind={kind} value={draft} onChange={setDraft} />}<div class="form-actions"><button disabled={saving}>{saving ? 'Saving…' : selected ? 'Save changes' : 'Create'}</button>{selected && <button type="button" class="danger" onClick={() => void remove()} disabled={saving}>Delete</button>}</div></form>}{kind === 'credentials' && selected && <div class="metadata"><h3>Credential metadata</h3><p class="muted">Credential records are read-only encrypted-store metadata. Import, rotation, revocation, and authorization are performed from the owning connection.</p><pre>{JSON.stringify(safeCredentialMetadata(selected.data),null,2)}</pre></div>}{kind === 'api_keys' && selected && <div class="metadata"><h3>API key metadata</h3><p class="muted">Metadata is read-only. Issue, rotate, and revoke use operational endpoints; secrets are never returned by list/get.</p><pre>{JSON.stringify(selected.data as APIKeyMetadataData,null,2)}</pre></div>}</section>;
}

function parseEntries(value: string) { return value.split(/[,\s]+/).map(entry => entry.trim()).filter(Boolean); }
function APIKeyIssuePanel({tenantID, onIssued}: {tenantID: string; onIssued: () => void}) {
  const [id, setID] = useState(''); const [name, setName] = useState(''); const [role, setRole] = useState<APIKeyGrantData['role']>('viewer'); const [permissions, setPermissions] = useState(''); const [aliases, setAliases] = useState(''); const [connections, setConnections] = useState(''); const [operations, setOperations] = useState(''); const [portable, setPortable] = useState(false); const [nativeAccount, setNativeAccount] = useState(false); const [realtime, setRealtime] = useState(false); const [result, setResult] = useState<unknown>(); const [error, setError] = useState<unknown>(); const [busy, setBusy] = useState(false);
  const issue = async (event: Event) => {
    event.preventDefault(); setBusy(true); setError(undefined); setResult(undefined);
    const data = {name: name.trim(), role, permissions: parseEntries(permissions), aliases: parseEntries(aliases), connections: parseEntries(connections), operations: parseEntries(operations), portable, native_account: nativeAccount, realtime} as APIKeyGrantData;
    try { setResult(await api.issueAPIKey(id.trim(), data)); setID(''); setName(''); setPermissions(''); setAliases(''); setConnections(''); setOperations(''); setPortable(false); setNativeAccount(false); setRealtime(false); onIssued(); }
    catch (failure) { setError(failure); }
    finally { setBusy(false); }
  };
  return <section class="actions key-issue"><h3>Issue API key</h3><p class="muted">The token is returned once and is not stored in resource metadata.</p><ErrorNotice error={error} onClose={() => setError(undefined)} /><form onSubmit={issue}><label class="field"><span>Key ID</span><input required value={id} onInput={e => setID(e.currentTarget.value)} /></label><label class="field"><span>Name</span><input required value={name} onInput={e => setName(e.currentTarget.value)} /></label><label class="field"><span>Role</span><select value={role} onChange={e => setRole(e.currentTarget.value as APIKeyGrantData['role'])}><option value="viewer">viewer</option><option value="operator">operator</option><option value="admin">admin</option></select></label><label class="field"><span>Permissions (space or comma separated)</span><input required value={permissions} onInput={e => setPermissions(e.currentTarget.value)} /></label><label class="field"><span>Aliases (space or comma separated)</span><input value={aliases} onInput={e => setAliases(e.currentTarget.value)} /></label><label class="field"><span>Connections (space or comma separated)</span><input value={connections} onInput={e => setConnections(e.currentTarget.value)} /></label><label class="field"><span>Operations (exact operation names, space or comma separated)</span><input required value={operations} onInput={e => setOperations(e.currentTarget.value)} /></label><label class="field"><span>Tenant</span><input value={tenantID} readOnly /></label><label class="field"><span><input type="checkbox" checked={portable} onChange={e => setPortable(e.currentTarget.checked)} /> Portable</span></label><label class="field"><span><input type="checkbox" checked={nativeAccount} onChange={e => setNativeAccount(e.currentTarget.checked)} /> Native account</span></label><label class="field"><span><input type="checkbox" checked={realtime} onChange={e => setRealtime(e.currentTarget.checked)} /> Realtime</span></label><button disabled={busy || !id.trim() || !name.trim() || !permissions.trim() || !operations.trim()}>{busy ? 'Issuing…' : 'Issue API key'}</button></form><ActionResult value={result} error={undefined} onDismiss={() => setResult(undefined)} /></section>;
}
type CredentialForm = {
  secret: string;
  rotation_secret: string;
  credential_version: string;
  state: string;
  code: string;
  redirect_uri: string;
  flow_id: string;
};
const blankCredential = (): CredentialForm => ({
  secret: '',
  rotation_secret: '',
  credential_version: '',
  state: '',
  code: '',
  redirect_uri: '',
  flow_id: '',
});
function ActionResult({value,error,onDismiss}: {value: unknown; error: unknown; onDismiss?: () => void}) {
  return <>{error && <ErrorNotice error={error} />}{value !== undefined && <div class="result-wrap"><button type="button" class="quiet" onClick={onDismiss} aria-label="Dismiss result">Dismiss result</button><pre class="result">{JSON.stringify(value,null,2)}</pre></div>}</>;
}
function ConnectionCredentialActions({item, initialCredentialStatus}: {item: Resource; initialCredentialStatus?: string}) {
  const data = item.data as Record<string, unknown>;
  const provider = String(data.connector ?? '');
  const accountID = String(data.account_id ?? '');
  const [form, setForm] = useState<CredentialForm>(blankCredential);
  const [result, setResult] = useState<CredentialActionResult>();
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const [credentialRevoked, setCredentialRevoked] = useState(() => (initialCredentialStatus ?? String(data.status ?? '')) === 'revoked');
  const [oauthAuthorizationURL, setOAuthAuthorizationURL] = useState('');
  const [deviceAuthorizationURL, setDeviceAuthorizationURL] = useState('');
  const [deviceUserCode, setDeviceUserCode] = useState('');
  const canManage = hasPermission(api.session!, 'connection:write');
  const invoke = async (action: 'status' | 'import' | 'revoke-credential' | 'oauth-start' | 'oauth-callback' | 'device-start' | 'device-poll', extra: Partial<CredentialActionData> = {}) => {
    setBusy(true);
    setError(undefined);
    setResult(undefined);
    const payload: CredentialActionData = {
      provider,
      account_id: accountID,
      credential_version: form.credential_version ? Number(form.credential_version) : undefined,
      ...extra,
    };
    try {
      const next = await api.credentialAction(item, action, payload);
      setResult(next);
      if (next.status === 'revoked') setCredentialRevoked(true);
      else if (next.status === 'active' || next.status === 'requires_credentials') setCredentialRevoked(false);
      if (action === 'oauth-start') setOAuthAuthorizationURL(next.authorization_url || next.authorization_url_complete || '');
      if (action === 'device-start') {
        setDeviceAuthorizationURL(next.authorization_url_complete || next.authorization_url || '');
        setDeviceUserCode(next.user_code || '');
      }
      setForm(previous => {
        const authorization = next.authorization_url || next.authorization_url_complete;
        let state = previous.state;
        let redirectURI = previous.redirect_uri;
        if (action === 'oauth-start' && authorization) {
          try {
            const url = new URL(authorization);
            state = url.searchParams.get('state') || state;
            redirectURI = url.searchParams.get('redirect_uri') || redirectURI;
          } catch {
            // The provider URL is still displayed for manual recovery.
          }
        }
        return {
          ...previous,
          credential_version: next.version ? String(next.version) : previous.credential_version,
          flow_id: next.flow_id || previous.flow_id,
          state: action === 'oauth-callback' ? '' : state,
          redirect_uri: redirectURI,
          secret: '',
          rotation_secret: '',
          code: '',
        };
      });
    } catch (failure) {
      setError(failure);
      setForm(previous => ({...previous, secret: '', rotation_secret: '', state: action === 'oauth-callback' ? '' : previous.state, code: ''}));
    } finally {
      setBusy(false);
    }
  };
  const revoke = () => {
    if (credentialRevoked || !window.confirm(`Revoke the credential for ${provider} / ${accountID}? New requests will fail until a replacement is authorized.`)) return;
    void invoke('revoke-credential');
  };
  return <aside class="actions credential-actions">
    <h3>Credential lifecycle</h3>
    <p class="muted">{provider} / {accountID}. Secrets are write-only and are cleared after every attempt.</p>
    <button type="button" disabled={busy} onClick={() => void invoke('status')}>Refresh encrypted-store metadata</button>
    {canManage && <>
      <label class="field"><span>Current credential version</span><input type="number" min="1" inputMode="numeric" value={form.credential_version} onInput={e => setForm({...form,credential_version:e.currentTarget.value})} /><small>Required when replacing or revoking an existing credential. Refresh metadata to fill it automatically.</small></label>
      <fieldset>
        <legend>Import or replace API key</legend>
        <p class="muted">Manual token import is supported only for API-key connections. Use the configured OAuth or device authorization controls below for OAuth credentials.</p>
        <label class="field"><span>New API key</span><input type="password" autoComplete="new-password" value={form.secret} onInput={e => setForm({...form,secret:e.currentTarget.value})} /></label>
        <button type="button" disabled={busy || !form.secret} onClick={() => void invoke('import',{kind:'api_key',secret:form.secret})}>Import API key</button>
        <label class="field"><span>Replacement API key</span><input type="password" autoComplete="new-password" value={form.rotation_secret} onInput={e => setForm({...form,rotation_secret:e.currentTarget.value})} /><small>Rotation is a version-checked import bound to this connection.</small></label>
        <button type="button" disabled={busy || !form.rotation_secret || !form.credential_version} onClick={() => void invoke('import',{kind:'api_key',secret:form.rotation_secret})}>Rotate API key</button>
      </fieldset>
      <fieldset>
        <legend>OAuth authorization</legend>
        <button type="button" disabled={busy} onClick={() => void invoke('oauth-start')}>Start OAuth</button>
        {oauthAuthorizationURL && <p><a href={oauthAuthorizationURL} target="_blank" rel="noreferrer">Continue authorization with provider</a></p>}
        <label class="field"><span>OAuth state</span><input autoComplete="off" value={form.state} onInput={e => setForm({...form,state:e.currentTarget.value})} /></label>
        <label class="field"><span>OAuth callback code</span><input autoComplete="off" value={form.code} onInput={e => setForm({...form,code:e.currentTarget.value})} /></label>
        <label class="field"><span>Registered redirect URI</span><input type="url" value={form.redirect_uri} onInput={e => setForm({...form,redirect_uri:e.currentTarget.value})} /><small>Must exactly match the provider registration configured for this connection.</small></label>
        <button type="button" disabled={busy || !form.state || !form.code || !form.redirect_uri} onClick={() => void invoke('oauth-callback',{state:form.state,code:form.code,redirect_uri:form.redirect_uri})}>Complete OAuth callback</button>
      </fieldset>
      <fieldset>
        <legend>Device authorization</legend>
        <button type="button" disabled={busy} onClick={() => void invoke('device-start')}>Start device flow</button>
        {deviceUserCode && <p role="status">Provider device code: <strong>{deviceUserCode}</strong></p>}
        {deviceAuthorizationURL && <p><a href={deviceAuthorizationURL} target="_blank" rel="noreferrer">Open device verification</a></p>}
        <label class="field"><span>Device flow ID</span><input value={form.flow_id} onInput={e => setForm({...form,flow_id:e.currentTarget.value})} /><small>Filled automatically when the provider starts a device flow.</small></label>
        <button type="button" disabled={busy || !form.flow_id} onClick={() => void invoke('device-poll',{flow_id:form.flow_id})}>Poll device flow</button>
      </fieldset>
      <fieldset class="danger-zone">
        <legend>Destructive action</legend>
        <p class="muted">Revocation is version checked and prevents new requests from leasing this credential.</p>
        <button type="button" class="danger" disabled={busy || credentialRevoked || !form.credential_version} onClick={revoke}>{credentialRevoked ? 'Credential revoked' : 'Revoke credential'}</button>
      </fieldset>
    </>}
    <ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} />
  </aside>;
}
function CredentialMetadataActions({item}: {item: Resource}) {
  const metadata = safeCredentialMetadata(item.data);
  const connectionID = String(metadata.connection_id ?? '');
  const tenantID = api.session?.principal.TenantID ?? '';
  const [connection, setConnection] = useState<Resource>();
  const [error, setError] = useState<unknown>();
  useEffect(() => {
    let active = true;
    setConnection(undefined);
    setError(undefined);
    if (!connectionID) return () => { active = false; };
    void api.get('connections', connectionID).then(next => {
      if (next.tenant_id !== tenantID) throw new Error('Credential connection belongs to another tenant.');
      if (active) setConnection(next);
    }).catch(failure => { if (active) setError(failure); });
    return () => { active = false; };
  }, [connectionID, tenantID]);
  if (!connectionID) return <aside class="actions"><h3>Credential lifecycle</h3><p class="muted">This credential has no owning connection metadata.</p></aside>;
  if (error) return <aside class="actions"><h3>Credential lifecycle</h3><ErrorNotice error={error} /></aside>;
  if (!connection) return <aside class="actions"><h3>Credential lifecycle</h3><Loading /></aside>;
  return <ConnectionCredentialActions item={connection} initialCredentialStatus={String(metadata.status ?? '')} />;
}
function ActionPanel({kind, item, onKeyChanged}: {kind: string; item: Resource; onKeyChanged: (change: KeyMetadataChange) => void}) {
  const [result, setResult] = useState<unknown>(); const [error, setError] = useState<unknown>(); const [busy, setBusy] = useState(false); const [keyRevoked, setKeyRevoked] = useState(false); const [currentKey, setCurrentKey] = useState(item); const [reconcile, setReconcile] = useState({reconciliation_id:'', mode:'provider_evidence', reason:'', source_reference:'', cost:'', input:'', output:'', total:'', source:''});
  const invoke = async (path: string, data?: ActionBody) => { setBusy(true); setError(undefined); try { setResult(await api.action(path, data, item.version)); } catch (e) { setError(e); } finally { setBusy(false); } };
  const rotateKey = async () => { setBusy(true); setError(undefined); setResult(undefined); try { const issued = await api.rotateAPIKey(currentKey); setCurrentKey(issued.resource); setResult(issued); onKeyChanged({id: currentKey.id, resource: issued.resource}); } catch (e) { setError(e); } finally { setBusy(false); } };
  const revokeKey = async () => { if (!window.confirm(`Revoke API key ${currentKey.id}? This cannot be undone.`)) return; setBusy(true); setError(undefined); setResult(undefined); try { const revoked = await api.revokeAPIKey(currentKey); setKeyRevoked(true); setResult(revoked); onKeyChanged({id: currentKey.id}); } catch (e) { setError(e); } finally { setBusy(false); } };
  if (kind === 'credentials') return <CredentialMetadataActions item={item} />;
  if (kind === 'connections') return <><aside class="actions"><h3>Connection actions</h3>{hasPermission(api.session!, 'connection:test') && <button disabled={busy} onClick={() => void invoke(`/connections/${encodeURIComponent(item.id)}/test`)}>Test</button>}{hasPermission(api.session!, 'connection:discover') && <button disabled={busy} onClick={() => void invoke(`/connections/${encodeURIComponent(item.id)}/discover`)}>Discover models</button>}{hasPermission(api.session!, 'resource:write') && <button class="danger" disabled={busy} onClick={() => void invoke(`/connections/${encodeURIComponent(item.id)}/disable`)}>Disable</button>}<ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside><ConnectionCredentialActions item={item} /></>;
  if (kind === 'api_keys' && hasPermission(api.session!, 'key:write')) return <aside class="actions"><h3>Key actions</h3><button disabled={busy || keyRevoked} onClick={() => void rotateKey()}>Rotate</button><button class="danger" disabled={busy || keyRevoked} onClick={() => void revokeKey()}>Revoke</button>{keyRevoked && <p class="muted" role="status">This key is revoked.</p>}<ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside>;
  if (kind === 'route_policies' && hasPermission(api.session!, 'route:write')) return <aside class="actions"><h3>Route analysis</h3><button disabled={busy} onClick={() => void invoke(`/route_policies/${encodeURIComponent(item.id)}/dry-run`, {data:item.data})}>Explain dry-run</button><ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside>;
  if (kind === 'upstream_operations' && hasPermission(api.session!, 'job:write')) return <aside class="actions"><h3>Job actions</h3><button class="danger" disabled={busy} onClick={() => void invoke(`/upstream_operations/${encodeURIComponent(item.id)}/cancel`)}>Request cancellation</button><ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside>;
  if (kind === 'admissions' && hasPermission(api.session!, 'accounting:reconcile')) return <aside class="actions"><h3>Reconcile admission</h3><label class="field"><span>Reconciliation ID</span><input value={reconcile.reconciliation_id} onInput={e => setReconcile({...reconcile,reconciliation_id:e.currentTarget.value})} /></label><label class="field"><span>Mode</span><select value={reconcile.mode} onChange={e => setReconcile({...reconcile,mode:e.currentTarget.value})}><option value="provider_evidence">Provider evidence</option><option value="charge_reserved_maximum">Charge reserved maximum</option></select></label><label class="field"><span>Reason</span><textarea value={reconcile.reason} onInput={e => setReconcile({...reconcile,reason:e.currentTarget.value})} /></label><label class="field"><span>Source reference</span><input value={reconcile.source_reference} onInput={e => setReconcile({...reconcile,source_reference:e.currentTarget.value})} /></label><label class="field"><span>Cost (nanodollars)</span><input inputMode="numeric" value={reconcile.cost} onInput={e => setReconcile({...reconcile,cost:e.currentTarget.value})} /></label><fieldset><legend>Usage</legend><label class="field"><span>Input tokens</span><input inputMode="numeric" value={reconcile.input} onInput={e => setReconcile({...reconcile,input:e.currentTarget.value})} /></label><label class="field"><span>Output tokens</span><input inputMode="numeric" value={reconcile.output} onInput={e => setReconcile({...reconcile,output:e.currentTarget.value})} /></label><label class="field"><span>Total tokens</span><input inputMode="numeric" value={reconcile.total} onInput={e => setReconcile({...reconcile,total:e.currentTarget.value})} /></label><label class="field"><span>Usage source</span><input value={reconcile.source} onInput={e => setReconcile({...reconcile,source:e.currentTarget.value})} /></label></fieldset><button disabled={busy || !reconcile.reconciliation_id || !reconcile.reason} onClick={() => void invoke(`/admissions/${encodeURIComponent(item.id)}/reconcile`,{reconciliation_id:reconcile.reconciliation_id,mode:reconcile.mode,reason:reconcile.reason,source_reference:reconcile.source_reference || undefined,cost:reconcile.cost === '' ? undefined : Number(reconcile.cost),usage:{Input:reconcile.input === '' ? null : Number(reconcile.input),Output:reconcile.output === '' ? null : Number(reconcile.output),Total:reconcile.total === '' ? null : Number(reconcile.total),Source:reconcile.source}})}>Reconcile</button><ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside>;
  return null;
}
function ResourceRoute({kind, onSession}: {kind: string; onSession?: (next: Session) => void}) {
  const [selected, setSelected] = useState<Resource>();
  const [keyChange, setKeyChange] = useState<KeyMetadataChange>();
  const keyChanged = (change: KeyMetadataChange) => { setKeyChange(change); if (change.resource) setSelected(change.resource); };
  return <><ResourceView kind={kind} onSelected={setSelected} onSession={onSession} keyChange={keyChange} />{selected && <ActionPanel key={kind === 'api_keys' ? selected.id : `${selected.id}:${selected.version}`} kind={kind} item={selected} onKeyChanged={keyChanged} />}</>;
}

function TenantSelector({session, onChanged}: {session: Session; onChanged: (next: Session) => void}) {
  const tenants = (session.tenants ?? []).filter(t => t.enabled); const current = session.principal.TenantID; const [switching, setSwitching] = useState(false); const [error, setError] = useState<unknown>();
  if (tenants.length < 2) return null;
  return <><label class="tenant-selector"><span>Tenant</span><select value={current} disabled={switching} onChange={e => { const id = e.currentTarget.value; if (id === current) return; setSwitching(true); setError(undefined); void api.selectTenant(id).then(next => { onChanged(next); }).catch(setError).finally(() => setSwitching(false)); }}>{tenants.map(t => <option key={t.tenant_id} value={t.tenant_id}>{t.name || t.tenant_id} ({t.role})</option>)}</select>{switching && <small role="status">Switching…</small>}</label><ErrorNotice error={error} onClose={() => setError(undefined)} /></>;
}
function Overview({visible}: {visible: string[]}) { return <section><h1>Operations overview</h1><p>Manage only the resources granted to this session. Every write is version-checked by the server.</p><div class="cards">{visible.slice(0, 6).map(kind => <a class="card" href={`/admin/${kind}`} key={kind}><strong>{LABELS[kind] ?? kind}</strong><span>Open collection</span></a>)}</div></section>; }
function AdminTokenPanel() {
  const [subject,setSubject] = useState(''); const [permissions,setPermissions] = useState(''); const [expiresAt,setExpiresAt] = useState(''); const [hash,setHash] = useState(''); const [result,setResult] = useState<unknown>(); const [error,setError] = useState<unknown>(); const [busy,setBusy] = useState(false);
  if (api.session?.principal.Role !== 'owner') return null;
  const issue = async (event: Event) => { event.preventDefault(); setBusy(true); setError(undefined); setResult(undefined); try { const expiry = new Date(expiresAt).toISOString(); setResult(await api.issueAdminToken(subject.trim(), permissions.split(/[ ,]+/).filter(Boolean), expiry)); setSubject(''); setPermissions(''); setExpiresAt(''); } catch (failure) { setError(failure); } finally { setBusy(false); } };
  const revoke = async (event: Event) => { event.preventDefault(); setBusy(true); setError(undefined); try { await api.revokeAdminToken(hash.trim()); setHash(''); setResult({status:'revoked'}); } catch (failure) { setError(failure); } finally { setBusy(false); } };
  return <section class="token-admin"><h2>Scoped admin tokens</h2><p class="muted">Owner-only. The issued secret is shown once and is held only in this page until dismissed.</p><ErrorNotice error={error} onClose={() => setError(undefined)} /><form onSubmit={issue}><label class="field"><span>Subject</span><input required value={subject} onInput={e=>setSubject(e.currentTarget.value)} /></label><label class="field"><span>Permissions (space or comma separated)</span><input required value={permissions} onInput={e=>setPermissions(e.currentTarget.value)} /></label><label class="field"><span>Expires at</span><input required type="datetime-local" value={expiresAt} onInput={e=>setExpiresAt(e.currentTarget.value)} /></label><button disabled={busy}>Issue scoped token</button></form><form onSubmit={revoke}><label class="field"><span>Token hash to revoke</span><input required value={hash} onInput={e=>setHash(e.currentTarget.value)} /></label><button type="submit" class="danger" disabled={busy}>Revoke token</button></form>{result !== undefined && <div class="result-wrap"><button type="button" class="quiet" onClick={()=>setResult(undefined)} aria-label="Dismiss token result">Dismiss result</button><pre class="result">{JSON.stringify(result,null,2)}</pre></div>}</section>;
}
function ConfigPage() { const [text,setText]=useState('{}'); const [revision,setRevision]=useState('0'); const [result,setResult]=useState<unknown>(); const [error,setError]=useState<unknown>(); const [busy,setBusy]=useState(false); const act=async(path:string)=>{setBusy(true);setError(undefined);try{setResult(await api.action(path,{expected_revision:Number(revision),config:JSON.parse(text)}));}catch(e){setError(e);}finally{setBusy(false);}}; return <section><h1>Configuration</h1><p class="muted">Preview changes before applying. Export contains references and redacted metadata only.</p><ErrorNotice error={error}/><label class="field"><span>Expected configuration revision</span><input type="number" min="0" value={revision} onInput={e=>setRevision(e.currentTarget.value)}/></label><label class="field"><span>Configuration JSON</span><textarea value={text} onInput={e=>setText(e.currentTarget.value)}/></label><div class="form-actions"><button disabled={busy} onClick={()=>void act('/config/diff')}>Preview changes</button><button disabled={busy} onClick={()=>void act('/config/apply')}>Apply</button><button onClick={()=>void api.request('/config/export').then(setResult).catch(setError)}>Export</button></div>{result !== undefined && <pre class="result">{JSON.stringify(result,null,2)}</pre>}<AdminTokenPanel /></section>; }
function NotFound() { return <section><h1>Not found</h1><p>The requested console route does not exist or is not enabled for this session.</p></section>; }
function Login({onSession}: {onSession: (s: Session) => void}) { const [code,setCode]=useState(''); const [error,setError]=useState<unknown>(); const [busy,setBusy]=useState(false); return <main class="login"><div class="login-card"><h1>Hoorific operations</h1><p>Sign in with the configured identity provider, or redeem the one-time local bootstrap code.</p><a class="button" href="/admin/api/v1/auth/login">Sign in with OIDC</a><div class="separator">or bootstrap locally</div><form onSubmit={e=>{e.preventDefault();setBusy(true);api.bootstrap(code).then(()=>api.loadSession()).then(onSession).catch(setError).finally(()=>setBusy(false));}}><label class="field"><span>Bootstrap code</span><input value={code} onInput={e=>setCode(e.currentTarget.value)} autoComplete="one-time-code" required /></label><button disabled={busy}>{busy?'Redeeming…':'Redeem code'}</button></form><ErrorNotice error={error}/></div></main>; }
function Dashboard({session, onChanged}: {session: Session; onChanged: (next: Session) => void}) {
  const visible = RESOURCE_KINDS.filter(kind => canRead(session, kind)); const permissions = permissionsFor(session); const role = principalRole(session); const canPlayground = role === 'owner' || role === 'admin' || permissions.includes('*') || permissions.includes('playground:execute'); const canConfig = role === 'owner' || role === 'admin' || permissions.includes('*') || permissions.includes('config:read'); const tenantID = session.principal.TenantID;
  return <div class="layout"><aside class="sidebar"><div class="brand">Hoorific</div><p class="muted">{role || 'operator'}</p><TenantSelector session={session} onChanged={onChanged}/><nav aria-label="Operations"><a href="/admin/">Overview</a>{visible.map(kind => <a key={kind} href={`/admin/${kind}`}>{LABELS[kind] ?? kind}</a>)}{canPlayground && <a href="/admin/playground">Playground</a>}{canConfig && <a href="/admin/config">Configuration</a>}</nav><button class="secondary" onClick={() => void api.logout().then(() => window.location.reload())}>Sign out</button></aside><main class="main"><div key={tenantID}><Router><Route path="/admin" component={() => <Overview visible={visible}/>} /><Route path="/admin/" component={() => <Overview visible={visible}/>} />{visible.map(kind => <Route key={kind} path={`/admin/${kind}`} component={() => <ResourceRoute kind={kind} onSession={onChanged}/>} />)}<Route path="/admin/playground" component={canPlayground ? Playground : NotFound}/><Route path="/admin/config" component={canConfig ? ConfigPage : NotFound}/><Route default component={NotFound}/></Router></div></main></div>;
}
export function App() { const [session,setSession]=useState<Session>(); const [error,setError]=useState<unknown>(); useEffect(()=>{api.loadSession().then(s=>{api.session=s;setSession(s);}).catch(setError);},[]); if (error instanceof APIError && (error.status===401 || error.status===403)) return <LocationProvider><Login onSession={s=>{api.session=s;setSession(s);setError(undefined);}}/></LocationProvider>; if (error) return <ErrorNotice error={error}/>; if (!session) return <Loading/>; api.session=session; return <LocationProvider><Dashboard key={session.principal.TenantID} session={session} onChanged={next=>{api.session=next;setSession(next);}}/></LocationProvider>; }
