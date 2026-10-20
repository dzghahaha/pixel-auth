(() => {
    window.newLocalJioReference = () => {
        const bytes = crypto.getRandomValues(new Uint8Array(16));
        return 'local-admin-' + Array.from(bytes, value => value.toString(16).padStart(2, '0')).join('');
    };
    const panel = document.createElement('section');
    panel.id = 'local-jio-panel';
    panel.hidden = true;
    panel.setAttribute('aria-labelledby', 'local-jio-title');
    panel.style.cssText = 'background:white;border:1px solid #cbd5e1;border-radius:12px;padding:24px;margin-bottom:24px;color:#0f172a;';
    panel.innerHTML = `
        <div style="display:flex;justify-content:space-between;gap:16px;align-items:center;flex-wrap:wrap;">
            <h2 id="local-jio-title" tabindex="-1" style="font-size:18px;margin:0;">Jio 自有链接库存</h2>
            <button type="button" class="btn-action btn-action-outline" id="local-jio-close">收起库存</button>
        </div>
        <p id="local-jio-summary" role="status" style="color:#475569;">正在读取库存…</p>
        <label class="field-label" for="local-jio-input">批量录入兑换链接</label>
        <textarea id="local-jio-input" class="field-input" rows="5" spellcheck="false" aria-describedby="local-jio-hint" style="resize:vertical;font-family:monospace;"></textarea>
        <p id="local-jio-hint" style="font-size:13px;color:#475569;">每行一条 HTTP/HTTPS 链接，最多 1000 条。重复链接自动跳过；有无效行时整批不录入。</p>
        <button type="button" class="btn-action btn-action-primary" id="local-jio-import">录入链接</button>
        <p id="local-jio-feedback" role="status" style="color:#475569;white-space:pre-wrap;"></p>
        <div style="display:flex;gap:12px;align-items:center;flex-wrap:wrap;margin:20px 0 12px;">
            <label for="local-jio-status">库存状态</label>
            <select id="local-jio-status" class="field-input" style="width:auto;">
                <option value="">全部</option><option value="available">未使用</option><option value="issued">已发放</option>
            </select>
            <button type="button" class="btn-action btn-action-outline" id="local-jio-refresh">刷新库存</button>
        </div>
        <div style="overflow:auto;">
            <table style="width:100%;border-collapse:collapse;text-align:left;font-size:13px;">
                <thead><tr><th scope="col">ID</th><th scope="col">兑换链接</th><th scope="col">状态</th><th scope="col">发放订单</th><th scope="col">录入时间</th><th scope="col">发放时间</th></tr></thead>
                <tbody id="local-jio-rows"></tbody>
            </table>
        </div>
        <div style="display:flex;gap:12px;align-items:center;margin-top:16px;">
            <button type="button" class="btn-action btn-action-outline" id="local-jio-prev">上一页</button>
            <span id="local-jio-page" aria-live="polite"></span>
            <button type="button" class="btn-action btn-action-outline" id="local-jio-next">下一页</button>
        </div>`;
    document.getElementById('suppliers-container').before(panel);
    const el = name => document.getElementById(`local-jio-${name}`);
    const endpoint = '/api/admin/jio/suppliers/local/links';
    let page = 1;
    let requestVersion = 0;
    let opener;
    let feedbackIsError = false;
    const feedback = (message, error = false) => {
        feedbackIsError = error;
        el('feedback').textContent = message;
        el('feedback').style.color = error ? '#b91c1c' : '#475569';
    };
    async function load() {
        const version = ++requestVersion;
        el('prev').disabled = el('next').disabled = true;
        el('summary').textContent = '正在读取库存…';
        el('rows').replaceChildren();
        try {
            const response = await fetch(`${endpoint}?page=${page}&status=${encodeURIComponent(el('status').value)}`, {cache: 'no-store'});
            const data = await response.json();
            if (version !== requestVersion) return;
            if (!response.ok || !data.success) throw new Error(data.message || '读取库存失败');
            if (feedbackIsError) feedback('');
            page = data.page;
            el('summary').textContent = `未使用 ${data.available} 条 · 已发放 ${data.issued} 条`;
            el('page').textContent = `第 ${page} / ${data.pages} 页 · 共 ${data.total} 条`;
            el('prev').disabled = page <= 1;
            el('next').disabled = page >= data.pages;
            for (const item of data.items) {
                const row = document.createElement('tr');
                for (const value of [item.id, item.link, item.status === 'available' ? '未使用' : '已发放', item.order_id || '—', item.created_at, item.issued_at || '—']) {
                    const cell = document.createElement('td');
                    cell.textContent = value;
                    cell.style.cssText = 'padding:12px 8px;border-top:1px solid #e2e8f0;vertical-align:top;';
                    row.appendChild(cell);
                }
                row.children[1].style.cssText += 'min-width:240px;max-width:440px;overflow-wrap:anywhere;font-family:monospace;user-select:all;';
                row.children[3].style.cssText += 'font-family:monospace;overflow-wrap:anywhere;';
                el('rows').appendChild(row);
            }
            if (!data.items.length) {
                const row = document.createElement('tr');
                const cell = document.createElement('td');
                cell.colSpan = 6;
                cell.style.padding = '24px 0';
                cell.textContent = '暂无符合条件的链接。可在上方按行粘贴并录入兑换链接。';
                row.appendChild(cell);
                el('rows').appendChild(row);
            }
        } catch (error) {
            if (version !== requestVersion) return;
            el('summary').textContent = '库存读取失败，请重试';
            el('page').textContent = '';
            feedback(error.message, true);
        }
    }
    window.openLocalJioLinks = () => {
        opener = document.activeElement;
        panel.hidden = false;
        el('title').focus();
        panel.scrollIntoView({block: 'start'});
        load();
    };
    el('close').addEventListener('click', () => { panel.hidden = true; opener?.focus(); });
    el('status').addEventListener('change', () => { page = 1; load(); });
    el('refresh').addEventListener('click', load);
    el('prev').addEventListener('click', () => { page--; load(); });
    el('next').addEventListener('click', () => { page++; load(); });
    el('import').addEventListener('click', async () => {
        const input = el('input');
        if (!input.value.trim()) { feedback('请至少输入一条兑换链接', true); input.focus(); return; }
        const submitted = input.value;
        const button = el('import');
        button.disabled = true;
        button.textContent = '正在录入…';
        feedback('');
        try {
            const response = await fetch(endpoint, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({links: submitted})});
            const data = await response.json();
            if (!response.ok || !data.success) throw new Error(data.message || '录入失败，请重试');
            if (input.value === submitted) input.value = '';
            feedback(`已新增 ${data.added} 条，跳过 ${data.duplicates} 条重复链接。`);
            page = 1;
            document.dispatchEvent(new Event('local-jio-updated'));
        } catch (error) { feedback(error.message, true); }
        finally { button.disabled = false; button.textContent = '录入链接'; }
    });
    document.addEventListener('local-jio-updated', () => { if (!panel.hidden) load(); });
})();
