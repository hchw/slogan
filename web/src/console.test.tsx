import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import App from './App';
import { adminAPI, APIError, clearSession, formatMoney, userAPI } from './api';

type Mock = ReturnType<typeof vi.fn>;

function json(data: unknown, status = 200, requestId = 'req-1') {
  return new Response(JSON.stringify({ code: 0, message: 'ok', data, requestId }), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function route(handler: (url: string, init?: RequestInit) => Response | undefined) {
  const mock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    return handler(url, init) ?? json({});
  });
  vi.stubGlobal('fetch', mock);
  return mock as Mock;
}

const adminMe = (permissions: Record<string, boolean> = {}) => ({
  id: 1, username: 'ops', displayName: 'Ops', permissions,
});

const allPermissions = {
  'provider:read': true, 'model:read': true, 'evaluation:read': true, 'policy:read': true,
  'user:read': true, 'quota:read': true, 'usage:read': true, 'audit:read': true,
};

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe('session isolation and error recovery', () => {
  it('sends only the token belonging to the console being called', async () => {
    localStorage.setItem('slogan.user.token', 'u-token');
    localStorage.setItem('slogan.admin.token', 'a-token');
    const seen: (string | null)[] = [];
    route((url, init) => {
      seen.push(new Headers(init?.headers).get('Authorization'));
      if (url.includes('/nodes')) return json({ list: [], activeScoreVersionId: 0 });
      return json({ balanceMicro: 0, reservedMicro: 0, availableMicro: 0 });
    });

    await adminAPI.nodes();
    await userAPI.quota();

    expect(seen).toEqual(['Bearer a-token', 'Bearer u-token']);
  });

  it('a 401 clears only the console it belongs to', async () => {
    localStorage.setItem('slogan.user.token', 'u-token');
    localStorage.setItem('slogan.admin.token', 'a-token');
    route(() => json({ message: 'unauthenticated' }, 401));

    await expect(adminAPI.me()).rejects.toBeInstanceOf(APIError);
    expect(localStorage.getItem('slogan.admin.token')).toBeNull();
    expect(localStorage.getItem('slogan.user.token')).toBe('u-token');
  });

  it('carries the request id from the envelope so failures are traceable', async () => {
    localStorage.setItem('slogan.admin.token', 'a-token');
    route(() => json({ message: 'provider unreachable' }, 502, 'req-trace-9'));
    await expect(adminAPI.nodes()).rejects.toMatchObject({ status: 502, requestId: 'req-trace-9' });
  });

  it('a 403 keeps the session and offers a recoverable state', async () => {
    localStorage.setItem('slogan.admin.token', 'a-token');
    route((url) => (url.endsWith('/me') ? json({ message: 'forbidden' }, 403) : json({ list: [] })));

    render(<MemoryRouter initialEntries={['/admin']}><App /></MemoryRouter>);
    expect(await screen.findByRole('heading', { name: '没有访问权限' })).toBeInTheDocument();
    expect(localStorage.getItem('slogan.admin.token')).toBe('a-token');
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument();
  });

  it('a 401 on the admin console redirects to the admin login without touching the user session', async () => {
    localStorage.setItem('slogan.user.token', 'u-token');
    localStorage.setItem('slogan.admin.token', 'a-token');
    route((url) => (url.endsWith('/me') ? json({ message: 'unauthenticated' }, 401) : json({ list: [] })));

    render(<MemoryRouter initialEntries={['/admin']}><App /></MemoryRouter>);
    expect(await screen.findByRole('heading', { name: '管理员登录' })).toBeInTheDocument();
    expect(localStorage.getItem('slogan.user.token')).toBe('u-token');
    expect(localStorage.getItem('slogan.admin.token')).toBeNull();
  });
});

describe('admin console permissions and secrets', () => {
  it('hides console areas the role cannot read and says so', async () => {
    localStorage.setItem('slogan.admin.token', 'a-token');
    route((url) => {
      if (url.endsWith('/me')) return json(adminMe({ 'provider:read': true, 'model:read': true }));
      return json({ list: [] });
    });

    render(<MemoryRouter initialEntries={['/admin']}><App /></MemoryRouter>);
    expect(await screen.findByText(/7 个页面入口已隐藏/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '节点健康' })).toBeNull();
    expect(screen.getAllByRole('button', { name: '供应商' }).length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: '模型目录' })).toBeInTheDocument();
  });

  it('creates a provider through a password field and never renders the stored secret', async () => {
    localStorage.setItem('slogan.admin.token', 'a-token');
    let created = false;
    route((url, init) => {
      if (url.endsWith('/me')) return json(adminMe(allPermissions));
      if (url.includes('/providers') && init?.method === 'POST') {
        created = true;
        const body = JSON.parse(String(init.body));
        expect(body.secret).toBe('sk-provider-secret');
        return json({ id: 5, name: 'mock', baseUrl: 'https://api.example.com/v1', status: 'enabled' });
      }
      if (url.includes('/providers')) {
        return json({ list: [{ id: 5, name: 'mock', baseUrl: 'https://api.example.com/v1', status: 'enabled', secretMasked: 'sk-****' }] });
      }
      return json({ list: [] });
    });

    render(<MemoryRouter initialEntries={['/admin']}><App /></MemoryRouter>);
    fireEvent.click(await screen.findByRole('button', { name: '供应商' }));
    fireEvent.click(screen.getByRole('button', { name: '添加供应商' }));
    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'mock' } });
    fireEvent.change(screen.getByLabelText('Base URL'), { target: { value: 'https://api.example.com/v1' } });
    const secretInput = screen.getByLabelText('API Secret');
    expect(secretInput).toHaveAttribute('type', 'password');
    fireEvent.change(secretInput, { target: { value: 'sk-provider-secret' } });
    fireEvent.click(screen.getByRole('button', { name: '保存供应商' }));

    await waitFor(() => expect(created).toBe(true));
    await waitFor(() => expect(screen.getByText('mock')).toBeInTheDocument());
    // The stored credential is never echoed back into the page.
    expect(screen.queryByText('sk-provider-secret')).toBeNull();
  });
});

describe('user console', () => {
  it('shows the one-time API Key secret only on the creation page', async () => {
    localStorage.setItem('slogan.user.token', 'u-token');
    route((url, init) => {
      if (url.endsWith('/me')) return json({ id: 1, email: 'u@example.test', nickname: 'U', status: 'active' });
      if (url.includes('/api-keys') && init?.method === 'POST') return json({ id: 3, name: 'client', keyPrefix: 'sk-abcd', status: 'active', createdAt: '2026-01-01T00:00:00Z', key: 'sk-secret-value-once' });
      if (url.includes('/api-keys')) return json({ list: [{ id: 3, name: 'client', keyPrefix: 'sk-abcd', status: 'active', createdAt: '2026-01-01T00:00:00Z' }] });
      if (url.includes('/quota')) return json({ balanceMicro: 1_000_000, reservedMicro: 0, availableMicro: 1_000_000 });
      if (url.includes('/usage')) return json({ requests: 3, inputTokens: 120, outputTokens: 60, chargeMicro: 5000 });
      if (url.includes('/requests')) return json({ list: [{ requestId: 'r1', requestedModel: 'auto', model: 'Displayed Model', status: 'success', inputTokens: 40, outputTokens: 20, chargeMicro: 1000, latencyMs: 120, createdAt: '2026-01-01T00:00:00Z' }] });
      return json({});
    });

    render(<MemoryRouter initialEntries={['/user']}><App /></MemoryRouter>);
    await screen.findByRole('heading', { name: '账户概览' });
    fireEvent.click(screen.getAllByRole('button', { name: 'API Keys' })[0]);
    fireEvent.change(await screen.findByLabelText('Key 名称'), { target: { value: 'client' } });
    fireEvent.click(screen.getByRole('button', { name: '创建 API Key' }));
    expect(await screen.findByText('sk-secret-value-once')).toBeInTheDocument();

    // Leaving the creation page drops the plaintext for good.
    fireEvent.click(screen.getAllByRole('button', { name: '概览' })[0]);
    await screen.findByRole('heading', { name: '账户概览' });
    expect(screen.queryByText('sk-secret-value-once')).toBeNull();
    fireEvent.click(screen.getAllByRole('button', { name: 'API Keys' })[0]);
    await screen.findByText('sk-abcd…');
    expect(screen.queryByText('sk-secret-value-once')).toBeNull();
  });

  it('treats tokens as the primary metric and separates requested from actual model', async () => {
    localStorage.setItem('slogan.user.token', 'u-token');
    route((url) => {
      if (url.endsWith('/me')) return json({ id: 1, email: 'u@example.test', nickname: 'U', status: 'active' });
      if (url.includes('/quota')) return json({ balanceMicro: 1_000_000, reservedMicro: 250, availableMicro: 999_750 });
      if (url.includes('/usage')) return json({ requests: 9, inputTokens: 1200, outputTokens: 600, chargeMicro: 30000 });
      if (url.includes('/requests')) return json({ list: [{ requestId: 'r1', requestedModel: 'auto', model: 'Displayed Model', status: 'success', inputTokens: 40, outputTokens: 20, chargeMicro: 1000, latencyMs: 120, createdAt: '2026-01-01T00:00:00Z' }] });
      return json({ list: [] });
    });

    render(<MemoryRouter initialEntries={['/user']}><App /></MemoryRouter>);
    await screen.findByRole('heading', { name: '账户概览' });
    expect(screen.getAllByText('输入 Token').length).toBeGreaterThan(0);
    expect(screen.getAllByText('输出 Token').length).toBeGreaterThan(0);
    expect(screen.getByText('辅助指标')).toBeInTheDocument();
    expect(screen.getByRole('columnheader', { name: '请求模型' })).toBeInTheDocument();
    expect(screen.getByRole('columnheader', { name: '实际模型' })).toBeInTheDocument();
    expect(screen.getAllByText('auto').length).toBeGreaterThan(0);
    expect(screen.getAllByText('Displayed Model').length).toBeGreaterThan(0);
  });
});

describe('admin evaluation and cost pages', () => {
  it('reports a pending publish as not successful', async () => {
    localStorage.setItem('slogan.admin.token', 'a-token');
    route((url) => {
      if (url.endsWith('/me')) return json(adminMe(allPermissions));
      if (url.includes('/evaluation/versions/12/publish')) return json({ status: 'pending' });
      if (url.includes('/evaluation/versions')) return json({ list: [{ id: 12, status: 'building', successRatio: 0.95, totalModels: 20, validModels: 19, fallbackModels: 1, createdAt: '2026-01-01T00:00:00Z' }] });
      if (url.includes('/evaluation/refresh')) return json({ list: [] });
      return json({ list: [] });
    });
    vi.spyOn(window, 'confirm').mockReturnValue(true);

    render(<MemoryRouter initialEntries={['/admin']}><App /></MemoryRouter>);
    fireEvent.click((await screen.findAllByRole('button', { name: '评估与评分' }))[0]);
    await screen.findByText('v12');
    fireEvent.change(screen.getByLabelText('发布/回滚原因'), { target: { value: '运营发布' } });
    fireEvent.click(screen.getByRole('button', { name: '显式发布' }));

    expect(await screen.findByText(/发布保持 pending/)).toBeInTheDocument();
    expect(screen.queryByText(/发布已生效/)).toBeNull();
    vi.restoreAllMocks();
  });

  it('shows upstream cost and user charge separately without prompts or secrets', async () => {
    localStorage.setItem('slogan.admin.token', 'a-token');
    route((url) => {
      if (url.endsWith('/me')) return json(adminMe(allPermissions));
      if (url.includes('/usage/models')) return json({ list: [{ modelId: 7, modelName: 'Displayed Model', requests: 12, inputTokens: 900, outputTokens: 300, costMicro: 12000, chargeMicro: 45000, avgLatencyMs: 210, successRate: 0.98 }] });
      if (url.includes('/usage/users')) return json({ list: [{ userId: 1, requests: 12, inputTokens: 900, outputTokens: 300, chargeMicro: 45000 }] });
      if (url.includes('/audit-logs')) return json({ list: [{ id: 1, actorType: 'admin', actorId: 1, action: 'evaluation.publish', targetType: 'score_version', targetId: '12', reason: '运营发布', result: 'success', requestId: 'req-1', createdAt: '2026-01-01T00:00:00Z' }] });
      return json({ list: [] });
    });

    render(<MemoryRouter initialEntries={['/admin']}><App /></MemoryRouter>);
    fireEvent.click((await screen.findAllByRole('button', { name: '用量与成本' }))[0]);
    await screen.findByText('上游成本合计');
    expect(screen.getByText('用户扣费合计')).toBeInTheDocument();
    expect(screen.getAllByText(formatMoney(12000)).length).toBeGreaterThan(0);
    expect(screen.getAllByText(formatMoney(45000)).length).toBeGreaterThan(0);
    expect(formatMoney(12000)).not.toBe(formatMoney(45000));

    fireEvent.click(screen.getAllByRole('button', { name: '审计日志' })[0]);
    await screen.findByText('evaluation.publish');
    expect(screen.getByText('运营发布')).toBeInTheDocument();
    // No credential or prompt content is ever rendered on the cost/audit pages.
    expect(document.body.textContent).not.toMatch(/Bearer\s+\S/);
    expect(document.body.textContent).not.toMatch(/sk-[A-Za-z0-9]/);
    expect(document.body.textContent).not.toMatch(/你好|user prompt/);
  });

  it('marks a node with an unready classifier as degraded while still serving traffic', async () => {
    localStorage.setItem('slogan.admin.token', 'a-token');
    route((url) => {
      if (url.endsWith('/me')) return json(adminMe(allPermissions));
      if (url.includes('/nodes')) {
        return json({
          list: [
            { id: 'gw-1', scoreVersionId: 12, ready: true, drain: false, classifierReady: true, classifierVersion: '0.3.20', degraded: false, lastSeen: '2026-01-01T00:00:00Z' },
            { id: 'gw-2', scoreVersionId: 11, ready: true, drain: false, classifierReady: false, classifierReason: 'checkpoint_not_ready', degraded: true, lastSeen: '2026-01-01T00:00:00Z' },
          ],
          activeScoreVersionId: 12,
        });
      }
      return json({ list: [] });
    });

    render(<MemoryRouter initialEntries={['/admin']}><App /></MemoryRouter>);
    fireEvent.click((await screen.findAllByRole('button', { name: '节点健康' }))[0]);
    await screen.findByText('gw-1');
    expect(screen.getByText('gw-2')).toBeInTheDocument();
    expect(screen.getByText(/降级 · checkpoint_not_ready/)).toBeInTheDocument();
    expect(screen.getAllByText('接流量').length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText('1')).toBeInTheDocument();
  });
});

describe('session helpers', () => {
  it('clearSession only removes the requested console token', () => {
    localStorage.setItem('slogan.user.token', 'u-token');
    localStorage.setItem('slogan.admin.token', 'a-token');
    clearSession('user');
    expect(localStorage.getItem('slogan.user.token')).toBeNull();
    expect(localStorage.getItem('slogan.admin.token')).toBe('a-token');
  });
});
