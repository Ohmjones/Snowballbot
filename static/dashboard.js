function createChartCard(container, asset) {
    const block = document.createElement('div');
    block.className = 'sixteen wide column chart-block';
    block.innerHTML = `
    <h3>${asset} (${currentRange})</h3>
    <canvas id="chart-${asset}"></canvas>
  `;
    container.appendChild(block);

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
                    maintainAspectRatio: false,
                    scales: {
                        y: { type: 'linear', position: 'left' },
                        y1: { type: 'linear', position: 'right', grid: { drawOnChartArea: false } }
                    }
                }
            });
        });
}
