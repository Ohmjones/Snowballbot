package main

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"sync"
	"time"
)

const stateFile = "state.json"

// State holds exactly the bits you need to survive a restart.
// Tweak fields to match your in-RAM variables.
// State holds exactly the bits you need to survive a restart.
type State struct {
	NonceCounter     int64              `json:"nonce_counter"`
	BaselineUSD      float64            `json:"baseline_usd"`
	OpenOrders       []string           `json:"open_orders"`
	AvgEntry         map[string]float64 `json:"avg_entry"`
	PositionVol      map[string]float64 `json:"position_vol"`
	ProfitCumulative map[string]float64 `json:"profit_cumulative"`
	CycleState       map[string]string  `json:"cycle_state"`

	// NEW – lets you reuse historical bars between cycles
	PriceHistory map[string][]float64 `json:"price_history"`
	VolHistory   map[string][]float64 `json:"vol_history"`
}

// a mutex so concurrent goroutines can safely call SaveState()
var stateMu sync.Mutex

// SaveState marshals to JSON and atomically writes to disk.
func SaveState(s *State) error {
	stateMu.Lock()
	defer stateMu.Unlock()

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// WriteFile is atomic on most OSes
	return ioutil.WriteFile(stateFile, data, 0o644)
}

// LoadState tries to read stateFile; if missing, returns zero-State.
func LoadState() (*State, error) {
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		return &State{NonceCounter: time.Now().UnixNano()}, nil
	}
	data, err := ioutil.ReadFile(stateFile)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	// ensure all maps are non-nil so lookups won’t panic
	if s.NonceCounter == 0 {
		s.NonceCounter = time.Now().UnixNano()
	}
	if s.AvgEntry == nil {
		s.AvgEntry = make(map[string]float64)
	}
	if s.PositionVol == nil {
		s.PositionVol = make(map[string]float64)
	}
	if s.ProfitCumulative == nil {
		s.ProfitCumulative = make(map[string]float64)
	}
	if s.CycleState == nil {
		s.CycleState = make(map[string]string)
	}

	return &s, nil
}
