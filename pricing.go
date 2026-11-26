//go:build windows

package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
)

func categoryAllowed(pc PC, category, group string) bool {
	if len(pc.PricingCategories) > 0 {
		return slices.Contains(pc.PricingCategories, category)
	}
	if group == "" {
		group = category
	}
	return pc.PricingGroup == "" || group == pc.PricingGroup
}

type PriceEntry struct {
	MinutePaise       *int   `json:"minute_paise,omitempty"`
	HourDiscountPaise int    `json:"hour_discount_paise,omitempty"`
	Group             string `json:"group,omitempty"`
	Category          string `json:"category"`
	Players           int    `json:"players"`
	DurationMinutes   int    `json:"duration_minutes"`
	Amount            int    `json:"amount"`
	Note              string `json:"note,omitempty"`
}

type PriceCatalog struct {
	ManualCollection bool         `json:"manual_collection,omitempty"`
	Currency         string       `json:"currency"`
	Entries          []PriceEntry `json:"entries"`
	Notes            []string     `json:"notes,omitempty"`
}

//go:embed default_pricing.json
var defaultPrices []byte
var currentPrices = func() PriceCatalog {
	var catalog PriceCatalog
	if err := json.Unmarshal(defaultPrices, &catalog); err != nil {
		panic(err)
	}
	return catalog
}()

func pricingCatalog() PriceCatalog {
	priceMu.RLock()
	defer priceMu.RUnlock()
	catalog := currentPrices
	catalog.Entries = append([]PriceEntry(nil), currentPrices.Entries...)
	sort.Slice(catalog.Entries, func(i, j int) bool {
		left, right := catalog.Entries[i], catalog.Entries[j]
		if left.Category != right.Category {
			return left.Category < right.Category
		}
		if left.Players != right.Players {
			return left.Players < right.Players
		}
		return left.DurationMinutes < right.DurationMinutes
	})
	return catalog
}

func lookupPrice(category string, players, durationMinutes int) (PriceEntry, error) {
	return priceAt(pricingCatalog().Entries, category, players, durationMinutes)
}

func priceAt(entries []PriceEntry, category string, players, minutes int) (PriceEntry, error) {
	if minutes < 1 || minutes > 525600 {
		return PriceEntry{}, errors.New("invalid duration")
	}
	var base, tier *PriceEntry
	for i := range entries {
		e := &entries[i]
		if e.Category != category || e.Players != players {
			continue
		}
		if base == nil || e.DurationMinutes < base.DurationMinutes {
			base = e
		}
		if e.DurationMinutes <= minutes && (tier == nil || e.DurationMinutes > tier.DurationMinutes) {
			tier = e
		}
	}
	if tier == nil {
		tier = base
	}
	if tier == nil {
		return PriceEntry{}, fmt.Errorf("no rate for %s and %d customers", category, players)
	}
	price := *tier
	price.Amount = (price.Amount*minutes + price.DurationMinutes - 1) / price.DurationMinutes
	if tier.MinutePaise != nil {
		price.Amount = (max(0, *tier.MinutePaise*minutes-tier.HourDiscountPaise*(minutes/60)) + 50) / 100
	}
	price.DurationMinutes = minutes
	return price, nil
}

func sessionQuote(s Session, minutes int) (PriceEntry, error) {
	var rates []PriceEntry
	if json.Unmarshal([]byte(s.PricingSnapshot), &rates) == nil && len(rates) > 0 {
		return priceAt(rates, s.Category, s.Players, minutes)
	}
	return lookupPrice(s.Category, s.Players, minutes)
}
func pricingSnapshot(entry PriceEntry, catalog []PriceEntry) string {
	var rates []PriceEntry
	for _, e := range catalog {
		if e.Category == entry.Category && e.Players == entry.Players {
			rates = append(rates, e)
		}
	}
	data, err := json.Marshal(rates)
	if err != nil {
		return "{}"
	}
	return string(data)
}

var priceMu sync.RWMutex

func validPricing(c PriceCatalog) error {
	if len(c.Entries) == 0 || len(c.Entries) > 2000 {
		return errors.New("enter at least one pricing rule (maximum 2000)")
	}
	groups := map[string]string{}
	seen := map[string]bool{}
	for _, e := range c.Entries {
		if strings.TrimSpace(e.Category) == "" || e.Players < 1 || e.Players > 4 || e.DurationMinutes < 1 || e.DurationMinutes > 1440 || e.Amount < 0 || e.Amount > 9999999 {
			return errors.New("invalid price rule")
		}
		if e.MinutePaise != nil && (*e.MinutePaise < 0 || *e.MinutePaise > 1000000 || e.HourDiscountPaise < 0 || e.HourDiscountPaise > 60*(*e.MinutePaise)) {
			return errors.New("invalid minute rate or hourly discount")
		}
		g := e.Group
		if g == "" {
			g = e.Category
		}
		if prior, ok := groups[e.Category]; ok && prior != g {
			return errors.New("each category must belong to one PC group")
		}
		groups[e.Category] = g
		key := fmt.Sprintf("%q/%d/%d", e.Category, e.Players, e.DurationMinutes)
		if seen[key] {
			return errors.New("duplicate category, player count and threshold")
		}
		seen[key] = true
	}
	return nil
}
func loadPricing() {
	data, err := os.ReadFile(appDataPath("pricing.json"))
	if os.IsNotExist(err) {
		return
	}
	var c PriceCatalog
	if err == nil {
		err = json.Unmarshal(data, &c)
	}
	if err == nil {
		err = validPricing(c)
	}
	if err != nil {
		log.Printf("could not load pricing: %v", err)
		return
	}
	priceMu.Lock()
	currentPrices = c
	priceMu.Unlock()
}
func pricingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		writeJSON(w, 200, pricingCatalog())
		return
	}
	if r.Method != "PUT" {
		http.Error(w, "GET or PUT required", 405)
		return
	}
	var c PriceCatalog
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
		writeBillingError(w, err)
		return
	}
	c.Currency = "INR"
	c.Notes = nil
	if err := validPricing(c); err != nil {
		writeBillingError(w, err)
		return
	}
	data, err := json.MarshalIndent(c, "", "  ")
	path := appDataPath("pricing.json")
	priceMu.Lock()
	if err == nil {
		err = os.WriteFile(path+".tmp", data, 0600)
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err == nil {
		currentPrices = c
	}
	priceMu.Unlock()
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, 200, c)
}
func distributePricing(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST required", 405)
		return
	}
	c := pricingCatalog()
	results := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, pc := range store.list() {
		endpoint, err := billingPeerEndpoint(pc, "/api/pricing")
		if err != nil || seen[endpoint] || pc.Missing || localBillingPC(pc) {
			continue
		}
		seen[endpoint] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			var saved PriceCatalog
			err := callBillingPeerContext(r.Context(), pc, "PUT", "/api/pricing", c, &saved)
			mu.Lock()
			defer mu.Unlock()
			results[endpoint] = "saved"
			if err != nil {
				results[endpoint] = err.Error()
			}
		}()
	}
	wg.Wait()
	writeJSON(w, 200, results)
}
