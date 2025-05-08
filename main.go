package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── 1) single HTTP client with timeout ───────────────────────────────────────
var httpClient = &http.Client{Timeout: 5 * time.Second}

// krakenPair returns the canonical Kraken pair name for a base asset.
// It looks up the pairMap built by RefreshAssetPairs.
func (c *KrakenClient) krakenPair(asset string) (string, error) {
	if c.pairMap == nil {
		return "", fmt.Errorf("pairMap not initialized")
	}
	p, ok := c.pairMap[asset]
	if !ok {
		return "", fmt.Errorf("no Kraken pair for asset %q", asset)
	}
	return p, nil
}

// Config holds credentials & strategy parameters
type Config struct {
	KrakenAPIKey    string            `json:"kraken_api_key"`
	KrakenAPISecret string            `json:"kraken_api_secret"`
	WithdrawKeys    map[string]string `json:"withdraw_keys"` // asset -> key name
	InitialUSD      float64           `json:"initial_usd"`
	Assets          []string          `json:"assets"`
	GridLevels      int               `json:"grid_levels"`
	StopLossPct     float64           `json:"stop_loss_pct"`
	WithdrawUSD     float64           `json:"withdraw_usd"`
	WithdrawReserve float64           `json:"withdraw_reserve"`
	RSIGate         float64           `json:"rsi_gate"`
	RSISlack        float64           `json:"rsi_slack"`        // how much to loosen RSI when vol is low
	BaseMALookback  int               `json:"base_ma_lookback"` // bars for MA and volume window
	ATRLookback     int               `json:"atr_lookback"`
	MinTTLsec       int               `json:"min_ttl_sec"` // minimum stale‐order TTL in seconds
	MaxTTLsec       int               `json:"max_ttl_sec"` // maximum stale‐order TTL in seconds
	DiscordWebhook  string            `json:"discord_webhook"`
}

var (
	cfg            Config
	kraken         *KrakenClient
	cg             *CoingeckoClient
	notify         *DiscordNotifier
	profits        = make(map[string]float64)
	mtx            sync.Mutex
	pricePrecision = map[string]int{
		"XBT": 4, // e.g. BTC/USD minTickSize = 0.0001
	}
	volumePrecision = map[string]int{
		"XBT": 6, // e.g. BTC lot size = 0.000001
		"ETH": 6,
		"SOL": 3,
	}
	// loaded from / saved to state.json
	state        *State
	openOrders   []string
	avgEntry     map[string]float64
	positionVol  map[string]float64
	profitCum    map[string]float64
	cycleState   map[string]string
	MakerFee     float64 = 0.0025 // 0.25%
	TakerFee     float64 = 0.0040 // 0.40%
	MinNetProfit float64 = 0.01   // 1% net after fees

	StepMultiplier float64 = 2.5 // widen your entry step, e.g. 2× ATR
	TpMultiplier   float64 = 2.0 // tighten or widen TP (but >1)
	TrailRatio     float64 = 1.0 // trail at same distance as TP
	// fill-rate stats for VWAP-tuned worst leg
	stats struct {
		mu          sync.Mutex
		totalCycles int
		vwapCycles  int
		vwapFills   int
	}
	rsiHistory = make(map[string][]float64)
	historyLen = 100 // Keep the last 100 RSI values per asset
)

// global price cache, updated in batch
var priceCache = struct {
	sync.RWMutex
	data map[string]float64
}{data: make(map[string]float64)}

// percentile returns the p-th percentile of xs (0 < p < 100)
func percentile(xs []float64, p int) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	dup := append([]float64(nil), xs...)
	sort.Float64s(dup)
	idx := (p * len(dup)) / 100
	if idx >= len(dup) {
		idx = len(dup) - 1
	}
	return dup[idx]
}

func recoverOpenPositions(assets []string) {
	log.Println("[BOOT] recovering open positions from Kraken…")
	for _, asset := range assets {
		bal, err := kraken.GetBalance(asset)
		if err != nil {
			log.Printf("[RECOVER] %s balance check failed: %v", asset, err)
			continue
		}
		if bal < 0.0001 {
			log.Printf("[RECOVER] %s balance is zero or dust — skip", asset)
			continue
		}

		price, err := GetAvgEntryFromTrades(asset)
		if err != nil {
			log.Printf("[RECOVER] %s avg entry from trades failed: %v — using fallback price", asset, err)
			price, err = kraken.GetTickerMid(asset)
			if err != nil {
				log.Printf("[RECOVER] %s price fetch failed: %v", asset, err)
				continue
			}
		}

		if err != nil {
			log.Printf("[RECOVER] %s price fetch failed: %v", asset, err)
			continue
		}

		log.Printf("[RECOVER] Detected %.5f %s — assuming entry @ %.2f", bal, asset, price)

		stateMu.Lock()
		if state.AvgEntry == nil {
			state.AvgEntry = make(map[string]float64)
		}
		if state.PositionVol == nil {
			state.PositionVol = make(map[string]float64)
		}
		state.PositionVol[asset] = bal
		state.AvgEntry[asset] = price
		stateMu.Unlock()
	}

	if err := saveStateUnlocked(state); err != nil {
		log.Printf("warning: failed to save recovered positions: %v", err)
	}
}

// asset → coingecko ID (drop into main.go just after imports)
func cgID(asset string) string {
	switch asset {
	case "XBT":
		return "bitcoin"
	case "ETH":
		return "ethereum"
	case "SOL":
		return "solana"
	default:
		return strings.ToLower(asset)
	}
}

// cgPrice now reads from the in-memory batch cache
func cgPrice(asset string) (float64, error) {
	priceCache.RLock()
	p, ok := priceCache.data[asset]
	priceCache.RUnlock()
	if ok && p > 0 {
		return p, nil
	}
	return 0, fmt.Errorf("price for %s not in cache", asset)
}

// cgATR wraps cg.GetATRAndRSI with Coingecko ID mapping
func cgATR(asset string, period int) (float64, float64, error) {
	return cg.GetATRAndRSI(cgID(asset), period)
}

// retry helper with exponential backoff
func retry(attempts int, sleep time.Duration, fn func() error) error {
	if err := fn(); err != nil {
		for i := 1; i < attempts; i++ {
			time.Sleep(sleep)
			if err = fn(); err == nil {
				return nil
			}
			sleep *= 2
		}
		return err
	}
	return nil
}

// fetchPairDecimals calls Kraken’s AssetPairs endpoint and returns the
// allowed decimal precision for prices on the given pair (e.g. "ETHUSD").
func fetchPairDecimals(pair string) (int, error) {
	type respType struct {
		Error  []interface{} `json:"error"`
		Result map[string]struct {
			PairDecimals int `json:"pair_decimals"`
		} `json:"result"`
	}

	resp, err := http.Get("https://api.kraken.com/0/public/AssetPairs?pair=" + pair)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var data respType
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return 0, err
	}
	if len(data.Error) > 0 {
		return 0, fmt.Errorf("Kraken error: %v", data.Error)
	}
	// there will be exactly one entry in Result
	for _, info := range data.Result {
		return info.PairDecimals, nil
	}
	return 0, fmt.Errorf("pair %q not found", pair)
}

// saveStateUnlocked serialises `s` to state.json.
// In the original repo this skipped locking because caller already held stateMu.
func saveStateUnlocked(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile("state.json", data, 0o644)
}

func main() {
	var err error

	// 0) Load persisted state
	state, err = LoadState()
	if err != nil {
		log.Fatalf("failed to load state.json: %v", err)
	}

	// 1) Load config
	data, err := ioutil.ReadFile("config.json")
	if err != nil {
		log.Fatalf("error reading config.json: %v", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("invalid config.json: %v", err)
	}
	log.Printf("[MAIN] loaded config: assets=%v  gridLevels=%d", cfg.Assets, cfg.GridLevels)

	// 1a) Default RSI gate to 50 if unset
	if cfg.RSIGate == 0 {
		cfg.RSIGate = 50.0
	}
	if cfg.RSISlack == 0 {
		cfg.RSISlack = 10.0
	}
	if cfg.BaseMALookback == 0 {
		cfg.BaseMALookback = 50
	}
	if cfg.ATRLookback == 0 {
		cfg.ATRLookback = 14
	}
	if cfg.MinTTLsec == 0 {
		cfg.MinTTLsec = 30
	}
	if cfg.MaxTTLsec == 0 {
		cfg.MaxTTLsec = 1200 // 20 minutes
	}

	// 2) Seed in-memory vars from loaded state
	openOrders = state.OpenOrders
	log.Printf("[MAIN] reconciling %d persisted orders with Kraken…", len(openOrders))
	var reconciled []string
	for _, oid := range openOrders {
		status, err := kraken.OrderStatus(oid)
		if err != nil {
			log.Printf("warning: cannot query order %s: %v", oid, err)
			// keep it in the list so we’ll retry cancellation later
			reconciled = append(reconciled, oid)
			continue
		}
		if status == "open" {
			// still open → keep for runAsset’s stale‑order logic
			reconciled = append(reconciled, oid)
		} else {
			log.Printf("[MAIN] order %s already %s → pruning", oid, status)
		}
	}

	stateMu.Lock()
	state.OpenOrders = reconciled
	openOrders = reconciled

	// ✅ Hydrate all nil maps in state (critical for runAsset stability)
	if state.AvgEntry == nil {
		state.AvgEntry = make(map[string]float64)
	}
	if state.PositionVol == nil {
		state.PositionVol = make(map[string]float64)
	}
	if state.ProfitCumulative == nil {
		state.ProfitCumulative = make(map[string]float64)
	}
	if state.CycleState == nil {
		state.CycleState = make(map[string]string)
	}
	if state.PriceHistory == nil {
		state.PriceHistory = make(map[string][]float64)
	}
	if state.VolHistory == nil {
		state.VolHistory = make(map[string][]float64)
	}
	if state.OrderStartT == nil {
		state.OrderStartT = make(map[string]int64)
	}
	if state.AssetUSDReserve == nil {
		state.AssetUSDReserve = make(map[string]float64)
	}
	if err := saveStateUnlocked(state); err != nil {
		log.Printf("warning: failed to persist reconciled state: %v", err)
	}
	stateMu.Unlock()

	// ✅ assign globally-accessed maps
	avgEntry = state.AvgEntry
	positionVol = state.PositionVol
	profitCum = state.ProfitCumulative
	cycleState = state.CycleState

	// ✅ hydrate profits → profits map
	profits = make(map[string]float64, len(state.ProfitCumulative))
	for asset, p := range state.ProfitCumulative {
		profits[asset] = p
	}

	// 3) Init clients & notifier
	kraken = NewKrakenClient(cfg.KrakenAPIKey, cfg.KrakenAPISecret)
	// load Kraken’s asset‑pair map before any calls:
	if err := kraken.RefreshAssetPairs(); err != nil {
		log.Fatalf("cannot load Kraken AssetPairs: %v", err)
	}

	// Recover Open Positions
	recoverOpenPositions(cfg.Assets)

	// validate and populate pricePrecision from Kraken
	for _, asset := range cfg.Assets {
		pair, ok := kraken.pairMap[asset]
		if !ok || pair == "" {
			log.Fatalf("fatal: no Kraken pair mapping found for asset %s", asset)
		}
		decs, err := fetchPairDecimals(pair)
		if err != nil {
			log.Fatalf("fatal: failed to fetch decimals for pair %s: %v", pair, err)
		}
		pricePrecision[asset] = decs
		//log.Printf("Set %s price precision = %d", asset, decs)
	}

	log.Println("[BOOT] launching dashboard…") // ✅ now actually reached
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[WEB ERROR] panic in dashboard server: %v", r)
			}
		}()
		startWebServer()
	}()

	// refresh once every 24h in background
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for range t.C {
			if err := kraken.RefreshAssetPairs(); err != nil {
				log.Printf("warning: refresh AssetPairs: %v", err)
				continue
			}
			// re-populate precisions after refresh
			for _, asset := range cfg.Assets {
				pair, ok := kraken.pairMap[asset]
				if !ok {
					log.Printf("Warning: no pair mapping for asset %s, skipping precision lookup", asset)
					continue
				}
				if decs, err := fetchPairDecimals(pair); err == nil {
					pricePrecision[asset] = decs
					log.Printf("Set %s price precision = %d", asset, decs)
				} else {
					log.Printf("Warning: could not fetch precision for %s: %v", asset, err)
				}
			}

		}
	}()

	cg = NewCoingeckoClient()
	notify = NewDiscordNotifier(cfg.DiscordWebhook)
	// ——— Coingecko price cache updater ———
	// build id→asset map and list of IDs
	idToAsset := make(map[string]string, len(cfg.Assets))
	ids := make([]string, len(cfg.Assets))
	for i, a := range cfg.Assets {
		id := cgID(a)
		ids[i] = id
		idToAsset[id] = a
	}

	// every 15s refresh all prices in one batch
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			prices, err := cg.GetPrices(ids)
			if err != nil {
				log.Printf("price cache update error: %v", err)
				continue
			}
			priceCache.Lock()
			for id, p := range prices {
				asset := idToAsset[id]
				priceCache.data[asset] = p
			}
			priceCache.Unlock()
		}
	}()
	// ——————————————————————————————————————

	// —–––––– Fetch current fees on startup ––––––
	// build Kraken pair list for fee queries (e.g. “XBTZUSD”)
	pairs := make([]string, len(cfg.Assets))
	for i, a := range cfg.Assets {
		p, err := kraken.krakenPair(a)
		if err != nil {
			log.Fatalf("unknown Kraken pair for asset %q: %v", a, err)
		}
		pairs[i] = p
	}

	// —–––––– Fetch current fees on startup –––––––
	if mk, tk, err := kraken.GetTradeFees(pairs); err != nil {
		log.Printf("warning: failed to fetch trade fees: %v", err)
	} else {
		MakerFee = mk
		TakerFee = tk
		log.Printf("Using maker=%.4f taker=%.4f", mk, tk)
	}

	// —–––––– Kick off a daily fee refresh ––––––
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			<-ticker.C
			if mk, tk, err := kraken.GetTradeFees(pairs); err != nil {
				log.Printf("warning: failed to refresh trade fees: %v", err)
			} else {
				MakerFee = mk
				TakerFee = tk
				log.Printf("Refreshed maker=%.4f taker=%.4f", mk, tk)
			}
		}
	}()

	go func() {
		var shouldFetch bool
		stateMu.Lock()
		shouldFetch = state.BaselineUSD == 0
		stateMu.Unlock()

		if shouldFetch {
			log.Printf("[BASELINE] fetching initial ZUSD balance…")
			freeUSD, err := kraken.GetBalance("ZUSD")
			if err != nil {
				log.Printf("[BASELINE] error fetching ZUSD balance: %v", err)
				return
			}

			stateMu.Lock()
			state.BaselineUSD = freeUSD
			err = SaveState(state)
			stateMu.Unlock()

			if err != nil {
				log.Printf("[BASELINE] error saving state: %v", err)
			} else {
				log.Printf("[BASELINE] got ZUSD balance = %.2f", freeUSD)
			}
		}
	}()

	// 5) Setup cancellation on Ctrl+C
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; cancel() }()

	// 6) Launch asset runners + loops
	var wg sync.WaitGroup
	for _, asset := range cfg.Assets {
		log.Printf("[MAIN] launching runAsset for %s", asset)
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[PANIC] runAsset for %s crashed: %v", a, r)
				}
			}()
			runAsset(ctx, a)
		}(asset)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		withdrawLoop(ctx)
	}()
	wg.Wait()
}

// fetchHistory returns the last `bars` 5‑minute closes and volumes for `asset`.
func fetchHistory(asset string, bars int) (prices, vols []float64, err error) {
	if bars <= 0 {
		return nil, nil, fmt.Errorf("invalid bars %d", bars)
	}
	since := time.Now().Add(-time.Duration(bars*5) * time.Minute).Unix()

	// Kraken OHLC: [time,open,high,low,close,vwap,volume,count]
	rows, err := kraken.GetOHLC(asset, 5, since)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) < bars {
		return nil, nil, fmt.Errorf("only %d bars returned, need %d", len(rows), bars)
	}

	prices = make([]float64, bars)
	vols = make([]float64, bars)
	start := len(rows) - bars
	for i := 0; i < bars; i++ {
		r := rows[start+i]
		prices[i] = r[3] // close (index 3 in our 4‑col slice)
		vols[i] = r[4]   // r[4] is zero now; adjust when GetOHLC keeps vol
	}
	return
}

// runAsset: endless snowball cycles
func runAsset(ctx context.Context, asset string) {
	//log.Printf("[%s] ▶ runAsset started", asset)
	cycleDelay := 5 * time.Minute

	// declare once so the whole function sees them
	var (
		entryPrice float64
		posVol     float64
	)

	// Rehydrate any persisted start times for TTL tracking
	startT := make(map[string]time.Time)
	stateMu.Lock()
	for oid, ts := range state.OrderStartT {
		startT[oid] = time.Unix(ts, 0)
	}
	stateMu.Unlock()

	// ---------- BOOTSTRAP HISTORY ----------
	const maxVolF = 3.0
	warm := int(float64(cfg.BaseMALookback)*(1+maxVolF)) + cfg.ATRLookback

	//log.Printf("[%s] ▶ bootstrap: calling fetchHistory(asset=%q, warm=%d)", asset, asset, warm)
	prices, vols, err := fetchHistory(asset, warm)
	if err != nil {
		log.Printf("[%s] ❌ fetchHistory error: %v", asset, err)
		return
	}
	//log.Printf("[%s] ✅ fetchHistory returned %d prices, %d vols", asset, len(prices), len(vols))

	state.PriceHistory[asset] = prices
	state.VolHistory[asset] = vols
	// ---------- END BOOTSTRAP --------------

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		//log.Printf("[CYCLE][%s] starting new cycle at %s", asset, time.Now().Format(time.RFC3339))
		// 1) Fetch price from cache (fallback to Kraken on miss)
		var price float64
		priceCache.RLock()
		p, ok := priceCache.data[asset]
		priceCache.RUnlock()
		if ok && p > 0 {
			price = p
			log.Printf("[%s] ← priceCache hit: %.4f", asset, price)
		} else {
			//log.Printf("[%s] ▶ priceCache miss → calling kraken.GetTickerMid", asset)
			var err error
			if price, err = kraken.GetTickerMid(asset); err != nil {
				log.Printf("[%s] ⚠️ Kraken.GetTickerMid failed: %v", asset, err)
				time.Sleep(cycleDelay)
				continue
			}
			//log.Printf("[%s] ← Kraken.GetTickerMid returned price=%.4f", asset, price)
		}

		// 1b) Fetch ATR & RSI
		var atr, rsi float64
		//log.Printf("[%s] ▶ about to retry cgATR", asset)
		if err := retry(3, 2*time.Second, func() error {
			var e error
			atr, rsi, e = cgATR(asset, 14)
			return e
		}); err != nil {
			//log.Printf("[%s] ⚠️ cgATR failed, entering fallback: %v", asset, err)
			since := time.Now().Add(-24 * time.Hour).Unix()
			ohlc, err := kraken.GetOHLC(asset, 5, since)
			if err != nil {
				notify.Send(fmt.Sprintf("[%s] Kraken OHLC failed: %v", asset, err))
				time.Sleep(cycleDelay)
				continue
			}
			//log.Printf("[%s] ✅ got ATR=%.4f RSI=%.2f", asset, atr, rsi)

			// compute True-Range ATR and RSI
			atr = computeATRFromOHLC(ohlc, 14)
			closes := make([]float64, len(ohlc))
			for i, bar := range ohlc {
				closes[i] = bar[3]
			}
			rsi = computeRSI(closes, 14)
		}

		if atr <= 0 || math.IsNaN(atr) {
			log.Printf("[%s] ATR=%.4f – data gap, skip cycle", asset, atr)
			time.Sleep(cycleDelay)
			continue
		}

		// 1c) Dynamic RSI gate based on rolling volume factor
		// fetch last BaseMALookback+1 volumes (implement cgVolumeSeries to return []float64)
		volArr, err := cgVolumeSeries(asset, cfg.BaseMALookback+1)
		if err != nil {
			log.Printf("[%s] volume fetch error: %v", asset, err)
			volArr = make([]float64, cfg.BaseMALookback+1)
			for i := range volArr {
				volArr[i] = 1.0
			}
		}
		sumVol := 0.0
		for i := 0; i < cfg.BaseMALookback; i++ {
			sumVol += volArr[i]
		}
		avgVol := sumVol / float64(cfg.BaseMALookback)
		volFactor := volArr[cfg.BaseMALookback] / avgVol
		if volFactor < 0.3 {
			volFactor = 0.3
		} else if volFactor > 2.5 {
			volFactor = 2.5
		}

		/*dynRSIGate := cfg.RSIGate + (1.0-volFactor)*cfg.RSISlack
		// ── DEBUG: log price, ATR, RSI, volFactor & dynRSIGate ─────────────
		log.Printf("[DEBUG][%s] price=%.4f ATR=%.4f RSI=%.2f volF=%.2f dynRSI=%.2f", asset, price, atr, rsi, volFactor, dynRSIGate)
		if rsi > dynRSIGate {
			log.Printf("[%s] RSI %.1f > dynGate %.1f (volFactor %.2f) – skip", asset, rsi, dynRSIGate, volFactor)
			time.Sleep(cycleDelay)
			continue
		}*/

		// ── UPDATE RSI HISTORY ────────────────────────────────────────────────
		hist := rsiHistory[asset]
		hist = append(hist, rsi)
		if len(hist) > historyLen {
			hist = hist[len(hist)-historyLen:]
		}
		rsiHistory[asset] = hist

		// ── DYNAMIC RSI GATE via 20th percentile ──────────────────────────────
		gate := percentile(rsiHistory[asset], 20) // dynamically finds sweet spot
		log.Printf("[%s] dynamic RSI gate (20th pct) = %.2f", asset, gate)

		if rsi > gate {
			log.Printf("[%s] RSI %.2f > gate %.2f (adaptive) – skip cycle", asset, rsi, gate)
			time.Sleep(cycleDelay)
			continue
		}

		// 1d) Trend filter ────────────────────────────────────────────────────────────
		// Dynamically size the MA look‑back by market energy (volFactor):
		maLB := int(float64(cfg.BaseMALookback) * (1.0 + volFactor))
		if maLB < 2 {
			maLB = 2
		}
		lookbackBars := maLB + 10 // +10 so we can compare the slope
		since := time.Now().Add(-time.Duration(lookbackBars*5) * time.Minute).Unix()

		ohlcTrend, err := kraken.GetOHLC(asset, 5, since) // 5‑minute candles
		if err != nil {
			notify.Send(fmt.Sprintf("[%s] trend OHLC failed: %v", asset, err))
			time.Sleep(cycleDelay)
			continue
		}
		if len(ohlcTrend) < lookbackBars {
			log.Printf("[%s] only %d bars (need %d) – skip", asset, len(ohlcTrend), lookbackBars)
			time.Sleep(cycleDelay)
			continue
		}

		// extract closes
		prices := make([]float64, len(ohlcTrend))
		for i, bar := range ohlcTrend {
			prices[i] = bar[3] // close price is at index 3
		}

		// MA and slope check
		ma := movingAverage(prices[len(prices)-maLB:], maLB)
		prevMA := movingAverage(prices[len(prices)-maLB-1:len(prices)-1], maLB)
		// after (allow small dips up to 0.05% before skipping):
		slopePct := (ma - prevMA) / prevMA
		// ── DEBUG slopePct ─────────────────────────────────────────────────
		log.Printf("[SLOPE][%s] slopePct=%.5f (threshold=–0.0005)", asset, slopePct)
		if slopePct < -0.0005 { // only skip on >0.05% down‑trend
			log.Printf("[SLOPE][%s] MA drop too steep (%.4f%%) – skip", asset, slopePct*100)
			time.Sleep(cycleDelay)
			continue
		}

		// 2) Dynamic bankroll sizing & equal split
		freeUSD, err := kraken.GetBalance("ZUSD")
		if err != nil {
			notify.Send(fmt.Sprintf("[%s] balance fetch failed: %v", asset, err))
			log.Printf("[%s] ERROR fetching ZUSD balance: %v", asset, err)
			time.Sleep(cycleDelay)
			continue
		}
		usable := freeUSD - cfg.WithdrawReserve
		basePerAsset := usable / float64(len(cfg.Assets))
		stateMu.Lock()
		extraAlloc := state.AssetUSDReserve[asset]
		// consume any extra for this cycle
		state.AssetUSDReserve[asset] = 0
		_ = saveStateUnlocked(state)
		stateMu.Unlock()
		totalAlloc := basePerAsset + extraAlloc
		log.Printf("[BANKROLL][%s] freeUSD=%.2f reserve=%.2f usable=%.2f baseAlloc=%.2f extraAlloc=%.2f totalAlloc=%.2f", asset, freeUSD, cfg.WithdrawReserve, usable, basePerAsset, extraAlloc, totalAlloc)
		if totalAlloc <= 0 {
			notify.Send(fmt.Sprintf("[%s] no usable USD, skipping grid: %.2f", asset, totalAlloc))
			time.Sleep(cycleDelay)
			continue
		}
		usdAlloc := totalAlloc

		// ——— Order-book imbalance tertiary filter ———
		bids, asks, err := kraken.GetOrderBook(asset, 10)
		if err != nil {
			log.Printf("[SKEW][%s] depth fetch failed: %v", asset, err)
		} else {
			// sumVolume is defined below
			totalBid := sumVolume(bids)
			totalAsk := sumVolume(asks)
			if totalBid+totalAsk > 0 {
				skew := totalBid / (totalBid + totalAsk)
				// dynamic minimum skew: start at 70%, relax by 10% per volFactor point
				minSkew := 0.70 - 0.1*volFactor
				if minSkew < 0.50 {
					minSkew = 0.50 // never go below 50%
				}
				log.Printf("[SKEW-DEBUG][%s] totalBid=%.2f totalAsk=%.2f skew=%.2f minSkew=%.2f", asset, totalBid, totalAsk, skew, minSkew)
				if skew < minSkew {
					log.Printf("[SKEW][%s] bid/ask skew %.2f < dynamicGate %.2f – skipping grid", asset, skew, minSkew)
					time.Sleep(cycleDelay)
					continue
				}
				log.Printf("[SKEW][%s] bid/ask skew %.2f ≥ dynamicGate %.2f – ok", asset, skew, minSkew)
			}
		}

		// 3) Build DCA grid (volatility-scaled)
		// ── GRID DEBUG: we’ve passed all filters, now entering placement ──
		log.Printf("[GRID][%s] passed filters; entering grid‑placement", asset)
		since = time.Now().Add(-250 * time.Minute).Unix()
		ohlc, err := kraken.GetOHLC(asset, 5, since)
		if err != nil {
			notify.Send(fmt.Sprintf("[%s] OHLC for stddev failed: %v", asset, err))
			time.Sleep(cycleDelay)
			continue
		}
		if len(ohlc) == 0 {
			notify.Send(fmt.Sprintf("[%s] empty OHLC, skip grid", asset))
			time.Sleep(cycleDelay)
			continue
		}
		closes := make([]float64, len(ohlc))
		for i, bar := range ohlc {
			closes[i] = bar[3]
		}
		mean := 0.0
		for _, v := range closes {
			mean += v
		}
		mean /= float64(len(closes))
		var variance float64
		for _, v := range closes {
			variance += (v - mean) * (v - mean)
		}
		std50 := math.Sqrt(variance / float64(len(closes)))

		// ---- fee‐aware & amplified grid sizing ----
		feeRoundTrip := MakerFee + TakerFee     // total fee cost, e.g. 0.0065
		minGross := feeRoundTrip + MinNetProfit // minimum gross capture, e.g. 0.0165
		// ATR‐based entry step (volatility‐scaled)
		k := 0.5
		// NEW
		// NEW
		dynamicStep := atr * (1 + k*(std50/price))
		atrPct := dynamicStep / price

		// adaptive amplifier: in high‑volume regimes draw the first chalk
		// line closer so wicks actually hit.
		amp := StepMultiplier // default 2.5
		if volFactor > 1.0 {  // “stormy” market
			amp = math.Min(amp, 1.5) // cap at 1.5 × to tighten grid
		}

		// amplify both sides to chase 10–15%
		stepPct := math.Max(minGross, atrPct*amp)
		tpTrigger := stepPct * TpMultiplier
		trailingPct := tpTrigger / TrailRatio

		// compute per‑asset USD and a tiered linear weight for each leg
		perAssetUSD := usdAlloc // usdAlloc is already freeUSD/numAssets
		// build linear weights [1,2,3,…]
		weights := make([]float64, cfg.GridLevels)
		sumW := 0.0
		for i := range weights {
			weights[i] = float64(i + 1)
			sumW += weights[i]
		}
		volumes := make([]float64, cfg.GridLevels)
		targets := make([]float64, cfg.GridLevels)
		// ---- ATR + orderbook‑aware grid sizing (unchanged) --
		// base step in USD = 1×ATR
		baseStep := atr
		// leg multipliers: 0.5×ATR, 1×ATR, 1.5×ATR, …
		legMults := []float64{0.5, 1.0, 1.5}
		// fetch top bids to find visible support
		bids, _, err = kraken.GetOrderBook(asset, 10)
		if err != nil {
			log.Printf("[%s] cannot fetch bids for support: %v", asset, err)
		}
		// determine support = Nth bid price or fallback
		support := price - baseStep*legMults[0]
		if len(bids) >= len(legMults) {
			support = bids[len(legMults)-1][0]
		}

		for i := 0; i < cfg.GridLevels; i++ {
			// allocate USD by linear weight, then convert to coin volume
			legUSD := perAssetUSD * (weights[i] / sumW)
			volumes[i] = legUSD / price
			// ATR‑based raw target
			t := price - baseStep*legMults[i]
			// for leg0, don’t undercut visible support
			if i == 0 {
				targets[i] = math.Max(t, support)
			} else {
				targets[i] = t
			}
		}

		// track whether VWAP tightened the worst leg this cycle
		original0 := price * (1 - stepPct) // what leg0 would have been
		vwapApplied := false

		// VWAP Bias Integration: tighten the worst-case leg based on VWAP
		bids, _, err = kraken.GetOrderBook(asset, 10)
		if err != nil {
			// at least surface failures so you know when order-book data is missing
			log.Printf("[VWAP][%s] depth fetch failed: %v", asset, err)
		} else {
			// computeVWAP should average price×volume over those top levels
			vwap := computeVWAP(bids)
			// new: only tighten by 0.5×ATR (or whatever your legMults[0] is)
			worst := vwap - baseStep*legMults[0]
			// still take the deeper dip between ATR‑target and VWAP floor
			targets[0] = math.Min(targets[0], worst)
			if targets[0] < original0 {
				vwapApplied = true
			}

			log.Printf("[VWAP][%s] vwap=%.4f, adjusted worst leg=%.4f", asset, vwap, targets[0])
		}

		// ── DEBUG: log MA & grid definitions ────────────────────────────────
		log.Printf("[DEBUG][%s] MA_lookback=%d MA=%.4f stepPct=%.4f", asset, maLB, ma, stepPct)
		for i, t := range targets {
			log.Printf("[DEBUG][%s] leg %d: target=%.4f vol=%.6f", asset, i+1, t, volumes[i])
		}

		// 4) Place the grid ───────────────────────────────────────────────
		oids := make([]string, 0, cfg.GridLevels)
		//log.Printf("[%s] about to place grid — entering placement loop", asset)
		for i := 0; i < cfg.GridLevels; i++ {
			//log.Printf("[%s] ATTEMPT grid leg %d: target=%.4f vol=%.6f", asset, i+1, targets[i], volumes[i])
			var oid string

			// run retry and capture its error
			//log.Printf("[%s] starting retry for leg %d at %s", asset, i+1, time.Now().Format(time.RFC3339))
			err := retry(3, 2*time.Second, func() error {
				pp, _ := pricePrecision[asset]
				priceStr := fmt.Sprintf("%.*f", pp, targets[i])
				vp, _ := volumePrecision[asset]
				volStr := fmt.Sprintf("%.*f", vp, volumes[i])
				log.Printf("[%s] RAW order params leg %d: priceStr=%s volStr=%s", asset, i+1, priceStr, volStr)

				var e error
				oid, e = kraken.PlaceLimitBuy(asset, volumes[i], targets[i])
				if e != nil {
					log.Printf("[%s] INSIDE closure leg %d got error: %v", asset, i+1, e)
				} else {
					log.Printf("[%s] INSIDE closure leg %d success, oid=%s", asset, i+1, oid)
				}
				return e
			})

			// — immediately log retry outcome —
			//log.Printf("[%s] finished retry for leg %d at %s with err=%v", asset, i+1, time.Now().Format(time.RFC3339), err)
			if err != nil {
				log.Printf("[%s] grid leg %d failed: %v", asset, i+1, err)
				notify.Send(fmt.Sprintf("[%s] grid leg %d failed: %v", asset, i+1, err))
				continue
			}

			oids = append(oids, oid)
			log.Printf("[%s] Placed grid leg %d: oid=%s target=%.4f vol=%.6f", asset, i+1, oid, targets[i], volumes[i])
			notify.Send(fmt.Sprintf("[%s] placed grid leg %d: oid=%s target=%.4f vol=%.6f",
				asset, i+1, oid, targets[i], volumes[i]))
		}

		if len(oids) == 0 {
			notify.Send(fmt.Sprintf("[%s] all grid legs failed; retry in 1 minute", asset))
			time.Sleep(1 * time.Minute)
			continue
		}

		firstOID := oids[0] // ← store once; slice may shrink later

		// 5) Monitor fills + stale cancellations
		prevVol := make(map[string]float64, len(oids))
		prevNot := make(map[string]float64, len(oids))
		// collect orders that fill normally so we can prune them later
		closedOIDs := make([]string, 0, len(oids))

		now := time.Now()
		stateMu.Lock()
		for _, oid := range oids {
			startT[oid] = now
			state.OrderStartT[oid] = now.Unix()
		}
		_ = saveStateUnlocked(state)
		stateMu.Unlock()

		filledP := []float64{}
		filledV := []float64{}
		tick := time.NewTicker(5 * time.Second)
		for len(oids) > 0 {
			select {
			case <-ctx.Done():
				tick.Stop()
				return
			case <-tick.C:
			}

			// dynamic order timeout: low vol → longer TTL (up to 20 m), high vol → very short (30 s)
			factor := (atr / price) / 0.02
			if factor < 0 {
				factor = 0
			} else if factor > 1 {
				factor = 1
			}
			inv := 1 - factor
			minTO := time.Duration(cfg.MinTTLsec) * time.Second
			maxTO := time.Duration(cfg.MaxTTLsec) * time.Second

			orderTO := time.Duration(
				float64(minTO) + inv*float64(maxTO-minTO),
			)

			// build next‐round list
			next := oids[:0]
			for _, oid := range oids {
				// accumulate fills
				pCum, vCum, _ := kraken.OrderFill(asset, oid)
				dv := vCum - prevVol[oid]
				if dv > 0 {
					totNot := pCum * vCum
					dn := totNot - prevNot[oid]
					eff := dn / dv
					filledP = append(filledP, eff)
					filledV = append(filledV, dv)
					prevVol[oid] = vCum
					prevNot[oid] = totNot
				}

				status, _ := kraken.OrderStatus(oid)
				if status == "closed" {
					// normally filled: mark for pruning
					closedOIDs = append(closedOIDs, oid)
					delete(startT, oid)
					stateMu.Lock()
					delete(state.OrderStartT, oid)
					_ = saveStateUnlocked(state)
					stateMu.Unlock()
					delete(prevVol, oid)
					delete(prevNot, oid)
					continue
				}
				if time.Since(startT[oid]) > orderTO {
					log.Printf("[%s] checking age of order %s — placed at %v, age = %v, ttl = %v",
						asset, oid, startT[oid], time.Since(startT[oid]), orderTO)

					_ = kraken.CancelOrder(oid)
					notify.Send(fmt.Sprintf("[%s] canceled stale %s", asset, oid))
					delete(startT, oid)
					stateMu.Lock()
					delete(state.OrderStartT, oid)
					_ = saveStateUnlocked(state)
					stateMu.Unlock()
					delete(prevVol, oid)
					delete(prevNot, oid)
					continue
				}
				next = append(next, oid)
			}
			oids = next
		}
		tick.Stop()

		// ✅ Immediately track fill stats before any state pruning or disk writes
		didFill0 := false
		for _, oid := range closedOIDs {
			if oid == firstOID {
				didFill0 = true
				break
			}
		}

		stats.mu.Lock()
		stats.totalCycles++
		if vwapApplied {
			stats.vwapCycles++
			if didFill0 {
				stats.vwapFills++
			}
		}
		fillRate := 0.0
		if stats.vwapCycles > 0 {
			fillRate = 100 * float64(stats.vwapFills) / float64(stats.vwapCycles)
		}
		stats.mu.Unlock()

		log.Printf(
			"[STATS][%s] totalCycles=%d, vwapCycles=%d, vwapFillRate=%.2f%%",
			asset,
			stats.totalCycles,
			stats.vwapCycles,
			fillRate,
		)

		// 🔻 THEN prune closed orders safely
		if len(closedOIDs) > 0 {
			stateMu.Lock()
			for _, oid := range closedOIDs {
				state.OpenOrders = removeOrder(state.OpenOrders, oid)
			}
			if err := saveStateUnlocked(state); err != nil {
				log.Printf("warning: failed to save state pruning closed orders: %v", err)
			}
			stateMu.Unlock()
		}

		// 6) Weighted-avg entry & persist state
		var cost, vol float64
		for i := range filledP {
			cost += filledP[i] * filledV[i]
			vol += filledV[i]
		}

		if vol == 0 {
			notify.Send(fmt.Sprintf("[%s] no fills, skip", asset))
		} else {
			entryPrice = cost / vol
			posVol = vol

			stateMu.Lock()
			avgEntry[asset] = entryPrice
			state.AvgEntry[asset] = entryPrice
			positionVol[asset] = posVol
			state.PositionVol[asset] = posVol
			if err := saveStateUnlocked(state); err != nil {
				log.Printf("warning: save state: %v", err)
			}
			stateMu.Unlock()

			// 7) Trailing TP & SL
			hardStop := entryPrice * (1 - cfg.StopLossPct)
			tpStart := entryPrice * (1 + tpTrigger)
			peak, trail := price, price*(1-trailingPct)
			trailingOn := false

			qtick := time.NewTicker(1 * time.Second)
			for {
				select {
				case <-ctx.Done():
					qtick.Stop()
					return
				case <-qtick.C:
				}
				if p2, err := kraken.GetTickerMid(asset); err == nil {
					price = p2
				}
				// ── ATR crash‑stop: if price falls more than 1×ATR below entry, emergency exit
				if price <= entryPrice-atr {
					log.Printf("[%s] crash‑stop triggered: price %.4f ≤ entry−ATR %.4f", asset, price, entryPrice-atr)
					// market‑sell full position
					if _, err := kraken.PlaceMarketSell(asset, posVol); err != nil {
						notify.Send(fmt.Sprintf("[%s] emergency sell failed: %v", asset, err))
					}
					qtick.Stop()
					break
				}
				if price > peak {
					peak = price
					trail = peak * (1 - trailingPct)
				}
				if !trailingOn && price >= tpStart {
					trailingOn = true
				}
				if price <= hardStop || (trailingOn && price <= trail) {
					var tx string
					sellFn := func() error {
						var e error
						tx, e = kraken.PlaceMarketSell(asset, posVol)
						return e
					}
					if err := retry(3, 2*time.Second, sellFn); err != nil {
						notify.Send(fmt.Sprintf("[%s] exit sell failed: %v", asset, err))
					} else {
						sp, sv, _ := kraken.OrderFill(asset, tx)
						pnl := sv * (sp - entryPrice)
						notify.Send(fmt.Sprintf("[%s] exit @%.4f, P/L %.4f", asset, sp, pnl))
						recordProfit(asset, pnl)
					}
					qtick.Stop()
					break
				}
			}
		}

		// 8) mark waiting & persist
		stateMu.Lock()
		cycleState[asset] = "waiting"
		state.CycleState[asset] = cycleState[asset]
		if err := saveStateUnlocked(state); err != nil {
			log.Printf("warning: save state: %v", err)
		}
		stateMu.Unlock()

		notify.Send(fmt.Sprintf("[%s] cycle done; polling every 1 min, full recalc in %v", asset, cycleDelay))
		// hybrid wait: 1 min public‐ticker pulses, full recalc after cycleDelay
		fast := time.NewTicker(1 * time.Minute)
		slow := time.NewTimer(cycleDelay)
		for {
			select {
			case <-ctx.Done():
				fast.Stop()
				slow.Stop()
				return
			case <-fast.C:
				// 1 min poll to keep existing grid “live”
				go func() {
					if _, err := kraken.GetTickerMid(asset); err == nil {
						//log.Printf("[%s] pulse price %.4f", asset, p)
					}
				}()
			case <-slow.C:
				fast.Stop()
				// exit inner loop → top of for{} → full recalc & new grid
				goto NextCycle
			}
		}
	NextCycle:
	}
}

// movingAverage computes the simple moving average over the last p elements of arr.
// Returns 0 if there isn’t enough data.
func movingAverage(arr []float64, p int) float64 {
	if p <= 0 || len(arr) < p {
		return 0
	}
	sum := 0.0
	// average the last p values
	for i := len(arr) - p; i < len(arr); i++ {
		sum += arr[i]
	}
	return sum / float64(p)
}

// withdrawLoop auto-withdraws per-asset profits as soon as total ≥ cfg.WithdrawUSD
func withdrawLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// 1) Sum up all in-memory profits
		mtx.Lock()
		total := 0.0
		for _, v := range profits {
			total += v
		}
		mtx.Unlock()

		// 2) Only proceed once we've hit the withdraw threshold
		if total < cfg.WithdrawUSD {
			continue
		}

		// 3) Withdraw per asset
		for _, asset := range cfg.Assets {
			// respect cancellation
			select {
			case <-ctx.Done():
				return
			default:
			}

			// grab this asset's profit
			mtx.Lock()
			prof := profits[asset]
			mtx.Unlock()
			if prof <= 0 {
				continue
			}

			// fetch USD price via Coingecko
			price, err := cgPrice(asset)
			if err != nil {
				notify.Send(fmt.Sprintf("[%s] price error: %v", asset, err))
				continue
			}
			if price <= 0 {
				notify.Send(fmt.Sprintf("[%s] invalid price %.4f, skipping withdraw", asset, price))
				continue
			}

			// calculate crypto amount to withdraw, but don’t exceed free (unlocked) balance
			wanted := prof / price
			freeBal, err := kraken.GetBalance(asset)
			if err != nil {
				notify.Send(fmt.Sprintf("[%s] failed to fetch free balance: %v", asset, err))
				continue
			}
			// withdraw only up to the free balance
			amt := math.Min(wanted, freeBal)

			prec, ok := volumePrecision[asset]
			if !ok {
				prec = 6
			}
			amtStr := fmt.Sprintf("%.*f", prec, amt)

			// find the correct withdrawal key
			keyName, ok := cfg.WithdrawKeys[asset]
			if !ok {
				notify.Send(fmt.Sprintf("[%s] no withdraw key configured", asset))
				continue
			}

			// perform the withdrawal with retries
			var ref string
			if err := retry(3, 2*time.Second, func() error {
				var e error
				ref, e = kraken.Withdraw(asset, keyName, amtStr)
				return e
			}); err != nil {
				notify.Send(fmt.Sprintf("[%s] withdraw failed: %v", asset, err))
				continue
			}

			// compute the actual USD amount we withdrew
			actualUSD := amt * price
			notify.Send(fmt.Sprintf("[%s] withdrew %s (~%.2f USD) ref %s", asset, amtStr, actualUSD, ref))

			// 4) Deduct only the USD we actually withdrew and persist
			mtx.Lock()
			profits[asset] -= actualUSD
			newTotal := profits[asset]
			mtx.Unlock()

			state.ProfitCumulative[asset] = newTotal
			if err := saveStateUnlocked(state); err != nil {
				log.Printf("warning: failed to save state after withdrawal: %v", err)
			}
		}
	}
}

func recordProfit(asset string, usd float64) {
	mtx.Lock()
	defer mtx.Unlock()

	// 1) update in-memory tracker
	profits[asset] += usd

	// 2) mirror into your persistent state
	stateMu.Lock()
	state.ProfitCumulative[asset] += usd
	state.AssetUSDReserve[asset] += usd
	_ = saveStateUnlocked(state)
	stateMu.Unlock()

	// 3) write state.json so it survives a restart
	if err := saveStateUnlocked(state); err != nil {
		log.Printf("warning: failed to save profit state: %v", err)
	}
}

// removeOrder returns a new slice with the first occurrence of oid removed.
func removeOrder(oids []string, oid string) []string {
	for i, x := range oids {
		if x == oid {
			return append(oids[:i], oids[i+1:]...)
		}
	}
	return oids
}

// cgVolumeSeries fetches the last `count` volumes (per bar) for `asset` using Kraken's OHLC endpoint.
// It returns a slice of float64 volumes in chronological order.
func cgVolumeSeries(asset string, count int) ([]float64, error) {
	if count <= 0 {
		return nil, fmt.Errorf("invalid count %d", count)
	}
	// Calculate a `since` timestamp far enough back to cover `count` bars of 5m each.
	// Add a small buffer: fetch count+1 bars.
	bars := count + 1
	since := time.Now().Add(-time.Duration(bars*5) * time.Minute).Unix()

	pair, err := kraken.krakenPair(asset)
	if err != nil {
		return nil, fmt.Errorf("volumeSeries: %w", err)
	}
	vals := url.Values{
		"pair":     {pair},
		"interval": {"5"},
		"since":    {strconv.FormatInt(since, 10)},
	}
	raw, err := kraken.callPublic("/0/public/OHLC", vals)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch OHLC for volumes: %w", err)
	}
	// Kraken returns: { error:[], result:{ "<pair>":[...], last: <nonce> } }
	// We unmarshal 'result' as raw messages, then extract only our pair.
	var envelope struct {
		Error  []interface{}              `json:"error"`
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("failed to decode OHLC response envelope: %w", err)
	}
	if len(envelope.Error) > 0 {
		return nil, fmt.Errorf("OHLC error: %v", envelope.Error)
	}
	rawBars, ok := envelope.Result[pair]
	if !ok {
		return nil, fmt.Errorf("no OHLC data for pair %s", pair)
	} // Now unmarshal just the bars array
	var rows [][]interface{}
	if err := json.Unmarshal(rawBars, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse bars for %s: %w", pair, err)
	}
	n := len(rows)
	if n < count {
		return nil, fmt.Errorf("not enough OHLC bars: have %d, need %d", n, count)
	}

	// build volume series of the last `count` bars
	vols := make([]float64, count)
	start := n - count
	for i := start; i < n; i++ {
		bar := rows[i]
		// bar[6] holds the volume as string
		volStr, ok := bar[6].(string)
		if !ok {
			return nil, fmt.Errorf("unexpected volume type %T", bar[6])
		}
		v, err := strconv.ParseFloat(volStr, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid volume %q: %w", volStr, err)
		}
		vols[i-start] = v
	}
	return vols, nil
}

func GetAvgEntryFromTrades(asset string) (float64, error) {
	vals := url.Values{
		"trades": {"true"},
	}
	raw, err := kraken.callPrivate("/0/private/TradesHistory", vals)
	if err != nil {
		return 0, err
	}
	var js struct {
		Error  []interface{}          `json:"error"`
		Result map[string]interface{} `json:"result"`
	}
	if err := json.Unmarshal(raw, &js); err != nil {
		return 0, err
	}
	if len(js.Error) > 0 {
		return 0, fmt.Errorf("trade history error: %v", js.Error)
	}

	trades := js.Result["trades"].(map[string]interface{})
	var (
		totalCost float64
		totalVol  float64
	)

	for _, tradeRaw := range trades {
		trade := tradeRaw.(map[string]interface{})
		ts := int64(trade["time"].(float64))
		if time.Since(time.Unix(ts, 0)) > 24*time.Hour {
			continue // only include trades within the last 24 hours
		}
		pair := trade["pair"].(string)
		if !strings.Contains(pair, asset) {
			continue
		}
		if trade["type"].(string) != "buy" {
			continue
		}
		if trade["vol"] == nil || trade["cost"] == nil {
			continue
		}
		vol, _ := strconv.ParseFloat(trade["vol"].(string), 64)
		cost, _ := strconv.ParseFloat(trade["cost"].(string), 64)

		// skip tiny fills (dust or test orders)
		if cost < 5.0 || vol < 0.0001 {
			continue
		}

		totalVol += vol
		totalCost += cost

	}

	if totalVol == 0 {
		return 0, fmt.Errorf("no filled buys found for %s", asset)
	}
	return totalCost / totalVol, nil
}

// ------------- KrakenClient -------------
type KrakenClient struct {
	key, secret string
	pairMap     map[string]string // asset → Kraken’s canonical pair name
}

// NewKrakenClient creates a KrakenClient with credentials and an empty pairMap.
func NewKrakenClient(k, s string) *KrakenClient {
	return &KrakenClient{
		key:     k,
		secret:  s,
		pairMap: make(map[string]string),
	}
}

// RefreshAssetPairs fetches Kraken’s AssetPairs list and builds pairMap.
func (c *KrakenClient) RefreshAssetPairs() error {
	raw, err := c.callPublic("/0/public/AssetPairs", nil)
	if err != nil {
		return err
	}
	var resp struct {
		Error  []interface{}                       `json:"error"`
		Result map[string]struct{ Altname string } `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode AssetPairs: %w", err)
	}
	if len(resp.Error) > 0 {
		return fmt.Errorf("AssetPairs error: %v", resp.Error)
	}
	m := make(map[string]string, len(resp.Result))
	for pair, info := range resp.Result {
		// info.Altname is e.g. "XBTZUSD", "ETHUSD", etc.
		// derive base asset by stripping known suffixes
		base := strings.TrimSuffix(info.Altname, "ZUSD")
		base = strings.TrimSuffix(base, "USD")
		m[base] = pair
	}
	c.pairMap = m
	return nil
}

// GetBalance returns your available balance for a currency (e.g. "ZUSD", "XBT", "ETH")
func (c *KrakenClient) GetBalance(currency string) (float64, error) {
	vals := url.Values{}
	raw, err := c.callPrivate("/0/private/Balance", vals)
	if err != nil {
		return 0, err
	}
	var js struct {
		Error  []interface{}     `json:"error"`
		Result map[string]string `json:"result"`
	}
	if err := json.Unmarshal(raw, &js); err != nil {
		return 0, err
	}
	if len(js.Error) > 0 {
		return 0, fmt.Errorf("balance error: %v", js.Error)
	}

	// Translate human-readable codes to Kraken's internal keys
	alias := map[string]string{
		"XBT":  "XXBT",
		"BTC":  "XXBT",
		"ETH":  "XETH",
		"SOL":  "SOL",
		"ZUSD": "ZUSD",
	}
	key := alias[currency]
	if key == "" {
		key = currency // fallback if not in map
	}

	s, ok := js.Result[key]
	if !ok {
		return 0, fmt.Errorf("no '%s' in balance result", key)
	}
	return strconv.ParseFloat(s, 64)
}

func (c *KrakenClient) GetOrderBook(asset string, count int) (bids, asks [][2]float64, err error) {
	pair, err := c.krakenPair(asset)
	if err != nil {
		return nil, nil, err
	}
	vals := url.Values{"pair": {pair}, "count": {strconv.Itoa(count)}}
	raw, err := c.callPublic("/0/public/Depth", vals)
	if err != nil {
		return nil, nil, err
	}

	var resp struct {
		Error  []interface{} `json:"error"`
		Result map[string]struct {
			Bids [][]interface{} `json:"bids"`
			Asks [][]interface{} `json:"asks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, err
	}
	if len(resp.Error) > 0 {
		return nil, nil, fmt.Errorf("Depth error: %v", resp.Error)
	}

	data, ok := resp.Result[pair]
	if !ok {
		return nil, nil, fmt.Errorf("no orderbook data for pair %s", pair)
	}

	// parser helper
	parse := func(rows [][]interface{}) [][2]float64 {
		out := make([][2]float64, len(rows))
		for i, r := range rows {
			price, _ := strconv.ParseFloat(r[0].(string), 64)
			vol, _ := strconv.ParseFloat(r[1].(string), 64)
			out[i] = [2]float64{price, vol}
		}
		return out
	}

	return parse(data.Bids), parse(data.Asks), nil
}

// GetOHLC fetches 5-min bars (or whatever interval) but only parses the pair’s array,
// ignoring Kraken’s "last" numeric field.
func (c *KrakenClient) GetOHLC(asset string, interval int, since int64) ([][]float64, error) {
	pair, err := c.krakenPair(asset)
	if err != nil {
		return nil, fmt.Errorf("GetOHLC krakenPair: %w", err)
	}
	vals := url.Values{
		"pair":     {pair},
		"interval": {strconv.Itoa(interval)},
		"since":    {strconv.FormatInt(since, 10)},
	}
	raw, err := c.callPublic("/0/public/OHLC", vals)
	if err != nil {
		return nil, err
	}

	// 1) Unmarshal into a map[string]RawMessage so we can skip "last"
	var env struct {
		Error  []interface{}              `json:"error"`
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if len(env.Error) > 0 {
		return nil, fmt.Errorf("OHLC error: %v", env.Error)
	}

	// 2) Extract only our pair’s bars
	barsRaw, ok := env.Result[pair]
	if !ok {
		return nil, fmt.Errorf("no OHLC data for %s", pair)
	}

	// 3) Parse the bars array
	var rows [][]interface{}
	if err := json.Unmarshal(barsRaw, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse bars for %s: %w", pair, err)
	}

	// 4) Convert to [][]float64 of [open,high,low,close]
	ohlc := make([][]float64, len(rows))
	for i, r := range rows {
		ohlc[i] = make([]float64, 5) // we only care about the 1–4 indices
		for j := 1; j <= 4; j++ {
			if s, ok := r[j].(string); ok {
				if f, err := strconv.ParseFloat(s, 64); err == nil {
					ohlc[i][j-1] = f
				}
			}
		}
	}
	return ohlc, nil
}

// CancelOrder cancels an open private order by its transaction ID (txid).
func (c *KrakenClient) CancelOrder(orderID string) error {
	vals := url.Values{"txid": {orderID}}
	raw, err := c.callPrivate("/0/private/CancelOrder", vals)
	if err != nil {
		return err
	}

	var js map[string]interface{}
	if err := json.Unmarshal(raw, &js); err != nil {
		return err
	}
	if errs, ok := js["error"].([]interface{}); ok && len(errs) > 0 {
		return fmt.Errorf("cancel error: %v", errs)
	}

	stateMu.Lock()
	for i, oid := range state.OpenOrders {
		if oid == orderID {
			state.OpenOrders = append(state.OpenOrders[:i], state.OpenOrders[i+1:]...)
			if err := saveStateUnlocked(state); err != nil {
				log.Printf("warning: failed to save state: %v", err)
			}
			break
		}
	}
	stateMu.Unlock()

	return nil
}

var (
	lastNonceMu sync.Mutex
	lastNonce   int64
)

func (c *KrakenClient) sign(path string, vals url.Values) string {
	now := time.Now().UnixNano() / int64(time.Millisecond)

	lastNonceMu.Lock()
	if now <= lastNonce {
		now = lastNonce + 1 // ensure strict monotonicity
	}
	lastNonce = now
	lastNonceMu.Unlock()

	vals.Set("nonce", fmt.Sprintf("%d", now))

	post := vals.Encode()
	sum := sha256.Sum256([]byte(vals.Get("nonce") + post))
	sec, _ := base64.StdEncoding.DecodeString(c.secret)
	mac := hmac.New(sha512.New, sec)
	mac.Write([]byte(path))
	mac.Write(sum[:])

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// callPrivate sends authenticated POSTs to Kraken private endpoints
func (c *KrakenClient) callPrivate(path string, vals url.Values) ([]byte, error) {
	// sign the request (this will use your persistent NonceCounter)
	sign := c.sign(path, vals)

	// build POST
	req, err := http.NewRequest("POST", "https://api.kraken.com"+path, strings.NewReader(vals.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("API-Key", c.key)
	req.Header.Set("API-Sign", sign)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// attach context and use single client with timeout
	req = req.WithContext(context.Background())
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, _ := ioutil.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("kraken private %s HTTP %d: %s", path, res.StatusCode, string(body))
	}

	return body, nil
}

// callPublic sends unauthenticated GETs to Kraken public endpoints
func (c *KrakenClient) callPublic(path string, vals url.Values) ([]byte, error) {
	urlStr := "https://api.kraken.com" + path
	if vals != nil {
		urlStr += "?" + vals.Encode()
	}
	// build context‑aware request and use shared client
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(context.Background())
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		b, _ := ioutil.ReadAll(res.Body)
		return nil, fmt.Errorf("kraken public %s HTTP %d: %s", path, res.StatusCode, string(b))
	}
	return ioutil.ReadAll(res.Body)
}

func (c *KrakenClient) PlaceLimitBuy(asset string, vol, price float64) (string, error) {
	pair, err := c.krakenPair(asset)
	if err != nil {
		return "", fmt.Errorf("PlaceLimitBuy: %w", err)
	}
	// format price & volume to the exact precision required
	pp, ok := pricePrecision[asset]
	if !ok {
		pp = 4
	}
	vp, ok := volumePrecision[asset]
	if !ok {
		vp = 6
	}
	priceStr := fmt.Sprintf("%.*f", pp, price)
	volStr := fmt.Sprintf("%.*f", vp, vol)

	vals := url.Values{
		"pair":      {pair},
		"type":      {"buy"},
		"ordertype": {"limit"},
		"price":     {priceStr},
		"volume":    {volStr},
	}
	r, err := c.callPrivate("/0/private/AddOrder", vals)
	if err != nil {
		return "", err
	}
	var js map[string]interface{}
	if err := json.Unmarshal(r, &js); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	if errs := js["error"].([]interface{}); len(errs) > 0 {
		return "", errors.New(fmt.Sprint(errs))
	}
	tx := js["result"].(map[string]interface{})["txid"].([]interface{})[0].(string)

	// ── record the new order in state and persist ──
	stateMu.Lock()
	state.OpenOrders = append(state.OpenOrders, tx)
	if err := saveStateUnlocked(state); err != nil {
		log.Printf("warning: failed to save state: %v", err)
	}
	stateMu.Unlock()

	return tx, nil
}

func (c *KrakenClient) OrderStatus(id string) (string, error) {
	vals := url.Values{"txid": {id}}
	r, err := c.callPrivate("/0/private/QueryOrders", vals)
	if err != nil {
		return "", err
	}
	var js map[string]interface{}
	if err := json.Unmarshal(r, &js); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	res, ok := js["result"].(map[string]interface{})[id].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("missing result for %s", id)
	}
	status, ok := res["status"].(string)
	if !ok {
		return "", fmt.Errorf("missing status for %s", id)
	}
	return status, nil
}

func (c *KrakenClient) OrderFill(asset, id string) (price, vol float64, err error) {
	vals := url.Values{"txid": {id}}
	r, err := c.callPrivate("/0/private/QueryOrders", vals)
	if err != nil {
		return 0, 0, err
	}
	var js map[string]interface{}
	if err := json.Unmarshal(r, &js); err != nil {
		return 0, 0, fmt.Errorf("invalid JSON: %w", err)
	}
	resMap, ok := js["result"].(map[string]interface{})[id].(map[string]interface{})
	if !ok {
		return 0, 0, fmt.Errorf("missing result for %s", id)
	}
	// parse price & executed volume
	if pStr, ok := resMap["price"].(string); ok {
		price, _ = strconv.ParseFloat(pStr, 64)
	} else {
		return 0, 0, fmt.Errorf("price field missing or not a string for %s", id)
	}
	if vStr, ok := resMap["vol_exec"].(string); ok {
		vol, _ = strconv.ParseFloat(vStr, 64)
	} else {
		return 0, 0, fmt.Errorf("vol_exec field missing or not a string for %s", id)
	}
	return
}

func (c *KrakenClient) PlaceMarketSell(asset string, vol float64) (string, error) {
	// look up the canonical Kraken pair for this asset
	pair, err := c.krakenPair(asset)
	if err != nil {
		return "", fmt.Errorf("PlaceMarketSell: %w", err)
	}

	// format volume to the exact precision required
	vp, ok := volumePrecision[asset]
	if !ok {
		vp = 6
	}
	volStr := fmt.Sprintf("%.*f", vp, vol)

	vals := url.Values{
		"pair":      {pair},
		"type":      {"sell"},
		"ordertype": {"market"},
		"volume":    {volStr},
	}
	r, err := c.callPrivate("/0/private/AddOrder", vals)
	if err != nil {
		return "", err
	}
	var js map[string]interface{}
	json.Unmarshal(r, &js)
	if errs := js["error"].([]interface{}); len(errs) > 0 {
		return "", errors.New(fmt.Sprint(errs))
	}
	tx := js["result"].(map[string]interface{})["txid"].([]interface{})[0].(string)

	// ── record the new order in state and persist ──
	stateMu.Lock()
	state.OpenOrders = append(state.OpenOrders, tx)
	if err := saveStateUnlocked(state); err != nil {
		log.Printf("warning: failed to save state: %v", err)
	}
	stateMu.Unlock()

	return tx, nil
}

func (c *KrakenClient) Withdraw(asset, key, amt string) (string, error) {
	vals := url.Values{"asset": {asset}, "key": {key}, "amount": {amt}}
	r, err := c.callPrivate("/0/private/Withdraw", vals)
	if err != nil {
		return "", err
	}
	var js map[string]interface{}
	json.Unmarshal(r, &js)
	if errs := js["error"].([]interface{}); len(errs) > 0 {
		return "", errors.New(fmt.Sprint(errs))
	}
	return js["result"].(map[string]interface{})["refid"].(string), nil
}

// GetTickerMid fetches the mid-price, handling string or numeric JSON fields.
// GetTickerMid fetches the mid-price, handling only the "a" and "b" arrays.
func (c *KrakenClient) GetTickerMid(asset string) (float64, error) {
	pair, err := c.krakenPair(asset)
	if err != nil {
		return 0, fmt.Errorf("GetTickerMid: %w", err)
	}
	vals := url.Values{"pair": {pair}}
	raw, err := c.callPublic("/0/public/Ticker", vals)
	if err != nil {
		return 0, err
	}

	// Only unmarshal the error array and the "a"/"b" keys
	var resp struct {
		Error  []interface{} `json:"error"`
		Result map[string]struct {
			A []interface{} `json:"a"`
			B []interface{} `json:"b"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, err
	}
	if len(resp.Error) > 0 {
		return 0, fmt.Errorf("ticker error: %v", resp.Error)
	}

	data, ok := resp.Result[pair]
	if !ok {
		return 0, fmt.Errorf("no ticker result for %s", pair)
	}

	// helper to parse string or numeric JSON values
	parseSide := func(arr []interface{}) (float64, error) {
		if len(arr) == 0 {
			return 0, fmt.Errorf("empty ticker array")
		}
		switch v := arr[0].(type) {
		case string:
			return strconv.ParseFloat(v, 64)
		case float64:
			return v, nil
		default:
			return 0, fmt.Errorf("unexpected ticker type %T", v)
		}
	}

	ask, err := parseSide(data.A)
	if err != nil {
		return 0, fmt.Errorf("parse ask: %w", err)
	}
	bid, err := parseSide(data.B)
	if err != nil {
		return 0, fmt.Errorf("parse bid: %w", err)
	}
	return (ask + bid) / 2, nil
}

// GetTradeFees pulls your current TradeVolume fees for all configured pairs.
// It returns the maker & taker fee (as decimals, e.g. 0.0025) for the first asset-pair.
func (c *KrakenClient) GetTradeFees(pairs []string) (maker, taker float64, err error) {
	vals := url.Values{"pair": {strings.Join(pairs, ",")}}
	raw, err := c.callPrivate("/0/private/TradeVolume", vals)
	if err != nil {
		return 0, 0, err
	}
	var resp struct {
		Error  []interface{} `json:"error"`
		Result struct {
			Fees map[string]struct {
				Fee float64 `json:"fee,string"`
			} `json:"fees"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, 0, err
	}
	if len(resp.Error) > 0 {
		return 0, 0, fmt.Errorf("TradeVolume error: %v", resp.Error)
	}
	// pick the first pair’s fee as a proxy
	for _, info := range resp.Result.Fees {
		maker = info.Fee // Kraken currently returns a single fee; treat as both maker/taker if needed
		taker = info.Fee
		break
	}
	return maker, taker, nil
}

// ------------- CoinGeckoClient -------------
type CoingeckoClient struct{}

func NewCoingeckoClient() *CoingeckoClient { return &CoingeckoClient{} }

// GetPrices fetches USD prices for multiple Coingecko IDs in one call.
func (c *CoingeckoClient) GetPrices(ids []string) (map[string]float64, error) {
	endpoint := fmt.Sprintf(
		"https://api.coingecko.com/api/v3/simple/price?ids=%s&vs_currencies=usd",
		strings.Join(ids, ","),
	)

	// simple GET + unmarshal
	req, _ := http.NewRequest("GET", endpoint, nil)
	req = req.WithContext(context.Background())
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate limited")
	}
	if res.StatusCode >= 400 {
		b, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, string(b))
	}

	var payload map[string]map[string]float64
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return nil, err
	}

	// flatten to id→price
	out := make(map[string]float64, len(payload))
	for id, m := range payload {
		if p, ok := m["usd"]; ok {
			out[id] = p
		}
	}
	return out, nil
}

// GetATRAndRSI fetches ATR & RSI from Coingecko using the given Gecko ID and candle period.
// coingeckoID should be the ID (e.g. "bitcoin", "ethereum", etc.).
// GetATRAndRSI fetches ATR & RSI via True Range + RSI formula for the given Gecko ID.
func (c *CoingeckoClient) GetATRAndRSI(coingeckoID string, period int) (atr, rsi float64, err error) {
	// fetch OHLC bars: [time, open, high, low, close]
	url := fmt.Sprintf(
		"https://api.coingecko.com/api/v3/coins/%s/ohlc?vs_currency=usd&days=1",
		coingeckoID,
	)
	req, _ := http.NewRequest("GET", url, nil)
	req = req.WithContext(context.Background())
	res, err := httpClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		b, _ := io.ReadAll(res.Body)
		return 0, 0, fmt.Errorf("coingecko ohlc HTTP %d: %s", res.StatusCode, string(b))
	}

	var ohlc [][]float64
	if err := json.NewDecoder(res.Body).Decode(&ohlc); err != nil {
		return 0, 0, err
	}
	// compute True-Range ATR
	atr = computeATRFromOHLC(ohlc, period)

	// build closes for RSI
	closes := make([]float64, len(ohlc))
	for i, v := range ohlc {
		closes[i] = v[4]
	}
	rsi = computeRSI(closes, period)

	return atr, rsi, nil
}

// computeATRFromOHLC averages the True Range over the last p bars.
// Works for both layouts:
//
//	Kraken  [open, high, low, close]          (len==4)
//	CoinGecko [ts, open, high, low, close]    (len==5)
func computeATRFromOHLC(ohlc [][]float64, p int) float64 {
	n := len(ohlc)
	if n < p+1 {
		return 0
	}

	// figure out column indexes once
	hiIdx, loIdx, clIdx := 1, 2, 3 // Kraken default
	if len(ohlc[0]) == 5 {         // CoinGecko layout
		hiIdx, loIdx, clIdx = 2, 3, 4
	}

	var sum float64
	for i := n - p; i < n; i++ {
		high := ohlc[i][hiIdx]
		low := ohlc[i][loIdx]
		prevClose := ohlc[i-1][clIdx]
		tr := math.Max(high-low,
			math.Max(math.Abs(high-prevClose), math.Abs(low-prevClose)))
		sum += tr
	}
	return sum / float64(p)
}

func computeVWAP(book [][2]float64) float64 {
	var cumPV, cumVol float64
	for _, lvl := range book {
		price, vol := lvl[0], lvl[1]
		cumPV += price * vol
		cumVol += vol
	}
	if cumVol == 0 {
		return 0
	}
	return cumPV / cumVol
}

// sumVolume sums the volume field out of an orderbook side.

func sumVolume(side [][2]float64) float64 {
	total := 0.0
	for _, lvl := range side {
		total += lvl[1]
	}
	return total
}

// computeRSI calculates the Relative Strength Index over the last p periods,
// using exactly p intervals (true textbook window).
func computeRSI(arr []float64, p int) float64 {
	// sanity: positive look-back
	if p <= 0 {
		return 50
	}
	n := len(arr)
	// need at least p+1 closes
	if n <= p {
		return 50
	}

	// start at the first of the last p+1 closes
	start := n - p
	var gain, loss float64

	// now loop i=start through n-1, giving p diffs: arr[i] - arr[i-1]
	for i := start; i < n; i++ {
		d := arr[i] - arr[i-1]
		if math.IsNaN(d) || math.IsInf(d, 0) {
			continue
		}
		if d > 0 {
			gain += d
		} else {
			loss -= d
		}
	}

	avgG := gain / float64(p)
	avgL := loss / float64(p)
	// zero‐loss case
	if avgL == 0 {
		return 100
	}

	rs := avgG / avgL
	return 100 - 100/(1+rs)
}

// ------------- Discord Notifier -------------
type DiscordNotifier struct{ webhook string }

func NewDiscordNotifier(hook string) *DiscordNotifier { return &DiscordNotifier{hook} }
func (d *DiscordNotifier) Send(msg string) {
	payload, _ := json.Marshal(map[string]string{"content": msg})
	res, err := http.Post(d.webhook, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		log.Printf("discord notify error: %v", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		log.Printf("discord notify non‑200: %d %s", res.StatusCode, string(body))
	}
}
