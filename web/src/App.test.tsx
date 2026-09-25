import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import App from './App';
import { formatMoney, formatNumber } from './api';

function json(data: unknown, status = 200) {
  return new Response(JSON.stringify({ code: 0, message: 'ok', data }), { status, headers: { 'Content-Type': 'application/json' } });
}

const adminMe = { id: 1, username: 'ops', displayName: 'Ops', permissions: { 'evaluation:read': true, 'evaluation:refresh': true, 'evaluation:publish': true } };

describe('landing page', () => {
  it('provides distinct user and admin entrances', () => {
    render(<MemoryRouter><App /></MemoryRouter>);
    expect(screen.getByRole('link', { name: '用户工作台' })).toHaveAttribute('href', '/user');
    expect(screen.getByRole('link', { name: '管理后台' })).toHaveAttribute('href', '/admin');
  });

  it('shows the brand hero image packaged with the web build', () => {
    render(<MemoryRouter><App /></MemoryRouter>);
    expect(screen.getByAltText('千丝傀智 · 多模型网关')).toHaveAttribute('src', '/slogan.png');
  });
});

describe('formatters', () => {
  it('formats micro-yuan as RMB and tokens as grouped integers', () => {
    expect(formatMoney(1_250_000)).toBe('¥1.25');
    expect(formatMoney(550)).toBe('¥0.000550');
    expect(formatNumber(12000)).toContain('12');
  });
});

describe('admin evaluation console', () => {
  beforeEach(() => {
    localStorage.clear();
    localStorage.setItem('slogan.admin.token', 'admintoken');
  });

  it('shows per-model task outcome, cost evidence and the publish threshold gap', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/admin/api/v1/me')) return json(adminMe);
      if (url.includes('/evaluation/versions')) {
        return json({ list: [{ id: 12, status: 'building', successRatio: 0.5, totalModels: 2, validModels: 1, fallbackModels: 1, createdAt: '2026-01-01T00:00:00Z' }] });
      }
      if (url.includes('/evaluation/refresh/7')) {
        return json({
          task: { taskId: 7, scoreVersionId: 12, status: 'completed', total: 2, succeeded: 1, failed: 1, pending: 0, costMicro: 5500 },
          items: [
            { modelId: 1, modelKey: 'good-model', status: 'succeeded', outputTokens: 200, costMicro: 1100 },
            { modelId: 2, modelKey: 'bad-model', status: 'timeout', errorMessage: 'invalid_output', outputTokens: 0, costMicro: 0 },
          ],
        });
      }
      if (url.includes('/evaluation/refresh')) {
        return json({ list: [{ taskId: 7, scoreVersionId: 12, status: 'completed', total: 2, succeeded: 1, failed: 1, pending: 0, costMicro: 5500 }] });
      }
      return json({});
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<MemoryRouter initialEntries={['/admin']}><App /></MemoryRouter>);
    await screen.findByRole('heading', { name: '运营概览' });
    fireEvent.click(screen.getAllByRole('button', { name: /评估与评分/ })[0]);

    // The candidate must be presented as a candidate, never as already published.
    expect(await screen.findByText(/候选达到阈值不等于已发布/)).toBeInTheDocument();
    expect(screen.getByText('50.0%')).toBeInTheDocument();
    expect(screen.getByText('1/2')).toBeInTheDocument();
    // Historical fallback must be visible evidence, not folded into the coverage ratio.
    expect(screen.getByText('1 个模型')).toBeInTheDocument();

    await waitFor(() => expect(screen.getAllByText('good-model').length).toBeGreaterThan(0));
    expect(screen.getAllByText('bad-model').length).toBeGreaterThan(0);
    expect(screen.getByText('超时')).toBeInTheDocument();
    expect(screen.getByText('invalid_output')).toBeInTheDocument();
    expect(screen.getByText(formatNumber(200))).toBeInTheDocument();
    // Item cost evidence plus the task-level cost rollup.
    expect(screen.getByText(/¥0\.001100/)).toBeInTheDocument();
    expect(screen.getByText(/评估成本 ¥0\.005500/)).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/admin/api/v1/evaluation/refresh/7'), expect.anything());

    vi.unstubAllGlobals();
  });
});
