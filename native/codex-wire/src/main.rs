use std::collections::HashMap;
use std::env;
use std::fs;
use std::future::Future;
use std::io::{self, Write as StdWrite};
use std::net::{IpAddr, SocketAddr};
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use http::{HeaderMap, HeaderName, HeaderValue, Method};
use openssl::version::version as openssl_version;
use reqwest::{Client, Response, Url};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{unix::OwnedReadHalf, UnixListener, UnixStream};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};
use tokio::task::JoinSet;
use tokio::time::timeout;

#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;

const PROTOCOL: u32 = 1;
const PROFILE: &str = "codex-exec";
const PROFILE_VERSION: &str = "0.153.4";
const MAX_HEADER_FRAME: usize = 64 * 1024;
const MAX_BODY_BYTES: usize = 32 * 1024 * 1024;
const MAX_CHUNK_BYTES: usize = 64 * 1024;
const MAX_ACTIVE_CONNECTIONS: usize = 256;
const MAX_CONNECTIONS_PER_HOST: usize = 65536;
const MAX_CLIENTS: usize = 256;
const MAX_HEADERS: usize = 1024;
const MAX_HEADER_NAME_BYTES: usize = 256;
const MAX_HEADER_VALUE_BYTES: usize = 16 * 1024;
const MAX_POOL_KEY_BYTES: usize = 4096;
const MAX_URL_BYTES: usize = 8192;
const MAX_ADDRESS_BYTES: usize = 128;
const IPC_TIMEOUT: Duration = Duration::from_secs(120);
const STREAM_FAILURE_MARKER: u32 = u32::MAX;

const CODEX_HEADER_ORDER: [&str; 11] = [
    "x-codex-beta-features",
    "x-codex-window-id",
    "x-codex-turn-metadata",
    "x-client-request-id",
    "session-id",
    "thread-id",
    "accept",
    "content-type",
    "authorization",
    "originator",
    "user-agent",
];

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RequestHeader {
    protocol: u32,
    pool_key: String,
    method: String,
    url: String,
    headers: Vec<[String; 2]>,
    addresses: Vec<String>,
    body_length: usize,
    max_connections: usize,
}

struct WireRequest {
    pool_key: String,
    method: Method,
    url: Url,
    host: String,
    headers: HeaderMap,
    addresses: Vec<SocketAddr>,
    max_connections: usize,
    body: Vec<u8>,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct ClientKey {
    pool_key: String,
    origin: String,
    addresses: Vec<SocketAddr>,
    max_connections: usize,
}

struct ClientEntry {
    client: Client,
    active: Arc<Semaphore>,
    limit: usize,
    last_used: u64,
}

#[derive(Clone)]
struct ClientHandle {
    client: Client,
    active: Arc<Semaphore>,
}

struct ClientCache {
    state: Mutex<ClientCacheState>,
}

struct ClientCacheState {
    entries: HashMap<ClientKey, ClientEntry>,
    clock: u64,
}

#[derive(Serialize)]
struct StartupIdentity<'a> {
    protocol: u32,
    profile: &'a str,
    version: &'a str,
    openssl: &'a str,
}

#[derive(Serialize)]
struct ResponseMetadata {
    status: u16,
    headers: Vec<[String; 2]>,
    error: String,
}

#[derive(Debug)]
enum PeerEvent {
    Disconnected,
    UnexpectedData,
    ReadFailure,
}

type PeerFuture = Pin<Box<dyn Future<Output = PeerEvent> + Send>>;

#[derive(Debug)]
enum WriteFailure {
    Io,
    Timeout,
    Peer,
}

impl ClientCache {
    fn new() -> Self {
        Self {
            state: Mutex::new(ClientCacheState {
                entries: HashMap::with_capacity(MAX_CLIENTS),
                clock: 0,
            }),
        }
    }

    fn client_for(&self, key: ClientKey, host: &str) -> Result<ClientHandle, &'static str> {
        {
            let mut state = self
                .state
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner());
            state.clock = state.clock.wrapping_add(1);
            let now = state.clock;
            if let Some(entry) = state.entries.get_mut(&key) {
                entry.last_used = now;
                return Ok(ClientHandle {
                    client: entry.client.clone(),
                    active: Arc::clone(&entry.active),
                });
            }
        }

        let client = Client::builder()
            // The Go side has already validated and pinned every address. Rust must not
            // consult proxy environment variables or perform an independent resolution.
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .http1_only()
            .pool_max_idle_per_host(key.max_connections.min(64))
            .connect_timeout(Duration::from_secs(10))
            .min_tls_version(reqwest::tls::Version::TLS_1_2)
            .resolve_to_addrs(host, &key.addresses)
            .use_native_tls()
            .build()
            .map_err(|_| "native client initialization failed")?;
        let active = Arc::new(Semaphore::new(key.max_connections));

        let mut state = self
            .state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        state.clock = state.clock.wrapping_add(1);
        let now = state.clock;
        if let Some(entry) = state.entries.get_mut(&key) {
            entry.last_used = now;
            return Ok(ClientHandle {
                client: entry.client.clone(),
                active: Arc::clone(&entry.active),
            });
        }
        let inserted_key = key.clone();
        state.entries.insert(
            key,
            ClientEntry {
                client: client.clone(),
                active: Arc::clone(&active),
                limit: inserted_key.max_connections,
                last_used: now,
            },
        );
        if state.entries.len() > MAX_CLIENTS {
            // A checked-out handle may still be waiting to acquire its permit.
            // Only entries owned exclusively by the cache are safe to evict.
            let victim = state
                .entries
                .iter()
                .filter(|(_, entry)| {
                    Arc::strong_count(&entry.active) == 1
                        && entry.active.available_permits() == entry.limit
                })
                .min_by_key(|(_, entry)| entry.last_used)
                .map(|(key, _)| key.clone());
            if let Some(victim) = victim {
                state.entries.remove(&victim);
            } else {
                state.entries.remove(&inserted_key);
                return Err("native client cache busy");
            }
        }
        Ok(ClientHandle { client, active })
    }
}
#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() {
    let exit_code = match run().await {
        Ok(()) => 0,
        Err(message) => {
            eprintln!("{message}");
            1
        }
    };
    std::process::exit(exit_code);
}

async fn run() -> Result<(), &'static str> {
    match parse_args()? {
        Command::Version => emit_startup_identity(),
        Command::Serve(socket) => {
            let listener = UnixListener::bind(&socket).map_err(|_| "native socket bind failed")?;
            if let Err(reason) = set_socket_permissions(&socket) {
                let _ = fs::remove_file(&socket);
                return Err(reason);
            }
            if let Err(reason) = emit_startup_identity() {
                let _ = fs::remove_file(&socket);
                return Err(reason);
            }
            serve(listener, socket).await
        }
    }
}

enum Command {
    Version,
    Serve(PathBuf),
}

fn parse_args() -> Result<Command, &'static str> {
    let mut args = env::args_os().skip(1);
    let Some(first) = args.next() else {
        return Err("usage: hoorific-codex-wire --socket PATH");
    };
    if first == "--version" {
        if args.next().is_some() {
            return Err("invalid arguments");
        }
        return Ok(Command::Version);
    }
    if first != "--socket" {
        return Err("usage: hoorific-codex-wire --socket PATH");
    }
    let Some(socket) = args.next() else {
        return Err("missing socket path");
    };
    if args.next().is_some() || socket.is_empty() {
        return Err("invalid arguments");
    }
    let socket = PathBuf::from(socket);
    if socket.as_os_str().to_string_lossy().len() > 100 {
        return Err("socket path too long");
    }
    Ok(Command::Serve(socket))
}

fn set_socket_permissions(path: &PathBuf) -> Result<(), &'static str> {
    #[cfg(unix)]
    {
        fs::set_permissions(path, fs::Permissions::from_mode(0o600))
            .map_err(|_| "native socket permission setup failed")?;
    }
    Ok(())
}

fn emit_startup_identity() -> Result<(), &'static str> {
    let identity = StartupIdentity {
        protocol: PROTOCOL,
        profile: PROFILE,
        version: PROFILE_VERSION,
        openssl: openssl_version(),
    };
    let encoded =
        serde_json::to_vec(&identity).map_err(|_| "startup identity serialization failed")?;
    let stdout = io::stdout();
    let mut stdout = stdout.lock();
    stdout
        .write_all(&encoded)
        .and_then(|_| stdout.write_all(b"\n"))
        .and_then(|_| stdout.flush())
        .map_err(|_| "startup identity output failed")
}

async fn serve(listener: UnixListener, socket: PathBuf) -> Result<(), &'static str> {
    let cache = Arc::new(ClientCache::new());
    let active = Arc::new(Semaphore::new(MAX_ACTIVE_CONNECTIONS));
    let mut tasks = JoinSet::new();
    let mut stdin = tokio::io::stdin();
    let mut stdin_probe = [0u8; 1];

    let result = loop {
        tokio::select! {
            accepted = listener.accept() => {
                let (stream, _) = match accepted {
                    Ok(accepted) => accepted,
                    Err(_) => break Err("native socket accept failed"),
                };
                match active.clone().try_acquire_owned() {
                    Ok(permit) => {
                        let cache = Arc::clone(&cache);
                        tasks.spawn(async move {
                            handle_connection(stream, permit, cache).await;
                        });
                    }
                    Err(_) => {
                        send_error_plain(stream, "native helper busy").await;
                    }
                }
            }
            stdin_result = stdin.read(&mut stdin_probe) => {
                match stdin_result {
                    Ok(0) | Err(_) => break Ok(()),
                    Ok(_) => {}
                }
            }
            joined = tasks.join_next(), if !tasks.is_empty() => {
                let _ = joined;
            }
        }
    };

    tasks.abort_all();
    while tasks.join_next().await.is_some() {}
    let _ = fs::remove_file(socket);
    result
}

async fn handle_connection(
    mut stream: UnixStream,
    _permit: OwnedSemaphorePermit,
    cache: Arc<ClientCache>,
) {
    let request = match read_request(&mut stream).await {
        Ok(Some(request)) => request,
        Ok(None) => return,
        Err(reason) => {
            send_error_plain(stream, reason).await;
            return;
        }
    };

    let (reader, mut writer) = stream.into_split();
    let mut peer: PeerFuture = Box::pin(watch_peer(reader));
    let key = ClientKey {
        pool_key: request.pool_key.clone(),
        origin: origin_key(&request.url, &request.host),
        addresses: request.addresses.clone(),
        max_connections: request.max_connections,
    };
    let client = match cache.client_for(key, &request.host) {
        Ok(client) => client,
        Err(reason) => {
            let _ = send_error_with_peer(&mut writer, &mut peer, reason).await;
            return;
        }
    };
    let _native_permit = tokio::select! {
        permit = client.active.clone().acquire_owned() => {
            match permit {
                Ok(permit) => permit,
                Err(_) => {
                    let _ = send_error_with_peer(&mut writer, &mut peer, "native client unavailable").await;
                    return;
                }
            }
        }
        _event = peer.as_mut() => {
            // Dropping the request future below is unnecessary: no upstream work has started.
            return;
        }
    };

    let upstream = client
        .client
        .request(request.method, request.url)
        .headers(request.headers)
        // The body is already encoded by the caller. Do not parse, transform, or re-encode it.
        .body(request.body)
        .send();
    let mut response = tokio::select! {
        timed = timeout(IPC_TIMEOUT, upstream) => {
            match timed {
                Ok(Ok(response)) => response,
                Ok(Err(_)) => {
                    let _ = send_error_with_peer(&mut writer, &mut peer, "upstream request failed").await;
                    return;
                }
                Err(_) => {
                    let _ = send_error_with_peer(&mut writer, &mut peer, "upstream response header timeout").await;
                    return;
                }
            }
        }
        _event = peer.as_mut() => {
            // A peer EOF or protocol violation cancels the in-flight reqwest future.
            return;
        }
    };

    let metadata = match response_metadata(&response) {
        Ok(metadata) => metadata,
        Err(reason) => {
            let _ = send_error_with_peer(&mut writer, &mut peer, reason).await;
            return;
        }
    };
    if write_metadata_with_peer(&mut writer, &mut peer, &metadata)
        .await
        .is_err()
    {
        return;
    }

    loop {
        tokio::select! {
            chunk = response.chunk() => {
                match chunk {
                    Ok(Some(bytes)) => {
                        let mut offset = 0;
                        while offset < bytes.len() {
                            let end = (offset + MAX_CHUNK_BYTES).min(bytes.len());
                            if write_chunk_with_peer(&mut writer, &mut peer, &bytes[offset..end])
                                .await
                                .is_err()
                            {
                                return;
                            }
                            offset = end;
                        }
                    }
                    Ok(None) => {
                        let _ = write_end_with_peer(&mut writer, &mut peer).await;
                        return;
                    }
                    Err(_) => {
                        let _ = write_failure_with_peer(&mut writer, &mut peer).await;
                        return;
                    }
                }
            }
            _event = peer.as_mut() => {
                // Dropping the response and its body future here cancels upstream work.
                return;
            }
        }
    }
}

async fn read_request(stream: &mut UnixStream) -> Result<Option<WireRequest>, &'static str> {
    let mut length_bytes = [0u8; 4];
    let first = timeout(IPC_TIMEOUT, stream.read(&mut length_bytes[..1]))
        .await
        .map_err(|_| "request header timeout")?
        .map_err(|_| "request header read failed")?;
    if first == 0 {
        return Ok(None);
    }
    if first != 1 {
        return Err("truncated request header");
    }
    read_exact_timeout(
        stream,
        &mut length_bytes[1..],
        "request header timeout",
        "truncated request header",
    )
    .await?;
    let header_len = u32::from_be_bytes(length_bytes) as usize;
    if header_len == 0 || header_len > MAX_HEADER_FRAME {
        return Err("invalid request header length");
    }
    let mut encoded = vec![0u8; header_len];
    read_exact_timeout(
        stream,
        &mut encoded,
        "request header timeout",
        "truncated request header",
    )
    .await?;
    let header: RequestHeader =
        serde_json::from_slice(&encoded).map_err(|_| "invalid request header")?;
    let (method, url, host, headers, addresses) = validate_request_header(&header)?;
    if header.body_length > MAX_BODY_BYTES {
        return Err("request body too large");
    }
    let mut body = vec![0u8; header.body_length];
    if !body.is_empty() {
        read_exact_timeout(
            stream,
            &mut body,
            "request body timeout",
            "truncated request body",
        )
        .await?;
    }
    Ok(Some(WireRequest {
        pool_key: header.pool_key,
        method,
        url,
        host,
        headers,
        addresses,
        max_connections: header.max_connections,
        body,
    }))
}

async fn read_exact_timeout<R: AsyncRead + Unpin>(
    reader: &mut R,
    buffer: &mut [u8],
    timeout_reason: &'static str,
    eof_reason: &'static str,
) -> Result<(), &'static str> {
    match timeout(IPC_TIMEOUT, reader.read_exact(buffer)).await {
        Ok(Ok(_)) => Ok(()),
        Ok(Err(error)) if error.kind() == io::ErrorKind::UnexpectedEof => Err(eof_reason),
        Ok(Err(_)) => Err("request frame read failed"),
        Err(_) => Err(timeout_reason),
    }
}

fn validate_request_header(
    header: &RequestHeader,
) -> Result<(Method, Url, String, HeaderMap, Vec<SocketAddr>), &'static str> {
    if header.protocol != PROTOCOL {
        return Err("unsupported IPC protocol");
    }
    if !(1..=MAX_CONNECTIONS_PER_HOST).contains(&header.max_connections) {
        return Err("invalid connection limit");
    }
    if header.pool_key.is_empty()
        || header.pool_key.len() > MAX_POOL_KEY_BYTES
        || header.pool_key.as_bytes().contains(&0)
    {
        return Err("invalid pool key");
    }
    if header.url.len() > MAX_URL_BYTES {
        return Err("URL too long");
    }
    let url = Url::parse(&header.url).map_err(|_| "invalid URL")?;
    if !matches!(url.scheme(), "http" | "https")
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.fragment().is_some()
    {
        return Err("invalid URL");
    }
    let host = url.host_str().ok_or("invalid URL")?.to_owned();
    let port = url.port_or_known_default().ok_or("invalid URL")?;
    let method = Method::from_bytes(header.method.as_bytes()).map_err(|_| "invalid method")?;
    if header.headers.len() > MAX_HEADERS {
        return Err("too many request headers");
    }
    let headers = build_request_headers(&header.headers)?;
    if header.addresses.is_empty() || header.addresses.len() > MAX_ACTIVE_CONNECTIONS {
        return Err("invalid address set");
    }
    let mut addresses = Vec::with_capacity(header.addresses.len());
    for raw in &header.addresses {
        if raw.is_empty() || raw.len() > MAX_ADDRESS_BYTES {
            return Err("invalid address set");
        }
        let address = raw
            .parse::<SocketAddr>()
            .map_err(|_| "invalid address set")?;
        if address.port() != port {
            return Err("address port mismatch");
        }
        addresses.push(address);
    }
    addresses.sort_unstable();
    addresses.dedup();
    if addresses.is_empty() {
        return Err("invalid address set");
    }
    // URL parsing canonicalizes legacy numeric authorities to IP literals.
    // Hyper connects literals directly, bypassing resolver overrides, so the
    // normalized destination must still be one of Go's approved addresses.
    if let Ok(literal) = host
        .trim_start_matches('[')
        .trim_end_matches(']')
        .parse::<IpAddr>()
    {
        if addresses
            .iter()
            .any(|address| address.ip().to_canonical() != literal.to_canonical())
        {
            return Err("literal address does not match approved pins");
        }
    }
    Ok((method, url, host, headers, addresses))
}

fn build_request_headers(input: &[[String; 2]]) -> Result<HeaderMap, &'static str> {
    struct InputHeader {
        name: HeaderName,
        value: HeaderValue,
        order: usize,
    }

    let mut entries = Vec::with_capacity(input.len());
    for (order, pair) in input.iter().enumerate() {
        if pair[0].is_empty() || pair[0].len() > MAX_HEADER_NAME_BYTES {
            return Err("invalid request header");
        }
        if pair[1].len() > MAX_HEADER_VALUE_BYTES {
            return Err("request header value too large");
        }
        let name =
            HeaderName::from_bytes(pair[0].as_bytes()).map_err(|_| "invalid request header")?;
        let lower = name.as_str();
        if matches!(
            lower,
            "host"
                | "content-length"
                | "transfer-encoding"
                | "connection"
                | "cookie"
                | "proxy-authorization"
        ) {
            return Err("forbidden request header");
        }
        let value =
            HeaderValue::from_bytes(pair[1].as_bytes()).map_err(|_| "invalid request header")?;
        entries.push(InputHeader { name, value, order });
    }

    let mut result = HeaderMap::new();
    for canonical in CODEX_HEADER_ORDER {
        for entry in &entries {
            if entry.name.as_str() == canonical {
                result.append(entry.name.clone(), entry.value.clone());
            }
        }
    }

    let mut remaining: Vec<usize> = entries
        .iter()
        .enumerate()
        .filter_map(|(index, entry)| {
            if CODEX_HEADER_ORDER
                .iter()
                .any(|canonical| entry.name.as_str() == *canonical)
            {
                None
            } else {
                Some(index)
            }
        })
        .collect();
    remaining.sort_unstable_by(|left, right| {
        entries[*left]
            .name
            .as_str()
            .cmp(entries[*right].name.as_str())
            .then(entries[*left].order.cmp(&entries[*right].order))
    });
    for index in remaining {
        let entry = &entries[index];
        result.append(entry.name.clone(), entry.value.clone());
    }
    Ok(result)
}

fn origin_key(url: &Url, host: &str) -> String {
    format!(
        "{}\0{}\0{}",
        url.scheme(),
        host.to_ascii_lowercase(),
        url.port_or_known_default().unwrap_or(0)
    )
}

fn response_metadata(response: &Response) -> Result<Vec<u8>, &'static str> {
    if response.headers().len() > MAX_HEADERS {
        return Err("response metadata too large");
    }
    let mut headers = Vec::with_capacity(response.headers().len());
    for (name, value) in response.headers() {
        let value = String::from_utf8_lossy(value.as_bytes()).into_owned();
        headers.push([name.as_str().to_owned(), value]);
    }
    let metadata = ResponseMetadata {
        status: response.status().as_u16(),
        headers,
        error: String::new(),
    };
    let encoded =
        serde_json::to_vec(&metadata).map_err(|_| "response metadata serialization failed")?;
    if encoded.len() > MAX_HEADER_FRAME {
        return Err("response metadata too large");
    }
    Ok(encoded)
}

fn error_metadata(reason: &'static str) -> Result<Vec<u8>, &'static str> {
    let metadata = ResponseMetadata {
        status: 0,
        headers: Vec::new(),
        error: reason.to_owned(),
    };
    serde_json::to_vec(&metadata).map_err(|_| "response metadata serialization failed")
}

async fn watch_peer(mut reader: OwnedReadHalf) -> PeerEvent {
    let mut probe = [0u8; 1];
    match reader.read(&mut probe).await {
        Ok(0) => PeerEvent::Disconnected,
        Ok(_) => PeerEvent::UnexpectedData,
        Err(_) => PeerEvent::ReadFailure,
    }
}

async fn send_error_plain(mut stream: UnixStream, reason: &'static str) {
    let Ok(metadata) = error_metadata(reason) else {
        return;
    };
    if write_metadata_plain(&mut stream, &metadata).await.is_ok() {
        let _ = write_frame_plain(&mut stream, &[]).await;
    }
}

async fn send_error_with_peer(
    writer: &mut tokio::net::unix::OwnedWriteHalf,
    peer: &mut PeerFuture,
    reason: &'static str,
) -> Result<(), WriteFailure> {
    let metadata = error_metadata(reason).map_err(|_| WriteFailure::Io)?;
    write_metadata_with_peer(writer, peer, &metadata).await?;
    write_end_with_peer(writer, peer).await
}

async fn write_metadata_plain<W: AsyncWrite + Unpin>(
    writer: &mut W,
    metadata: &[u8],
) -> Result<(), WriteFailure> {
    if metadata.is_empty() || metadata.len() > MAX_HEADER_FRAME {
        return Err(WriteFailure::Io);
    }
    write_plain(writer, &(metadata.len() as u32).to_be_bytes()).await?;
    write_plain(writer, metadata).await
}

async fn write_metadata_with_peer<W: AsyncWrite + Unpin>(
    writer: &mut W,
    peer: &mut PeerFuture,
    metadata: &[u8],
) -> Result<(), WriteFailure> {
    if metadata.is_empty() || metadata.len() > MAX_HEADER_FRAME {
        return Err(WriteFailure::Io);
    }
    write_with_peer(writer, &(metadata.len() as u32).to_be_bytes(), peer).await?;
    write_with_peer(writer, metadata, peer).await
}

async fn write_frame_plain<W: AsyncWrite + Unpin>(
    writer: &mut W,
    payload: &[u8],
) -> Result<(), WriteFailure> {
    if payload.len() > MAX_CHUNK_BYTES {
        return Err(WriteFailure::Io);
    }
    write_plain(writer, &(payload.len() as u32).to_be_bytes()).await?;
    if payload.is_empty() {
        Ok(())
    } else {
        write_plain(writer, payload).await
    }
}

async fn write_chunk_with_peer<W: AsyncWrite + Unpin>(
    writer: &mut W,
    peer: &mut PeerFuture,
    payload: &[u8],
) -> Result<(), WriteFailure> {
    if payload.is_empty() || payload.len() > MAX_CHUNK_BYTES {
        return Err(WriteFailure::Io);
    }
    write_with_peer(writer, &(payload.len() as u32).to_be_bytes(), peer).await?;
    write_with_peer(writer, payload, peer).await
}

async fn write_end_with_peer<W: AsyncWrite + Unpin>(
    writer: &mut W,
    peer: &mut PeerFuture,
) -> Result<(), WriteFailure> {
    write_with_peer(writer, &[0u8; 4], peer).await
}

async fn write_failure_with_peer<W: AsyncWrite + Unpin>(
    writer: &mut W,
    peer: &mut PeerFuture,
) -> Result<(), WriteFailure> {
    write_with_peer(writer, &STREAM_FAILURE_MARKER.to_be_bytes(), peer).await
}

async fn write_plain<W: AsyncWrite + Unpin>(
    writer: &mut W,
    data: &[u8],
) -> Result<(), WriteFailure> {
    match timeout(IPC_TIMEOUT, writer.write_all(data)).await {
        Ok(Ok(())) => Ok(()),
        Ok(Err(_)) => Err(WriteFailure::Io),
        Err(_) => Err(WriteFailure::Timeout),
    }
}

async fn write_with_peer<W: AsyncWrite + Unpin>(
    writer: &mut W,
    data: &[u8],
    peer: &mut PeerFuture,
) -> Result<(), WriteFailure> {
    tokio::select! {
        result = timeout(IPC_TIMEOUT, writer.write_all(data)) => {
            match result {
                Ok(Ok(())) => Ok(()),
                Ok(Err(_)) => Err(WriteFailure::Io),
                Err(_) => Err(WriteFailure::Timeout),
            }
        }
        _event = peer.as_mut() => Err(WriteFailure::Peer),
    }
}
