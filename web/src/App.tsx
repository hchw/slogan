import { FormEvent, useCallback, useEffect, useState } from 'react';
import { Link, Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom';
import {
  APIError, adminAPI, APIKeyRow, AuditRow, clearSession, formatMoney, formatNumber, ModelRow,
  ModelUsageRow, NodeRow, PackageRow, ProviderRow, RefreshTask, RefreshTaskItem, RequestRow,
  ScoreVersion, UserRow, UserUsageRow, userAPI,
} from './api';

type Area = 'user' | 'admin';
type Notice = { kind: 'success' | 'error'; text: string } | null;

function App() {
  return <Routes>
    <Route path="/" element={<Landing />} />
    <Route path="/user/*" element={<UserRoot />} />
    <Route path="/admin/*" element={<AdminRoot />} />
    <Route path="*" element={<Landing />} />
  </Routes>;
}

function Landing() {
  return <main className="landing">
    <img className="landing-hero" src="/slogan.png" alt="千丝傀智 · 多模型网关" width={2048} height={1152} />
    <div className="brand-mark">千</div>
    <p className="eyebrow">MULTI-MODEL AI GATEWAY</p>
    <h1>一线分两路，<br />双傀各承长</h1>
    <p className="muted landing-copy">统一 OpenAI 兼容入口，清晰掌控模型用量与费用。</p>
    <div className="landing-actions"><Link className="button primary" to="/user">用户工作台</Link><Link className="button secondary" to="/admin">管理后台</Link></div>
  </main>;
}

function UserRoot() {
  return <Routes>
    <Route path="login" element={<LoginPage area="user" />} />
    <Route path="*" element={<UserGuard />} />
  </Routes>;
}
function AdminRoot() {
  return <Routes>
    <Route path="login" element={<LoginPage area="admin" />} />
    <Route path="*" element={<AdminGuard />} />
  </Routes>;
}

function LoginPage({ area }: { area: Area }) {
  const [isRegister, setRegister] = useState(false);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const navigate = useNavigate();
  const title = area === 'user' ? '用户登录' : '管理员登录';

  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault(); setError(''); setBusy(true);
    const fd = new FormData(e.currentTarget);
    const username = String(fd.get('username') || '');
    const password = String(fd.get('password') || '');
    try {
      const result = area === 'user'
        ? (isRegister
            ? await userAPI.register(username, password, String(fd.get('nickname') || ''))
            : await userAPI.login(username, password))
        : await adminAPI.login(username, password);
      localStorage.setItem(`slogan.${area}.token`, result.token);
      navigate(area === 'user' ? '/user' : '/admin');
    } catch (err) { setError(errorText(err)); }
    finally { setBusy(false); }
  }

  return <main className="auth-page">
    <Link to="/" className="back-link">← 返回首页</Link>
    <section className="auth-card">
      <div className="brand-mark small">千</div>
      <p className="eyebrow">{area === 'user' ? 'USER CONSOLE' : 'ADMIN CONSOLE'}</p>
      <h1>{isRegister ? '创建用户账户' : title}</h1>
      <p className="muted">{area === 'admin' ? '使用已引导创建的管理员账户登录。' : '登录以管理 API Key、额度和调用记录。'}</p>
      <form onSubmit={submit} className="form-stack">
        {isRegister && area === 'user' && <label>昵称<input name="nickname" autoComplete="nickname" /></label>}
        <label>{area === 'user' ? '邮箱' : '管理员用户名'}<input name="username" type={area === 'user' ? 'email' : 'text'} required autoComplete="username" /></label>
        <label>密码<input name="password" type="password" required minLength={10} autoComplete={isRegister ? 'new-password' : 'current-password'} /></label>
        {error && <div role="alert" className="alert error">{error}</div>}
        <button className="button primary full" disabled={busy}>{busy ? '请稍候…' : isRegister ? '注册并登录' : '登录'}</button>
      </form>
      {area === 'user' && <button className="text-button" onClick={() => setRegister(!isRegister)}>{isRegister ? '已有账户？登录' : '还没有账户？创建账户'}</button>}
    </section>
  </main>;
}

type SessionStatus = 'loading' | 'ok' | 'unauthorized' | 'forbidden' | 'error';

// Sessions are namespaced per console: a failed admin check never clears the
// user token, and vice versa. 401 sends the operator back to that login page;
// 403 keeps the session and offers a retry instead of silently logging out.
function useSession(area: Area): { status: SessionStatus; retry: () => void } {
  const [status, setStatus] = useState<SessionStatus>('loading');
  const check = useCallback(() => {
    setStatus('loading');
    const call = area === 'user' ? userAPI.me() : adminAPI.me();
    call.then(() => setStatus('ok')).catch((err: unknown) => {
      if (err instanceof APIError && err.status === 401) setStatus('unauthorized');
      else if (err instanceof APIError && err.status === 403) setStatus('forbidden');
      else setStatus('error');
    });
  }, [area]);
  useEffect(() => { check(); }, [check]);
  return { status, retry: check };
}

function SessionGate({ area, children }: { area: Area; children: React.ReactNode }) {
  const { status, retry } = useSession(area);
  if (status === 'loading') return <Loading />;
  if (status === 'unauthorized') return <Navigate to={`/${area}/login`} replace />;
  const signOut = () => { clearSession(area); window.location.href = `/${area}/login`; };
  if (status === 'forbidden') {
    return <SessionProblem title="没有访问权限" detail="当前账户已登录，但缺少该控制台所需权限。请联系管理员授予权限后重试。" onRetry={retry} onSignOut={signOut} />;
  }
  if (status === 'error') {
    return <SessionProblem title="无法确认会话状态" detail="控制台暂时无法连接服务端。请稍后重试，或重新登录。" onRetry={retry} onSignOut={signOut} />;
  }
  return <>{children}</>;
}

function SessionProblem({ title, detail, onRetry, onSignOut }: { title: string; detail: string; onRetry: () => void; onSignOut: () => void }) {
  return <main className="auth-page"><section className="auth-card"><p className="eyebrow">SESSION</p><h1>{title}</h1><p className="muted">{detail}</p><div className="landing-actions"><button className="button primary" onClick={onRetry}>重试</button><button className="button secondary" onClick={onSignOut}>退出登录</button></div></section></main>;
}

function UserGuard() { return <SessionGate area="user"><UserConsole /></SessionGate>; }
function AdminGuard() { return <SessionGate area="admin"><AdminConsole /></SessionGate>; }

function UserConsole() {
  const location = useLocation();
  const [tab, setTab] = useState(location.pathname.includes('keys') ? 'keys' : location.pathname.includes('requests') ? 'requests' : 'overview');
  const [me, setMe] = useState<{ email: string; nickname: string } | null>(null);
  const [quota, setQuota] = useState({ balanceMicro: 0, reservedMicro: 0, availableMicro: 0 });
  const [usage, setUsage] = useState({ requests: 0, inputTokens: 0, outputTokens: 0, chargeMicro: 0 });
  const [requests, setRequests] = useState<RequestRow[]>([]);
  const [keys, setKeys] = useState<APIKeyRow[]>([]);
  const [notice, setNotice] = useState<Notice>(null);
  const refresh = useCallback(async () => {
    try {
      const [m, q, u, r, k] = await Promise.all([userAPI.me(), userAPI.quota(), userAPI.usage(), userAPI.requests(), userAPI.keys()]);
      setMe(m); setQuota(q); setUsage(u); setRequests(r.list || []); setKeys(k.list || []);
    } catch (e) { setNotice({ kind: 'error', text: errorText(e) }); }
  }, []);
  useEffect(() => { void refresh(); }, [refresh]);
  async function logout() { try { await userAPI.logout(); } catch { /* token is cleared below */ } clearSession('user'); window.location.href = '/user/login'; }

  return <Shell area="user" title={tabTitle(tab, 'user')} me={me?.email} onTab={setTab} activeTab={tab} onLogout={logout}>
    {notice && <NoticeBox notice={notice} onClose={() => setNotice(null)} />}
    {tab === 'overview' && <section>
      <div className="page-heading"><div><p className="eyebrow">OVERVIEW</p><h1>账户概览</h1><p className="muted">欢迎回来{me?.nickname ? `，${me.nickname}` : ''}。掌握额度与 Token 用量。</p></div><button className="button secondary" onClick={() => setTab('keys')}>创建 API Key</button></div>
      <div className="metric-grid">
        <Metric label="可用额度" value={formatMoney(quota.availableMicro)} hint={`冻结 ${formatMoney(quota.reservedMicro)}`} />
        <Metric label="输入 Token" value={formatNumber(usage.inputTokens)} hint="本统计周期" />
        <Metric label="输出 Token" value={formatNumber(usage.outputTokens)} hint="本统计周期" />
      </div>
      <div className="submetric">请求数 <strong>{formatNumber(usage.requests)}</strong><span>辅助指标</span><i />本期扣费 <strong>{formatMoney(usage.chargeMicro)}</strong></div>
      <section className="panel quickstart"><div><p className="eyebrow">QUICK START</p><h2>快速接入</h2><p className="muted">使用 OpenAI SDK，将 Base URL 指向你的网关地址。</p></div><div className="code-line"><code>https://your-domain/v1</code><button className="button small-button" onClick={() => void navigator.clipboard?.writeText(`${window.location.origin}/v1`)}>复制</button></div><p className="muted">模型可指定名称，或使用 <code>auto</code> 自动路由。</p></section>
      <div className="section-heading"><h2>最近请求</h2><button className="text-button" onClick={() => setTab('requests')}>查看全部 →</button></div>
      <RequestTable rows={requests.slice(0, 5)} />
    </section>}
    {tab === 'keys' && <KeysPanel keys={keys} refresh={refresh} notify={setNotice} />}
    {tab === 'redeem' && <RedeemPanel refresh={refresh} notify={setNotice} />}
    {tab === 'requests' && <section><div className="page-heading"><div><p className="eyebrow">USAGE</p><h1>用量与请求</h1><p className="muted">输入/输出 Token 与实际模型分开展示。</p></div></div><div className="metric-grid compact"><Metric label="输入 Token" value={formatNumber(usage.inputTokens)} /><Metric label="输出 Token" value={formatNumber(usage.outputTokens)} /><Metric label="用户扣费" value={formatMoney(usage.chargeMicro)} /></div><RequestTable rows={requests} /></section>}
    {tab === 'account' && <section className="panel"><p className="eyebrow">ACCOUNT</p><h1>账户与安全</h1><dl className="details"><dt>邮箱</dt><dd>{me?.email}</dd><dt>昵称</dt><dd>{me?.nickname || '—'}</dd><dt>会话</dt><dd>服务端可撤销 · 24 小时有效</dd></dl></section>}
  </Shell>;
}

function KeysPanel({ keys, refresh, notify }: { keys: APIKeyRow[]; refresh: () => Promise<void>; notify: (n: Notice) => void }) {
  const [name, setName] = useState(''); const [secret, setSecret] = useState(''); const [busy, setBusy] = useState(false);
  async function create(e: FormEvent) { e.preventDefault(); setBusy(true); try { const k = await userAPI.createKey(name || 'default', 0); setSecret(k.key); setName(''); await refresh(); } catch (e) { notify({ kind: 'error', text: errorText(e) }); } finally { setBusy(false); } }
  async function toggle(key: APIKeyRow) { try { if (key.status === 'active') await userAPI.setKey(key.id, false); else await userAPI.setKey(key.id, true); await refresh(); } catch (e) { notify({ kind: 'error', text: errorText(e) }); } }
  async function revoke(key: APIKeyRow) { if (!window.confirm(`确定撤销 ${key.name}？撤销后不可恢复。`)) return; try { await userAPI.revokeKey(key.id); await refresh(); } catch (e) { notify({ kind: 'error', text: errorText(e) }); } }
  return <section><div className="page-heading"><div><p className="eyebrow">CREDENTIALS</p><h1>API Keys</h1><p className="muted">完整密钥只在创建成功后显示一次。</p></div></div>
    {secret && <div className="alert success"><div><strong>请立即保存此密钥，离开后无法再次查看。</strong><code className="secret-value">{secret}</code></div><button className="button small-button" onClick={() => void navigator.clipboard?.writeText(secret)}>复制</button><button className="icon-button" aria-label="关闭" onClick={() => setSecret('')}>×</button></div>}
    <form className="inline-form panel" onSubmit={create}><label>Key 名称<input value={name} onChange={e => setName(e.target.value)} placeholder="例如 coding-client" /></label><button className="button primary" disabled={busy}>{busy ? '创建中…' : '创建 API Key'}</button></form>
    <div className="panel table-panel"><table><thead><tr><th>名称</th><th>前缀</th><th>状态</th><th>创建时间</th><th>操作</th></tr></thead><tbody>{keys.map(k => <tr key={k.id}><td>{k.name}</td><td><code>{k.keyPrefix}…</code></td><td><Status value={k.status} /></td><td>{date(k.createdAt)}</td><td className="actions"><button className="text-button" onClick={() => void toggle(k)}>{k.status === 'active' ? '禁用' : '启用'}</button><button className="text-button danger-text" onClick={() => void revoke(k)}>撤销</button></td></tr>)}{keys.length === 0 && <EmptyRow cols={5} text="还没有 API Key。" />}</tbody></table></div>
  </section>;
}

function RedeemPanel({ refresh, notify }: { refresh: () => Promise<void>; notify: (n: Notice) => void }) {
  const [code, setCode] = useState(''); const [busy, setBusy] = useState(false); const [result, setResult] = useState<{ grantedMicro: number; balanceMicro: number } | null>(null);
  async function submit(e: FormEvent) { e.preventDefault(); setBusy(true); setResult(null); try { const res = await userAPI.redeem(code.trim()); setResult(res); setCode(''); await refresh(); } catch (e) { notify({ kind: 'error', text: errorText(e) }); } finally { setBusy(false); } }
  return <section><div className="page-heading"><div><p className="eyebrow">REDEEM</p><h1>兑换额度</h1><p className="muted">兑换后额度进入全局余额，不按模型区分或单独过期。</p></div></div><form className="panel redeem-form" onSubmit={submit}><label>兑换码<input value={code} onChange={e => setCode(e.target.value)} required placeholder="输入兑换码" /></label><button className="button primary" disabled={busy}>{busy ? '兑换中…' : '兑换'}</button>{result && <div className="alert success full-width">已到账 {formatMoney(result.grantedMicro)}，当前余额 {formatMoney(result.balanceMicro)}</div>}</form></section>;
}

// Nav entries map to the permission the server enforces, so an operator never
// sees a console area their role cannot read (the server still rejects any
// direct call, hiding is convenience only).
const adminNavPermissions: Record<string, string> = {
  providers: 'provider:read', models: 'model:read', evaluation: 'evaluation:read',
  nodes: 'evaluation:read', policy: 'policy:read', users: 'user:read',
  packages: 'quota:read', usage: 'usage:read', audit: 'audit:read',
};

function AdminConsole() {
  const [tab, setTab] = useState('overview');
  const [me, setMe] = useState<{ username: string; permissions?: Record<string, boolean> } | null>(null);
  const [notice, setNotice] = useState<Notice>(null);
  useEffect(() => { adminAPI.me().then(setMe).catch(e => setNotice({ kind: 'error', text: errorText(e) })); }, []);
  async function logout() { try { await adminAPI.logout(); } catch { /* revoke locally */ } clearSession('admin'); window.location.href = '/admin/login'; }
  // Permissions must be known before rendering navigation, otherwise a role
  // could briefly see entries the server will reject.
  if (!me) return <Loading />;
  const permissions = me.permissions || {};
  const allowedTabs = Object.keys(adminNavPermissions).filter((id) => permissions[adminNavPermissions[id]]);
  const hiddenTabs = Object.keys(adminNavPermissions).filter((id) => !permissions[adminNavPermissions[id]]).length;
  return <Shell area="admin" title={tabTitle(tab, 'admin')} me={me?.username} onTab={setTab} activeTab={tab} onLogout={logout} allowedTabs={allowedTabs}>
    {notice && <NoticeBox notice={notice} onClose={() => setNotice(null)} />}
    {hiddenTabs > 0 && <div className="alert info">当前角色权限受限：<strong>{hiddenTabs} 个页面入口已隐藏</strong>；服务端同样会拒绝越权请求。</div>}
    {tab === 'overview' && <AdminOverview setTab={setTab} />}
    {tab === 'providers' && <ProviderPanel notify={setNotice} />}
    {tab === 'models' && <ModelPanel notify={setNotice} />}
    {tab === 'evaluation' && <EvaluationPanel notify={setNotice} />}
    {tab === 'users' && <UsersPanel notify={setNotice} />}
    {tab === 'packages' && <PackagesPanel notify={setNotice} />}
    {tab === 'nodes' && <NodeHealthPanel />}
    {tab === 'usage' && <AdminUsage />}
    {tab === 'policy' && <PolicyPanel notify={setNotice} />}
    {tab === 'audit' && <AuditPanel />}
  </Shell>;
}

function AdminOverview({ setTab }: { setTab: (s: string) => void }) {
  return <section><div className="page-heading"><div><p className="eyebrow">CONTROL PLANE</p><h1>运营概览</h1><p className="muted">管理供应商、评分版本、额度和运行状态。</p></div></div><div className="admin-tiles">{[['providers','供应商与模型','维护连接、模型能力与价格'],['evaluation','评估与评分','查看候选版本并显式发布'],['users','用户与额度','用户状态与额度账务'],['nodes','节点健康','节点评分版本、接流量与分类器降级'],['usage','用量与成本','Token、用户扣费与上游成本']].map(([id,title,desc]) => <button key={id} className="panel tile" onClick={() => setTab(id)}><strong>{title}</strong><span>{desc}</span><i>进入 →</i></button>)}</div><div className="alert info">评分刷新只生成候选；只有具备发布权限的管理员显式发布，且全体接流量网关节点 ACK 后才报告成功。</div></section>;
}

function ProviderPanel({ notify }: { notify: (n: Notice) => void }) {
  const [rows, setRows] = useState<ProviderRow[]>([]); const [showForm, setShowForm] = useState(false);
  const load = useCallback(() => adminAPI.providers().then(x => setRows(x.list || [])).catch(e => notify({ kind: 'error', text: errorText(e) })), [notify]);
  useEffect(() => { void load(); }, [load]);
  async function create(e: FormEvent<HTMLFormElement>) { e.preventDefault(); const fd = new FormData(e.currentTarget); try { await adminAPI.createProvider({ name: fd.get('name'), baseUrl: fd.get('baseUrl'), secret: fd.get('secret'), type: 'openai_compatible', authType: 'bearer' }); notify({ kind: 'success', text: '供应商已创建。' }); setShowForm(false); await load(); } catch (e) { notify({ kind: 'error', text: errorText(e) }); } }
  async function action(p: ProviderRow, kind: 'test' | 'discover' | 'toggle') { try { if (kind === 'test') { const x = await adminAPI.testProvider(p.id); notify({ kind: x.reachable ? 'success' : 'error', text: `连通 ${x.reachable ? '成功' : '失败'} · ${x.latencyMs} ms · ${x.modelsDetected} 个模型` }); } else if (kind === 'discover') { const x = await adminAPI.discover(p.id); notify({ kind: 'success', text: `发现 ${x.discovered}，新增 ${x.added}，更新 ${x.updated}` }); } else await adminAPI.providerStatus(p.id, p.status !== 'enabled'); await load(); } catch (e) { notify({ kind: 'error', text: errorText(e) }); } }
  return <section><PageHeader eyebrow="CATALOG" title="供应商" action={<button className="button primary" onClick={() => setShowForm(!showForm)}>添加供应商</button>} />{showForm && <form className="panel form-grid" onSubmit={create}><label>名称<input name="name" required /></label><label>Base URL<input name="baseUrl" required placeholder="https://api.example.com/v1" /></label><label className="full-width">API Secret<input name="secret" type="password" required autoComplete="new-password" /></label><div className="full-width"><button className="button primary">保存供应商</button></div></form>}<div className="panel table-panel"><table><thead><tr><th>名称</th><th>Base URL</th><th>状态</th><th>操作</th></tr></thead><tbody>{rows.map(p => <tr key={p.id}><td>{p.name}</td><td><code>{p.baseUrl}</code></td><td><Status value={p.status} /></td><td className="actions"><button className="text-button" onClick={() => void action(p,'test')}>测试</button><button className="text-button" onClick={() => void action(p,'discover')}>发现模型</button><button className="text-button" onClick={() => void action(p,'toggle')}>{p.status === 'enabled' ? '停用' : '启用'}</button></td></tr>)}{rows.length===0 && <EmptyRow cols={4} text="暂无供应商。" />}</tbody></table></div></section>;
}

function ModelPanel({ notify }: { notify: (n: Notice) => void }) {
  const [rows, setRows] = useState<ModelRow[]>([]); const [providers, setProviders] = useState<ProviderRow[]>([]); const [showForm, setShowForm] = useState(false);
  const load = useCallback(async () => { try { const [m,p]=await Promise.all([adminAPI.models(),adminAPI.providers()]); setRows(m.list||[]); setProviders(p.list||[]); } catch(e){notify({kind:'error',text:errorText(e)});} },[notify]);
  useEffect(()=>{void load();},[load]);
  async function create(e:FormEvent<HTMLFormElement>){e.preventDefault();const fd=new FormData(e.currentTarget);try{await adminAPI.createModel({providerId:Number(fd.get('providerId')),name:fd.get('name'),modelKey:fd.get('modelKey'),contextLength:Number(fd.get('contextLength')||0),supportsStream:fd.get('supportsStream')==='on',supportsTools:fd.get('supportsTools')==='on',inputPriceMicro:Number(fd.get('inputPriceMicro')||0),outputPriceMicro:Number(fd.get('outputPriceMicro')||0),chargeInputMicro:Number(fd.get('chargeInputMicro')||0),chargeOutputMicro:Number(fd.get('chargeOutputMicro')||0),inputModalities:['text']});notify({kind:'success',text:'模型已添加。'});setShowForm(false);await load();}catch(e){notify({kind:'error',text:errorText(e)});}}
  async function toggle(m:ModelRow){try{await adminAPI.modelStatus(m.id,m.status!=='available');await load();}catch(e){notify({kind:'error',text:errorText(e)});}}
  return <section><PageHeader eyebrow="CATALOG" title="模型目录" action={<button className="button primary" onClick={()=>setShowForm(!showForm)}>手动添加模型</button>} />{showForm&&<form className="panel form-grid" onSubmit={create}><label>供应商<select name="providerId" required>{providers.map(p=><option value={p.id} key={p.id}>{p.name}</option>)}</select></label><label>模型名称<input name="name" required /></label><label>供应商模型 ID<input name="modelKey" required /></label><label>上下文长度<input name="contextLength" type="number" min="0" /></label><label>上游输入单价（微元/token）<input name="inputPriceMicro" type="number" min="0" /></label><label>上游输出单价（微元/token）<input name="outputPriceMicro" type="number" min="0" /></label><label>用户输入售价（微元/token）<input name="chargeInputMicro" type="number" min="0" /></label><label>用户输出售价（微元/token）<input name="chargeOutputMicro" type="number" min="0" /></label><label className="check-label"><input name="supportsStream" type="checkbox" defaultChecked />支持流式</label><label className="check-label"><input name="supportsTools" type="checkbox" />支持工具调用</label><div className="full-width"><button className="button primary">保存模型</button></div></form>}<div className="panel table-panel"><table><thead><tr><th>模型</th><th>供应商</th><th>上游输入价</th><th>用户输入价</th><th>状态</th><th></th></tr></thead><tbody>{rows.map(m=><tr key={m.id}><td><strong>{m.name}</strong><small>{m.modelKey}</small></td><td>{m.providerName}</td><td>{m.inputPriceMicro}</td><td>{m.chargeInputMicro}</td><td><Status value={m.status}/></td><td><button className="text-button" onClick={()=>void toggle(m)}>{m.status==='available'?'停用':'启用'}</button></td></tr>)}{rows.length===0&&<EmptyRow cols={6} text="暂无模型。"/>}</tbody></table></div></section>;
}

function EvaluationPanel({ notify }: { notify: (n: Notice) => void }) {
  const [versions,setVersions]=useState<ScoreVersion[]>([]);const [task,setTask]=useState<RefreshTask|null>(null);const [items,setItems]=useState<RefreshTaskItem[]>([]);const [reason,setReason]=useState('');
  const taskId=task?(task.id??task.taskId??0):0;
  const load=useCallback(()=>adminAPI.versions().then(x=>setVersions(x.list||[])).catch(e=>notify({kind:'error',text:errorText(e)})),[notify]);useEffect(()=>{void load();},[load]);
  const loadTask=useCallback(async(id:number)=>{try{const x=await adminAPI.task(id);setTask(x.task);setItems(x.items||[]);}catch(e){notify({kind:'error',text:errorText(e)});}},[notify]);
  useEffect(()=>{void (async()=>{try{const x=await adminAPI.refreshes();const latest=(x.list||[])[0];const id=latest?(latest.id??latest.taskId??0):0;if(id)await loadTask(id);}catch{/* progress is optional on load */}})();},[loadTask]);
  async function refresh(){try{const x=await adminAPI.refresh();setTask(x);setItems([]);notify({kind:'success',text:`评估任务 #${x.id??x.taskId} 已启动`});}catch(e){notify({kind:'error',text:errorText(e)});}}
  useEffect(()=>{if(!taskId||task?.status!=='running')return;const timer=setInterval(()=>void loadTask(taskId),3000);return()=>clearInterval(timer);},[taskId,task?.status,loadTask]);
  async function retry(){if(!taskId)return;try{const x=await adminAPI.retryRefresh(taskId);notify({kind:'success',text:`已重试 ${x.retried} 个失败项`});await loadTask(taskId);}catch(e){notify({kind:'error',text:errorText(e)});}}
  async function rollback(v:ScoreVersion){if(!reason.trim()){notify({kind:'error',text:'发布/回滚需要填写原因。'});return;}if(!window.confirm(`确认回滚到评分版本 ${v.id}？在所有接流量节点 ACK 前状态会保持 pending。`))return;try{const x=await adminAPI.rollout(v.id,reason);notify({kind:x.status==='published'?'success':'error',text:x.status==='published'?'回滚已生效':'回滚保持 pending，等待节点 ACK'});setReason('');await load();}catch(e){notify({kind:'error',text:errorText(e)});}}
  async function publish(v:ScoreVersion){if(!reason.trim()){notify({kind:'error',text:'发布/回滚需要填写原因。'});return;}const fallback=v.fallbackModels?`，其中 ${v.fallbackModels} 个模型沿用历史评分（不计入本次覆盖率）`:'';if(!window.confirm(`确认发布评分版本 ${v.id}？新评分覆盖率 ${(v.successRatio*100).toFixed(1)}%${fallback}。节点未全部 ACK 时状态会保持 pending。`))return;try{const x=await adminAPI.publish(v.id,reason);notify({kind:x.status==='published'?'success':'error',text:x.status==='published'?'发布已生效':`发布保持 pending：${x.status}（节点未全部 ACK，不计为成功）`});setReason('');await load();}catch(e){notify({kind:'error',text:errorText(e)});}}
  return <section><PageHeader eyebrow="EVALUATION" title="评估与评分" action={<button className="button primary" onClick={()=>void refresh()}>刷新评分</button>} />{task&&<div className="panel task-card"><div><strong>任务 #{taskId}</strong><Status value={task.status}/></div><p>参与 {task.total} · 成功 {task.succeeded??0} · 失败 {task.failed??0} · 待处理 {task.pending??0} · 评估成本 {formatMoney(task.costMicro??0)}</p>{task.status!=='running'&&(task.failed??0)>0&&<button className="button secondary" onClick={()=>void retry()}>重试失败项</button>}<div className="table-panel"><table><thead><tr><th>模型</th><th>结果</th><th>输出 Token</th><th>成本</th><th>说明</th></tr></thead><tbody>{items.map(i=><tr key={i.modelId}><td><strong>{i.modelKey}</strong></td><td><Status value={i.status}/></td><td>{formatNumber(i.outputTokens||0)}</td><td>{formatMoney(i.costMicro||0)}</td><td className="muted">{i.errorMessage||'—'}</td></tr>)}{items.length===0&&<EmptyRow cols={5} text="暂无任务明细。"/>}</tbody></table></div></div>}<div className="panel"><div className="section-heading"><h2>评分版本</h2><span className="muted">候选达到阈值不等于已发布</span></div><label>发布/回滚原因<input value={reason} onChange={e=>setReason(e.target.value)} placeholder="填写运营原因" /></label><div className="table-panel"><table><thead><tr><th>版本</th><th>状态</th><th>新评分覆盖率</th><th>参与/有效</th><th>历史评分回退</th><th>创建时间</th><th>操作</th></tr></thead><tbody>{versions.map(v=><tr key={v.id}><td>v{v.id}</td><td><Status value={v.status}/></td><td>{(v.successRatio*100).toFixed(1)}%</td><td>{v.validModels}/{v.totalModels}</td><td>{v.fallbackModels?`${v.fallbackModels} 个模型`: '—'}</td><td>{date(v.createdAt)}</td><td className="actions"><button className="text-button" disabled={v.status==='published'} onClick={()=>void publish(v)}>显式发布</button><button className="text-button" disabled={v.status!=='published'&&v.status!=='superseded'} onClick={()=>void rollback(v)}>回滚到此版本</button></td></tr>)}{versions.length===0&&<EmptyRow cols={7} text="尚无评分版本。"/>}</tbody></table></div></div></section>;
}

function UsersPanel({ notify }: { notify: (n: Notice) => void }) {
  const [rows,setRows]=useState<UserRow[]>([]);const [amount,setAmount]=useState<Record<number,string>>({});const [reason,setReason]=useState<Record<number,string>>({});
  const load=useCallback(()=>adminAPI.users().then(x=>setRows(x.list||[])).catch(e=>notify({kind:'error',text:errorText(e)})),[notify]);useEffect(()=>{void load();},[load]);
  async function toggle(u:UserRow){try{await adminAPI.userStatus(u.id,u.status!=='active');await load();notify({kind:'success',text:'用户状态已更新。'});}catch(e){notify({kind:'error',text:errorText(e)});}}
  async function adjust(u:UserRow){const micro=Math.round(Number(amount[u.id])*1_000_000);if(!micro||!reason[u.id]?.trim()){notify({kind:'error',text:'请填写非零金额和调整原因。'});return;}try{await adminAPI.adjustQuota(u.id,micro,reason[u.id],crypto.randomUUID());notify({kind:'success',text:'额度已调整并写入审计。'});setAmount({...amount,[u.id]:''});setReason({...reason,[u.id]:''});}catch(e){notify({kind:'error',text:errorText(e)});}}
  return <section><PageHeader eyebrow="ACCOUNTS" title="用户与额度"/><div className="panel table-panel"><table><thead><tr><th>用户</th><th>状态</th><th>额度调整（元）</th><th>原因</th><th>操作</th></tr></thead><tbody>{rows.map(u=><tr key={u.id}><td><strong>{u.email}</strong><small>{u.nickname} · #{u.id}</small></td><td><Status value={u.status}/></td><td><input className="table-input" type="number" step="0.01" value={amount[u.id]||''} onChange={e=>setAmount({...amount,[u.id]:e.target.value})}/></td><td><input className="table-input reason-input" value={reason[u.id]||''} onChange={e=>setReason({...reason,[u.id]:e.target.value})} placeholder="必填"/></td><td className="actions"><button className="text-button" onClick={()=>void adjust(u)}>调整</button><button className="text-button" onClick={()=>void toggle(u)}>{u.status==='active'?'停用':'启用'}</button></td></tr>)}{rows.length===0&&<EmptyRow cols={5} text="暂无用户。"/>}</tbody></table></div></section>;
}

function PackagesPanel({ notify }: { notify: (n: Notice) => void }) {
  const [rows,setRows]=useState<PackageRow[]>([]);const [codes,setCodes]=useState<string[]>([]);const [selected,setSelected]=useState<number>(0);
  const load=useCallback(()=>adminAPI.packages().then(x=>setRows(x.list||[])).catch(e=>notify({kind:'error',text:errorText(e)})),[notify]);useEffect(()=>{void load();},[load]);
  async function create(e:FormEvent<HTMLFormElement>){e.preventDefault();const fd=new FormData(e.currentTarget);try{await adminAPI.createPackage(String(fd.get('name')),Math.round(Number(fd.get('amount'))*1e6));notify({kind:'success',text:'流量包已创建。'});e.currentTarget.reset();await load();}catch(e){notify({kind:'error',text:errorText(e)});}}
  async function generate(e:FormEvent<HTMLFormElement>){e.preventDefault();const fd=new FormData(e.currentTarget);try{const x=await adminAPI.generateCodes(Number(fd.get('packageId')),Number(fd.get('quantity')));setCodes(x.codes);notify({kind:'success',text:`已生成 ${x.quantity} 个兑换码；明文只在本次结果展示。`});}catch(e){notify({kind:'error',text:errorText(e)});}}
  return <section><PageHeader eyebrow="QUOTA" title="流量包与兑换码"/><div className="two-col"><form className="panel form-stack" onSubmit={create}><h2>创建流量包</h2><label>名称<input name="name" required/></label><label>面额（元）<input name="amount" type="number" min="0.01" step="0.01" required/></label><button className="button primary">创建</button><p className="muted">首版兑换后进入全局余额，不区分模型且不单独过期。</p></form><form className="panel form-stack" onSubmit={generate}><h2>生成兑换码</h2><label>流量包<select name="packageId" required value={selected||''} onChange={e=>setSelected(Number(e.target.value))}><option value="" disabled>选择流量包</option>{rows.map(p=><option key={p.id} value={p.id}>{p.name} · {formatMoney(p.faceValueMicro)}</option>)}</select></label><label>数量<input name="quantity" type="number" min="1" max="10000" defaultValue="10" required/></label><button className="button secondary">生成兑换码</button></form></div>{codes.length>0&&<div className="alert success"><strong>请立即导出/保存这些兑换码。</strong><div className="code-list">{codes.join('\n')}</div><button className="button small-button" onClick={()=>void navigator.clipboard?.writeText(codes.join('\n'))}>复制全部</button></div>}<div className="panel table-panel"><table><thead><tr><th>名称</th><th>面额</th><th>状态</th></tr></thead><tbody>{rows.map(p=><tr key={p.id}><td>{p.name}</td><td>{formatMoney(p.faceValueMicro)}</td><td><Status value={p.status}/></td></tr>)}{rows.length===0&&<EmptyRow cols={3} text="暂无流量包。"/>}</tbody></table></div></section>;
}

function AdminUsage() {
  const [models, setModels] = useState<ModelUsageRow[]>([]);
  const [users, setUsers] = useState<UserUsageRow[]>([]);
  const [state, setState] = useState<'loading' | 'ok' | 'error'>('loading');
  const [error, setError] = useState('');
  const load = useCallback(() => {
    setState('loading');
    Promise.all([adminAPI.usageCost(), adminAPI.usageByUser()])
      .then(([m, u]) => { setModels(m.list || []); setUsers(u.list || []); setState('ok'); })
      .catch((e) => { setError(errorText(e)); setState('error'); });
  }, []);
  useEffect(() => { load(); }, [load]);
  if (state === 'loading') return <Loading />;
  const totals = models.reduce((acc, row) => ({ cost: acc.cost + (row.costMicro || 0), charge: acc.charge + (row.chargeMicro || 0) }), { cost: 0, charge: 0 });
  return <section>
    <PageHeader eyebrow="ANALYTICS" title="用量与成本" action={<button className="button secondary" onClick={load}>刷新</button>} />
    <div className="alert info">用户扣费与供应商成本分列；统计基于校验通过的 usage，不含任何 prompt 或密钥内容。</div>
    {state === 'error' && <div className="alert error" role="alert">{error}<button className="button small-button" onClick={load}>重试</button></div>}
    <div className="metric-grid compact"><Metric label="上游成本合计" value={formatMoney(totals.cost)} hint="供应商视角" /><Metric label="用户扣费合计" value={formatMoney(totals.charge)} hint="用户视角" /></div>
    <div className="panel table-panel"><table>
      <thead><tr><th>模型</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th><th>上游成本</th><th>用户扣费</th><th>成功率</th><th>平均延迟</th></tr></thead>
      <tbody>
        {models.map((m) => <tr key={m.modelId}><td><strong>{m.modelName || `#${m.modelId}`}</strong></td><td>{formatNumber(m.requests)}</td><td>{formatNumber(m.inputTokens)}</td><td>{formatNumber(m.outputTokens)}</td><td>{formatMoney(m.costMicro)}</td><td>{formatMoney(m.chargeMicro)}</td><td>{`${(m.successRate * 100).toFixed(1)}%`}</td><td>{`${Math.round(m.avgLatencyMs || 0)} ms`}</td></tr>)}
        {models.length === 0 && <EmptyRow cols={8} text="当前时间范围内暂无模型用量。" />}
      </tbody>
    </table></div>
    <div className="section-heading"><h2>按用户汇总</h2></div>
    <div className="panel table-panel"><table>
      <thead><tr><th>用户</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th><th>用户扣费</th></tr></thead>
      <tbody>
        {users.map((u) => <tr key={u.userId}><td>#{u.userId}</td><td>{formatNumber(u.requests)}</td><td>{formatNumber(u.inputTokens)}</td><td>{formatNumber(u.outputTokens)}</td><td>{formatMoney(u.chargeMicro)}</td></tr>)}
        {users.length === 0 && <EmptyRow cols={5} text="当前时间范围内暂无用户用量。" />}
      </tbody>
    </table></div>
  </section>;
}

function NodeHealthPanel() {
  const [nodes, setNodes] = useState<NodeRow[]>([]);
  const [active, setActive] = useState(0);
  const [state, setState] = useState<'loading' | 'ok' | 'error'>('loading');
  const [error, setError] = useState('');
  const load = useCallback(() => {
    setState('loading');
    adminAPI.nodes()
      .then((x) => { setNodes(x.list || []); setActive(x.activeScoreVersionId || 0); setState('ok'); })
      .catch((e) => { setError(errorText(e)); setState('error'); });
  }, []);
  useEffect(() => { load(); }, [load]);
  if (state === 'loading') return <Loading />;
  const degraded = nodes.filter((n) => n.degraded).length;
  return <section>
    <PageHeader eyebrow="RUNTIME" title="节点健康" action={<button className="button secondary" onClick={load}>刷新</button>} />
    <div className="alert info">节点是接流量的 Go 网关进程。语义分类不可用时节点仍提供通用路由，但会标记 degraded；评分发布只有在全部接流量节点 ACK 后才报告成功。</div>
    {state === 'error' && <div className="alert error" role="alert">{error}<button className="button small-button" onClick={load}>重试</button></div>}
    <div className="metric-grid compact"><Metric label="在线节点" value={formatNumber(nodes.length)} /><Metric label="当前评分版本" value={active ? `v${active}` : '未发布'} /><Metric label="分类器降级节点" value={formatNumber(degraded)} hint={degraded ? '仅影响 auto 分类质量' : '全部正常'} /></div>
    <div className="panel table-panel"><table>
      <thead><tr><th>节点</th><th>评分版本</th><th>接流量</th><th>分类器</th><th>最后心跳</th></tr></thead>
      <tbody>
        {nodes.map((n) => <tr key={n.id}><td><strong>{n.id}</strong></td><td>{n.scoreVersionId ? `v${n.scoreVersionId}` : '—'}{n.scoreVersionId !== active && active !== 0 ? <small>落后于 v{active}</small> : null}</td><td><Status value={n.drain ? 'draining' : n.ready ? 'ready' : 'pending'} /></td><td>{n.degraded ? <span className="status bad"><i />降级{n.classifierReason ? ` · ${n.classifierReason}` : ''}</span> : <span className="status good"><i />{n.classifierVersion || '正常'}</span>}</td><td>{date(n.lastSeen)}</td></tr>)}
        {nodes.length === 0 && <EmptyRow cols={5} text="当前没有在线网关节点。" />}
      </tbody>
    </table></div>
  </section>;
}

function PolicyPanel({ notify }: { notify: (n: Notice) => void }) {
  const [p,setP]=useState<import('./api').RoutingPolicy|null>(null);useEffect(()=>{adminAPI.policy().then(setP).catch(e=>notify({kind:'error',text:errorText(e)}));},[notify]);if(!p)return <Loading/>;
  async function save(e:FormEvent){e.preventDefault();if(!p)return;try{const updated=await adminAPI.updatePolicy(p);setP(updated);notify({kind:'success',text:'策略已保存并审计。'});}catch(e){notify({kind:'error',text:errorText(e)});}}
  return <section><PageHeader eyebrow="ROUTING" title="路由策略"/><form className="panel form-stack" onSubmit={save}><label>低能力/低成本倾向：{p.lowCapabilityBias}<input type="range" min="0" max="100" value={p.lowCapabilityBias} onChange={e=>setP({...p,lowCapabilityBias:Number(e.target.value)})}/><small>质量优先 ← → 成本优先；不改变硬约束</small></label><label>发布最低成功率<input type="number" min="0.01" max="1" step="0.01" value={p.minPublishRatio} onChange={e=>setP({...p,minPublishRatio:Number(e.target.value)})}/></label><label className="check-label"><input type="checkbox" checked={p.highRiskForceQuality} onChange={e=>setP({...p,highRiskForceQuality:e.target.checked})}/>高风险请求强制质量优先</label><button className="button primary">保存策略</button></form></section>;
}

function AuditPanel() {
  const [rows, setRows] = useState<AuditRow[]>([]);
  const [state, setState] = useState<'loading' | 'ok' | 'error'>('loading');
  const [error, setError] = useState('');
  const load = useCallback(() => {
    setState('loading');
    adminAPI.audit().then((x) => { setRows(x.list || []); setState('ok'); }).catch((e) => { setError(errorText(e)); setState('error'); });
  }, []);
  useEffect(() => { load(); }, [load]);
  if (state === 'loading') return <Loading />;
  return <section>
    <PageHeader eyebrow="SECURITY" title="审计日志" action={<button className="button secondary" onClick={load}>刷新</button>} />
    <div className="alert info">审计记录操作者、动作、目标与原因，不包含用户 prompt、供应商密钥或 API Key 明文。</div>
    {state === 'error' && <div className="alert error" role="alert">{error}<button className="button small-button" onClick={load}>重试</button></div>}
    <div className="panel table-panel"><table>
      <thead><tr><th>时间</th><th>操作者</th><th>动作</th><th>目标</th><th>原因</th><th>结果</th></tr></thead>
      <tbody>
        {rows.map((a) => <tr key={a.id}><td>{date(a.createdAt)}</td><td>{a.actorType}{a.actorId ? `#${a.actorId}` : ''}</td><td><code>{a.action}</code></td><td>{a.targetType ? `${a.targetType}${a.targetId ? `#${a.targetId}` : ''}` : '—'}</td><td className="muted">{a.reason || '—'}</td><td><Status value={a.result === 'success' ? 'success' : a.result || 'success'} /></td></tr>)}
        {rows.length === 0 && <EmptyRow cols={6} text="暂无审计记录。" />}
      </tbody>
    </table></div>
  </section>;
}

function Shell({ area, title, me, activeTab, onTab, onLogout, allowedTabs, children }: { area: Area; title: string; me?: string; activeTab: string; onTab: (v:string)=>void; onLogout:()=>void; allowedTabs?: string[]; children: React.ReactNode }) {
  const userNav=[['overview','概览'],['keys','API Keys'],['redeem','兑换额度'],['requests','用量与请求'],['account','账户与安全']];
  const adminNav=[['overview','概览'],['providers','供应商'],['models','模型目录'],['evaluation','评估与评分'],['nodes','节点健康'],['policy','路由策略'],['users','用户与额度'],['packages','流量包/兑换码'],['usage','用量与成本'],['audit','审计日志']];
  const nav=(area==='user'?userNav:adminNav).filter(([id])=>area==='user'||id==='overview'||!allowedTabs||allowedTabs.includes(id));
  return <div className={`app-shell ${area}`}><aside className="sidebar"><Link to="/" className="sidebar-brand"><span className="brand-mark mini">千</span><span><strong>千丝傀智</strong><small>{area==='user'?'USER CONSOLE':'ADMIN CONSOLE'}</small></span></Link><nav>{nav.map(([id,label])=><button key={id} className={activeTab===id?'nav-item active':'nav-item'} onClick={()=>onTab(id)}><span className="nav-dot"/>{label}</button>)}</nav><div className="sidebar-bottom"><span className="avatar">{(me||'U').slice(0,1).toUpperCase()}</span><span className="account-name">{me||'账户'}</span><button className="icon-button" aria-label="退出登录" onClick={onLogout}>↗</button></div></aside><main className="main-area"><header className="topbar"><div><span className="breadcrumb">{area==='user'?'用户工作台':'管理后台'}</span><span className="breadcrumb-sep">/</span><strong>{title}</strong></div><div className="topbar-right"><span className="env-chip">本地控制台</span><span className="avatar small-avatar">{(me||'U').slice(0,1).toUpperCase()}</span></div></header><div className="content">{children}</div></main></div>;
}

function RequestTable({ rows }: { rows: RequestRow[] }) {
  return <div className="panel table-panel"><table><thead><tr><th>时间</th><th>请求模型</th><th>实际模型</th><th>输入 Token</th><th>输出 Token</th><th>费用</th><th>状态</th></tr></thead><tbody>{rows.map(r=><tr key={r.requestId}><td>{date(r.createdAt)}</td><td><code>{r.requestedModel}</code></td><td>{String(r.model)}</td><td>{formatNumber(r.inputTokens||0)}</td><td>{formatNumber(r.outputTokens||0)}</td><td>{formatMoney(r.chargeMicro||0)}</td><td><Status value={r.status}/></td></tr>)}{rows.length===0&&<EmptyRow cols={7} text="当前时间范围内暂无请求。"/>}</tbody></table></div>;
}
function Metric({label,value,hint}:{label:string;value:string;hint?:string}){return <div className="metric-card"><span>{label}</span><strong>{value}</strong>{hint&&<small>{hint}</small>}</div>;}
function PageHeader({eyebrow,title,action}:{eyebrow:string;title:string;action?:React.ReactNode}){return <div className="page-heading"><div><p className="eyebrow">{eyebrow}</p><h1>{title}</h1></div>{action}</div>;}
function Status({value}:{value:string}){const good=['active','available','enabled','published','success','succeeded','completed','ready'].includes(value);const bad=['disabled','unavailable','failed','timeout','error','draining'].includes(value);return <span className={`status ${good?'good':bad?'bad':'pending'}`}><i/>{statusText(value)}</span>;}
function statusText(s:string){const map:Record<string,string>={active:'启用',available:'可用',enabled:'启用',published:'已发布',success:'成功',succeeded:'成功',completed:'已完成',disabled:'停用',unavailable:'不可用',failed:'失败',timeout:'超时',error:'错误',building:'候选',running:'处理中',pending:'待确认',ready:'接流量',draining:'摘流量中',superseded:'已被取代'};return map[s]||s;}
function EmptyRow({cols,text}:{cols:number;text:string}){return <tr><td colSpan={cols} className="empty-cell">{text}</td></tr>;}
function NoticeBox({notice,onClose}:{notice:Notice;onClose:()=>void}){if(!notice)return null;return <div className={`alert ${notice.kind}`} role="status">{notice.text}<button className="icon-button" onClick={onClose} aria-label="关闭">×</button></div>;}
function Loading(){return <div className="loading"><span className="spinner"/>加载中…</div>;}
function errorText(err:unknown){return err instanceof APIError?err.message:err instanceof Error?err.message:'发生未知错误';}
function date(value?:string){return value?new Date(value).toLocaleString('zh-CN',{dateStyle:'short',timeStyle:'short'}):'—';}
function tabTitle(tab:string,area:Area){const user:Record<string,string>={overview:'概览',keys:'API Keys',redeem:'兑换额度',requests:'用量与请求',account:'账户与安全'};const admin:Record<string,string>={overview:'运营概览',providers:'供应商',models:'模型目录',evaluation:'评估与评分',nodes:'节点健康',policy:'路由策略',users:'用户与额度',packages:'流量包与兑换码',usage:'用量与成本',audit:'审计日志'};return (area==='user'?user:admin)[tab]||'工作台';}

export default App;
