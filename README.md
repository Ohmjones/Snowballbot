
# Snowballin Grid Bot

A high-frequency DCA grid trading bot for Kraken, with Coingecko-based indicators, built for continuous deployment on a lightweight VPS (e.g., t2.medium). It auto-manages capital using volatility, trend, and RSI filters with full restart and recovery support.

---

## 🚀 Features

- ATR-scaled laddered buy orders (DCA grid)
- Dynamic RSI gating with volume-aware thresholds
- VWAP & order book skew bias filters
- Live trailing take-profit & ATR crash stops
- Full position recovery using Kraken trade history
- Profit tracking & auto-withdrawals per asset
- Configurable per-asset grid levels & TTL ranges
- Optional Discord webhook notifications
- Batched Coingecko price fetches to respect rate limits
- Fully restart-safe with disk-backed `state.json`

---

## 🧱 Prerequisites

- **VPS:** t2.medium (2 vCPU / 4GB RAM) or equivalent
- **OS:** Ubuntu 20.04+ or Debian-based distro
- **Go:** 1.20+
- **Git**
- **Kraken API Key/Secret** with trade & withdraw perms
- **No Coingecko key required** (free-tier safe)

---

## 🔧 Installation

```bash
# Install Go and Git
sudo apt update && sudo apt install -y golang-go git

# Clone your repo
git clone https://github.com/your-org/snowballin.git
cd snowballin

# Build the binary
go build -o snowballbot main.go
```

---

## ⚙️ Configuration

Create `config.json`:

```json
{
  "kraken_api_key": "YOUR_KRAKEN_API_KEY",
  "kraken_api_secret": "YOUR_KRAKEN_API_SECRET",
  "withdraw_keys": {
    "XBT": "YOUR_KRAKEN_WITHDRAW_KEY",
    "ETH": "YOUR_KRAKEN_WITHDRAW_KEY",
    "SOL": "YOUR_KRAKEN_WITHDRAW_KEY"
  },
  "initial_usd": 1000.0,
  "assets": ["XBT", "ETH", "SOL"],
  "grid_levels": 3,
  "stop_loss_pct": 0.15,
  "withdraw_usd": 500.0,
  "withdraw_reserve": 0.0,
  "rsi_gate": 50.0,
  "rsi_slack": 10.0,
  "base_ma_lookback": 50,
  "atr_lookback": 14,
  "min_ttl_sec": 30,
  "max_ttl_sec": 1200,
  "discord_webhook": "YOUR_DISCORD_WEBHOOK"
}
```

---

## 🧪 Running the Bot

```bash
# Start the bot
./snowballbot > snowballbot.out 2> snowballbot.err &
```

- `snowballbot.out` — stdout logs
- `snowballbot.err` — execution & error logs

Or use `screen`:
```bash
screen -S snowbot
./snowballbot
```
Detach: Ctrl+A then D, Reattach: `screen -r snowbot`

---

## 🔁 Restart & Recovery

- Rehydrates held positions using Kraken trade history (last 24h)
- Ignores tiny test trades & stale fills
- Uses mid-price fallback if history unavailable
- Persisted state: `AvgEntry`, `PositionVol`, `PriceHistory`, `ProfitCumulative`

---

## 🛠 Optional: systemd Setup

```ini
# /etc/systemd/system/snowballbot.service
[Unit]
Description=Snowballin Grid Bot
After=network.target

[Service]
WorkingDirectory=/home/ubuntu/snowballin
ExecStart=/home/ubuntu/snowballin/snowballbot
Restart=always
User=ubuntu

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable snowballbot
sudo systemctl start snowballbot
sudo journalctl -u snowballbot -f
```

---

## 🧠 Tips

- Set a `custom nonce window` on Kraken to prevent `EAPI:Invalid nonce`
- Filter trades by 1-day window to ensure true cost basis
- Adjust RSI gates, slack, and grid width to match asset volatility
- Batch price updates via Coingecko every 30s–60s to avoid rate limits

---

## 🧾 Example Output

```text
[MAIN] loaded config: assets=[XBT ETH SOL]  gridLevels=3
[RECOVER] Detected 0.20568 ETH — avg entry from trades = 1806.72
[SELL] SOL trailing TP hit — sold 0.26401 @ 149.20 for +$35.11
```

---

### ✍️ Author

Built by **[Shane Jones](https://github.com/Ohmjones)** — a security consultant and market automation enthusiast.  
This project was designed to snowball crypto profits through smart grid logic and lightweight VPS deployment.  
Development assistance provided by AI (ChatGPT) — all logic, flow, and architecture hand-validated. 

---


---

## 💸 Donations

If Snowballin has helped you automate and grow your trading strategy, consider supporting future development:

**BTC Address:** `bc1qnu8pxqp0mf8794fyn6cnxcxllceaywysde3uu2`

Your donation helps keep this project alive and evolving. Thank you!

---

## 🛡 License

Provided for research & educational use. No warranty. Use at your own risk.
