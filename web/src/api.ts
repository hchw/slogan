export type Envelope<T> = { code: number; message: string; data: T; requestId?: string };

export class APIError extends Error {
  constructor(message: string, readonly status: number, readonly code?: number, readonly requestId?: string) {
    super(message);
    this.name = 'APIError';
  }
}

async function request<T>(area: 'user' | 'admin', path: string, init: RequestInit = {}): Promise<T> {
  const prefix = area === 'user' ? '/user/api/v1' : '/admin/api/v1';
  const token = localStorage.getItem(`slogan.${area}.token`);
  const headers = new Headers(init.headers);
  if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
  if (token) headers.set('Authorization', `Bearer ${token}`);
  const response = await fetch(`${prefix}${path}`, { ...init, headers });
  const body = await response.json().catch(() => ({}));
  if (!response.ok || (typeof body.code === 'number' && body.code !== 0)) {
    if (response.status === 401) localStorage.removeItem(`slogan.${area}.token`);
    const requestId = typeof body.requestId === 'string' ? body.requestId : (response.headers.get('X-Request-Id') ?? undefined);
    throw new APIError(body.message || body.error?.message || `请求失败 (${response.status})`, response.status, body.code, requestId);
  }
  return (body.data ?? body) as T;
}

export function sessionToken(area: 'user' | 'admin'): string | null {
  return localStorage.getItem(`slogan.${area}.token`);
}

export function clearSession(area: 'user' | 'admin'): void {
  localStorage.removeItem(`slogan.${area}.token`);
}

export const userAPI = {
  login: (email: string, password: string) => request<{ token: string; expiresIn: number; user: { id: number; email: string } }>('user', '/auth/login', { method: 'POST', body: JSON.stringify({ email, password }) }),
  register: (email: string, password: string, nickname: string) => request<{ token: string; expiresIn: number; user: { id: number; email: string } }>('user', '/auth/register', { method: 'POST', body: JSON.stringify({ email, password, nickname }) }),
  logout: () => request('user', '/auth/logout', { method: 'POST' }),
  me: () => request<{ id: number; email: string; nickname: string; status: string }>('user', '/me'),
  quota: () => request<{ balanceMicro: number; reservedMicro: number; availableMicro: number }>('user', '/quota'),
  usage: () => request<{ requests: number; inputTokens: number; outputTokens: number; chargeMicro: number }>('user', '/usage'),
  requests: () => request<{ list: RequestRow[] }>('user', '/requests?page=1&pageSize=20'),
  keys: () => request<{ list: APIKeyRow[] }>('user', '/api-keys?page=1&pageSize=100'),
  createKey: (name: string, rateLimitPerMin: number) => request<APIKeyRow & { key: string }>('user', '/api-keys', { method: 'POST', body: JSON.stringify({ name, rateLimitPerMin }) }),
  revokeKey: (id: number) => request('user', `/api-keys/${id}`, { method: 'DELETE' }),
  setKey: (id: number, enabled: boolean) => request('user', `/api-keys/${id}/${enabled ? 'enable' : 'disable'}`, { method: 'POST' }),
  redeem: (code: string) => request<{ grantedMicro: number; balanceMicro: number }>('user', '/redemption/redeem', { method: 'POST', body: JSON.stringify({ code }) }),
};

export type RequestRow = {
  requestId: string; requestedModel: string; model: number | string; status: string;
  inputTokens: number; outputTokens: number; chargeMicro: number; latencyMs: number; createdAt: string;
};
export type APIKeyRow = {
  id: number; name: string; keyPrefix: string; status: string; expiresAt?: string; lastUsedAt?: string; createdAt: string;
};

export const adminAPI = {
  login: (username: string, password: string) => request<{ token: string; expiresIn: number; admin: { id: number; username: string } }>('admin', '/auth/login', { method: 'POST', body: JSON.stringify({ username, password }) }),
  logout: () => request('admin', '/auth/logout', { method: 'POST' }),
  me: () => request<{ id: number; username: string; displayName: string; permissions: Record<string, boolean> }>('admin', '/me'),
  providers: () => request<{ list: ProviderRow[] }>('admin', '/providers?page=1&pageSize=100'),
  createProvider: (body: unknown) => request<ProviderRow>('admin', '/providers', { method: 'POST', body: JSON.stringify(body) }),
  testProvider: (id: number) => request<{ reachable: boolean; latencyMs: number; modelsDetected: number }>('admin', `/providers/${id}/test`, { method: 'POST' }),
  discover: (id: number) => request<{ discovered: number; added: number; updated: number; unavailable: number }>('admin', `/providers/${id}/discover`, { method: 'POST' }),
  providerStatus: (id: number, enabled: boolean) => request('admin', `/providers/${id}/${enabled ? 'enable' : 'disable'}`, { method: 'POST' }),
  models: () => request<{ list: ModelRow[] }>('admin', '/models?page=1&pageSize=100'),
  createModel: (body: unknown) => request<ModelRow>('admin', '/models', { method: 'POST', body: JSON.stringify(body) }),
  modelStatus: (id: number, enabled: boolean) => request('admin', `/models/${id}/${enabled ? 'enable' : 'disable'}`, { method: 'POST' }),
  refresh: () => request<RefreshTask>('admin', '/evaluation/refresh', { method: 'POST' }),
  task: (id: number) => request<{ task: RefreshTask; items: RefreshTaskItem[] }>('admin', `/evaluation/refresh/${id}`),
  refreshes: () => request<{ list: RefreshTask[] }>('admin', '/evaluation/refresh'),
  retryRefresh: (id: number) => request<{ retried: number }>('admin', `/evaluation/refresh/${id}/retry`, { method: 'POST' }),
  versions: () => request<{ list: ScoreVersion[] }>('admin', '/evaluation/versions'),  publish: (id: number, reason: string) => request<{ status: string }>('admin', `/evaluation/versions/${id}/publish`, { method: 'POST', body: JSON.stringify({ reason }) }),
  users: () => request<{ list: UserRow[] }>('admin', '/users?page=1&pageSize=100'),
  userStatus: (id: number, enabled: boolean) => request('admin', `/users/${id}/${enabled ? 'enable' : 'disable'}`, { method: 'POST' }),
  adjustQuota: (id: number, amountMicro: number, remark: string, idempotencyKey: string) => request('admin', `/users/${id}/quota/adjust`, { method: 'POST', body: JSON.stringify({ amountMicro, remark, idempotencyKey }) }),
  packages: () => request<{ list: PackageRow[] }>('admin', '/quota-packages?page=1&pageSize=100'),
  createPackage: (name: string, faceValueMicro: number) => request<PackageRow>('admin', '/quota-packages', { method: 'POST', body: JSON.stringify({ name, faceValueMicro }) }),
  generateCodes: (id: number, quantity: number, expiresAt?: string) => request<{ batchId: number; quantity: number; codes: string[] }>('admin', `/quota-packages/${id}/codes`, { method: 'POST', body: JSON.stringify({ quantity, expiresAt }) }),
  policy: () => request<RoutingPolicy>('admin', '/routing-policy'),
  updatePolicy: (policy: RoutingPolicy) => request<RoutingPolicy>('admin', '/routing-policy', { method: 'PUT', body: JSON.stringify(policy) }),
  audit: () => request<{ list: AuditRow[] }>('admin', '/audit-logs?page=1&pageSize=100'),
  nodes: () => request<{ list: NodeRow[]; activeScoreVersionId: number }>('admin', '/nodes'),
  rollout: (id: number, reason: string) => request<{ status: string }>('admin', `/evaluation/versions/${id}/rollback`, { method: 'POST', body: JSON.stringify({ reason }) }),
  usageCost: () => request<{ list: ModelUsageRow[] }>('admin', '/usage/models'),
  usageByUser: () => request<{ list: UserUsageRow[] }>('admin', '/usage/users'),
};

export type ProviderRow = { id: number; name: string; baseUrl: string; status: string; secretMasked?: string };
export type ModelRow = { id: number; providerId: number; providerName: string; name: string; modelKey: string; status: string; inputPriceMicro: number; outputPriceMicro: number; chargeInputMicro: number; chargeOutputMicro: number; supportsTools: boolean; supportsStream: boolean };
export type ScoreVersion = { id: number; status: string; successRatio: number; totalModels: number; validModels: number; fallbackModels?: number; createdAt: string };
export type RefreshTask = { id?: number; taskId?: number; scoreVersionId: number; status: string; total: number; succeeded?: number; failed?: number; pending?: number; costMicro?: number };
export type RefreshTaskItem = { modelId: number; modelKey: string; status: string; errorMessage?: string; outputTokens: number; costMicro: number };
export type UserRow = { id: number; email: string; nickname: string; status: string };
export type PackageRow = { id: number; name: string; faceValueMicro: number; status: string };
export type RoutingPolicy = { lowCapabilityBias: number; minPublishRatio: number; highRiskForceQuality: boolean; evalMaxTokens: number; evalConcurrency: number; evalTimeoutSeconds: number; contentLogEnabled: boolean; contentLogRetentionDays: number; version: string };

export function formatMoney(micro: number): string {
  const yuan = (micro || 0) / 1_000_000;
  // Sub-cent amounts (evaluation/upstream costs) keep micro precision instead of rounding to zero.
  const digits = yuan !== 0 && Math.abs(yuan) < 0.01 ? 6 : 2;
  return `¥${yuan.toFixed(digits)}`;
}
export function formatNumber(value: number): string {
  return new Intl.NumberFormat('zh-CN').format(value || 0);
}

export type NodeRow = { id: string; scoreVersionId: number; ready: boolean; drain: boolean; classifierReady: boolean; classifierVersion?: string; classifierReason?: string; degraded: boolean; lastSeen?: string };
export type ModelUsageRow = { modelId: number; modelName: string; requests: number; inputTokens: number; outputTokens: number; costMicro: number; chargeMicro: number; avgLatencyMs: number; successRate: number };
export type UserUsageRow = { userId: number; requests: number; inputTokens: number; outputTokens: number; chargeMicro: number };
export type AuditRow = { id: number; createdAt: string; actorType: string; actorId: number; action: string; targetType: string; targetId: string; reason: string; result: string; requestId: string };
