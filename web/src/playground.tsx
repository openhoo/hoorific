import { useEffect, useRef, useState } from 'react';
import type { ChangeEvent, FormEvent, ReactNode } from 'react';
import { Alert, AlertDescription, AlertTitle } from './components/ui/alert';
import { Badge } from './components/ui/badge';
import { Button } from './components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './components/ui/card';
import { Input } from './components/ui/input';
import { NativeSelect } from './components/ui/select';
import { Textarea } from './components/ui/textarea';
import { Check, Copy, Paperclip, Send, Square, Trash2, X } from 'lucide-react';
import { api, APIError } from './api';

type Operation = 'chat' | 'responses' | 'image' | 'audio';
type RunStatus = 'ready' | 'streaming' | 'completed' | 'error' | 'cancelled';
type PreviewKind = 'image' | 'audio';
function isTenantContextChanged(error: unknown) {
  if (!(error instanceof APIError) || error.status !== 409 || !error.body || typeof error.body !== 'object' || Array.isArray(error.body)) return false;
  return (error.body as Record<string, unknown>).code === 'tenant_context_changed';
}

function describeFailure(failure: unknown) {
  if (failure instanceof APIError && failure.body && typeof failure.body === 'object' && !Array.isArray(failure.body)) {
    const body = failure.body as Record<string, unknown>;
    const message = typeof body.message === 'string' ? body.message : typeof body.detail === 'string' ? body.detail : typeof body.title === 'string' ? body.title : failure.message;
    const code = typeof body.code === 'string' ? body.code : undefined;
    return code && !message.includes(code) ? `${message} (${code})` : message;
  }
  if (failure instanceof Error) return failure.message;
  return String(failure);
}


type OperationDetails = {
  label: string;
  description: string;
  bodyDescription: string;
  outputTitle: string;
  outputDescription: string;
  path: string;
  template: string;
  supportsFile: boolean;
  previewKind?: PreviewKind;
};

const operationDetails: Record<Operation, OperationDetails> = {
  chat: {
    label: 'Chat generation',
    description: 'OpenAI Chat-compatible text generation.',
    bodyDescription: 'JSON is parsed in this tab and sent as-is. It is not saved.',
    outputTitle: 'Chat response',
    outputDescription: 'Raw response events remain visible while the gateway sends them.',
    path: '/playground/v1/chat/completions',
    template: '{\n  "model": "",\n  "messages": [{"role":"user","content":""}],\n  "stream": true\n}',
    supportsFile: true,
  },
  responses: {
    label: 'Responses',
    description: 'Stateless OpenAI Responses generation; store:false is required here.',
    bodyDescription: 'The starter body uses input content and store:false for the portable Responses contract.',
    outputTitle: 'Responses output',
    outputDescription: 'Raw response events remain inspectable; the body is not saved.',
    path: '/playground/v1/responses',
    template: '{\n  "model": "",\n  "input": [{"role":"user","content":[{"type":"input_text","text":""}]}],\n  "store": false,\n  "stream": true\n}',
    supportsFile: true,
  },
  image: {
    label: 'Image generation',
    description: 'Image generation with a base64 response for an in-page preview.',
    bodyDescription: 'Use image-generation fields such as prompt, size, and response_format.',
    outputTitle: 'Image response',
    outputDescription: 'A safe returned image is previewed first; response details remain available on demand.',
    path: '/playground/v1/images/generations',
    template: '{\n  "model": "",\n  "prompt": "",\n  "n": 1,\n  "size": "1024x1024",\n  "response_format": "b64_json"\n}',
    supportsFile: false,
    previewKind: 'image',
  },
  audio: {
    label: 'Audio speech',
    description: 'Speech synthesis; the starter requests a playable WAV response.',
    bodyDescription: 'Use speech fields such as input, voice, response_format, and speed.',
    outputTitle: 'Audio response',
    outputDescription: 'Binary audio is playable first when the provider returns a supported media type; response details remain available on demand.',
    path: '/playground/v1/audio/speech',
    template: '{\n  "model": "",\n  "input": "",\n  "voice": "alloy",\n  "response_format": "wav"\n}',
    supportsFile: false,
    previewKind: 'audio',
  },
};

const operationPaths = Object.fromEntries(Object.entries(operationDetails).map(([key, details]) => [key, details.path])) as Record<Operation, string>;
const operationTemplates = Object.fromEntries(Object.entries(operationDetails).map(([key, details]) => [key, details.template])) as Record<Operation, string>;

const statusLabels: Record<RunStatus, string> = {
  ready: 'Ready',
  streaming: 'Receiving',
  completed: 'Completed',
  error: 'Error',
  cancelled: 'Cancelled',
};

const statusDescriptions: Record<RunStatus, string> = {
  ready: 'Choose an operation and send a request.',
  streaming: 'Receiving response data from the gateway…',
  completed: 'Response received. Review the raw output below.',
  error: 'The response could not be completed. Review the message and raw output.',
  cancelled: 'Browser stream aborted; the upstream outcome is unknown.',
};

function PlaygroundField({ label, htmlFor, description, children }: { label: string; htmlFor: string; description?: ReactNode; children: ReactNode }) {
  return <div className="field min-w-0 space-y-2">
    <label htmlFor={htmlFor} className="text-sm font-medium leading-none text-foreground">{label}</label>
    {children}
    {description && <p id={`${htmlFor}-description`} className="text-xs leading-relaxed text-muted-foreground">{description}</p>}
  </div>;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function isSafePreviewURL(value: string) {
  if (value.startsWith('data:')) return /^data:(?:image|audio)\/[^;,]+;base64,[A-Za-z0-9+/=]+$/i.test(value);
  if (typeof window === 'undefined') return false;
  try {
    const url = new URL(value, window.location.origin);
    return (url.protocol === 'http:' || url.protocol === 'https:') && url.origin === window.location.origin;
  } catch {
    return false;
  }
}

function findMediaCandidate(value: unknown, operation: PreviewKind): string | undefined {
  if (Array.isArray(value)) {
    for (const item of value) {
      const candidate = findMediaCandidate(item, operation);
      if (candidate) return candidate;
    }
    return undefined;
  }
  if (!isRecord(value)) return undefined;
  for (const [key, item] of Object.entries(value)) {
    if (key === 'b64_json' && typeof item === 'string' && /^[A-Za-z0-9+/]+={0,2}$/.test(item)) {
      return `data:${operation === 'image' ? 'image/png' : 'audio/mpeg'};base64,${item}`;
    }
    if (key === 'url' && operation === 'image' && typeof item === 'string') return item;
    const nested = findMediaCandidate(item, operation);
    if (nested) return nested;
  }
  return undefined;
}

function extractMediaPreview(text: string, operation: Operation): string | undefined {
  const previewKind = operationDetails[operation].previewKind;
  if (!previewKind) return undefined;
  const directPattern = previewKind === 'image'
    ? /data:image\/[^;,\s]+;base64,[A-Za-z0-9+/=]+/i
    : /data:audio\/[^;,\s]+;base64,[A-Za-z0-9+/=]+/i;
  const direct = text.match(directPattern)?.[0];
  if (direct && isSafePreviewURL(direct)) return direct;
  const parsed: unknown[] = [];
  try {
    parsed.push(JSON.parse(text));
  } catch {
    // Streaming responses are parsed one data line at a time below.
  }
  for (const line of text.split(/\r?\n/)) {
    const data = line.trim().replace(/^data:\s*/, '');
    if (!data || data === '[DONE]') continue;
    try {
      parsed.push(JSON.parse(data));
    } catch {
      // The next chunk may complete this event.
    }
  }
  for (const value of parsed) {
    const candidate = findMediaCandidate(value, previewKind);
    if (candidate && isSafePreviewURL(candidate)) return candidate;
  }
  return undefined;
}

function findResponseError(value: unknown): string | undefined {
  if (Array.isArray(value)) {
    for (const item of value) {
      const message = findResponseError(item);
      if (message) return message;
    }
    return undefined;
  }
  if (!isRecord(value)) return undefined;
  const error = value.error;
  if (typeof error === 'string' && error.trim()) return error;
  if (isRecord(error)) {
    for (const key of ['message', 'detail', 'title']) {
      if (typeof error[key] === 'string' && error[key].trim()) return error[key] as string;
    }
  }
  if (typeof value.type === 'string' && /error/i.test(value.type)) {
    for (const key of ['message', 'detail', 'title']) {
      if (typeof value[key] === 'string' && value[key].trim()) return value[key] as string;
    }
  }
  for (const item of Object.values(value)) {
    const message = findResponseError(item);
    if (message) return message;
  }
  return undefined;
}

function extractResponseError(text: string) {
  const parsed: unknown[] = [];
  try {
    parsed.push(JSON.parse(text));
  } catch {
    // SSE responses are parsed one data line at a time below.
  }
  for (const line of text.split(/\r?\n/)) {
    const data = line.trim().replace(/^data:\s*/, '');
    if (!data || data === '[DONE]') continue;
    try {
      parsed.push(JSON.parse(data));
    } catch {
      // The next chunk may complete this event.
    }
  }
  for (const value of parsed) {
    const message = findResponseError(value);
    if (message) return message;
  }
  return undefined;
}

function appendSelectedFile(operation: Operation, body: unknown, file: File, data: string) {
  if (!isRecord(body)) throw new Error('The selected file needs an object request body.');
  if (operation !== 'chat' && operation !== 'responses') throw new Error('This operation does not accept an input file.');
  const image = file.type.toLowerCase().startsWith('image/');
  const next = { ...body };
  if (operation === 'chat') {
    const messages = Array.isArray(next.messages) ? next.messages.map(item => isRecord(item) ? { ...item } : item) : [];
    let userIndex = -1;
    for (let index = messages.length - 1; index >= 0; index -= 1) {
      if (isRecord(messages[index]) && messages[index].role === 'user') {
        userIndex = index;
        break;
      }
    }
    if (userIndex < 0 || !isRecord(messages[userIndex])) throw new Error('Add a user message before attaching a file.');

    const message = messages[userIndex] as Record<string, unknown>;
    const content = Array.isArray(message.content)
      ? [...message.content]
      : [{ type: 'text', text: typeof message.content === 'string' ? message.content : '' }];
    content.push(image
      ? { type: 'image_url', image_url: { url: data } }
      : { type: 'file', file: { file_data: data, filename: file.name } });
    message.content = content;
    next.messages = messages;
  } else {
    const input = Array.isArray(next.input) ? next.input.map(item => isRecord(item) ? { ...item } : item) : [];
    let userIndex = -1;
    for (let index = input.length - 1; index >= 0; index -= 1) {
      if (isRecord(input[index]) && input[index].role === 'user') {
        userIndex = index;
        break;
      }
    }
    if (userIndex < 0 || !isRecord(input[userIndex])) throw new Error('Add a user input before attaching a file.');
    const message = input[userIndex] as Record<string, unknown>;
    const content = Array.isArray(message.content)
      ? [...message.content]
      : [{ type: 'input_text', text: typeof message.content === 'string' ? message.content : '' }];
    content.push(image
      ? { type: 'input_image', image_url: data }
      : { type: 'input_file', file_data: data, filename: file.name });
    message.content = content;
    next.input = input;
  }
  return next;
}

function formatBytes(bytes: number) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}

export function Playground({ onReloadTenantContext }: { onReloadTenantContext: () => Promise<void> }) {
  const [operation, setOperation] = useState<Operation>('chat');
  const [path, setPath] = useState(operationPaths.chat);
  const [payload, setPayload] = useState(operationTemplates.chat);
  const [file, setFile] = useState<File>();
  const [output, setOutput] = useState('');
  const [mediaURL, setMediaURL] = useState('');
  const [mediaOperation, setMediaOperation] = useState<Operation>();
  const [mediaMime, setMediaMime] = useState('');
  const [mediaIsBinary, setMediaIsBinary] = useState(false);
  const [resultOperation, setResultOperation] = useState<Operation>('chat');
  const [error, setError] = useState<unknown>();
  const [running, setRunning] = useState(false);
  const [status, setStatus] = useState<RunStatus>('ready');
  const [copying, setCopying] = useState(false);
  const [copyFeedback, setCopyFeedback] = useState<'copied' | 'failed'>();
  const [reloadingContext, setReloadingContext] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const starting = useRef(false);

  const fileInput = useRef<HTMLInputElement | null>(null);
  const mediaObjectURL = useRef<string | null>(null);
  const payloadDrafts = useRef<Record<Operation, string>>({...operationTemplates});
  const pathDrafts = useRef<Record<Operation, string>>({...operationPaths});
  const mounted = useRef(true);

  const revokeMediaObjectURL = () => {
    const url = mediaObjectURL.current;
    if (!url) return;
    if (typeof URL !== 'undefined') URL.revokeObjectURL(url);
    mediaObjectURL.current = null;
  };
  const clearMedia = () => {
    revokeMediaObjectURL();
    setMediaURL('');
    setMediaOperation(undefined);
    setMediaMime('');
    setMediaIsBinary(false);
  };

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      controller.current?.abort();
      controller.current = null;
      revokeMediaObjectURL();
    };
  }, []);

  const handleOperationChange = (event: ChangeEvent<HTMLSelectElement>) => {
    const next = event.currentTarget.value as Operation;
    if (!operationDetails[next]) return;
    payloadDrafts.current[operation] = payload;
    pathDrafts.current[operation] = path;
    setOperation(next);
    setPath(pathDrafts.current[next]);
    setPayload(payloadDrafts.current[next]);
    setError(undefined);

  };
  const handlePayloadChange = (value: string) => {
    setPayload(value);
    payloadDrafts.current[operation] = value;
    if (error instanceof Error && error.message === 'Request JSON is invalid.') setError(undefined);
  };
  const handlePathChange = (value: string) => {
    setPath(value);
    pathDrafts.current[operation] = value;
  };
  const handleFileChange = (event: ChangeEvent<HTMLInputElement>) => {
    setFile(event.currentTarget.files?.[0]);
  };
  const clearFileSelection = () => {
    setFile(undefined);
    if (fileInput.current) fileInput.current.value = '';
  };

  const start = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (running || starting.current) return;
    starting.current = true;
    setRunning(true);

    setResultOperation(operation);
    setError(undefined);
    setOutput('');
    clearMedia();
    setCopyFeedback(undefined);
    setStatus('ready');
    let body: unknown;
    try {
      body = JSON.parse(payload);
    } catch {
      if (mounted.current) {
        setError(new Error('Request JSON is invalid.'));
        setStatus('error');
        starting.current = false;
        setRunning(false);
      }
      return;

    }
    if (file && operationDetails[operation].supportsFile) {

      try {
        const data = await new Promise<string>((resolve, reject) => {
          const reader = new FileReader();
          reader.onload = () => resolve(String(reader.result));
          reader.onerror = () => reject(reader.error ?? new Error('File read failed'));
          reader.readAsDataURL(file);
        });
        if (!mounted.current) return;
        body = appendSelectedFile(operation, body, file, data);
      } catch (failure) {
        if (mounted.current) {
          setError(failure);
          setStatus('error');
          starting.current = false;
          setRunning(false);
        }
        return;

      }
    }
    if (!mounted.current) return;
    const abort = new AbortController();
    controller.current = abort;
    setStatus('streaming');

    let accumulatedOutput = '';
    let discoveredPreview = '';
    try {
      await api.stream(path, body, abort.signal, chunk => {
        if (!mounted.current) return;
        accumulatedOutput += chunk;
        setOutput(accumulatedOutput);
        const preview = extractMediaPreview(accumulatedOutput, operation);
        if (preview && preview !== discoveredPreview) {
          discoveredPreview = preview;
          revokeMediaObjectURL();
          setMediaURL(preview);
          setMediaOperation(operation);
          setMediaMime(operationDetails[operation].previewKind === 'image' ? 'image/png' : 'audio/mpeg');
          setMediaIsBinary(false);
        }
      }, {
        onMedia: (blob, contentType) => {
          if (!mounted.current) return;
          if (typeof URL === 'undefined' || typeof URL.createObjectURL !== 'function') throw new Error('This browser cannot preview media responses.');
          revokeMediaObjectURL();
          const objectURL = URL.createObjectURL(blob);
          mediaObjectURL.current = objectURL;
          discoveredPreview = objectURL;
          setMediaURL(objectURL);
          const actualMediaOperation = contentType.toLowerCase().startsWith('image/') ? 'image' : contentType.toLowerCase().startsWith('audio/') ? 'audio' : operation;
          setMediaOperation(actualMediaOperation);
          setMediaMime(contentType);
          setMediaIsBinary(true);
          accumulatedOutput = `Received ${contentType} media response (${formatBytes(blob.size)}).`;
          setOutput(accumulatedOutput);
        },
      });
      const responseError = extractResponseError(accumulatedOutput);
      if (responseError) throw new Error(responseError);
      if (mounted.current) setStatus('completed');
    } catch (failure) {
      if (!mounted.current) return;
      if (abort.signal.aborted || (failure && typeof failure === 'object' && 'name' in failure && failure.name === 'AbortError')) {
        setStatus('cancelled');
      } else {
        setError(failure);
        setStatus('error');
      }
    } finally {
      starting.current = false;
      if (controller.current === abort) controller.current = null;
      if (mounted.current) setRunning(false);
    }
  };
  const cancel = () => {
    const active = controller.current;
    if (!active) return;
    setStatus('cancelled');
    active.abort();
  };
  const clearOutput = () => {
    if (running) return;
    setOutput('');
    clearMedia();
    setCopyFeedback(undefined);
  };
  const copyOutput = async () => {
    if (!output || copying) return;
    setCopying(true);
    setCopyFeedback(undefined);
    try {
      if (typeof navigator === 'undefined' || !navigator.clipboard?.writeText) throw new Error('Clipboard access is unavailable.');
      await navigator.clipboard.writeText(output);
      if (mounted.current) setCopyFeedback('copied');
    } catch {
      if (mounted.current) setCopyFeedback('failed');
    } finally {
      if (mounted.current) setCopying(false);
    }
  };
  const reloadContext = async () => {
    if (reloadingContext) return;
    setReloadingContext(true);
    try {
      await onReloadTenantContext();
      if (mounted.current) {
        setError(undefined);
        setStatus('ready');
      }
    } catch (failure) {
      if (mounted.current) {
        setError(failure);
        setStatus('error');
      }
    } finally {
      if (mounted.current) setReloadingContext(false);
    }
  };
  const details = operationDetails[operation];
  const outputDetails = operationDetails[resultOperation];
  const statusLabel = statusLabels[status];
  const statusDescription = statusDescriptions[status];
  const statusVariant = status === 'error' ? 'destructive' : status === 'streaming' ? 'default' : status === 'completed' ? 'outline' : 'secondary';
  const invalidPayload = error instanceof Error && error.message === 'Request JSON is invalid.';
  const errorMessage = describeFailure(error);
  const emptyOutput = running ? 'Waiting for response data…' : 'No response yet.';
  const mediaExtension = mediaMime.split('/')[1]?.split(';')[0] || (mediaOperation === 'audio' ? 'wav' : 'png');
  const mediaDownloadName = `${mediaOperation === 'audio' ? 'playground-audio' : 'playground-image'}.${mediaExtension}`;
  return <section className="playground mx-auto w-full max-w-[88rem] space-y-5">
    <header className="flex flex-wrap items-start justify-between gap-4">
      <div className="min-w-0">
        <h1 className="text-2xl font-semibold tracking-tight text-foreground">Inference playground</h1>
        <p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">Prepare an authenticated gateway request and inspect its response without leaving this page.</p>
      </div>
    </header>

    <div className="grid items-start gap-5 xl:grid-cols-2">
      <Card className="flex min-w-0 flex-col overflow-hidden">
        <CardHeader className="border-b border-border/70 pb-5">
          <CardTitle className="text-lg">Request</CardTitle>
          <CardDescription>Choose an operation, edit its JSON body, and submit it to the gateway.</CardDescription>
        </CardHeader>
        <form onSubmit={start} className="flex min-h-0 flex-1 flex-col">
          <CardContent className="flex-1 space-y-5 pt-6">
            <div className="flex flex-wrap items-center justify-end gap-2 border-b border-border/70 pb-4">
              <div className="form-actions mt-0 flex flex-wrap gap-2">
                <Button type="submit" disabled={running}><Send aria-hidden="true" />Send request</Button>
                {running && <Button type="button" variant="destructive" onClick={cancel}><Square aria-hidden="true" />Cancel</Button>}
              </div>
            </div>
            <fieldset className="space-y-5 border-0 p-0">
              <legend className="sr-only">Request configuration</legend>
              <div className="grid gap-4 sm:grid-cols-2">
                <PlaygroundField label="Operation" htmlFor="playground-operation" description={details.description}>
                  <NativeSelect id="playground-operation" value={operation} onChange={handleOperationChange} disabled={running} aria-describedby="playground-operation-description">
                    <option value="chat">{operationDetails.chat.label}</option>
                    <option value="responses">{operationDetails.responses.label}</option>
                    <option value="image">{operationDetails.image.label}</option>
                    <option value="audio">{operationDetails.audio.label}</option>
                  </NativeSelect>
                </PlaygroundField>
                <PlaygroundField label="Gateway operation path" htmlFor="playground-path" description="Defaults to the selected operation and remains editable for compatible gateway paths.">
                  <Input id="playground-path" value={path} disabled={running} onInput={event => handlePathChange(event.currentTarget.value)} aria-describedby="playground-path-description" />
                </PlaygroundField>
              </div>
              <PlaygroundField label="Request JSON" htmlFor="playground-payload" description={details.bodyDescription}>
                <Textarea id="playground-payload" disabled={running} className="play-editor min-h-[18rem] resize-y font-mono text-sm leading-relaxed" value={payload} onInput={event => handlePayloadChange(event.currentTarget.value)} aria-describedby="playground-payload-description" aria-invalid={invalidPayload ? 'true' : undefined} aria-errormessage={invalidPayload ? 'playground-request-error' : undefined} />
              </PlaygroundField>
              {details.supportsFile && <PlaygroundField label="Optional input media/document" htmlFor="playground-file" description="Added to the last user message as an image or file content at submission.">
                <div className="flex items-center gap-3"><Paperclip aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" /><Input ref={fileInput} id="playground-file" type="file" disabled={running} onChange={handleFileChange} aria-describedby="playground-file-description" className="h-auto min-w-0 cursor-pointer py-2 file:mr-3 file:rounded-md file:border-0 file:bg-primary file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-primary-foreground" /></div>
                {file !== undefined && <div className="flex flex-wrap items-center justify-between gap-2"><p className="text-xs text-muted-foreground">Selected: {file.name}</p><Button type="button" variant="ghost" size="sm" disabled={running} onClick={clearFileSelection}><X aria-hidden="true" />Remove</Button></div>}

              </PlaygroundField>}
              {file !== undefined && !details.supportsFile && <div className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border/70 px-3 py-2"><p className="text-xs text-muted-foreground">Selected file retained, but {details.label} does not accept file input; it will not be sent.</p><Button type="button" variant="ghost" size="sm" disabled={running} onClick={clearFileSelection}><X aria-hidden="true" />Remove</Button></div>}
            </fieldset>

            {Boolean(error) && (isTenantContextChanged(error) ? (
              <Alert id="playground-request-error" variant="destructive">
                <AlertTitle>Tenant context changed</AlertTitle>
                <AlertDescription className="flex flex-wrap items-center gap-3">
                  <span>Another tab changed the active tenant. Nothing was retried. Reload the tenant context before continuing.</span>
                  <Button type="button" variant="outline" size="sm" disabled={reloadingContext} onClick={() => void reloadContext()}>
                    {reloadingContext ? 'Reloading…' : 'Reload tenant context'}
                  </Button>
                </AlertDescription>
              </Alert>
            ) : (
              <Alert id="playground-request-error" variant="destructive"><AlertTitle>Request failed</AlertTitle><AlertDescription>{errorMessage}</AlertDescription></Alert>
            ))}
          </CardContent>
        </form>
      </Card>

      <Card className="stream-output flex min-w-0 flex-col overflow-hidden">
        <CardHeader className="sticky top-0 z-10 border-b border-border/70 bg-card/95 pb-5 backdrop-blur">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <CardTitle className="text-lg">{outputDetails.outputTitle}</CardTitle>
              <CardDescription>{outputDetails.outputDescription}</CardDescription>
            </div>
            <Badge variant={statusVariant} className="shrink-0" aria-label={`Request status: ${statusLabel}`}>{statusLabel}</Badge>
          </div>
        </CardHeader>
        <CardContent className="flex min-h-0 flex-1 flex-col gap-4 pt-6">
          <div className="flex flex-wrap items-center justify-between gap-3">
            {(status === 'streaming' || status === 'cancelled') && <p className="text-sm text-muted-foreground" role="status" aria-live="polite">{statusDescription}</p>}
            <div className="flex flex-wrap gap-2">
              <Button type="button" variant="outline" size="sm" onClick={copyOutput} disabled={!output || copying} aria-label="Copy response text">
                {copyFeedback === 'copied' ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
                {copying ? 'Copying…' : 'Copy response text'}
              </Button>
              <Button type="button" variant="ghost" size="sm" onClick={clearOutput} disabled={running || (!output && !mediaURL)} aria-label="Clear output"><Trash2 aria-hidden="true" />Clear output</Button>
            </div>
          </div>
          {mediaURL && mediaOperation === 'image' && <div className="space-y-2 rounded-lg border border-border/70 p-3">
            <p className="text-xs font-medium text-muted-foreground">Image preview</p>
            <img className="media-result block max-h-[30rem] max-w-full rounded-lg object-contain" src={mediaURL} alt="Generated image returned by provider" loading="lazy" decoding="async" referrerPolicy="no-referrer" />
            <a className="text-sm text-primary underline-offset-4 hover:underline" href={mediaURL} download={mediaDownloadName}>Download image</a>
          </div>}
          {mediaURL && mediaOperation === 'audio' && <div className="space-y-2 rounded-lg border border-border/70 p-3">
            <p className="text-xs font-medium text-muted-foreground">{mediaIsBinary ? `Playable ${mediaMime || 'audio'} response` : 'Audio preview'}</p>
            <audio className="block w-full" controls preload="metadata" src={mediaURL} />
            <a className="text-sm text-primary underline-offset-4 hover:underline" href={mediaURL} download={mediaDownloadName}>Download audio</a>
          </div>}
          {mediaURL && mediaOperation && mediaOperation !== 'chat' ? (
            <details className="min-w-0 rounded-lg border border-border/70">
              <summary className="cursor-pointer px-3 py-2 text-sm font-medium text-foreground">Response details</summary>
              <div className="result-wrap min-w-0 px-3 pb-3">
                <pre role="region" aria-label={`Raw ${outputDetails.outputTitle.toLowerCase()}`} tabIndex={0} className="result max-h-[24rem] overflow-auto rounded-lg bg-slate-950 p-4 font-mono text-sm leading-relaxed text-slate-100 shadow-inner">{output || emptyOutput}</pre>
              </div>
            </details>
          ) : (
            <div className="result-wrap min-w-0 flex-1">
              <pre role="region" aria-label={`Raw ${outputDetails.outputTitle.toLowerCase()}`} tabIndex={0} className="result min-h-[18rem] max-h-[32rem] overflow-auto rounded-lg border border-border/70 bg-slate-950 p-4 font-mono text-sm leading-relaxed text-slate-100 shadow-inner">{output || emptyOutput}</pre>
            </div>
          )}
          {copyFeedback === 'copied' && <p className="text-xs text-muted-foreground" role="status" aria-live="polite">Response text copied to the clipboard.</p>}
          {copyFeedback === 'failed' && <p className="text-xs text-destructive" role="status" aria-live="polite">Could not copy response text. Check clipboard permissions and try again.</p>}
        </CardContent>
      </Card>
    </div>
  </section>;
}
