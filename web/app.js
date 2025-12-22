const REFRESH_INTERVAL = 30000; // 30 seconds

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

function updateSummary(summary) {
    if (!summary) return;
    document.getElementById('balance').textContent = formatCurrency(summary.balance);
    const pnlEl = document.getElementById('pnl');
    pnlEl.textContent = formatCurrency(summary.pnl);
    pnlEl.className = `value ${summary.pnl >= 0 ? 'text-green' : 'text-red'}`;
}

function updatePositions(positions) {
    const tbody = document.getElementById('positions-body');
    if (!positions || positions.length === 0) {
        tbody.innerHTML = '<tr><td colspan="7" style="text-align:center; color: var(--text-secondary)">No positions</td></tr>';
        document.getElementById('active-positions-count').textContent = '0';
        return;
    }

    document.getElementById('active-positions-count').textContent = positions.length;

    tbody.innerHTML = positions.map(p => `
        <tr>
            <td>${p.tradingsymbol}</td>
            <td><span class="status-indicator" style="background: rgba(255,255,255,0.1); color: #fff;">${p.product}</span></td>
            <td class="${p.quantity > 0 ? 'text-green' : 'text-red'}">${p.quantity > 0 ? 'LONG' : 'SHORT'}</td>
            <td>${Math.abs(p.quantity)}</td>
            <td>${p.average_price}</td>
            <td>${p.last_price}</td>
            <td class="${p.pnl >= 0 ? 'text-green' : 'text-red'}">${formatCurrency(p.pnl)}</td>
        </tr>
    `).join('');
}

function updateOrders(orders) {
    const tbody = document.getElementById('orders-body');
    if (!orders || orders.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" style="text-align:center; color: var(--text-secondary)">No open orders</td></tr>';
        return;
    }

    tbody.innerHTML = orders.map(o => `
        <tr>
            <td style="font-family: monospace; font-size: 0.8rem; color: var(--text-secondary);">${o.id}</td>
            <td>${o.symbol}</td>
            <td>${o.type}</td>
            <td class="${o.side === 'BUY' ? 'text-green' : 'text-red'}">${o.side}</td>
            <td>${o.quantity}</td>
            <td><span class="status-indicator" style="border: 1px solid var(--accent-color); color: var(--accent-color); background: transparent;">${o.status}</span></td>
        </tr>
    `).join('');
}

async function refreshCycle() {
    const now = new Date();
    document.getElementById('last-update-time').textContent = now.toLocaleTimeString();

    // Fetch in parallel
    const [summary, positions, orders] = await Promise.all([
        fetchData('summary'),
        fetchData('positions'),
        fetchData('orders')
    ]);

    // Update UI
    updateSummary(summary);
    updatePositions(positions);
    updateOrders(orders);
}

// Start polling
refreshCycle(); // Initial run
setInterval(refreshCycle, REFRESH_INTERVAL);
