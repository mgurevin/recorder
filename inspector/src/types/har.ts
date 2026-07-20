// HAR 1.2 types plus the "_"-prefixed extensions produced by
// github.com/mgurevin/recorder. Unknown extension fields are preserved via
// the index signature so nothing is lost between upload and the raw viewer.

export interface Har {
  log: HarLog;
}

export interface HarLog {
  version: string;
  creator: { name: string; version: string };
  entries: HarEntry[];
  comment?: string;
}

export interface NameValue {
  name: string;
  value: string;
  comment?: string;
}

export interface HarCookie {
  name: string;
  value: string;
  path?: string;
  domain?: string;
  expires?: string;
  httpOnly?: boolean;
  secure?: boolean;
}

export interface PostParam {
  name: string;
  value?: string;
  fileName?: string;
  contentType?: string;
}

export interface PostData {
  mimeType: string;
  params?: PostParam[];
  text?: string;
  /** recorder extension: "base64" when the body is binary. */
  _encoding?: string;
}

export interface HarContent {
  size: number;
  compression?: number;
  mimeType: string;
  text?: string;
  encoding?: string;
  /** recorder extension: text/size describe the decoded form. */
  _decoded?: boolean;
}

export interface HarRequest {
  method: string;
  url: string;
  httpVersion: string;
  cookies: HarCookie[];
  headers: NameValue[];
  queryString: NameValue[];
  postData?: PostData;
  headersSize: number;
  bodySize: number;
}

export interface HarResponse {
  status: number;
  statusText: string;
  httpVersion: string;
  cookies: HarCookie[];
  headers: NameValue[];
  content: HarContent;
  redirectURL: string;
  headersSize: number;
  bodySize: number;
}

export interface Timings {
  blocked: number;
  dns: number;
  connect: number;
  send: number;
  wait: number;
  receive: number;
  ssl: number;
}

export interface ErrorInfo {
  phase: string;
  type: string;
  message: string;
  timeout: boolean;
  temporary: boolean;
  contextCanceled: boolean;
  contextDeadlineExceeded: boolean;
  cause?: string;
  unwrapChain?: string[];
}

export interface PutIdleInfo {
  returned: boolean;
  error?: string;
}

export interface NetworkInfo {
  dnsAddresses?: string[];
  dnsCoalesced?: boolean;
  network?: string;
  localAddress?: string;
  remoteAddress?: string;
  ipVersion?: string;
  connectionReused: boolean;
  wasIdle: boolean;
  idleTimeMs?: number;
  proxy?: string;
  http2: boolean;
  putIdle?: PutIdleInfo;
}

export interface CertInfo {
  subject: string;
  issuer: string;
  serialNumber: string;
  dnsNames?: string[];
  ipAddresses?: string[];
  notBefore: string;
  notAfter: string;
  publicKeyAlgorithm: string;
  signatureAlgorithm: string;
  sha256Fingerprint: string;
  rawDER?: string;
}

export interface TlsInfo {
  version: string;
  cipherSuite: string;
  negotiatedProtocol?: string;
  serverName?: string;
  handshakeComplete: boolean;
  didResume: boolean;
  ocspStapled: boolean;
  sctCount: number;
  verifiedChains: number;
  peerCertificates?: CertInfo[];
}

export interface BodyInfo {
  present: boolean;
  complete: boolean;
  closedEarly?: boolean;
  truncated?: boolean;
  capturedBytes: number;
  totalBytes: number;
  hash?: string;
  hashAlgorithm?: string;
  readError?: string;
  closeError?: string;
  store?: string;
}

export interface BodyRedactionInfo {
  kind: string;
  outcome: "processed" | "redacted" | "unchanged" | "failed" | string;
  replacements?: number;
}

export interface RedactionScopeInfo {
  url?: number;
  headers?: number;
  queryParameters?: number;
  cookies?: number;
  body?: BodyRedactionInfo;
}

export interface RedactionInfo {
  request?: RedactionScopeInfo;
  response?: RedactionScopeInfo;
  errors?: number;
  rawTrace?: number;
}

export interface TraceEvent {
  name: string;
  time: string;
  detail?: string;
}

export interface Expect100Info {
  waited: boolean;
  continueReceived: boolean;
  waitMs?: number;
}

export interface InformationalResponse {
  status: number;
  headers?: NameValue[];
}

export interface HarEntry {
  startedDateTime: string;
  time: number;
  request: HarRequest;
  response: HarResponse;
  cache: Record<string, unknown>;
  timings: Timings;
  serverIPAddress?: string;
  connection?: string;
  comment?: string;

  _traceId?: string;
  _exchangeId?: string;
  _redirectIndex?: number;
  _state?: string;
  _error?: ErrorInfo;
  _network?: NetworkInfo;
  _tls?: TlsInfo;
  _expect100?: Expect100Info;
  _informational?: InformationalResponse[];
  _requestBody?: BodyInfo;
  _responseBody?: BodyInfo;
  _requestTrailers?: NameValue[];
  _responseTrailers?: NameValue[];
  _requestTransferEncoding?: string[];
  _responseTransferEncoding?: string[];
  _trace?: TraceEvent[];
  _redaction?: RedactionInfo;

  /** Unknown "_" extensions survive parsing untouched. */
  [key: `_${string}`]: unknown;
}

/** Normalized, render-friendly view of one entry. */
export interface NEntry {
  id: number;
  e: HarEntry;
  /** epoch ms, or null when startedDateTime did not parse. */
  startMs: number | null;
  /** total duration in ms, coerced finite and >= 0. */
  timeMs: number;
  method: string;
  url: string;
  host: string;
  path: string;
  status: number;
  state: string;
  errorPhase: string | null;
  respSize: number;
  traceId: string | null;
  redirectIndex: number | null;
  failed: boolean;
  truncated: boolean;
  closedEarly: boolean;
}

export interface TraceGroup {
  traceId: string | null;
  entries: NEntry[];
  hops: number;
  finalStatus: number;
  totalMs: number;
  hasFailed: boolean;
}
