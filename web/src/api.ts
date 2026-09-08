import type { components, paths } from './generated/api';

type Schemas = components['schemas'];
export type Principal = Schemas['Principal'];
export type Session = Schemas['SessionData'];
type ResourceSchemaName = Exclude<Extract<keyof Schemas, `TypedResource${string}Data`>, `TypedResourceEnvelope${string}` | `TypedResourcePage${string}` | 'TypedResourceData' | 'TypedResourceCredentialData'>;
export type Resource = Schemas[ResourceSchemaName];
export type ResourceInput = { id: string; data: Resource['data'] };
export type ResourcePage = { items: Resource[]; next_cursor?: string };
export type CredentialActionData = Schemas['CredentialActionData'];
export type CredentialActionResult = Schemas['CredentialActionResult'];
export type APIKeyGrantData = Schemas['APIKeyGrantData'];
export type APIKeyMetadataData = Schemas['APIKeyMetadataData'];
export type APIKeyIssuedData = Schemas['APIKeyIssuedData'];
export type APIKeyRevokedData = Schemas['APIKeyRevokedData'];
export type CredentialAction = 'status' | 'import' | 'revoke-credential' | 'oauth-start' | 'oauth-callback' | 'device-start' | 'device-poll';
export type Reconciliation = Schemas['Reconciliation'];
export type ActionInput = Schemas['ActionInput'];
export type ActionBody = ActionInput | Schemas['Reconciliation'];
export type ActionResult = unknown;
type HTTPMethod = NonNullable<RequestInit['method']>;
const mutation = new Set<HTTPMethod>(['POST', 'PUT', 'DELETE']);

export class APIError extends Error {
  status: number;
  body: unknown;
  constructor(status: number, body: unknown) {
    const message = body && typeof body === 'object' && 'message' in body && typeof body.message === 'string' ? body.message : body && typeof body === 'object' && 'detail' in body && typeof body.detail === 'string' ? body.detail : `Request failed (${status})`;
    super(message);
    this.status = status;
    this.body = body;
  }
}

export class AdminAPI {
  session?: Session;
  constructor(private readonly base = '/admin/api/v1') {}
  async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const method = (init.method ?? 'GET').toUpperCase() as HTTPMethod;
    const headers = new Headers(init.headers);
    headers.set('Accept', 'application/json');
    if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    if (mutation.has(method) && this.session?.csrf_token) headers.set('X-CSRF-Token', this.session.csrf_token);
    if (mutation.has(method) && !headers.has('Origin')) headers.set('Origin', window.location.origin);
    // /session is deliberately context-free so a stale tab can explicitly
    // reload the server-selected tenant. Bootstrap and OIDC are pre-session
    // authentication; every other authenticated request is tenant-scoped.
    const contextFree = path === '/session' || path.startsWith('/auth/bootstrap') || path.startsWith('/auth/login') || path.startsWith('/auth/callback');
    if (!contextFree && this.session?.principal.TenantID) headers.set('X-Hoorific-Expected-Tenant', this.session.principal.TenantID);
    const response = await fetch(`${this.base}${path}`, { ...init, method, headers, credentials: 'include' });
    const text = await response.text();
    let body: unknown;
    if (text) { try { body = JSON.parse(text); } catch { body = text; } }
    if (!response.ok) throw new APIError(response.status, body);
    return body as T;
  }
  async loadSession() { this.session = await this.request<Session>('/session'); return this.session; }
  async bootstrap(code: string) { return this.request<Session>('/auth/bootstrap', {method: 'POST', body: JSON.stringify({code})}); }
  async logout() { return this.request('/auth/logout', {method: 'POST'}); }
  async selectTenant(tenantID: string) { this.session = await this.request<Session>('/session/tenant', {method:'POST', body:JSON.stringify({tenant_id:tenantID})}); return this.session; }
  async issueAdminToken(subject: string, permissions: string[], expiresAt: string) {
    const body: Schemas['AdminTokenIssueInputBody'] = {subject, permissions, expires_at: expiresAt};
    return this.request<Schemas['AdminTokenIssueData']>('/auth/tokens', {method:'POST', body:JSON.stringify(body)});
  }
  async revokeAdminToken(tokenHash: string) { return this.request<void>(`/auth/tokens/${encodeURIComponent(tokenHash)}`, {method:'DELETE'}); }
  async list(kind: string, cursor?: string, limit = 50) { const query = new URLSearchParams({limit: String(Math.min(200, Math.max(1, limit)))}); if (cursor) query.set('cursor', cursor); const page = await this.request<ResourcePage>(`/${encodeURIComponent(kind)}?${query}`); return {...page, items: page.items ?? []}; }
  async get(kind: string, id: string) { return this.request<Resource>(`/${encodeURIComponent(kind)}/${encodeURIComponent(id)}`); }
  async create(kind: string, id: string, data: ResourceInput['data']) { const body: ResourceInput = {id, data}; return this.request<Resource>(`/${encodeURIComponent(kind)}`, {method:'POST', body:JSON.stringify(body)}); }
  async update(kind: string, item: Resource, data: ResourceInput['data']) { const body: Pick<ResourceInput, 'data'> = {data}; return this.request<Resource>(`/${encodeURIComponent(kind)}/${encodeURIComponent(item.id)}`, {method:'PUT', headers:{'If-Match': `\"${item.version}\"`}, body:JSON.stringify(body)}); }
  async remove(kind: string, item: Resource) { return this.request<Resource>(`/${encodeURIComponent(kind)}/${encodeURIComponent(item.id)}`, {method:'DELETE', headers:{'If-Match': `\"${item.version}\"`}}); }
  async issueAPIKey(id: string, data: APIKeyGrantData) { return this.request<APIKeyIssuedData>(`/api_keys/${encodeURIComponent(id)}/issue`, {method:'POST', body:JSON.stringify({data})}); }
  async rotateAPIKey(item: Resource) { return this.action<APIKeyIssuedData>(`/api_keys/${encodeURIComponent(item.id)}/rotate`, {data:{}}, item.version); }
  async revokeAPIKey(item: Resource) { return this.action<APIKeyRevokedData>(`/api_keys/${encodeURIComponent(item.id)}/revoke`, {data:{}}, item.version); }
  async action<T = ActionResult>(path: string, data?: ActionBody, version?: number) { const headers: Record<string,string> = {}; if (version !== undefined) headers['If-Match'] = `\"${version}\"`; return this.request<T>(path, {method:'POST', headers, body:JSON.stringify(data ?? {data:{}})}); }
  async credentialAction(item: Resource, action: CredentialAction, data: CredentialActionData) {
    return this.action<CredentialActionResult>(`/connections/${encodeURIComponent(item.id)}/${action}`, {data}, action === 'status' ? undefined : item.version);
  }
  async stream(path: string, body: unknown, signal: AbortSignal, onChunk: (text: string) => void) { const headers = new Headers({'Content-Type':'application/json','Accept':'text/event-stream, application/json'}); if (this.session?.csrf_token) headers.set('X-CSRF-Token', this.session.csrf_token); headers.set('Origin', window.location.origin); if (this.session?.principal.TenantID) headers.set('X-Hoorific-Expected-Tenant', this.session.principal.TenantID); const response = await fetch(`${this.base}${path}`, {method:'POST', credentials:'include', headers, body:JSON.stringify(body), signal}); if (!response.ok) { const text = await response.text(); let body: unknown; if (text) { try { body = JSON.parse(text); } catch { body = text; } } throw new APIError(response.status, body); } if (!response.body) return; const reader = response.body.getReader(); const decoder = new TextDecoder(); try { while (true) { const next = await reader.read(); if (next.done) break; onChunk(decoder.decode(next.value, {stream:true})); } onChunk(decoder.decode()); } finally { reader.releaseLock(); } }
}
export type AdminPaths = paths;
export const api = new AdminAPI();
