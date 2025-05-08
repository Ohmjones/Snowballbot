const rangeOptions = ['7d', '30d', 'lifetime'];
let currentRange = '7d';
let charts = {};

function setRange(r) {
    currentRange = r;
    loadCharts();
}

function loadCharts() {
    fetch('/api/config')
        .then(r => r.json())
        .then(cfg => {
            const container = document.getElementById('charts');
            container.innerHTML = ''; // Clear
            charts = {};
            cfg.assets.forEach(asset => createChartForAsset(container, asset));
        });
}

function createChartForAsset(container, asset) {
    const box = document.createElement('div');
    box.className = 'chart-box';
    box.innerHTML = `
    <h3 class="ui dividing header">${asset}</h3>
    <canvas id="chart-${asset}" width="800" height="300"></canvas>
  `;
    container.appendChild(box);

    fetch(`/api/metrics?asset=${asset}&range=${currentRange}`)
        .then(r => r.json())
        .then(data => {
            const ctx = document.getElementById(`chart-${asset}`).getContext('2d');
            const timestamps = data.map(p => new Date(p.timestamp * 1000).toLocaleString());
            charts[asset] = new Chart(ctx, {
                type: 'line',
                data: {
                    labels: timestamps,
                    datasets: [
                        {
                            label: 'Sharpe Ratio',
                            data: data.map(p => p.sharpe),
                            borderColor: 'blue',
                            yAxisID: 'y',
                        },
                        {
                            label: 'Return ($)',
                            data: data.map(p => p.return),
                            borderColor: 'green',
                            yAxisID: 'y1',
                        },
                        {
                            label: 'PnL ($)',
                            data: data.map(p => p.pnl),
                            borderColor: 'orange',
                            yAxisID: 'y1',
                        },
                    ]
                },
                options: {
                    responsive: true,
                    scales: {
                        y: { type: 'linear', position: 'left' },
                        y1: { type: 'linear', position: 'right', grid: { drawOnChartArea: false } }
                    }
                }
            });
        });
}

setInterval(loadCharts, 60000); // auto-refresh every 60s
loadCharts(); // initial