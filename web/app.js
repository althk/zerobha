const REFRESH_INTERVAL = 15000; // 15 seconds, matching the tracker's reconcile interval

let rangeDays = 30; // 0 = all time
let equityChart = null;
let intradayChart = null;

async function fetchData(endpoint) {
    try {
        const response = await fetch(`/api/${endpoint}`);
        if (!response.ok) return null;
        return await response.json();
    } catch (e) {
        console.error(`Error fetching ${endpoint}:`, e);
        return null;
    }
}

function formatCurrency(amount) {
    return new Intl.NumberFormat('en-IN', {
        style: 'currency',
        currency: 'INR'
    }).format(amount);
}

function pnlClass(v) {
    return v >= 0 ? 'text-green' : 'text-red';
}

function rangeLabel() {
    if (rangeDays === 0) return 'all time';
    if (rangeDays === 1) return 'today';
    return `last ${rangeDays}d`;
}

function formatDuration(ms) {
    const mins = Math.round(ms / 60000);
    if (mins < 60) return `${mins}m`;
    const h = Math.floor(mins / 60);
    return `${h}h ${mins % 60}m`;
}

// --- Summary / Positions / Orders (live snapshot) ---

function updateSummary(summary) {
    if (!summary) return;
    document.getElementById('balance').textContent = formatCurrency(summary.balance);
    const pnlEl = document.getElementById('pnl');
    pnlEl.textContent = formatCurrency(summary.pnl);
    pnlEl.className = `value ${pnlClass(summary.pnl)}`;

    document.getElementById('pnl-breakdown').innerHTML = `
        <span class="${pnlClass(summary.realized_pnl)}">R: ${formatCurrency(summary.realized_pnl)}</span>
        <span class="sep">|</span>
        <span class="${pnlClass(summary.unrealized_pnl)}">U: ${formatCurrency(summary.unrealized_pnl)}</span>
    `;

    const badge = document.getElementById('status-badge');
    if (badge) {
        if (summary.paper_mode) {
            badge.textContent = 'PAPER TRADING';
            badge.className = 'status-indicator paper';
        } else {
            badge.textContent = 'LIVE';
            badge.className = 'status-indicator online';
        }
    }
}

function updatePositions(positions) {
    const tbody = document.getElementById('positions-body');
    if (!positions || positions.length === 0) {
        tbody.innerHTML = '<tr><td colspan="9" class="empty-row">No positions</td></tr>';
        document.getElementById('active-positions-count').textContent = '0';
        return;
    }

    const openCount = positions.filter(p => p.net_quantity !== 0).length;
    document.getElementById('active-positions-count').textContent = openCount;

    tbody.innerHTML = positions.map(p => {
        const isOpen = p.net_quantity !== 0;
        const statusBadge = isOpen ? '<span class="badge badge-open">OPEN</span>' : '<span class="badge badge-closed">CLOSED</span>';

        return `
        <tr>
            <td>
                ${p.tradingsymbol}
                ${p.strategy ? `<br><small class="text-muted">${p.strategy}</small>` : ''}
            </td>
            <td><span class="pill">${p.product}</span></td>
            <td>${statusBadge}</td>
            <td>${p.net_quantity}</td>
            <td>${formatCurrency(p.average_price)}</td>
            <td>${formatCurrency(p.last_price)}</td>
            <td class="${pnlClass(p.pnl)}" style="font-weight: 600;">${formatCurrency(p.pnl)}</td>
            <td class="mono">${p.index_stop != null ? `${fmtNum(p.index_stop, 2)} <small class="text-muted">${p.index_side || ''}</small>` : '<span class="text-muted">--</span>'}</td>
            <td class="mono">${p.index_last != null ? fmtNum(p.index_last, 2) : '<span class="text-muted">--</span>'}</td>
        </tr>
    `}).join('');
}

function fmtNum(v, dp) {
    const n = parseFloat(v);
    if (!isFinite(n)) return '--';
    return n.toLocaleString('en-IN', { minimumFractionDigits: dp, maximumFractionDigits: dp });
}

function fmtSigned(v, dp) {
    const n = parseFloat(v);
    if (!isFinite(n)) return '--';
    return (n > 0 ? '+' : '') + n.toFixed(dp);
}

function updateOrders(orders) {
    const tbody = document.getElementById('orders-body');
    if (!orders || orders.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" class="empty-row">No open orders</td></tr>';
        return;
    }

    tbody.innerHTML = orders.map(o => `
        <tr>
            <td class="mono">${o.id}</td>
            <td>${o.symbol}</td>
            <td>${o.type}</td>
            <td class="${o.side === 'BUY' ? 'text-green' : 'text-red'}">${o.side}</td>
            <td>${o.quantity}</td>
            <td><span class="pill pill-accent">${o.status}</span></td>
        </tr>
    `).join('');
}

// --- Performance (historical, from DB) ---

function updatePerformanceMetrics(overall) {
    document.querySelectorAll('.range-label').forEach(el => el.textContent = `(${rangeLabel()})`);

    const netEl = document.getElementById('net-pnl');
    netEl.textContent = formatCurrency(overall.net_pnl);
    netEl.className = `value ${pnlClass(overall.net_pnl)}`;
    document.getElementById('trade-count').textContent = `${overall.trades} closed trades`;

    document.getElementById('win-rate').textContent = overall.trades > 0 ? `${overall.win_rate.toFixed(1)}%` : '--';
    document.getElementById('profit-factor').textContent =
        overall.trades > 0 ? `Profit Factor: ${overall.profit_factor.toFixed(2)}` : '';

    const ddEl = document.getElementById('max-drawdown');
    ddEl.textContent = formatCurrency(-overall.max_drawdown);
    ddEl.className = `value ${overall.max_drawdown > 0 ? 'text-red' : ''}`;
    document.getElementById('sharpe').textContent =
        overall.trades >= 2 ? `Sharpe (per-trade): ${overall.sharpe.toFixed(2)}` : '';
}

function updateStrategiesTable(strategies) {
    const tbody = document.getElementById('strategies-body');
    if (!strategies || strategies.length === 0) {
        tbody.innerHTML = '<tr><td colspan="9" class="empty-row">No closed trades yet</td></tr>';
        return;
    }

    strategies.sort((a, b) => b.net_pnl - a.net_pnl);

    tbody.innerHTML = strategies.map(s => `
        <tr>
            <td style="font-weight: 600;">${s.strategy}</td>
            <td>${s.trades}</td>
            <td>${s.win_rate.toFixed(1)}%</td>
            <td class="${pnlClass(s.net_pnl)}" style="font-weight: 600;">${formatCurrency(s.net_pnl)}</td>
            <td>${s.profit_factor.toFixed(2)}</td>
            <td class="text-green">${formatCurrency(s.avg_win)}</td>
            <td class="text-red">${formatCurrency(-s.avg_loss)}</td>
            <td>${s.sharpe.toFixed(2)}</td>
            <td class="text-red">${formatCurrency(-s.max_drawdown)}</td>
        </tr>
    `).join('');
}

function updateTradeHistory(trades) {
    const tbody = document.getElementById('trade-history-body');
    if (!trades || trades.length === 0) {
        tbody.innerHTML = '<tr><td colspan="13" class="empty-row">No closed trades yet</td></tr>';
        return;
    }

    tbody.innerHTML = trades.map(t => {
        const exit = new Date(t.exit_time);
        const entry = new Date(t.entry_time);
        const pnl = parseFloat(t.pnl);
        const sideClass = t.side === 'LONG' ? 'badge-open' : 'badge-short';
        return `
        <tr>
            <td class="mono">${exit.toLocaleString('en-IN', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' })}</td>
            <td>${t.symbol}${t.underlying ? `<br><small class="text-muted">${t.underlying} · ${t.entry_reason || ''}</small>` : ''}</td>
            <td><span class="badge ${sideClass}">${t.side}</span></td>
            <td>${t.quantity}</td>
            <td>${formatCurrency(t.entry_price)}</td>
            <td>${formatCurrency(t.exit_price)}</td>
            <td class="text-muted">${formatDuration(exit - entry)}</td>
            <td class="text-muted">${t.has_dte ? t.dte : '--'}</td>
            <td><span class="pill" title="${t.exit_detail || ''}">${t.exit_reason}</span></td>
            <td class="${t.has_index ? pnlClass(t.index_bps) : 'text-muted'} mono">${t.has_index ? fmtSigned(t.index_bps, 1) : '--'}</td>
            <td class="${pnlClass(t.net_bps)} mono">${fmtSigned(t.net_bps, 0)}</td>
            <td class="text-muted mono">${fmtNum(t.costs, 0)}</td>
            <td class="${pnlClass(pnl)}" style="font-weight: 600;">${formatCurrency(pnl)}</td>
        </tr>
    `}).join('');
}

// --- Strategy monitor ---

function statsRow(g, label) {
    const tClass = Math.abs(g.t) >= 2 ? 't-strong' : '';
    return `
        <tr>
            <td>${label ?? g.group}</td>
            <td class="num">${g.n}</td>
            <td class="num ${pnlClass(g.net_bps)}">${fmtSigned(g.net_bps, 1)}</td>
            <td class="num ${pnlClass(g.gross_bps)}">${fmtSigned(g.gross_bps, 1)}</td>
            <td class="num ${g.index_n ? pnlClass(g.index_bps) : 'text-muted'}">${g.index_n ? fmtSigned(g.index_bps, 2) : '--'}</td>
            <td class="num ${tClass}">${g.n > 1 ? g.t.toFixed(2) : '--'}</td>
            <td class="num">${g.n ? g.win_rate.toFixed(1) : '--'}</td>
            <td class="num">${g.n ? g.profit_factor.toFixed(2) : '--'}</td>
            <td class="num">${g.n > 1 ? g.sharpe.toFixed(3) : '--'}</td>
            <td class="num text-green">${fmtSigned(g.avg_win_bps, 0)}</td>
            <td class="num text-red">${fmtSigned(g.avg_loss_bps, 0)}</td>
            <td class="num ${pnlClass(g.net_rupees)}">${fmtNum(g.net_rupees, 0)}</td>
            <td class="num text-muted">${fmtNum(g.costs_rupees, 0)}</td>
            <td class="num text-red">${fmtNum(g.max_dd_rupees, 0)}</td>
            <td class="num text-muted">${g.n ? Math.round(g.median_hold_min) + 'm' : '--'}</td>
        </tr>`;
}

function groupBlock(title, rows) {
    if (!rows || rows.length === 0) return '';
    return `<tr class="group-head"><td colspan="15">${title}</td></tr>` + rows.map(g => statsRow(g)).join('');
}

function updateMonitor(m) {
    if (!m) return;
    document.getElementById('monitor-strategy').textContent = m.strategy || '';

    // Funnel: every signal the strategy formed, and what became of it.
    const f = m.funnel || { total: 0, placed: 0, detail: [] };
    const chips = [`<span class="chip placed">signals <b>${f.total}</b> → placed <b>${f.placed}</b></span>`];
    (f.detail || []).forEach(d => {
        const warn = d.outcome === 'declined' || d.outcome === 'order-failed' || d.outcome === 'broker-error' || d.outcome === 'no-capital';
        chips.push(`<span class="chip ${warn ? 'warn' : ''}" title="${d.reason || ''}">${d.outcome}${d.reason ? ': ' + d.reason : ''} <b>${d.count}</b></span>`);
    });
    document.getElementById('funnel').innerHTML = chips.join('');

    const body = document.getElementById('monitor-body');
    if (!m.overall || m.overall.n === 0) {
        body.innerHTML = '<tr><td colspan="15" class="empty-row">No closed trades in range</td></tr>';
    } else {
        body.innerHTML = statsRow(m.overall, 'ALL')
            + groupBlock('By underlying', m.by_underlying)
            + groupBlock('By view', m.by_side)
            + groupBlock('By exit', m.by_exit)
            + groupBlock('By days to expiry', m.by_dte)
            + groupBlock('By week', m.by_week);
    }

    const ref = document.getElementById('reference-body');
    if (!m.reference || m.reference.length === 0) {
        ref.innerHTML = '<tr><td colspan="7" class="empty-row">No recorded backtest for this strategy</td></tr>';
    } else {
        ref.innerHTML = m.reference.map(r => `
            <tr class="reference" title="${r.note || ''}">
                <td>${r.label}</td>
                <td class="num">${r.n || '--'}</td>
                <td class="num ${r.net_bps ? pnlClass(r.net_bps) : ''}">${r.net_bps ? fmtSigned(r.net_bps, 1) : '--'}</td>
                <td class="num ${r.index_bps ? pnlClass(r.index_bps) : ''}">${r.index_bps ? fmtSigned(r.index_bps, 2) : '--'}</td>
                <td class="num">${r.t ? r.t.toFixed(2) : '--'}</td>
                <td class="num">${r.win_rate ? r.win_rate.toFixed(1) : '--'}</td>
                <td class="num">${r.profit_factor ? r.profit_factor.toFixed(2) : '--'}</td>
            </tr>`).join('');
    }

    const legs = document.getElementById('legs-body');
    if (!m.open_legs || m.open_legs.length === 0) {
        legs.innerHTML = '<tr><td colspan="7" class="empty-row">No open option legs</td></tr>';
    } else {
        legs.innerHTML = m.open_legs.map(l => `
            <tr>
                <td>${l.symbol}<br><small class="text-muted">${l.underlying}</small></td>
                <td><span class="badge ${l.side === 'LONG' ? 'badge-open' : 'badge-short'}">${l.side}</span></td>
                <td class="num mono">${fmtNum(l.index_entry, 2)}</td>
                <td class="num mono">${fmtNum(l.index_stop, 2)}</td>
                <td class="num mono">${l.index_last != null ? fmtNum(l.index_last, 2) : '--'}</td>
                <td class="num ${l.index_move_bps != null ? pnlClass(l.index_move_bps) : ''}">${l.index_move_bps != null ? fmtSigned(l.index_move_bps, 1) : '--'}</td>
                <td class="num">${l.room_to_stop_bps != null ? fmtSigned(l.room_to_stop_bps, 1) : '--'}</td>
            </tr>`).join('');
    }
}

// --- Charts ---

const chartDefaults = {
    responsive: true,
    maintainAspectRatio: false,
    plugins: {
        legend: { labels: { color: '#94a3b8', boxWidth: 12 } }
    },
    scales: {
        x: {
            ticks: { color: '#94a3b8', maxTicksLimit: 10 },
            grid: { color: 'rgba(255,255,255,0.05)' }
        },
        y: {
            ticks: { color: '#94a3b8' },
            grid: { color: 'rgba(255,255,255,0.08)' }
        }
    }
};

function updateEquityChart(daily) {
    const labels = daily.map(d => d.date);
    const dailyPnl = daily.map(d => d.pnl);
    let cum = 0;
    const cumPnl = dailyPnl.map(v => (cum += v));
    const barColors = dailyPnl.map(v => v >= 0 ? 'rgba(16, 185, 129, 0.6)' : 'rgba(239, 68, 68, 0.6)');

    if (!equityChart) {
        equityChart = new Chart(document.getElementById('equity-chart'), {
            data: {
                labels,
                datasets: [
                    {
                        type: 'line',
                        label: 'Cumulative PnL',
                        data: cumPnl,
                        borderColor: '#3b82f6',
                        backgroundColor: 'rgba(59, 130, 246, 0.15)',
                        fill: true,
                        tension: 0.25,
                        pointRadius: 2,
                        yAxisID: 'y'
                    },
                    {
                        type: 'bar',
                        label: 'Daily PnL',
                        data: dailyPnl,
                        backgroundColor: barColors,
                        yAxisID: 'y1'
                    }
                ]
            },
            options: {
                ...chartDefaults,
                scales: {
                    ...chartDefaults.scales,
                    y1: {
                        position: 'right',
                        ticks: { color: '#94a3b8' },
                        grid: { drawOnChartArea: false }
                    }
                }
            }
        });
        return;
    }

    equityChart.data.labels = labels;
    equityChart.data.datasets[0].data = cumPnl;
    equityChart.data.datasets[1].data = dailyPnl;
    equityChart.data.datasets[1].backgroundColor = barColors;
    equityChart.update('none');
}

function updateIntradayChart(points) {
    points = points || [];
    const labels = points.map(p =>
        new Date(p.timestamp).toLocaleTimeString('en-IN', { hour: '2-digit', minute: '2-digit' }));
    const totalPnl = points.map(p => p.realized_pnl + p.unrealized_pnl);

    if (!intradayChart) {
        intradayChart = new Chart(document.getElementById('intraday-chart'), {
            type: 'line',
            data: {
                labels,
                datasets: [{
                    label: 'Total PnL',
                    data: totalPnl,
                    borderColor: '#10b981',
                    backgroundColor: 'rgba(16, 185, 129, 0.15)',
                    fill: true,
                    tension: 0.25,
                    pointRadius: 0
                }]
            },
            options: chartDefaults
        });
        return;
    }

    intradayChart.data.labels = labels;
    intradayChart.data.datasets[0].data = totalPnl;
    intradayChart.update('none');
}

// --- Refresh loop ---

async function refreshCycle() {
    const now = new Date();
    document.getElementById('last-update-time').textContent = now.toLocaleTimeString();

    const [summary, positions, orders, performance, intraday, monitor] = await Promise.all([
        fetchData('summary'),
        fetchData('positions'),
        fetchData('orders'),
        fetchData(`performance?days=${rangeDays}`),
        fetchData('intraday'),
        fetchData(`strategy?days=${rangeDays}`)
    ]);

    updateSummary(summary);
    updatePositions(positions);
    updateOrders(orders);
    updateIntradayChart(intraday);

    if (performance) {
        updatePerformanceMetrics(performance.overall);
        updateStrategiesTable(performance.strategies);
        updateEquityChart(performance.daily || []);
    }
    if (monitor) {
        updateMonitor(monitor);
        updateTradeHistory(monitor.trades);
    }
}

// Range selector
document.getElementById('range-selector').addEventListener('click', (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;
    rangeDays = parseInt(btn.dataset.days, 10);
    document.querySelectorAll('#range-selector button').forEach(b => b.classList.toggle('active', b === btn));
    refreshCycle();
});

// Start polling
refreshCycle(); // Initial run
setInterval(refreshCycle, REFRESH_INTERVAL);
