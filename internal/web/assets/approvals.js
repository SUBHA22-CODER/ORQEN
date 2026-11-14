/* ORQEN judge approval panel — self-contained; loaded after app.js. */
(() => {
  'use strict';

  const $ = (sel, root = document) => root.querySelector(sel);

  const state = { approvals: [], open: null, es: null, filter: 'PENDING' };

  function ensureModal() {
    if ($('#approvals-modal')) return;
    const wrap = document.createElement('div');
    wrap.className = 'modal';
    wrap.id = 'approvals-modal';
    wrap.hidden = true;
    wrap.innerHTML = `
      <div class="modal-card approvals-card" role="dialog" aria-modal="true" aria-labelledby="approvals-title">
        <div class="approvals-head">
          <h2 id="approvals-title">Judge approvals</h2>
          <button class="ghost" id="approvals-close" type="button" aria-label="Close">✕</button>
        </div>
        <div class="approvals-demo">
          <span class="approvals-demo-label">SIMULATED demo events (isolated fixture)</span>
          <div class="approvals-demo-btns">
            <button type="button" data-demo="LOW_RISK_FIX">LOW_RISK_FIX</button>
            <button type="button" data-demo="HIGH_RISK_FIX">HIGH_RISK_FIX</button>
            <button type="button" data-demo="UNCERTAIN_DIAGNOSIS">UNCERTAIN_DIAGNOSIS</button>
            <button type="button" data-demo="DESTRUCTIVE_ACTION">DESTRUCTIVE_ACTION</button>
          </div>
          <div id="approvals-demo-result" class="approvals-demo-result"></div>
        </div>
        <div class="approvals-filter" id="approvals-filter"></div>
        <div class="approvals-body">
          <div id="approvals-list" class="approvals-list"></div>
          <div id="approvals-detail" class="approvals-detail"></div>
        </div>
      </div>`;
    document.body.appendChild(wrap);

    $('#approvals-close', wrap).addEventListener('click', close);
    wrap.addEventListener('click', (e) => { if (e.target === wrap) close(); });
    $$('.approvals-demo-btns button', wrap).forEach((b) =>
      b.addEventListener('click', () => triggerDemo(b.dataset.demo)));
    renderFilter();
  }

  function renderFilter() {
    const holder = $('#approvals-filter');
    if (!holder) return;
    holder.innerHTML = '';
    ['PENDING', 'APPROVED', 'REJECTED', 'EXECUTED', 'FAILED', 'EXPIRED', 'ALL'].forEach((f) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = f;
      b.className = state.filter === f ? 'chip chip-active' : 'chip muted';
      b.addEventListener('click', () => { state.filter = f; renderFilter(); renderList(); });
      holder.appendChild(b);
    });
  }

  async function open() {
    ensureModal();
    $('#approvals-modal').hidden = false;
    await refresh();
    connectStream();
  }

  function close() {
    const m = $('#approvals-modal');
    if (m) m.hidden = true;
    if (state.es) { state.es.close(); state.es = null; }
  }

  function connectStream() {
    if (state.es) return;
    try {
      state.es = new EventSource('/api/approvals/stream');
      state.es.onmessage = (ev) => {
        try {
          const data = JSON.parse(ev.data);
          if (data.approval) refresh();
          const toastEl = document.getElementById('toast');
          if (toastEl && data.type && data.type.startsWith('approval_')) {
            toastEl.textContent = 'approval: ' + data.type.replace('approval_', '');
            toastEl.hidden = false;
            clearTimeout(toastEl._t);
            toastEl._t = setTimeout(() => { toastEl.hidden = true; }, 3500);
          }
        } catch { /* ignore malformed frames */ }
      };
    } catch { /* SSE unavailable; polling fallback below */ }
    setInterval(() => { const m = $('#approvals-modal'); if (m && !m.hidden) refresh().catch(() => {}); }, 5000);
  }

  async function refresh() {
    try {
      state.approvals = await api('/api/approvals');
      renderList();
      if (state.open) {
        const current = state.approvals.find((a) => a.approval_id === state.open);
        if (current) renderDetail(current); else { state.open = null; renderDetail(null); }
      }
    } catch { /* server unavailable */ }
  }

  function visible() {
    if (state.filter === 'ALL') return state.approvals;
    return state.approvals.filter((a) => a.status === state.filter);
  }

  function renderList() {
    const holder = $('#approvals-list');
    if (!holder) return;
    const items = visible();
    if (!items.length) {
      holder.innerHTML = '<div class="approvals-empty">No approval requests.</div>';
      return;
    }
    holder.innerHTML = '';
    for (const a of items) {
      const row = document.createElement('button');
      row.type = 'button';
      row.className = 'approvals-row' + (state.open === a.approval_id ? ' active' : '');
      const sim = a.simulated ? ' <span class="chip">SIMULATED</span>' : '';
      row.innerHTML = `
        <span class="approvals-status s-${String(a.status).toLowerCase()}">${a.status}</span>
        <span class="approvals-row-title">${escapeHTML(a.action_summary || a.action_type)}${sim}</span>
        <span class="approvals-row-meta">${a.risk_level || ''} · ${a.confidence_tier || ''}</span>`;
      row.addEventListener('click', () => { state.open = a.approval_id; renderList(); renderDetail(a); });
      holder.appendChild(row);
    }
  }

  function renderDetail(a) {
    const holder = $('#approvals-detail');
    if (!holder) return;
    if (!a) { holder.innerHTML = '<div class="approvals-empty">Select an approval request.</div>'; return; }
    const expiry = a.expires_at ? new Date(a.expires_at) : null;
    const mins = expiry ? Math.max(0, Math.round((expiry - Date.now()) / 60000)) : null;
    const actions = [];
    if (a.status === 'PENDING') {
      actions.push(`<button class="danger" id="apr-reject" type="button">REJECT</button>`);
      if (a.uncertain) {
        actions.push(`<button class="ghost" id="apr-investigate" type="button">APPROVE ADDITIONAL INVESTIGATION</button>`);
        actions.push(`<button class="primary" id="apr-approve" type="button">APPROVE PROPOSED REPAIR</button>`);
      } else {
        actions.push(`<button class="primary" id="apr-approve" type="button">APPROVE</button>`);
      }
    } else {
      actions.push(`<span class="chip">${a.status}</span>`);
    }
    holder.innerHTML = `
      <h3>${escapeHTML(a.action_summary || a.action_type)} ${a.simulated ? '<span class="chip">SIMULATED</span>' : ''}</h3>
      <div class="approvals-meta">
        <div><span class="k">Approval ID</span><span class="v mono">${escapeHTML(a.approval_id)}</span></div>
        <div><span class="k">Project</span><span class="v mono">${escapeHTML(a.project_id || '')}</span></div>
        <div><span class="k">Action type</span><span class="v">${escapeHTML(a.action_type)}</span></div>
        <div><span class="k">Risk</span><span class="v risk">${escapeHTML(a.risk_level || '')}</span></div>
        <div><span class="k">Confidence</span><span class="v">${escapeHTML(a.confidence_tier || '')} (${(a.confidence_score ?? 0).toFixed(2)})</span></div>
        <div><span class="k">Expires in</span><span class="v">${mins === null ? '—' : mins + ' min'}</span></div>
        ${a.uncertain ? '<div><span class="k">Uncertainty</span><span class="v warn">diagnosis is actionable but uncertain — review the evidence before choosing</span></div>' : ''}
      </div>
      <h4>Problem</h4><p>${escapeHTML(a.action_summary || '')}</p>
      ${a.technical_reason ? `<h4>Root-cause explanation</h4><p>${escapeHTML(a.technical_reason)}</p>` : ''}
      ${(a.evidence || []).length ? `<h4>Evidence collected</h4><ul class="approvals-evidence">${a.evidence.map((e) => `<li>${escapeHTML(e)}</li>`).join('')}</ul>` : ''}
      ${(a.affected_files || []).length ? `<h4>Affected files</h4><div>${a.affected_files.map((f) => `<span class="chip muted">${escapeHTML(f)}</span>`).join(' ')}</div>` : ''}
      ${a.proposed_diff ? `<h4>Proposed changes</h4><pre class="approvals-diff">${escapeHTML(a.proposed_diff)}</pre>` : ''}
      ${a.expected_impact ? `<h4>Potential impact</h4><p>${escapeHTML(a.expected_impact)}</p>` : ''}
      ${a.rollback_plan ? `<h4>Rollback plan</h4><p>${escapeHTML(a.rollback_plan)}</p>` : ''}
      ${a.verification_result ? `<h4>Verification</h4><p>${escapeHTML(a.verification_result)}</p>` : ''}
      ${a.rollback_result ? `<h4>Rollback result</h4><p>${escapeHTML(a.rollback_result)}</p>` : ''}
      <div class="approvals-actions">${actions.join('')}</div>
      <div class="approvals-note">Approval is bound to action hash <span class="mono">${escapeHTML((a.action_hash || '').slice(0, 16))}…</span>, one-time, and expires.</div>`;
    const approve = $('#apr-approve', holder);
    const investigate = $('#apr-investigate', holder);
    const reject = $('#apr-reject', holder);
    if (approve) approve.addEventListener('click', () => decide(a, true, 'repair'));
    if (investigate) investigate.addEventListener('click', () => decide(a, true, 'investigation'));
    if (reject) reject.addEventListener('click', () => decide(a, false, 'repair'));
  }

  async function decide(a, approved, decision) {
    const path = `/api/approvals/${encodeURIComponent(a.approval_id)}/${approved ? 'approve' : 'reject'}`;
    try {
      await api(path, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          approval_id: a.approval_id,
          action_hash: a.action_hash,
          project_id: a.project_id || '',
          session_id: a.session_id || '',
          decision,
          approved,
          decided_by: 'judge-ui',
          reason: approved ? '' : 'rejected by judge',
        }),
      });
      await refresh();
    } catch (e) {
      const toastEl = document.getElementById('toast');
      if (toastEl) { toastEl.textContent = 'decision failed: ' + (e.message || 'error'); toastEl.hidden = false; }
    }
  }

  async function triggerDemo(event) {
    const out = $('#approvals-demo-result');
    if (out) out.textContent = 'running SIMULATED event ' + event + '…';
    try {
      const report = await api('/api/approvals/demo', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ event }),
      });
      if (out) {
        const oc = report.outcome;
        out.textContent = `SIMULATED ${event}: ${report.decision}` +
          (report.approval_id ? ` (approval ${report.approval_id})` : '') +
          (oc && typeof oc === 'object' ? ` — executed: ${oc.executed}, ${oc.verification}` : typeof oc === 'string' ? ` — ${oc}` : '');
      }
      await refresh();
    } catch (e) {
      if (out) out.textContent = 'demo failed: ' + (e.message || 'error');
    }
  }

  function escapeHTML(value) {
    return String(value ?? '').replace(/[&<>"']/g, (c) =>
      ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  function boot() {
    const btn = document.getElementById('approvals-btn');
    if (btn) btn.addEventListener('click', () => open());
    document.addEventListener('keydown', (e) => {
      if (e.shiftKey && (e.key === 'A' || e.key === 'a') && !/input|textarea/i.test(document.activeElement?.tagName || '')) {
        open();
      }
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
