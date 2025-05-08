package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type Metric struct {
	Timestamp int64   `json:"timestamp"`
	Sharpe    float64 `json:"sharpe"`
	Return    float64 `json:"return"`
	PnL       float64 `json:"pnl"`
}

var metricsStore = make(map[string]map[string][]Metric)

func serveMetrics(w http.ResponseWriter, r *http.Request) {
	asset := r.URL.Query().Get("asset")
	rangeStr := r.URL.Query().Get("range")

	assetMetrics, ok := metricsStore[asset]
	if !ok {
		http.Error(w, "asset not found", http.StatusNotFound)
		return
	}

	data, ok := assetMetrics[rangeStr]
	if !ok {
		http.Error(w, "range not found", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func startWebServer() {
	for _, asset := range cfg.Assets {
		metricsStore[asset] = map[string][]Metric{
			"7d":       {},
			"30d":      {},
			"lifetime": {},
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("./static")))
	mux.HandleFunc("/api/metrics", serveMetrics)
	mux.HandleFunc("/api/config", serveConfig)

	srv := &http.Server{
		Addr:         "127.0.0.1:8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Println("[WEB] starting dashboard server on http://127.0.0.1:8080")
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("[WEB ERROR] %v", err)
	}

}

func serveConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"assets": cfg.Assets,
	})
}
