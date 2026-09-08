import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from 'react';
import {
  BrowserRouter,
  Link,
  NavLink,
  Navigate,
  Routes,
  Route,
  useLocation,
} from 'react-router-dom';
import {
  Activity,
  AlertTriangle,
  ArrowRight,
  BookOpen,
  Boxes,
  Check,
  ChevronRight,
  CircleHelp,
  ClipboardCheck,
  CloudCog,
  Code2,
  Database,
  FileClock,
  KeyRound,
  LayoutDashboard,
  LogOut,
  Menu,
  Moon,
  Network,
  PanelLeftClose,
  PanelLeftOpen,
  RefreshCw,
  Route as RouteIcon,
  Server,
  Settings2,
  ShieldCheck,
  Sun,
  Users,
  X,
  Zap,
} from 'lucide-react';
import {
  api,
  APIError,
  type ActionBody,
  type APIKeyGrantData,
  type CredentialActionData,
  type CredentialActionResult,
  type Resource,
  type ResourcePage,
  type Session,
} from './api';
import {
  ResourceForm,
  initialResourceData,
  isWritableResourceKind,
  type ResourceData,
} from './resource-forms';
import { Playground } from './playground';
import { Alert, AlertDescription, AlertTitle } from './components/ui/alert';
import { Badge } from './components/ui/badge';
import {
  Button,
  buttonVariants,
} from './components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from './components/ui/card';
import { Input } from './components/ui/input';
import { NativeSelect } from './components/ui/select';
import { Separator } from './components/ui/separator';
import { Textarea } from './components/ui/textarea';
import { cn } from './lib/utils';

const RESOURCE_KINDS = [
  'tenants',
  'operators',
  'role_bindings',
  'connections',
  'credentials',
  'api_keys',
  'account_pools',
  'oauth_sessions',
  'models',
  'model_aliases',
  'route_policies',
  'policy_limits',
  'usage_ledger',
  'audit_events',
  'upstream_operations',
  'admissions',
  'reconciliations',
] as const;

type ResourceKind = (typeof RESOURCE_KINDS)[number];

const LABELS: Record<string, string> = {
  tenants: 'Tenants',
  operators: 'Operators',
  role_bindings: 'Role bindings',
  connections: 'Connections',
  credentials: 'Credentials',
  api_keys: 'API keys',
  account_pools: 'Account pools',
  oauth_sessions: 'OAuth sessions',
  models: 'Models',
  model_aliases: 'Aliases',
  route_policies: 'Routes',
  policy_limits: 'Limits',
  usage_ledger: 'Usage',
  audit_events: 'Audit',
  upstream_operations: 'Jobs',
  admissions: 'Admissions',
  reconciliations: 'Reconciliations',
};
const SINGULAR_LABELS: Record<string, string> = {
  tenants: 'tenant',
  operators: 'operator',
  role_bindings: 'role binding',
  connections: 'connection',
  credentials: 'credential',
  api_keys: 'API key',
  account_pools: 'account pool',
  oauth_sessions: 'OAuth session',
  models: 'model',
  model_aliases: 'alias',
  route_policies: 'route',
  policy_limits: 'limit',
  usage_ledger: 'usage entry',
  audit_events: 'audit event',
  upstream_operations: 'job',
  admissions: 'admission',
  reconciliations: 'reconciliation',
};

const RESOURCE_DESCRIPTIONS: Record<string, string> = {
  tenants: 'Define tenant boundaries, origins, and request limits.',
  operators: 'Manage operator identities allowed to work in this tenant.',
  role_bindings: 'Grant a subject a role for the active tenant.',
  connections: 'Register provider endpoints and the account they serve.',
  credentials: 'Review credential metadata without exposing secret values.',
  api_keys: 'Review tenant API-key metadata and issue keys when permitted.',
  account_pools: 'Group provider accounts for pooled routing.',
  oauth_sessions: 'Inspect provider authorization sessions and expiry.',
  models: 'Catalog provider models and the operations they support.',
  model_aliases: 'Give one or more cataloged models a stable route name.',
  route_policies: 'Choose targets and fallback behavior for an alias.',
  policy_limits: 'Set request, token, cost, and concurrency boundaries.',
  usage_ledger: 'Inspect server-recorded usage and cost entries.',
  audit_events: 'Review immutable administrative actions for this tenant.',
  upstream_operations: 'Inspect provider operations and their current status.',
  admissions: 'Inspect admitted requests and reconciliation state.',
  reconciliations: 'Review accounting corrections and provider evidence.',
};


const RESOURCE_GROUPS: Array<{
  label: string;
  icon: typeof Users;
  kinds: ResourceKind[];
}> = [
  { label: 'Connections', icon: Network, kinds: ['connections', 'credentials', 'oauth_sessions'] },
  { label: 'Catalog', icon: Boxes, kinds: ['account_pools', 'models', 'model_aliases'] },
  { label: 'Policy', icon: RouteIcon, kinds: ['route_policies', 'policy_limits'] },
  { label: 'Operations', icon: Activity, kinds: ['usage_ledger', 'audit_events', 'upstream_operations', 'admissions', 'reconciliations'] },
  { label: 'Access & identity', icon: KeyRound, kinds: ['api_keys', 'tenants', 'operators', 'role_bindings'] },
];

function requiredPermission(kind: string) {
  if (kind === 'tenants' || kind === 'operators' || kind === 'role_bindings') return 'tenant:read';
  if (kind === 'audit_events') return 'audit:read';
  if (kind === 'oauth_sessions') return 'session:read';
  if (kind === 'usage_ledger' || kind === 'admissions' || kind === 'reconciliations') return 'usage:read';
  if (kind === 'models' || kind === 'model_aliases' || kind === 'account_pools') return 'catalog:read';
  if (kind === 'connections' || kind === 'credentials') return 'connection:read';
  if (kind === 'api_keys') return 'key:read';
  if (kind === 'route_policies') return 'route:read';
  if (kind === 'policy_limits') return 'budget:read';
  if (kind === 'upstream_operations') return 'job:read';
  return 'resource:read';
}

function writePermission(kind: string) {
  return requiredPermission(kind).replace(':read', ':write');
}

function principalRole(session: Session) {
  return session.principal.Role;
}

function permissionsFor(session: Session) {
  return session.permissions ?? session.principal.Permissions ?? [];
}

function hasPermission(session: Session, permission: string) {
  const role = principalRole(session);
  const permissions = permissionsFor(session);
  return role === 'owner' || role === 'admin' || permissions.includes('*') || permissions.includes(permission);
}

function canRead(session: Session, kind: string) {
  return hasPermission(session, requiredPermission(kind)) || (
    principalRole(session) === 'operator' &&
    ['connections', 'models', 'model_aliases', 'upstream_operations'].includes(kind)
  );
}

function canWrite(session: Session, kind: string) {
  return hasPermission(session, writePermission(kind));
}
function hasResourceActions(kind: string, session: Session) {
  if (kind === 'connections' || kind === 'credentials') return true;
  if (kind === 'api_keys') return hasPermission(session, 'key:write');
  if (kind === 'route_policies') return hasPermission(session, 'route:write');
  if (kind === 'upstream_operations') return hasPermission(session, 'job:write');
  if (kind === 'admissions') return hasPermission(session, 'accounting:reconcile');
  return false;
}

function ErrorNotice({ error, onClose }: { error: unknown; onClose?: () => void }) {
  if (!error) return null;
  const text = error instanceof Error ? error.message : String(error);
  return (
    <Alert variant="destructive" className="notice error my-4">
      <AlertTriangle className="h-4 w-4" aria-hidden="true" />
      <AlertTitle>Error</AlertTitle>
      <AlertDescription className="flex flex-wrap items-center gap-3">
        <span className="break-words">{text}</span>
        {onClose && (
          <Button type="button" variant="ghost" size="sm" onClick={onClose} aria-label="Dismiss error">
            Dismiss
          </Button>
        )}
      </AlertDescription>
    </Alert>
  );
}

function Loading() {
  return (
    <main className="grid min-h-screen place-items-center bg-zinc-50 p-6 dark:bg-zinc-950">
      <div className="flex items-center gap-3 text-sm text-zinc-600 dark:text-zinc-300" role="status">
        <RefreshCw className="h-4 w-4 animate-spin" aria-hidden="true" />
        Loading console…
      </div>
    </main>
  );
}

const CREDENTIAL_METADATA_FIELDS = [
  'connection_id',
  'credential_id',
  'provider',
  'account_id',
  'status',
  'version',
  'rotated_at',
] as const;

function safeCredentialMetadata(data: unknown): Record<string, unknown> {
  if (!data || typeof data !== 'object') return {};
  const source = data as Record<string, unknown>;
  return Object.fromEntries(
    CREDENTIAL_METADATA_FIELDS
      .filter((field) => Object.prototype.hasOwnProperty.call(source, field))
      .map((field) => [field, source[field]]),
  );
}

function displayResourceData(kind: string, data: unknown) {
  return kind === 'credentials' ? safeCredentialMetadata(data) : data;
}

function resourceRecord(data: unknown): Record<string, unknown> {
  return data && typeof data === 'object' && !Array.isArray(data) ? data as Record<string, unknown> : {};
}

function resourceText(value: unknown): string {
  if (typeof value === 'string') return value.trim();
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  return '';
}

function resourceCount(value: unknown, noun: string): string {
  const count = Array.isArray(value) ? value.length : 0;
  return `${count} ${noun}${count === 1 ? '' : 's'}`;
}

function resourceStatus(kind: string, data: unknown): string | undefined {
  const obj = resourceRecord(displayResourceData(kind, data));
  const status = resourceText(obj.status);
  if (status) return status;
  if (typeof obj.enabled === 'boolean') return obj.enabled ? 'Enabled' : 'Disabled';
  return undefined;
}

function summarize(kind: string, data: unknown) {
  const obj = resourceRecord(displayResourceData(kind, data));
  switch (kind) {
    case 'tenants':
      return resourceText(obj.name) || 'Unnamed tenant';
    case 'operators':
      return resourceText(obj.display_name) || resourceText(obj.subject) || 'Unnamed operator';
    case 'role_bindings':
      return `${resourceText(obj.subject) || 'Unknown subject'} · ${resourceText(obj.role) || 'Role not set'}`;
    case 'connections':
      return [resourceText(obj.connector) || 'Provider not set', resourceText(obj.account_id) || 'Account not set', resourceText(obj.region)].filter(Boolean).join(' · ');
    case 'credentials':
      return [resourceText(obj.provider) || 'Provider not set', resourceText(obj.account_id) || 'Account not set', resourceText(obj.credential_id) || 'Credential metadata'].join(' · ');
    case 'api_keys':
      return [resourceText(obj.name) || 'Unnamed API key', resourceText(obj.role) || 'Role not set'].join(' · ');
    case 'account_pools':
      return [resourceText(obj.provider) || 'Provider not set', resourceCount(obj.account_ids, 'account')].join(' · ');
    case 'oauth_sessions':
      return [resourceText(obj.connector) || 'Provider not set', resourceText(obj.expires_at) ? `Expires ${resourceText(obj.expires_at)}` : 'Expiry not provided'].join(' · ');
    case 'models':
      return [resourceText(obj.upstream_id) || 'Upstream model not set', resourceText(obj.connection_id) || 'Connection not set'].join(' · ');
    case 'model_aliases':
      return [resourceText(obj.description) || 'Unnamed alias', resourceCount(obj.model_ids, 'model')].join(' · ');
    case 'route_policies':
      return [resourceText(obj.alias) || 'Alias not set', resourceCount(obj.targets, 'target')].join(' · ');
    case 'policy_limits':
      return [resourceText(obj.scope) || 'Scope not set', resourceText(obj.scope_id) || 'Scope ID not set', obj.requests_per_minute === undefined ? '' : `${String(obj.requests_per_minute)} RPM`].filter(Boolean).join(' · ');
    case 'usage_ledger':
      return [resourceText(obj.kind) || 'Usage entry', resourceText(obj.attempt_id) || 'Attempt not set'].join(' · ');
    case 'audit_events':
      return [resourceText(obj.action) || 'Action not set', resourceText(obj.actor) ? `by ${resourceText(obj.actor)}` : 'Actor not set'].join(' · ');
    case 'upstream_operations':
      return [resourceText(obj.operation) || 'Operation not set', resourceText(obj.connection_id) || 'Connection not set'].join(' · ');
    case 'admissions':
      return [resourceText(obj.state) || 'State not set', resourceText(obj.request_id) || 'Request not set'].join(' · ');
    case 'reconciliations':
      return [resourceText(obj.state) || 'State not set', resourceText(obj.mode) || 'Mode not set', resourceText(obj.reconciliation_id) || 'Reconciliation not set'].join(' · ');
    default:
      return 'Resource details available';
  }
}

function serializeResourceData(data: unknown): string {
  const encoded = JSON.stringify(data);
  return encoded === undefined ? String(data) : encoded;
}

function ResourceTable({
  kind,
  page,
  selected,
  onSelect,
  loading,
  filter,
}: {
  kind: string;
  page?: ResourcePage;
  selected?: Resource;
  onSelect: (resource: Resource) => void;
  loading: boolean;
  filter: string;
}) {
  if (!page) {
    if (loading) {
      return <div className="grid min-h-36 place-items-center rounded-xl border border-dashed border-zinc-200 bg-white p-6 text-sm text-zinc-500 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-400" role="status"><RefreshCw className="mr-2 inline h-4 w-4 animate-spin" aria-hidden="true" />Loading {LABELS[kind]?.toLowerCase() ?? 'resources'}…</div>;
    }
    return <div className="rounded-xl border border-dashed border-zinc-200 bg-white p-6 text-sm text-zinc-600 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-300">The collection is unavailable. Use Refresh to try again.</div>;
  }
  const allItems = page.items ?? [];
  const query = filter.trim().toLowerCase();
  const items = query ? allItems.filter((item) => `${item.id} ${summarize(kind, item.data)}`.toLowerCase().includes(query)) : allItems;
  const emptyMessage = allItems.length ? 'No matching resources on this page.' : `No ${LABELS[kind]?.toLowerCase() ?? 'resources'} found for this tenant.`;
  return (
    <div className="table-wrap overflow-x-auto rounded-xl border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900" aria-busy={loading}>
      <table className="w-full min-w-[38rem] text-left text-sm">
        <caption className="sr-only">{LABELS[kind] ?? kind}</caption>
        <thead className="border-b border-zinc-200 bg-zinc-50 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800 dark:bg-zinc-950/50 dark:text-zinc-400">
          <tr>
            <th scope="col" className="px-4 py-3 font-medium">ID</th>
            <th scope="col" className="px-4 py-3 font-medium">Version</th>
            <th scope="col" className="px-4 py-3 font-medium">Summary</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-zinc-100 dark:divide-zinc-800">
          {items.length ? items.map((item) => {
            const selectedRow = selected?.id === item.id;
            const status = resourceStatus(kind, item.data);
            const statusVariant = status && ['disabled', 'revoked', 'expired', 'failed'].includes(status.toLowerCase()) ? 'destructive' : 'secondary';
            return (
              <tr key={item.id} className={cn('transition-colors hover:bg-indigo-50/60 dark:hover:bg-indigo-950/30', selectedRow && 'bg-indigo-50 dark:bg-indigo-950/40')} aria-current={selectedRow ? 'true' : undefined}>
                <td className="px-4 py-3 align-top">
                  <button type="button" className="table-link max-w-[18rem] break-all text-left font-mono text-sm font-medium text-indigo-700 underline-offset-4 hover:underline dark:text-indigo-300" onClick={() => onSelect(item)} aria-label={item.id}>{item.id}</button>
                </td>
                <td className="whitespace-nowrap px-4 py-3 align-top font-mono text-xs text-zinc-600 dark:text-zinc-300">{item.version}</td>
                <td className="max-w-[34rem] px-4 py-3 align-top text-zinc-700 dark:text-zinc-200"><div className="flex flex-wrap items-center gap-2"><span>{summarize(kind, item.data)}</span>{status && <Badge variant={statusVariant}>{status}</Badge>}</div></td>
              </tr>
            );
          }) : <tr><td colSpan={3} className="px-5 py-10 text-center text-sm text-zinc-500 dark:text-zinc-400"><p className="empty">{emptyMessage}</p></td></tr>}
        </tbody>
      </table>
    </div>
  );
}


type KeyMetadataChange = { id: string; resource?: Resource };

function ResourceView({
  kind,
  onSelected,
  onSession,
  keyChange,
}: {
  kind: string;
  onSelected?: (resource?: Resource) => void;
  onSession?: (next: Session) => void;
  keyChange?: KeyMetadataChange;
}) {
  const session = api.session!;
  const tenantID = session.principal.TenantID ?? '';
  const writable = isWritableResourceKind(kind) && canWrite(session, kind);
  const blankDraft = useMemo(
    () => (writable ? initialResourceData(kind, tenantID) as ResourceData : undefined),
    [kind, tenantID, writable],
  );
  const [page, setPage] = useState<ResourcePage>();
  const [cursor, setCursor] = useState<string>();
  const [selected, setSelected] = useState<Resource>();
  const [draft, setDraft] = useState<ResourceData>();
  const [newID, setNewID] = useState('');
  const [error, setError] = useState<unknown>();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState('');
  const [conflict, setConflict] = useState<Resource>();
  const [filter, setFilter] = useState('');
  const [draftBaseline, setDraftBaseline] = useState('');
  const [editorEpoch, setEditorEpoch] = useState(0);
  const loadSequence = useRef(0);
  const editorHeading = useRef<HTMLHeadingElement>(null);
  const newResourceButton = useRef<HTMLButtonElement>(null);
  const editorForm = useRef<HTMLFormElement>(null);
  const activeEditor = draft && writable ? selected?.id ?? 'new' : null;
  useEffect(() => {
    if (activeEditor !== null) editorHeading.current?.focus();
  }, [activeEditor]);

  const load = useCallback(async (next?: string) => {
    const sequence = ++loadSequence.current;
    setLoading(true);
    setError(undefined);
    try {
      const result = await api.list(kind, next);
      if (sequence !== loadSequence.current) return;
      setPage(result);
      setCursor(result.next_cursor);
    } catch (failure) {
      if (sequence === loadSequence.current) setError(failure);
    } finally {
      if (sequence === loadSequence.current) setLoading(false);
    }
  }, [kind]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (kind !== 'api_keys' || !keyChange) return;
    setPage((previous) => previous && ({
      ...previous,
      items: keyChange.resource
        ? previous.items.map((row) => row.id === keyChange.id ? keyChange.resource! : row)
        : previous.items.filter((row) => row.id !== keyChange.id),
    }));
    setSelected((previous) => previous?.id === keyChange.id ? keyChange.resource : previous);
  }, [keyChange, kind]);

  const draftDirty = Boolean((draft && serializeResourceData(draft) !== draftBaseline) || (!selected && newID.trim()));

  const confirmDiscard = (message: string) =>
    (!draftDirty && !editorForm.current?.querySelector('[aria-invalid="true"]')) || window.confirm(message);

  const choose = (resource: Resource) => {
    if (selected?.id === resource.id && draft) return;
    if (!confirmDiscard('Discard unsaved changes and open this resource?')) return;
    setSelected(resource);
    setDraft(writable ? resource.data as ResourceData : undefined);
    setDraftBaseline(writable ? serializeResourceData(resource.data) : '');
    setEditorEpoch((epoch) => epoch + 1);
    setConflict(undefined);
    setNotice('');
    onSelected?.(resource);
  };

  const newResource = () => {
    if (!blankDraft || !confirmDiscard('Discard unsaved changes and start a new resource?')) return;
    setSelected(undefined);
    onSelected?.(undefined);
    setDraft(blankDraft);
    setDraftBaseline(serializeResourceData(blankDraft));
    setNewID('');
    setEditorEpoch((epoch) => epoch + 1);
    setConflict(undefined);
    setNotice('');
  };

  const closeEditor = () => {
    if (!confirmDiscard('Discard unsaved changes and return to the list?')) return;
    setSelected(undefined);
    onSelected?.(undefined);
    setDraft(undefined);
    setDraftBaseline('');
    setNewID('');
    setEditorEpoch((epoch) => epoch + 1);
    setConflict(undefined);
    setNotice('');
    newResourceButton.current?.focus();
  };

  const save = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!writable || !draft) return;
    setSaving(true);
    setError(undefined);
    setNotice('');
    try {
      if (selected) {
        const saved = await api.update(kind, selected, draft as Parameters<typeof api.update>[2]);
        setSelected(saved);
        onSelected?.(saved);
        setDraft(saved.data as ResourceData);
        setDraftBaseline(serializeResourceData(saved.data));
        setEditorEpoch((epoch) => epoch + 1);
        setConflict(undefined);
        setNotice('Saved.');
      } else {
        if (!newID.trim()) throw new Error('An ID is required.');
        const saved = await api.create(kind, newID.trim(), draft as Parameters<typeof api.create>[2]);
        setSelected(saved);
        onSelected?.(saved);
        setDraft(saved.data as ResourceData);
        setDraftBaseline(serializeResourceData(saved.data));
        setNewID('');
        setEditorEpoch((epoch) => epoch + 1);
        setNotice('Created.');
        if (kind === 'tenants' && onSession) onSession(await api.loadSession());
      }
      await load();
    } catch (failure) {
      if (failure instanceof APIError && failure.status === 412 && selected) {
        setError(undefined);
        setNotice('This resource changed on the server. Your draft is retained.');
        try {
          setConflict(await api.get(kind, selected.id));
        } catch (fetchFailure) {
          setError(fetchFailure);
        }
      } else {
        setError(failure);
      }
    } finally {
      setSaving(false);
    }
  };

  const remove = async () => {
    if (!writable || !selected || !window.confirm(`Delete ${selected.id}?`)) return;
    setSaving(true);
    setError(undefined);
    try {
      await api.remove(kind, selected);
      setSelected(undefined);
      onSelected?.(undefined);
      setDraft(undefined);
      setDraftBaseline('');
      setEditorEpoch((epoch) => epoch + 1);
      setConflict(undefined);
      setNotice('Deleted.');
      await load();
    } catch (failure) {
      setError(failure);
      if (failure instanceof APIError && failure.status === 412) {
        setNotice('Delete rejected because the version is stale; your editor is retained.');
      }
    } finally {
      setSaving(false);
    }
  };

  const reloadServerVersion = () => {
    if (!conflict) return;
    setSelected(conflict);
    onSelected?.(conflict);
    setDraft(conflict.data as ResourceData);
    setDraftBaseline(serializeResourceData(conflict.data));
    setEditorEpoch((epoch) => epoch + 1);
    setConflict(undefined);
    setError(undefined);
    setNotice('Reloaded server version.');
  };

  const singular = SINGULAR_LABELS[kind] ?? kind;
  const description = RESOURCE_DESCRIPTIONS[kind] ?? 'Review server-owned resources for the active tenant.';
  const editorTitle = selected ? `Edit ${singular} ${selected.id}` : `New ${singular}`;
  const metadata = selected && !draft ? displayResourceData(kind, selected.data) : undefined;
  const detailOpen = Boolean(draft || metadata !== undefined);
  const showActions = selected && hasResourceActions(kind, session);

  return (
    <section className="resource space-y-6" aria-busy={loading}>
      <div className="section-head flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="mb-2 flex flex-wrap items-center gap-2">
            <Badge variant="secondary">Collection</Badge>
            <span className="text-xs font-medium uppercase tracking-[0.14em] text-zinc-500 dark:text-zinc-400">{kind.replaceAll('_', ' ')}</span>
          </div>
          <h1 className="text-2xl font-semibold tracking-tight text-zinc-950 dark:text-white sm:text-3xl">{LABELS[kind] ?? kind}</h1>
          <p className="mt-2 max-w-2xl text-sm leading-6 text-zinc-600 dark:text-zinc-300">{description}</p>
        </div>
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button type="button" variant="outline" onClick={() => void load()} disabled={loading}>
            <RefreshCw className="mr-2 h-4 w-4" aria-hidden="true" />
            Refresh
          </Button>
          {writable && <Button ref={newResourceButton} type="button" onClick={newResource} disabled={loading || !page} aria-label="New resource">New {singular}</Button>}
          {showActions && <a href="#resource-actions" className="inline-flex h-10 items-center gap-1 rounded-md px-3 text-sm font-medium text-indigo-700 underline-offset-4 hover:underline dark:text-indigo-300">View actions<ArrowRight className="h-4 w-4" aria-hidden="true" /></a>}
        </div>
      </div>
      {loading && page && <p className="text-sm text-zinc-500 dark:text-zinc-400" role="status"><RefreshCw className="mr-2 inline h-4 w-4 animate-spin" aria-hidden="true" />Refreshing this page…</p>}
      <ErrorNotice error={error} />
      {notice && <p className="notice rounded-lg border border-indigo-200 bg-indigo-50 px-4 py-3 text-sm text-indigo-800 dark:border-indigo-900 dark:bg-indigo-950/40 dark:text-indigo-200" role="status">{notice}</p>}
      <div className={cn('resource-grid grid grid-cols-1 gap-6', detailOpen && 'xl:grid-cols-[minmax(18rem,0.8fr)_minmax(0,1.2fr)]')}>
        <div className="min-w-0 space-y-4">
          {page && <label className="grid max-w-sm gap-1.5"><span className="text-xs font-medium uppercase tracking-[0.14em] text-zinc-500 dark:text-zinc-400">Filter this page</span><Input value={filter} onChange={(event) => setFilter(event.currentTarget.value)} placeholder="ID or summary" /></label>}
          <ResourceTable kind={kind} page={page} selected={selected} onSelect={choose} loading={loading} filter={filter} />
          {cursor && (
            <Button type="button" variant="outline" onClick={() => void load(cursor)} disabled={loading}>
              Next page <ChevronRight className="ml-2 h-4 w-4" aria-hidden="true" />
            </Button>
          )}
        </div>
        {draft && writable ? (
          <form ref={editorForm} key={`${kind}:${selected?.id ?? 'new'}:${tenantID}:${editorEpoch}`} className="editor min-w-0 rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900" onSubmit={save}>
            <div className="mb-5 flex items-start justify-between gap-3">
              <div>
                <p className="text-xs font-medium uppercase tracking-[0.14em] text-zinc-500 dark:text-zinc-400">{selected ? 'Edit existing resource' : 'Create resource'}</p>
                <h2 ref={editorHeading} tabIndex={-1} className="mt-1 scroll-mt-28 text-xl font-semibold text-zinc-950 dark:text-white">{editorTitle}</h2>
              </div>
              <div className="flex shrink-0 flex-col items-end gap-2">
                <Button type="button" variant="ghost" size="sm" disabled={saving} onClick={closeEditor}>Return to list</Button>
                {selected && <Badge variant="outline">Version {selected.version}</Badge>}
              </div>
            </div>
            <label className="field mb-5 grid gap-2"><span className="text-sm font-medium text-zinc-800 dark:text-zinc-200">ID</span><Input value={selected?.id ?? newID} onChange={(event) => setNewID(event.currentTarget.value)} readOnly={Boolean(selected)} required aria-label="ID" /></label>
            <ResourceForm key={`${kind}:${selected?.id ?? 'new'}:${tenantID}:fields:${editorEpoch}`} kind={kind} value={draft} onChange={setDraft} />
            <div className="actions-row mt-6 flex flex-wrap gap-2 border-t border-zinc-200 pt-4 dark:border-zinc-800">
              <Button type="submit" disabled={saving}>{saving ? <RefreshCw className="mr-2 h-4 w-4 animate-spin" aria-hidden="true" /> : <Check className="mr-2 h-4 w-4" aria-hidden="true" />}{saving ? (selected ? 'Saving…' : 'Creating…') : (selected ? 'Save changes' : 'Create')}</Button>
              {selected && <Button type="button" variant="destructive" disabled={saving} onClick={() => void remove()}>Delete</Button>}
            </div>
            {selected && <p className="mt-4 text-xs leading-5 text-zinc-500 dark:text-zinc-400">Version {selected.version}. Saves use an If-Match concurrency check.</p>}
          </form>
        ) : selected && metadata !== undefined ? (
          <Card className="metadata min-w-0">
            <CardHeader>
              <CardTitle className="text-base">Selected {singular}</CardTitle>
              <CardDescription><span className="font-mono">{selected.id}</span> · Version {selected.version}</CardDescription>
            </CardHeader>
            <CardContent>
              <pre className="max-h-[26rem] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-zinc-950 p-4 text-xs leading-relaxed text-zinc-100">{JSON.stringify(metadata, null, 2)}</pre>
            </CardContent>
          </Card>
        ) : null}
      </div>
      {conflict && selected && (
        <Card className="conflict border-amber-300 dark:border-amber-800">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base"><AlertTriangle className="h-4 w-4 text-amber-600" aria-hidden="true" />Version conflict</CardTitle>
            <CardDescription>Server version {conflict.version} is newer than the version you edited. Your draft is retained until you choose how to proceed.</CardDescription>
          </CardHeader>
          <CardContent className="conflict-grid grid gap-4 lg:grid-cols-2">
            <div>
              <h3 className="mb-2 text-sm font-medium">Server version</h3>
              <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-zinc-950 p-3 text-xs text-zinc-100">{JSON.stringify(displayResourceData(kind, conflict.data), null, 2)}</pre>
            </div>
            <div>
              <h3 className="mb-2 text-sm font-medium">Your retained draft</h3>
              <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-zinc-950 p-3 text-xs text-zinc-100">{JSON.stringify(displayResourceData(kind, draft), null, 2)}</pre>
            </div>
          </CardContent>
          <CardFooter><Button type="button" variant="outline" onClick={reloadServerVersion}>Reload server version</Button></CardFooter>
        </Card>
      )}
      {kind === 'api_keys' && hasPermission(session, 'key:write') && <APIKeyIssuePanel tenantID={tenantID} onIssued={() => void load()} />}
    </section>
  );
}

function parseEntries(value: string) {
  return value.split(/[,\s]+/).map((entry) => entry.trim()).filter(Boolean);
}

function APIKeyIssuePanel({ tenantID, onIssued }: { tenantID: string; onIssued: () => void }) {
  const [id, setID] = useState('');
  const [name, setName] = useState('');
  const [role, setRole] = useState<APIKeyGrantData['role']>('viewer');
  const [permissions, setPermissions] = useState('');
  const [aliases, setAliases] = useState('');
  const [connections, setConnections] = useState('');
  const [operations, setOperations] = useState('');
  const [portable, setPortable] = useState(false);
  const [nativeAccount, setNativeAccount] = useState(false);
  const [realtime, setRealtime] = useState(false);
  const [result, setResult] = useState<unknown>();
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);

  const issue = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    setResult(undefined);
    const data = {
      name: name.trim(),
      role,
      permissions: parseEntries(permissions),
      aliases: parseEntries(aliases),
      connections: parseEntries(connections),
      operations: parseEntries(operations),
      portable,
      native_account: nativeAccount,
      realtime,
    } as APIKeyGrantData;
    try {
      setResult(await api.issueAPIKey(id.trim(), data));
      setID('');
      setName('');
      setPermissions('');
      setAliases('');
      setConnections('');
      setOperations('');
      setPortable(false);
      setNativeAccount(false);
      setRealtime(false);
      onIssued();
    } catch (failure) {
      setError(failure);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="actions key-issue rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <div className="mb-5 flex items-start gap-3">
        <div className="grid h-10 w-10 shrink-0 place-items-center rounded-lg bg-indigo-100 text-indigo-700 dark:bg-indigo-950/60 dark:text-indigo-300"><KeyRound className="h-5 w-5" aria-hidden="true" /></div>
        <div><h2 className="text-lg font-semibold">Issue API key</h2><p className="mt-1 text-sm text-zinc-600 dark:text-zinc-300">The token is returned once and is not stored in resource metadata.</p></div>
      </div>
      <ErrorNotice error={error} onClose={() => setError(undefined)} />
      <form onSubmit={issue} className="grid gap-1">
        <label className="field"><span>Key ID</span><Input required value={id} onChange={(event) => setID(event.currentTarget.value)} /></label>
        <label className="field"><span>Name</span><Input required value={name} onChange={(event) => setName(event.currentTarget.value)} /></label>
        <label className="field"><span>Role</span><NativeSelect value={role} onChange={(event) => setRole(event.currentTarget.value as APIKeyGrantData['role'])}><option value="viewer">viewer</option><option value="operator">operator</option><option value="admin">admin</option></NativeSelect></label>
        <label className="field"><span>Permissions (space or comma separated)</span><Input value={permissions} onChange={(event) => setPermissions(event.currentTarget.value)} /></label>
        <label className="field"><span>Aliases (space or comma separated)</span><Input value={aliases} onChange={(event) => setAliases(event.currentTarget.value)} /></label>
        <label className="field"><span>Connections (space or comma separated)</span><Input value={connections} onChange={(event) => setConnections(event.currentTarget.value)} /></label>
        <label className="field"><span>Operations (exact operation names, space or comma separated)</span><Input value={operations} onChange={(event) => setOperations(event.currentTarget.value)} /></label>
        <label className="field"><span>Tenant</span><Input value={tenantID} readOnly /></label>
        <label className="field flex-row items-center gap-2"><span className="flex items-center gap-2"><Input type="checkbox" checked={portable} onChange={(event) => setPortable(event.currentTarget.checked)} /> Portable</span></label>
        <label className="field flex-row items-center gap-2"><span className="flex items-center gap-2"><Input type="checkbox" checked={nativeAccount} onChange={(event) => setNativeAccount(event.currentTarget.checked)} /> Native account</span></label>
        <label className="field flex-row items-center gap-2"><span className="flex items-center gap-2"><Input type="checkbox" checked={realtime} onChange={(event) => setRealtime(event.currentTarget.checked)} /> Realtime</span></label>
        <Button type="submit" className="mt-3 w-fit" disabled={busy || !id.trim() || !name.trim() || !permissions.trim() || !operations.trim()}>
          {busy ? <RefreshCw className="mr-2 h-4 w-4 animate-spin" aria-hidden="true" /> : <KeyRound className="mr-2 h-4 w-4" aria-hidden="true" />}
          Issue API key
        </Button>
      </form>
      <ActionResult value={result} error={undefined} onDismiss={() => setResult(undefined)} />
    </section>
  );
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

function ActionResult({ value, error, onDismiss }: { value: unknown; error: unknown; onDismiss?: () => void }) {
  return (
    <>
      {error && <ErrorNotice error={error} />}
      {value !== undefined && (
        <div className="result-wrap mt-4 rounded-lg border border-zinc-200 bg-zinc-50 p-3 dark:border-zinc-700 dark:bg-zinc-950/60">
          <Button type="button" variant="ghost" size="sm" onClick={onDismiss} aria-label="Dismiss result">Dismiss result</Button>
          <pre className="result mt-2 max-h-80 overflow-auto whitespace-pre-wrap break-words text-xs text-zinc-700 dark:text-zinc-200">{JSON.stringify(value, null, 2)}</pre>
        </div>
      )}
    </>
  );
}

function ConnectionCredentialActions({ item, initialCredentialStatus }: { item: Resource; initialCredentialStatus?: string }) {
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

  const invoke = async (
    action: 'status' | 'import' | 'revoke-credential' | 'oauth-start' | 'oauth-callback' | 'device-start' | 'device-poll',
    extra: Partial<CredentialActionData> = {},
  ) => {
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
      setForm((previous) => {
        const authorization = next.authorization_url || next.authorization_url_complete;
        let state = previous.state;
        let redirectURI = previous.redirect_uri;
        if (action === 'oauth-start' && authorization) {
          try {
            const url = new URL(authorization);
            state = url.searchParams.get('state') || state;
            redirectURI = url.searchParams.get('redirect_uri') || redirectURI;
          } catch {
            // Keep the provider URL visible for manual recovery when it is not a URL.
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
      setForm((previous) => ({
        ...previous,
        secret: '',
        rotation_secret: '',
        state: action === 'oauth-callback' ? '' : previous.state,
        code: '',
      }));
    } finally {
      setBusy(false);
    }
  };

  const revoke = () => {
    if (credentialRevoked || !window.confirm(`Revoke the credential for ${provider} / ${accountID}? New requests will fail until a replacement is authorized.`)) return;
    void invoke('revoke-credential');
  };

  return (
    <aside className="actions credential-actions rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <div className="mb-4 flex items-start gap-3"><ShieldCheck className="mt-0.5 h-5 w-5 text-indigo-600 dark:text-indigo-400" aria-hidden="true" /><div><h2 className="text-lg font-semibold">Credential lifecycle</h2><p className="mt-1 text-sm text-zinc-600 dark:text-zinc-300">{provider} / {accountID}. Secrets are write-only and are cleared after every attempt.</p></div></div>
      <Button type="button" variant="outline" disabled={busy} onClick={() => void invoke('status')}><RefreshCw className="mr-2 h-4 w-4" aria-hidden="true" />Refresh encrypted-store metadata</Button>
      {canManage && <div className="mt-5 space-y-5">
        <label className="field"><span>Current credential version</span><Input type="number" min="1" inputMode="numeric" value={form.credential_version} onChange={(event) => setForm({ ...form, credential_version: event.currentTarget.value })} /><small>Required when replacing or revoking an existing credential. Refresh metadata to fill it automatically.</small></label>
        <fieldset className="space-y-3 rounded-lg border border-zinc-200 p-4 dark:border-zinc-700"><legend className="px-1 text-sm font-semibold">Import or replace API key</legend><p className="text-sm text-zinc-600 dark:text-zinc-300">Manual token import is supported only for API-key connections. Use the configured OAuth or device authorization controls below for OAuth credentials.</p><label className="field"><span>New API key</span><Input type="password" autoComplete="new-password" value={form.secret} onChange={(event) => setForm({ ...form, secret: event.currentTarget.value })} /></label><Button type="button" disabled={busy || !form.secret} onClick={() => void invoke('import', { kind: 'api_key', secret: form.secret })}>Import API key</Button><label className="field"><span>Replacement API key</span><Input type="password" autoComplete="new-password" value={form.rotation_secret} onChange={(event) => setForm({ ...form, rotation_secret: event.currentTarget.value })} /><small>Rotation is a version-checked import bound to this connection.</small></label><Button type="button" disabled={busy || !form.rotation_secret || !form.credential_version} onClick={() => void invoke('import', { kind: 'api_key', secret: form.rotation_secret })}>Rotate API key</Button></fieldset>
        <fieldset className="space-y-3 rounded-lg border border-zinc-200 p-4 dark:border-zinc-700"><legend className="px-1 text-sm font-semibold">OAuth authorization</legend><Button type="button" variant="outline" disabled={busy} onClick={() => void invoke('oauth-start')}>Start OAuth</Button>{oauthAuthorizationURL && <p><a className="text-sm font-medium text-indigo-700 underline underline-offset-4 dark:text-indigo-300" href={oauthAuthorizationURL} target="_blank" rel="noreferrer">Continue authorization with provider</a></p>}<label className="field"><span>OAuth state</span><Input autoComplete="off" value={form.state} onChange={(event) => setForm({ ...form, state: event.currentTarget.value })} /></label><label className="field"><span>OAuth callback code</span><Input autoComplete="off" value={form.code} onChange={(event) => setForm({ ...form, code: event.currentTarget.value })} /></label><label className="field"><span>Registered redirect URI</span><Input type="url" value={form.redirect_uri} onChange={(event) => setForm({ ...form, redirect_uri: event.currentTarget.value })} /><small>Must exactly match the provider registration configured for this connection.</small></label><Button type="button" disabled={busy || !form.state || !form.code || !form.redirect_uri} onClick={() => void invoke('oauth-callback', { state: form.state, code: form.code, redirect_uri: form.redirect_uri })}>Complete OAuth callback</Button></fieldset>
        <fieldset className="space-y-3 rounded-lg border border-zinc-200 p-4 dark:border-zinc-700"><legend className="px-1 text-sm font-semibold">Device authorization</legend><Button type="button" variant="outline" disabled={busy} onClick={() => void invoke('device-start')}>Start device flow</Button>{deviceUserCode && <p className="text-sm" role="status">Provider device code: <strong>{deviceUserCode}</strong></p>}{deviceAuthorizationURL && <p><a className="text-sm font-medium text-indigo-700 underline underline-offset-4 dark:text-indigo-300" href={deviceAuthorizationURL} target="_blank" rel="noreferrer">Open device verification</a></p>}<label className="field"><span>Device flow ID</span><Input value={form.flow_id} onChange={(event) => setForm({ ...form, flow_id: event.currentTarget.value })} /><small>Filled automatically when the provider starts a device flow.</small></label><Button type="button" disabled={busy || !form.flow_id} onClick={() => void invoke('device-poll', { flow_id: form.flow_id })}>Poll device flow</Button></fieldset>
        <fieldset className="danger-zone space-y-3 rounded-lg border border-red-200 p-4 dark:border-red-950"><legend className="px-1 text-sm font-semibold">Destructive action</legend><p className="text-sm text-zinc-600 dark:text-zinc-300">Revocation is version checked and prevents new requests from leasing this credential.</p><Button type="button" variant="destructive" disabled={busy || credentialRevoked || !form.credential_version} onClick={revoke}>{credentialRevoked ? 'Credential revoked' : 'Revoke credential'}</Button></fieldset>
      </div>}
      <ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} />
    </aside>
  );
}

function CredentialMetadataActions({ item }: { item: Resource }) {
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
    void api.get('connections', connectionID).then((next) => {
      if (next.tenant_id !== tenantID) throw new Error('Credential connection belongs to another tenant.');
      if (active) setConnection(next);
    }).catch((failure) => { if (active) setError(failure); });
    return () => { active = false; };
  }, [connectionID, tenantID]);

  if (!connectionID) return <aside className="actions rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900"><h2 className="text-lg font-semibold">Credential lifecycle</h2><p className="mt-2 text-sm text-zinc-600 dark:text-zinc-300">This credential has no owning connection metadata.</p></aside>;
  if (error) return <aside className="actions rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900"><h2 className="text-lg font-semibold">Credential lifecycle</h2><ErrorNotice error={error} /></aside>;
  if (!connection) return <aside className="actions rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900"><h2 className="text-lg font-semibold">Credential lifecycle</h2><p className="mt-3 text-sm text-zinc-500 dark:text-zinc-400" role="status"><RefreshCw className="mr-2 inline h-4 w-4 animate-spin" aria-hidden="true" />Loading connection metadata…</p></aside>;
  return <ConnectionCredentialActions item={connection} initialCredentialStatus={String(metadata.status ?? '')} />;
}

function ActionPanel({ kind, item, onKeyChanged }: { kind: string; item: Resource; onKeyChanged: (change: KeyMetadataChange) => void }) {
  const [result, setResult] = useState<unknown>();
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const [keyRevoked, setKeyRevoked] = useState(false);
  const [currentKey, setCurrentKey] = useState(item);
  const [reconcile, setReconcile] = useState({ reconciliation_id: '', mode: 'provider_evidence', reason: '', source_reference: '', cost: '', input: '', output: '', total: '', source: '' });

  const invoke = async (path: string, data?: ActionBody) => {
    setBusy(true);
    setError(undefined);
    try {
      setResult(await api.action(path, data, item.version));
    } catch (failure) {
      setError(failure);
    } finally {
      setBusy(false);
    }
  };

  const rotateKey = async () => {
    setBusy(true);
    setError(undefined);
    setResult(undefined);
    try {
      const issued = await api.rotateAPIKey(currentKey);
      setCurrentKey(issued.resource);
      setResult(issued);
      onKeyChanged({ id: currentKey.id, resource: issued.resource });
    } catch (failure) {
      setError(failure);
    } finally {
      setBusy(false);
    }
  };

  const revokeKey = async () => {
    if (!window.confirm(`Revoke API key ${currentKey.id}? This cannot be undone.`)) return;
    setBusy(true);
    setError(undefined);
    setResult(undefined);
    try {
      const revoked = await api.revokeAPIKey(currentKey);
      setKeyRevoked(true);
      setResult(revoked);
      onKeyChanged({ id: currentKey.id });
    } catch (failure) {
      setError(failure);
    } finally {
      setBusy(false);
    }
  };

  if (kind === 'credentials') return <CredentialMetadataActions item={item} />;
  if (kind === 'connections') return <div className="space-y-5"><aside className="actions rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900"><h2 className="text-lg font-semibold">Connection actions</h2><div className="mt-4 flex flex-wrap gap-2">{hasPermission(api.session!, 'connection:test') && <Button variant="outline" disabled={busy} onClick={() => void invoke(`/connections/${encodeURIComponent(item.id)}/test`)}>Test</Button>}{hasPermission(api.session!, 'connection:discover') && <Button variant="outline" disabled={busy} onClick={() => void invoke(`/connections/${encodeURIComponent(item.id)}/discover`)}>Discover models</Button>}{hasPermission(api.session!, 'resource:write') && <Button variant="destructive" disabled={busy} onClick={() => void invoke(`/connections/${encodeURIComponent(item.id)}/disable`)}>Disable</Button>}</div><ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside><ConnectionCredentialActions item={item} /></div>;
  if (kind === 'api_keys' && hasPermission(api.session!, 'key:write')) return <aside className="actions rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900"><h2 className="text-lg font-semibold">Key actions</h2><div className="mt-4 flex flex-wrap gap-2"><Button disabled={busy || keyRevoked} onClick={() => void rotateKey()}>Rotate</Button><Button variant="destructive" disabled={busy || keyRevoked} onClick={() => void revokeKey()}>Revoke</Button></div>{keyRevoked && <p className="mt-3 text-sm text-zinc-600 dark:text-zinc-300" role="status">This key is revoked.</p>}<ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside>;
  if (kind === 'route_policies' && hasPermission(api.session!, 'route:write')) return <aside className="actions rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900"><h2 className="text-lg font-semibold">Route analysis</h2><Button className="mt-4" disabled={busy} onClick={() => void invoke(`/route_policies/${encodeURIComponent(item.id)}/dry-run`, { data: item.data })}>Explain dry-run</Button><ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside>;
  if (kind === 'upstream_operations' && hasPermission(api.session!, 'job:write')) return <aside className="actions rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900"><h2 className="text-lg font-semibold">Job actions</h2><Button className="mt-4" variant="destructive" disabled={busy} onClick={() => void invoke(`/upstream_operations/${encodeURIComponent(item.id)}/cancel`)}>Request cancellation</Button><ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside>;
  if (kind === 'admissions' && hasPermission(api.session!, 'accounting:reconcile')) return <aside className="actions rounded-xl border border-zinc-200 bg-white p-5 shadow-sm dark:border-zinc-800 dark:bg-zinc-900"><h2 className="text-lg font-semibold">Reconcile admission</h2><div className="mt-4 space-y-1"><label className="field"><span>Reconciliation ID</span><Input value={reconcile.reconciliation_id} onChange={(event) => setReconcile({ ...reconcile, reconciliation_id: event.currentTarget.value })} /></label><label className="field"><span>Mode</span><NativeSelect value={reconcile.mode} onChange={(event) => setReconcile({ ...reconcile, mode: event.currentTarget.value })}><option value="provider_evidence">Provider evidence</option><option value="charge_reserved_maximum">Charge reserved maximum</option></NativeSelect></label><label className="field"><span>Reason</span><Textarea value={reconcile.reason} onChange={(event) => setReconcile({ ...reconcile, reason: event.currentTarget.value })} /></label><label className="field"><span>Source reference</span><Input value={reconcile.source_reference} onChange={(event) => setReconcile({ ...reconcile, source_reference: event.currentTarget.value })} /></label><label className="field"><span>Cost (nanodollars)</span><Input inputMode="numeric" value={reconcile.cost} onChange={(event) => setReconcile({ ...reconcile, cost: event.currentTarget.value })} /></label><fieldset className="space-y-1 rounded-lg border border-zinc-200 p-3 dark:border-zinc-700"><legend className="px-1 text-sm font-medium">Usage</legend><label className="field"><span>Input tokens</span><Input inputMode="numeric" value={reconcile.input} onChange={(event) => setReconcile({ ...reconcile, input: event.currentTarget.value })} /></label><label className="field"><span>Output tokens</span><Input inputMode="numeric" value={reconcile.output} onChange={(event) => setReconcile({ ...reconcile, output: event.currentTarget.value })} /></label><label className="field"><span>Total tokens</span><Input inputMode="numeric" value={reconcile.total} onChange={(event) => setReconcile({ ...reconcile, total: event.currentTarget.value })} /></label><label className="field"><span>Usage source</span><Input value={reconcile.source} onChange={(event) => setReconcile({ ...reconcile, source: event.currentTarget.value })} /></label></fieldset><Button className="mt-3" disabled={busy || !reconcile.reconciliation_id || !reconcile.reason} onClick={() => void invoke(`/admissions/${encodeURIComponent(item.id)}/reconcile`, { reconciliation_id: reconcile.reconciliation_id, mode: reconcile.mode, reason: reconcile.reason, source_reference: reconcile.source_reference || undefined, cost: reconcile.cost === '' ? undefined : Number(reconcile.cost), usage: { Input: reconcile.input === '' ? null : Number(reconcile.input), Output: reconcile.output === '' ? null : Number(reconcile.output), Total: reconcile.total === '' ? null : Number(reconcile.total), Source: reconcile.source } })}>Reconcile</Button></div><ActionResult value={result} error={error} onDismiss={() => setResult(undefined)} /></aside>;
  return null;
}

function ResourceRoute({ kind, onSession }: { kind: string; onSession?: (next: Session) => void }) {
  const [selected, setSelected] = useState<Resource>();
  const [keyChange, setKeyChange] = useState<KeyMetadataChange>();
  const keyChanged = (change: KeyMetadataChange) => {
    setKeyChange(change);
    if (change.resource) setSelected(change.resource);
  };
  return (
    <>
      <ResourceView kind={kind} onSelected={setSelected} onSession={onSession} keyChange={keyChange} />
      {selected && hasResourceActions(kind, api.session!) && (
        <div id="resource-actions" className="mt-6 scroll-mt-24">
          <ActionPanel key={kind === 'api_keys' ? selected.id : `${selected.id}:${selected.version}`} kind={kind} item={selected} onKeyChanged={keyChanged} />
        </div>
      )}
    </>
  );
}

function TenantSelector({ session, onChanged }: { session: Session; onChanged: (next: Session) => void }) {
  const tenants = (session.tenants ?? []).filter((tenant) => tenant.enabled);
  const current = session.principal.TenantID;
  const [switching, setSwitching] = useState(false);
  const [error, setError] = useState<unknown>();
  if (tenants.length < 2) return null;
  return <div className="space-y-2"><label className="tenant-selector grid gap-2"><span className="text-xs font-medium uppercase tracking-[0.14em] text-zinc-500 dark:text-zinc-400">Tenant</span><NativeSelect value={current} disabled={switching} onChange={(event) => { const id = event.currentTarget.value; if (id === current) return; setSwitching(true); setError(undefined); void api.selectTenant(id).then(onChanged).catch(setError).finally(() => setSwitching(false)); }}>{tenants.map((tenant) => <option key={tenant.tenant_id} value={tenant.tenant_id}>{tenant.name || tenant.tenant_id} ({tenant.role})</option>)}</NativeSelect>{switching && <small className="text-xs text-zinc-400" role="status">Switching…</small>}</label><ErrorNotice error={error} onClose={() => setError(undefined)} /></div>;
}

function Overview({ visible, canPlayground }: { visible: string[]; canPlayground: boolean }) {
  const groups = RESOURCE_GROUPS
    .map((group) => ({ ...group, kinds: group.kinds.filter((kind) => visible.includes(kind)) }))
    .filter((group) => group.kinds.length);
  const modelKind = visible.includes('models') ? 'models' : visible.includes('model_aliases') ? 'model_aliases' : undefined;
  const policyKind = visible.includes('route_policies') ? 'route_policies' : visible.includes('policy_limits') ? 'policy_limits' : undefined;
  const workflows = [
    { key: 'connections', title: 'Connect a provider', detail: 'Register a connection before cataloging models.', href: '/admin/connections', available: visible.includes('connections') },
    { key: 'catalog', title: 'Add a model or alias', detail: 'Build the catalog that routes can target.', href: modelKind ? `/admin/${modelKind}` : '', available: Boolean(modelKind) },
    { key: 'policy', title: 'Set routing and limits', detail: 'Shape traffic with an alias route and tenant limits.', href: policyKind ? `/admin/${policyKind}` : '', available: Boolean(policyKind) },
    { key: 'playground', title: 'Run a request', detail: 'Exercise an allowed operation from the playground.', href: '/admin/playground', available: canPlayground },
  ].filter((workflow) => workflow.available);

  return (
    <section className="space-y-8">
      <div>
        <div className="mb-3 inline-flex items-center gap-2 rounded-full bg-indigo-100 px-3 py-1 text-xs font-medium text-indigo-800 dark:bg-indigo-950/60 dark:text-indigo-200"><LayoutDashboard className="h-3.5 w-3.5" aria-hidden="true" />Operations overview</div>
        <h1 className="text-3xl font-semibold tracking-tight text-zinc-950 dark:text-white sm:text-4xl">Operations overview</h1>
        <p className="mt-3 max-w-2xl text-base leading-7 text-zinc-600 dark:text-zinc-300">Follow the path from provider connection to a named model, a controlled route, and an optional request. Collections below show only what this session can read.</p>
      </div>
      {workflows.length ? (
        <section aria-labelledby="workflow-heading" className="rounded-xl border border-indigo-200 bg-indigo-50/60 p-5 dark:border-indigo-900 dark:bg-indigo-950/30">
          <div className="mb-4">
            <h2 id="workflow-heading" className="text-lg font-semibold text-indigo-950 dark:text-indigo-100">Start with a workflow</h2>
            <p className="mt-1 text-sm text-indigo-900/80 dark:text-indigo-200/80">Open a collection to review existing resources or create one when your role allows it.</p>
          </div>
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
            {workflows.map((workflow) => (
              <Link key={workflow.key} to={workflow.href} className="group rounded-lg border border-indigo-200/80 bg-white/70 p-4 transition hover:border-indigo-400 hover:bg-white dark:border-indigo-800 dark:bg-zinc-900/50 dark:hover:border-indigo-600">
                <span className="flex items-start justify-between gap-3 text-sm font-semibold text-zinc-950 dark:text-white"><span>{workflow.title}</span><ArrowRight className="mt-0.5 h-4 w-4 shrink-0 text-indigo-600 transition group-hover:translate-x-1 dark:text-indigo-300" aria-hidden="true" /></span>
                <span className="mt-2 block text-sm leading-5 text-zinc-600 dark:text-zinc-300">{workflow.detail}</span>
              </Link>
            ))}
          </div>
        </section>
      ) : (
        <div className="rounded-xl border border-dashed border-zinc-300 bg-white p-6 text-sm text-zinc-600 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300">No collections or tools are available for this session. Ask an administrator for the permissions needed to operate this tenant.</div>
      )}
      {groups.length ? (
        <section aria-labelledby="collections-heading" className="space-y-4">
          <div>
            <h2 id="collections-heading" className="text-xl font-semibold text-zinc-950 dark:text-white">Browse collections</h2>
            <p className="mt-1 text-sm text-zinc-600 dark:text-zinc-300">Open a collection to inspect its server-owned, versioned records.</p>
          </div>
          <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
            {groups.map((group) => {
              const GroupIcon = group.icon;
              return (
                <div key={group.label} className="rounded-xl border border-zinc-200 bg-white p-4 dark:border-zinc-800 dark:bg-zinc-900">
                  <div className="mb-3 flex items-center gap-2"><GroupIcon className="h-4 w-4 text-indigo-600 dark:text-indigo-400" aria-hidden="true" /><h3 className="text-sm font-semibold uppercase tracking-[0.14em] text-zinc-500 dark:text-zinc-400">{group.label}</h3></div>
                  <ul className="space-y-1">
                    {group.kinds.map((kind) => <li key={kind}><Link to={`/admin/${kind}`} className="group flex items-center justify-between rounded-md px-2 py-2 text-sm font-medium text-zinc-700 hover:bg-zinc-100 hover:text-zinc-950 dark:text-zinc-200 dark:hover:bg-zinc-800 dark:hover:text-white"><span>{LABELS[kind]}</span><ArrowRight className="h-4 w-4 text-zinc-400 transition group-hover:translate-x-1 group-hover:text-indigo-600 dark:group-hover:text-indigo-300" aria-hidden="true" /></Link></li>)}
                  </ul>
                </div>
              );
            })}
          </div>
        </section>
      ) : null}
    </section>
  );
}

function AdminTokenPanel() {
  const [subject, setSubject] = useState('');
  const [permissions, setPermissions] = useState('');
  const [expiresAt, setExpiresAt] = useState('');
  const [hash, setHash] = useState('');
  const [result, setResult] = useState<unknown>();
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  if (api.session?.principal.Role !== 'owner') return null;

  const issue = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    setResult(undefined);
    try {
      const expiry = new Date(expiresAt).toISOString();
      setResult(await api.issueAdminToken(subject.trim(), permissions.split(/[ ,]+/).filter(Boolean), expiry));
      setSubject('');
      setPermissions('');
      setExpiresAt('');
    } catch (failure) {
      setError(failure);
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.revokeAdminToken(hash.trim());
      setHash('');
      setResult({ status: 'revoked' });
    } catch (failure) {
      setError(failure);
    } finally {
      setBusy(false);
    }
  };

  return <section className="token-admin"><div className="mb-5"><h2 className="text-xl font-semibold">Scoped admin tokens</h2><p className="mt-1 text-sm text-zinc-600 dark:text-zinc-300">Owner-only. The issued secret is shown once and is held only in this page until dismissed.</p></div><ErrorNotice error={error} onClose={() => setError(undefined)} /><div className="grid gap-5 lg:grid-cols-2"><form className="rounded-xl border border-zinc-200 p-4 dark:border-zinc-800" onSubmit={issue}><h3 className="font-medium">Issue token</h3><label className="field"><span>Subject</span><Input required value={subject} onChange={(event) => setSubject(event.currentTarget.value)} /></label><label className="field"><span>Permissions (space or comma separated)</span><Input required value={permissions} onChange={(event) => setPermissions(event.currentTarget.value)} /></label><label className="field"><span>Expires at</span><Input required type="datetime-local" value={expiresAt} onChange={(event) => setExpiresAt(event.currentTarget.value)} /></label><Button disabled={busy}>Issue scoped token</Button></form><form className="rounded-xl border border-zinc-200 p-4 dark:border-zinc-800" onSubmit={revoke}><h3 className="font-medium">Revoke token</h3><label className="field"><span>Token hash to revoke</span><Input required value={hash} onChange={(event) => setHash(event.currentTarget.value)} /></label><Button type="submit" variant="destructive" disabled={busy}>Revoke token</Button></form></div>{result !== undefined && <div className="result-wrap mt-5 rounded-lg border border-zinc-200 bg-zinc-50 p-3 dark:border-zinc-700 dark:bg-zinc-950/60"><Button type="button" variant="ghost" size="sm" onClick={() => setResult(undefined)} aria-label="Dismiss token result">Dismiss result</Button><pre className="result mt-2 whitespace-pre-wrap break-words text-xs">{JSON.stringify(result, null, 2)}</pre></div>}</section>;
}

function ConfigPage() {
  const [text, setText] = useState('{}');
  const [revision, setRevision] = useState('0');
  const [result, setResult] = useState<unknown>();
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const act = async (path: string) => {
    setBusy(true);
    setError(undefined);
    try {
      setResult(await api.action(path, { expected_revision: Number(revision), config: JSON.parse(text) }));
    } catch (failure) {
      setError(failure);
    } finally {
      setBusy(false);
    }
  };
  return <section className="space-y-6"><div><div className="mb-3 inline-flex items-center gap-2 rounded-full bg-zinc-100 px-3 py-1 text-xs font-medium text-zinc-700 dark:bg-zinc-800 dark:text-zinc-200"><Settings2 className="h-3.5 w-3.5" aria-hidden="true" />Runtime controls</div><h1 className="text-3xl font-semibold tracking-tight text-zinc-950 dark:text-white">Configuration</h1><p className="mt-2 max-w-2xl text-sm text-zinc-600 dark:text-zinc-300">Preview changes before applying. Export contains references and redacted metadata only.</p></div><Card><CardContent className="space-y-4 p-5"><ErrorNotice error={error} /><label className="field"><span>Expected configuration revision</span><Input type="number" min="0" value={revision} onChange={(event) => setRevision(event.currentTarget.value)} /></label><label className="field"><span>Configuration JSON</span><Textarea className="config-editor min-h-56 font-mono text-xs" value={text} onChange={(event) => setText(event.currentTarget.value)} /></label><div className="form-actions flex flex-wrap gap-2"><Button type="button" disabled={busy} onClick={() => void act('/config/diff')}>Preview changes</Button><Button type="button" disabled={busy} onClick={() => void act('/config/apply')}>Apply</Button><Button type="button" variant="outline" disabled={busy} onClick={() => void api.request('/config/export').then(setResult).catch(setError)}>Export</Button></div>{result !== undefined && <div className="result-wrap rounded-lg bg-zinc-950 p-4"><pre className="result max-h-96 overflow-auto whitespace-pre-wrap break-words text-xs text-zinc-100">{JSON.stringify(result, null, 2)}</pre></div>}</CardContent></Card><AdminTokenPanel /></section>;
}

function NotFound() {
  return <section className="grid min-h-[50vh] place-items-center"><Card className="max-w-lg"><CardHeader><CardTitle>Not found</CardTitle><CardDescription>The requested console route does not exist or is not enabled for this session.</CardDescription></CardHeader><CardFooter><Button asChild variant="outline"><Link to="/admin/"><LayoutDashboard className="mr-2 h-4 w-4" aria-hidden="true" />Return to overview</Link></Button></CardFooter></Card></section>;
}

function Login({ onSession }: { onSession: (session: Session) => void }) {
  const [code, setCode] = useState('');
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setBusy(true);
    setError(undefined);
    void api.bootstrap(code).then(() => api.loadSession()).then(onSession).catch(setError).finally(() => setBusy(false));
  };
  return <main className="login grid min-h-screen place-items-center bg-zinc-50 px-4 py-10 dark:bg-zinc-950"><Card className="login-card w-full max-w-md shadow-xl"><CardHeader className="space-y-4"><div className="grid h-12 w-12 place-items-center rounded-xl bg-indigo-600 text-white"><Zap className="h-6 w-6" aria-hidden="true" /></div><div><CardTitle className="text-2xl">Hoorific operations</CardTitle><CardDescription className="mt-2 text-sm leading-6">Sign in with the configured identity provider, or redeem the one-time local bootstrap code.</CardDescription></div></CardHeader><CardContent className="space-y-5"><Button asChild className="w-full"><a href="/admin/api/v1/auth/login">Sign in with OIDC</a></Button><div className="relative"><Separator /><span className="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 bg-white px-3 text-xs uppercase tracking-wide text-zinc-500 dark:bg-zinc-900">or bootstrap locally</span></div><form onSubmit={submit}><label className="field"><span>Bootstrap code</span><Input value={code} onChange={(event) => setCode(event.currentTarget.value)} autoComplete="one-time-code" required /></label><Button className="mt-2 w-full" disabled={busy}>{busy ? <><RefreshCw className="mr-2 h-4 w-4 animate-spin" aria-hidden="true" />Redeeming…</> : 'Redeem code'}</Button></form><ErrorNotice error={error} /></CardContent></Card></main>;
}

function Dashboard({ session, onChanged }: { session: Session; onChanged: (next: Session) => void }) {
  const visible = RESOURCE_KINDS.filter((kind) => canRead(session, kind));
  const permissions = permissionsFor(session);
  const role = principalRole(session);
  const canPlayground = role === 'owner' || role === 'admin' || permissions.includes('*') || permissions.includes('playground:execute');
  const canConfig = role === 'owner' || role === 'admin' || permissions.includes('*') || permissions.includes('config:read');
  const tenantID = session.principal.TenantID;
  const location = useLocation();
  const routeKey = location.pathname.replace(/^\/admin\/?/, '').split('/')[0] || 'overview';
  const routeLabel = routeKey === 'overview' ? 'Overview' : routeKey === 'playground' ? 'Playground' : routeKey === 'config' ? 'Configuration' : LABELS[routeKey] ?? 'Operations';
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  const [isMobile, setIsMobile] = useState(false);
  const [theme, setTheme] = useState<'light' | 'dark'>(() => {
    try {
      const stored = window.localStorage.getItem('hoorific-theme');
      if (stored === 'dark' || stored === 'light') return stored;
    } catch {
      // Storage may be unavailable in hardened browsing contexts.
    }
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  });
  const toggleRef = useRef<HTMLButtonElement>(null);
  const sidebarRef = useRef<HTMLElement>(null);

  useEffect(() => {
    const media = window.matchMedia('(max-width: 767px)');
    const update = () => setIsMobile(media.matches);
    update();
    media.addEventListener?.('change', update);
    return () => media.removeEventListener?.('change', update);
  }, []);

  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark');
    document.documentElement.dataset.theme = theme;
    try { window.localStorage.setItem('hoorific-theme', theme); } catch { /* no-op */ }
  }, [theme]);

  useEffect(() => {
    const sidebar = sidebarRef.current;
    if (!sidebar) return;
    const hidden = isMobile && !mobileNavOpen;
    sidebar.inert = hidden;
    sidebar.setAttribute('aria-hidden', String(hidden));
  }, [isMobile, mobileNavOpen]);

  useEffect(() => {
    if (!mobileNavOpen || !isMobile) return;
    const sidebar = sidebarRef.current;
    if (!sidebar) return;
    const focusables = () => Array.from(sidebar.querySelectorAll<HTMLElement>(
      'a[href], button:not([disabled]), select:not([disabled]), input:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
    )).filter((element) => element.getClientRects().length > 0);
    const firstLink = sidebar.querySelector<HTMLElement>('nav a, nav select, nav button');
    (firstLink ?? focusables()[0])?.focus();
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        setMobileNavOpen(false);
        requestAnimationFrame(() => toggleRef.current?.focus());
        return;
      }
      if (event.key !== 'Tab') return;
      const currentFocusables = focusables();
      if (!currentFocusables.length) {
        event.preventDefault();
        return;
      }
      const active = document.activeElement;
      if (!sidebar.contains(active)) {
        event.preventDefault();
        currentFocusables[0].focus();
        return;
      }
      const first = currentFocusables[0];
      const last = currentFocusables[currentFocusables.length - 1];
      if (event.shiftKey && active === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && active === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [isMobile, mobileNavOpen]);

  useEffect(() => {
    if (!mobileNavOpen || !isMobile) return;
    setMobileNavOpen(false);
    requestAnimationFrame(() => toggleRef.current?.focus());
  }, [location.pathname]);

  const closeMobileNav = () => {
    if (!isMobile) return;
    setMobileNavOpen(false);
    requestAnimationFrame(() => toggleRef.current?.focus());
  };

  const navLink = (kind: string, label: string, icon: typeof LayoutDashboard) => {
    const Icon = icon;
    return <NavLink key={kind} to={kind === 'overview' ? '/admin/' : `/admin/${kind}`} end={kind === 'overview'} onClick={closeMobileNav} className={({ isActive }) => cn('group flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm font-medium transition', isActive ? 'bg-indigo-600 text-white shadow-sm' : 'text-zinc-600 hover:bg-zinc-100 hover:text-zinc-950 dark:text-zinc-300 dark:hover:bg-zinc-800 dark:hover:text-white')}><Icon className="h-4 w-4 shrink-0" aria-hidden="true" /><span className="truncate">{label}</span></NavLink>;
  };

  return (
    <div className="layout min-h-screen overflow-x-hidden bg-zinc-50 text-zinc-950 dark:bg-zinc-950 dark:text-zinc-100 md:grid md:grid-cols-[17rem_minmax(0,1fr)]">
      <div className={cn('fixed inset-0 z-30 bg-zinc-950/40 backdrop-blur-sm transition-opacity md:hidden', mobileNavOpen ? 'opacity-100' : 'pointer-events-none opacity-0')} aria-hidden="true" onClick={closeMobileNav} />
      <aside ref={sidebarRef} className={cn('sidebar fixed inset-y-0 left-0 z-40 flex w-[min(19rem,calc(100vw-2rem))] -translate-x-full flex-col border-r border-zinc-200 bg-white px-4 py-4 shadow-xl transition-transform dark:border-zinc-800 dark:bg-zinc-900 md:inset-auto md:sticky md:top-0 md:h-dvh md:z-auto md:w-auto md:translate-x-0 md:shadow-none', mobileNavOpen && 'translate-x-0')}>
        <div className="flex items-center justify-between gap-3">
          <Link to="/admin/" onClick={closeMobileNav} className="brand flex items-center gap-2 text-lg font-semibold tracking-tight"><span className="grid h-9 w-9 place-items-center rounded-lg bg-indigo-600 text-white"><Zap className="h-4 w-4" aria-hidden="true" /></span>Hoorific</Link>
          <button type="button" className="rounded-lg p-2 text-zinc-500 hover:bg-zinc-100 dark:hover:bg-zinc-800 md:hidden" onClick={closeMobileNav} aria-label="Close navigation"><X className="h-5 w-5" aria-hidden="true" /></button>
        </div>
        <div className="mt-4 rounded-lg border border-zinc-200 bg-zinc-50 px-3 py-2.5 dark:border-zinc-800 dark:bg-zinc-950/50">
          <div className="flex items-center justify-between gap-2"><span className="text-xs font-medium uppercase tracking-[0.14em] text-zinc-500 dark:text-zinc-400">Session</span><Badge variant="outline" className="capitalize">{role || 'operator'}</Badge></div>
          <p className="mt-1.5 truncate text-sm font-medium text-zinc-800 dark:text-zinc-100">{session.principal.SubjectID}</p>
          <p className="mt-0.5 truncate text-xs text-zinc-500 dark:text-zinc-400">Tenant {tenantID}</p>
        </div>
        <div className="mt-3"><TenantSelector session={session} onChanged={onChanged} /></div>
        <Separator className="my-4" />
        <nav aria-label="Operations" className="min-h-0 flex-1 space-y-4 overflow-y-auto pr-1">
          <div className="space-y-0.5">{navLink('overview', 'Overview', LayoutDashboard)}{canPlayground && navLink('playground', 'Playground', Code2)}</div>
          {RESOURCE_GROUPS.map((group) => {
            const GroupIcon = group.icon;
            const kinds = group.kinds.filter((kind) => visible.includes(kind));
            if (!kinds.length) return null;
            return <div key={group.label}><div className="mb-1.5 flex items-center gap-2 px-3 text-[0.68rem] font-semibold uppercase tracking-[0.16em] text-zinc-400"><GroupIcon className="h-3.5 w-3.5" aria-hidden="true" />{group.label}</div><div className="space-y-0.5">{kinds.map((kind) => navLink(kind, LABELS[kind], group.icon))}</div></div>;
          })}
          {canConfig && <div>{navLink('config', 'Configuration', Settings2)}</div>}
        </nav>
        <div className="mt-4"><Button variant="outline" className="w-full justify-start" onClick={() => { void api.logout().then(() => window.location.reload()); }}><LogOut className="mr-2 h-4 w-4" aria-hidden="true" />Sign out</Button></div>
      </aside>
      <main className="main min-w-0 overflow-x-hidden px-4 sm:px-6 lg:px-10">
        <header className="sticky top-0 z-20 -mx-4 mb-8 border-b border-zinc-200/80 bg-zinc-50/90 px-4 py-3 backdrop-blur-xl dark:border-zinc-800/80 dark:bg-zinc-950/90 sm:-mx-6 sm:px-6 lg:-mx-10 lg:px-10">
          <div className="flex items-center justify-between gap-3">
            <div className="flex min-w-0 items-center gap-3">
              <button ref={toggleRef} type="button" className="rounded-lg border border-zinc-200 bg-white p-2 text-zinc-700 shadow-sm hover:bg-zinc-100 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-200 dark:hover:bg-zinc-800 md:hidden" onClick={() => setMobileNavOpen((open) => !open)} aria-expanded={mobileNavOpen} aria-label={mobileNavOpen ? 'Close navigation' : 'Open navigation'}>{mobileNavOpen ? <PanelLeftClose className="h-5 w-5" aria-hidden="true" /> : <PanelLeftOpen className="h-5 w-5" aria-hidden="true" />}</button>
              <div className="min-w-0"><p className="truncate text-xs font-medium uppercase tracking-[0.14em] text-zinc-500 dark:text-zinc-400">{routeLabel}</p><p className="truncate text-sm font-medium text-zinc-800 dark:text-zinc-100">Tenant {tenantID} · {role || 'operator'}</p></div>
            </div>
            <div className="flex items-center gap-1">
              <button type="button" className="rounded-lg p-2 text-zinc-600 hover:bg-zinc-200/70 dark:text-zinc-300 dark:hover:bg-zinc-800" onClick={() => setTheme((current) => current === 'dark' ? 'light' : 'dark')} aria-label={theme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'}>{theme === 'dark' ? <Sun className="h-5 w-5" aria-hidden="true" /> : <Moon className="h-5 w-5" aria-hidden="true" />}</button>
              <Link to="/admin/" className="hidden rounded-lg p-2 text-zinc-600 hover:bg-zinc-200/70 dark:text-zinc-300 dark:hover:bg-zinc-800 sm:block" aria-label="Open operations overview"><LayoutDashboard className="h-5 w-5" aria-hidden="true" /></Link>
            </div>
          </div>
        </header>
        <div key={tenantID} className="mx-auto w-full max-w-[100rem]">
          <Routes>
            <Route index element={<Overview visible={visible} canPlayground={canPlayground} />} />
            {visible.map((kind) => <Route key={kind} path={kind} element={<ResourceRoute key={kind} kind={kind} onSession={onChanged} />} />)}
            <Route path="playground" element={canPlayground ? <Playground /> : <NotFound />} />
            <Route path="config" element={canConfig ? <ConfigPage /> : <NotFound />} />
            <Route path="*" element={<NotFound />} />
          </Routes>
        </div>
      </main>
    </div>
  );
}

function AppContent() {
  const [session, setSession] = useState<Session>();
  const [error, setError] = useState<unknown>();
  const loaded = useRef(false);
  useEffect(() => {
    if (loaded.current) return;
    loaded.current = true;
    void api.loadSession().then((next) => { api.session = next; setSession(next); }).catch(setError);
  }, []);
  if (error instanceof APIError && (error.status === 401 || error.status === 403)) return <Login onSession={(next) => { api.session = next; setSession(next); setError(undefined); }} />;
  if (error) return <main className="grid min-h-screen place-items-center bg-zinc-50 p-6 dark:bg-zinc-950"><div className="w-full max-w-lg"><ErrorNotice error={error} /></div></main>;
  if (!session) return <Loading />;
  api.session = session;
  return <Routes><Route path="/admin/*" element={<Dashboard key={session.principal.TenantID} session={session} onChanged={(next) => { api.session = next; setSession(next); }} />} /><Route path="*" element={<Navigate to="/admin/" replace />} /></Routes>;
}

export function App() {
  return <BrowserRouter><AppContent /></BrowserRouter>;
}
