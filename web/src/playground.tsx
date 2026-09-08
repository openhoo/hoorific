import { useEffect, useRef, useState } from 'react';
import type { ChangeEvent, FormEvent, ReactNode } from 'react';
import { Alert, AlertDescription, AlertTitle } from './components/ui/alert';
import { Badge } from './components/ui/badge';
import { Button } from './components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './components/ui/card';
import { Input } from './components/ui/input';
import { NativeSelect } from './components/ui/select';
import { Textarea } from './components/ui/textarea';
import { Check, Copy, Paperclip, Send, Square, Trash2 } from 'lucide-react';
import { api, APIError } from './api';

type Operation = 'chat' | 'responses' | 'image' | 'audio' | 'video';
type RunStatus = 'ready' | 'streaming' | 'completed' | 'error' | 'cancelled';

const operationPaths: Record<Operation, string> = {
  chat: '/playground/v1/chat/completions',
  responses: '/playground/v1/responses',
  image: '/playground/v1/images/generations',
  audio: '/playground/v1/audio/speech',
  video: '/playground/v1/videos',
};

const statusLabels: Record<RunStatus, string> = {
  ready: 'Ready',
  streaming: 'Streaming',
  completed: 'Completed',
  error: 'Error',
  cancelled: 'Cancelled',
};

const statusDescriptions: Record<RunStatus, string> = {
  ready: 'Ready to send a request.',
  streaming: 'Streaming response events from the gateway…',
  completed: 'Completed. The response is ready to review.',
  error: 'Error. Review the message and try again.',
  cancelled: 'Cancelled. The request was stopped.',
};

function PlaygroundField({ label, htmlFor, description, children }: { label: string; htmlFor: string; description?: ReactNode; children: ReactNode }) {
  return <div className="field min-w-0 space-y-2">
    <label htmlFor={htmlFor} className="text-sm font-medium leading-none text-foreground">{label}</label>
    {children}
    {description && <p id={`${htmlFor}-description`} className="text-xs leading-relaxed text-muted-foreground">{description}</p>}
  </div>;
}

function isSafePreviewURL(value: string) {
  if (value.startsWith('data:')) return /^data:(?:image|audio|video)\/[^;,]+;base64,[A-Za-z0-9+/=]+$/i.test(value);
  try {
    const url = new URL(value, window.location.origin);
    return url.protocol === 'http:' || url.protocol === 'https:';
  } catch {
    return false;
  }
}

export function Playground() {
  const [operation, setOperation] = useState<Operation>('chat');
  const [path, setPath] = useState(operationPaths.chat);
  const [payload, setPayload] = useState('{\n  "model": "",\n  "messages": [{"role":"user","content":""}],\n  "stream": true\n}');
  const [file, setFile] = useState<File>();
  const [output, setOutput] = useState('');
  const [mediaURL, setMediaURL] = useState('');
  const [error, setError] = useState<unknown>();
  const [running, setRunning] = useState(false);
  const [status, setStatus] = useState<RunStatus>('ready');
  const [copying, setCopying] = useState(false);
  const [copyFeedback, setCopyFeedback] = useState<'copied' | 'failed'>();
  const controller = useRef<AbortController | null>(null);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      controller.current?.abort();
      controller.current = null;
    };
  }, []);

  const handleOperationChange = (event: ChangeEvent<HTMLSelectElement>) => {
    const next = event.currentTarget.value as Operation;
    setOperation(next);
    setPath(operationPaths[next]);
  };
  const handleFileChange = (event: ChangeEvent<HTMLInputElement>) => {
    setFile(event.currentTarget.files?.[0]);
  };
  const start = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (running) return;
    setError(undefined);
    setOutput('');
    setMediaURL('');
    setCopyFeedback(undefined);
    setStatus('ready');
    let body: unknown;
    try {
      body = JSON.parse(payload);
    } catch {
      if (mounted.current) {
        setError(new Error('Request JSON is invalid.'));
        setStatus('error');
      }
      return;
    }
    if (file) {
      try {
        const data = await new Promise<string>((resolve, reject) => {
          const reader = new FileReader();
          reader.onload = () => resolve(String(reader.result));
          reader.onerror = () => reject(reader.error ?? new Error('File read failed'));
          reader.readAsDataURL(file);
        });
        if (!mounted.current) return;
        if (body && typeof body === 'object') body = { ...body, input_file: { name: file.name, media_type: file.type, data } };
      } catch (failure) {
        if (mounted.current) {
          setError(failure);
          setStatus('error');
        }
        return;
      }
    }
    if (!mounted.current) return;
    const abort = new AbortController();
    controller.current = abort;
    setRunning(true);
    setStatus('streaming');
    try {
      await api.stream(path, body, abort.signal, chunk => {
        if (!mounted.current) return;
        setOutput(current => current + chunk);
        const match = chunk.match(/https?:\/\/[^\s"'<>]+|data:(?:image|audio|video)\/[^;,]+;base64,[A-Za-z0-9+/=]+/);
        if (match && isSafePreviewURL(match[0])) setMediaURL(match[0]);
      });
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
    setMediaURL('');
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
  const errorMessage = error instanceof APIError || error instanceof Error ? error.message : String(error);
  const statusLabel = statusLabels[status];
  const statusDescription = statusDescriptions[status];
  const statusVariant = status === 'error' ? 'destructive' : status === 'streaming' ? 'default' : status === 'completed' ? 'outline' : 'secondary';
  const invalidPayload = error instanceof Error && error.message === 'Request JSON is invalid.';
  const emptyOutput = running ? 'Waiting for events…' : 'No output yet.';
  return <section className="playground mx-auto w-full max-w-[88rem] space-y-5">
    <header className="flex flex-wrap items-start justify-between gap-4">
      <div className="min-w-0">
        <h1 className="text-2xl font-semibold tracking-tight text-foreground">Inference playground</h1>
        <p className="mt-1 max-w-3xl text-sm leading-relaxed text-muted-foreground">Send a JSON request through the authenticated gateway and inspect streamed response events without leaving this page.</p>
      </div>
      <Badge variant={statusVariant} className="shrink-0" aria-label={`Request status: ${statusLabel}`}>{statusLabel}</Badge>
    </header>

    <div className="grid items-start gap-5 xl:grid-cols-2">
      <Card className="flex min-w-0 flex-col overflow-hidden">
        <CardHeader className="border-b border-border/70 pb-5">
          <CardTitle className="text-lg">Request</CardTitle>
          <CardDescription>Choose an operation, edit its JSON body, and submit it to the gateway.</CardDescription>
        </CardHeader>
        <form onSubmit={start} className="flex min-h-0 flex-1 flex-col">
          <CardContent className="flex-1 space-y-5 pt-6">
            <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border/70 pb-4">
              <p className="text-xs leading-relaxed text-muted-foreground">{statusDescription}</p>
              <div className="form-actions mt-0 flex flex-wrap gap-2">
                <Button type="submit" disabled={running}><Send aria-hidden="true" />Send request</Button>
                {running && <Button type="button" variant="destructive" onClick={cancel}><Square aria-hidden="true" />Cancel</Button>}
              </div>
            </div>
            <fieldset className="space-y-5 border-0 p-0">
              <legend className="sr-only">Request configuration</legend>
              <div className="grid gap-4 sm:grid-cols-2">
                <PlaygroundField label="Operation" htmlFor="playground-operation" description="Select the gateway operation.">
                  <NativeSelect id="playground-operation" value={operation} onChange={handleOperationChange} aria-describedby="playground-operation-description">
                    <option value="chat">Chat generation</option>
                    <option value="responses">Responses</option>
                    <option value="image">Image generation</option>
                    <option value="audio">Audio speech</option>
                    <option value="video">Video job</option>
                  </NativeSelect>
                </PlaygroundField>
                <PlaygroundField label="Gateway operation path" htmlFor="playground-path" description="Editable gateway endpoint.">
                  <Input id="playground-path" value={path} onInput={event => setPath(event.currentTarget.value)} aria-describedby="playground-path-description" />
                </PlaygroundField>
              </div>
              <PlaygroundField label="JSON request" htmlFor="playground-payload" description="The request body is parsed in this tab and sent as JSON. It is not saved.">
                <Textarea id="playground-payload" className="play-editor min-h-[18rem] resize-y font-mono text-sm leading-relaxed" value={payload} onInput={event => { setPayload(event.currentTarget.value); if (invalidPayload) setError(undefined); }} aria-describedby="playground-payload-description" aria-invalid={invalidPayload ? 'true' : undefined} aria-errormessage={invalidPayload ? 'playground-request-error' : undefined} />
              </PlaygroundField>
              <PlaygroundField label="Optional input media/document" htmlFor="playground-file" description="Files stay in this tab until submission and are encoded as input_file data.">
                <div className="flex items-center gap-3"><Paperclip aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" /><Input id="playground-file" type="file" onChange={handleFileChange} aria-describedby="playground-file-description" className="h-auto min-w-0 cursor-pointer py-2 file:mr-3 file:rounded-md file:border-0 file:bg-primary file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-primary-foreground" /></div>
                {file !== undefined && <p className="text-xs text-muted-foreground">Selected: {file.name}</p>}
              </PlaygroundField>
            </fieldset>

            {Boolean(error) && <Alert id="playground-request-error" variant="destructive"><AlertTitle>Request failed</AlertTitle><AlertDescription>{errorMessage}</AlertDescription></Alert>}
          </CardContent>
        </form>
      </Card>

      <Card className="stream-output flex min-w-0 flex-col overflow-hidden">
        <CardHeader className="sticky top-0 z-10 border-b border-border/70 bg-card/95 pb-5 backdrop-blur">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <CardTitle className="text-lg">Stream output</CardTitle>
              <CardDescription>Response chunks appear here as the gateway sends them. Media URLs are previewed when the selected operation supports them.</CardDescription>
            </div>
            <Badge variant={statusVariant} className="shrink-0">{statusLabel}</Badge>
          </div>
        </CardHeader>
        <CardContent className="flex min-h-0 flex-1 flex-col gap-4 pt-6">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="text-sm text-muted-foreground" role="status" aria-live="polite">{statusDescription}</p>
            <div className="flex flex-wrap gap-2">
              <Button type="button" variant="outline" size="sm" onClick={copyOutput} disabled={!output || copying} aria-label="Copy output">
                {copyFeedback === 'copied' ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
                {copying ? 'Copying…' : 'Copy output'}
              </Button>
              <Button type="button" variant="ghost" size="sm" onClick={clearOutput} disabled={running || (!output && !mediaURL)} aria-label="Clear output"><Trash2 aria-hidden="true" />Clear output</Button>
            </div>
          </div>
          <div className="result-wrap min-w-0 flex-1">
            <pre className="result min-h-[18rem] max-h-[32rem] overflow-auto rounded-lg border border-border/70 bg-slate-950 p-4 font-mono text-sm leading-relaxed text-slate-100 shadow-inner">{output || emptyOutput}</pre>
          </div>
          {copyFeedback === 'copied' && <p className="text-xs text-muted-foreground" role="status" aria-live="polite">Output copied to the clipboard.</p>}
          {copyFeedback === 'failed' && <p className="text-xs text-destructive" role="status" aria-live="polite">Could not copy output. Check clipboard permissions and try again.</p>}
          {mediaURL && operation === 'image' && <img className="media-result mt-2 block max-h-[30rem] max-w-full rounded-lg border border-border/70 object-contain" src={mediaURL} alt="Generated image returned by provider" loading="lazy" decoding="async" referrerPolicy="no-referrer" />}
          {mediaURL && operation === 'audio' && <audio className="mt-2 block w-full" controls preload="metadata" src={mediaURL} />}
          {mediaURL && operation === 'video' && <video className="mt-2 block max-h-[30rem] max-w-full rounded-lg border border-border/70" controls preload="metadata" playsInline src={mediaURL} />}
        </CardContent>
      </Card>
    </div>
  </section>;
}
