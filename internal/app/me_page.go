package app

// The /me page is served as one self-contained document (no admin bundle, no
// admin session). All data arrives through /me/api/* JSON and is rendered with
// textContent, so key names or model ids cannot inject markup.

const mePageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>AIWorkGate · 我的 Key</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; font: 14px/1.55 system-ui, "Segoe UI", sans-serif; background: #f6f7f9; color: #1c1f23; }
  @media (prefers-color-scheme: dark) { body { background: #14161a; color: #e7e9ee; } .card, table { background: #1c1f25; } th { color: #9aa3b2; } }
  main { max-width: 880px; margin: 40px auto; padding: 0 16px 48px; }
  h1 { font-size: 20px; margin: 0 0 6px; }
  .sub { opacity: .65; margin-bottom: 22px; }
  .card { background: #fff; border-radius: 10px; padding: 16px 18px; box-shadow: 0 1px 3px rgba(0,0,0,.08); margin-bottom: 16px; }
  .row { display: flex; gap: 10px; align-items: center; }
  input { flex: 1; padding: 9px 11px; border: 1px solid #c9ced6; border-radius: 8px; font: inherit; background: transparent; color: inherit; }
  button { padding: 9px 16px; border: 0; border-radius: 8px; background: #2f6fed; color: #fff; font: inherit; cursor: pointer; }
  button.ghost { background: transparent; color: inherit; border: 1px solid #c9ced6; }
  .err { color: #c0392b; min-height: 20px; margin: 8px 0 0; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 10px; }
  .kv { border: 1px solid rgba(128,128,128,.25); border-radius: 8px; padding: 10px 12px; }
  .kv b { display: block; font-size: 12px; opacity: .65; font-weight: 500; }
  .kv span { font-size: 16px; }
  table { width: 100%; border-collapse: collapse; background: #fff; border-radius: 8px; overflow: hidden; }
  th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid rgba(128,128,128,.18); font-size: 13px; }
  th { font-size: 12px; font-weight: 500; color: #667085; }
  .muted { opacity: .6; }
  .badge { font-size: 12px; padding: 2px 8px; border-radius: 999px; border: 1px solid rgba(128,128,128,.35); }
</style>
</head>
<body>
<main>
  <h1>AIWorkGate · 我的 Key</h1>
  <div class="sub">查看这把 Key 的限额与用量。页面不显示请求正文，也不能管理其它 Key。</div>

  <section id="login" class="card">
    <div class="row"><input id="key" type="password" autocomplete="off" placeholder="粘贴管理员发给你的客户端 Key"><button id="loginBtn">登录</button></div>
    <p id="err" class="err"></p>
  </section>

  <section id="panel" hidden>
    <div class="card">
      <div class="row" style="justify-content: space-between">
        <div><strong id="name"></strong> <span id="owner" class="badge" hidden>属主</span></div>
        <button id="logout" class="ghost">退出</button>
      </div>
      <div id="limits" class="grid" style="margin-top: 12px"></div>
    </div>

    <div class="card"><h3 style="margin:0 0 10px;font-size:15px">用量</h3><div id="usage" class="grid"></div></div>

    <div class="card"><h3 style="margin:0 0 10px;font-size:15px">按模型（近 7 天）</h3><table><thead><tr><th>模型</th><th>请求</th><th>输入 token</th><th>输出 token</th></tr></thead><tbody id="models"></tbody></table></div>

    <div class="card"><h3 style="margin:0 0 10px;font-size:15px">最近请求</h3><table><thead><tr><th>时间</th><th>模型</th><th>状态</th><th>耗时</th><th>tokens</th></tr></thead><tbody id="logs"></tbody></table></div>
  </section>
</main>
<script>
(function () {
  const $ = (id) => document.getElementById(id);
  const err = $('err');
  function text(node, value) { node.textContent = value == null ? '' : String(value); }
  function kv(label, value) {
    const box = document.createElement('div');
    box.className = 'kv';
    const b = document.createElement('b'); text(b, label);
    const s = document.createElement('span'); text(s, value);
    box.append(b, s);
    return box;
  }
  function fillGrid(node, items) { node.replaceChildren(...items); }
  function when(unix) { return unix ? new Date(unix * 1000).toLocaleString() : '—'; }
  function quota(v) { return v < 0 ? '不限' : v; }

  async function api(path, options) {
    const res = await fetch(path, Object.assign({ credentials: 'same-origin' }, options || {}));
    let data = null;
    try { data = await res.json(); } catch (_) {}
    if (!res.ok) {
      const message = data && data.error && data.error.message ? data.error.message : ('HTTP ' + res.status);
      const e = new Error(message); e.status = res.status; throw e;
    }
    return data;
  }

  function render(data) {
    $('login').hidden = true;
    $('panel').hidden = false;
    text($('name'), data.key.name || '(未命名)');
    $('owner').hidden = !data.key.owner;
    fillGrid($('limits'), [
      kv('到期', data.key.expires_at ? when(data.key.expires_at) : '永不过期'),
      kv('每日配额', data.key.daily_quota > 0 ? data.key.daily_quota : '不限'),
      kv('总请求上限', data.key.max_requests_total > 0 ? data.key.max_requests_total : '不限'),
      kv('今日剩余', quota(data.key.quota_left)),
      kv('模型范围', (data.key.model_scope && data.key.model_scope.length) ? data.key.model_scope.join('、') : '不限'),
    ]);
    fillGrid($('usage'), [
      kv('今日请求', data.today.requests || 0),
      kv('今日 token', (data.today.prompt_tokens || 0) + ' / ' + (data.today.completion_tokens || 0)),
      kv('近 7 天请求', data.week.requests || 0),
      kv('近 7 天 token', (data.week.prompt_tokens || 0) + ' / ' + (data.week.completion_tokens || 0)),
    ]);
    const models = $('models'); models.replaceChildren();
    (data.models || []).forEach((row) => {
      const tr = document.createElement('tr');
      [row.Model, row.Requests, row.PromptTokens, row.CompletionTokens].forEach((v) => {
        const td = document.createElement('td'); text(td, v); tr.append(td);
      });
      models.append(tr);
    });
    if (!models.children.length) {
      const tr = document.createElement('tr'); const td = document.createElement('td');
      td.colSpan = 4; td.className = 'muted'; text(td, '暂无记录'); tr.append(td); models.append(tr);
    }
    const logs = $('logs'); logs.replaceChildren();
    (data.logs || []).forEach((row) => {
      const tr = document.createElement('tr');
      const cells = [when(row.TS ? Date.parse(row.TS) / 1000 : 0), row.Model, row.Status,
        (row.LatencyMS || 0) + ' ms', (row.PromptTokens || 0) + ' / ' + (row.CompletionTokens || 0)];
      cells.forEach((v) => { const td = document.createElement('td'); text(td, v); tr.append(td); });
      logs.append(tr);
    });
    if (!logs.children.length) {
      const tr = document.createElement('tr'); const td = document.createElement('td');
      td.colSpan = 5; td.className = 'muted'; text(td, '暂无记录'); tr.append(td); logs.append(tr);
    }
  }

  async function load() {
    try { render(await api('/me/api/me')); }
    catch (e) { if (e.status === 401) { $('panel').hidden = true; $('login').hidden = false; } else { text(err, e.message); } }
  }

  $('loginBtn').addEventListener('click', async () => {
    text(err, '');
    try {
      await api('/me/api/session', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ key: $('key').value }) });
      $('key').value = '';
      await load();
    } catch (e) { text(err, e.message); }
  });
  $('logout').addEventListener('click', async () => {
    try { await api('/me/api/session', { method: 'DELETE' }); } catch (_) {}
    $('panel').hidden = true; $('login').hidden = false;
  });
  load();
})();
</script>
</body>
</html>
`
