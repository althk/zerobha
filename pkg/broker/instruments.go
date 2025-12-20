package broker

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// Instrument represents a tradable instrument
type Instrument struct {
	Token           uint32
	ExchangeToken   string
	Tradingsymbol   string
	Name            string
	LastPrice       float64
	Expiry          time.Time
	Strike          float64
	TickSize        float64
	LotSize         int
	InstrumentType  string // CE, PE, FUT, EQ
	Segment         string
	Exchange        string
	UnderlyingToken uint32 // Token of the underlying index/stock
}

// InstrumentManager handles loading and querying instruments
type InstrumentManager struct {
	instruments       []Instrument
	symbolToToken     map[string]uint32
	tokenToInstrument map[uint32]*Instrument
	underlyingMap     map[string][]Instrument // Map underlying symbol (e.g., NIFTY 50) to its derivatives
}

// NewInstrumentManager creates a new manager
func NewInstrumentManager() *InstrumentManager {
	return &InstrumentManager{
		symbolToToken:     make(map[string]uint32),
		tokenToInstrument: make(map[uint32]*Instrument),
		underlyingMap:     make(map[string][]Instrument),
	}
}

// FetchInstruments downloads the master instrument list from Zerodha
func (im *InstrumentManager) FetchInstruments(kc *kiteconnect.Client) error {
	// We can use the Kite Connect library's GetInstruments, but for full control and potentially larger lists,
	// sometimes downloading the CSV directly is preferred. However, let's use the library first.
	// We need both NSE (for indices) and NFO (for options).

	exchanges := []string{"NSE", "NFO"}
	var allInstruments []kiteconnect.Instrument

	for _, ex := range exchanges {
		instrs, err := kc.GetInstrumentsByExchange(ex)
		if err != nil {
			return fmt.Errorf("failed to fetch instruments for %s: %v", ex, err)
		}
		allInstruments = append(allInstruments, instrs...)
	}

	im.instruments = make([]Instrument, 0, len(allInstruments))

	// Helper to find underlying token for NFO instruments
	// We might need a second pass or a map of Name -> Token for indices
	nameToToken := make(map[string]uint32)

	// First pass: Process NSE instruments (Indices/Equities) to populate nameToToken
	for _, kInst := range allInstruments {
		if kInst.Exchange == "NSE" {
			nameToToken[kInst.Name] = uint32(kInst.InstrumentToken)
			// Also map Tradingsymbol to Token for direct lookups
			im.symbolToToken[kInst.Tradingsymbol] = uint32(kInst.InstrumentToken)
		}
	}

	// Second pass: Process all and build our Instrument structs
	for _, kInst := range allInstruments {
		inst := Instrument{
			Token:          uint32(kInst.InstrumentToken),
			ExchangeToken:  strconv.Itoa(int(kInst.ExchangeToken)), // Convert int to string
			Tradingsymbol:  kInst.Tradingsymbol,
			Name:           kInst.Name,
			Expiry:         kInst.Expiry.Time,
			Strike:         kInst.StrikePrice,
			TickSize:       kInst.TickSize,
			LotSize:        int(kInst.LotSize), // Convert float64 to int
			InstrumentType: kInst.InstrumentType,
			Segment:        kInst.Segment,
			Exchange:       kInst.Exchange,
		}

		// Link underlying
		if kInst.Exchange == "NFO" {
			if token, ok := nameToToken[kInst.Name]; ok {
				inst.UnderlyingToken = token
			}
		}

		im.instruments = append(im.instruments, inst)
		im.tokenToInstrument[inst.Token] = &im.instruments[len(im.instruments)-1]
		im.symbolToToken[inst.Tradingsymbol] = inst.Token

		// Group by underlying name (e.g., "NIFTY")
		// Note: kInst.Name for Nifty 50 is usually "NIFTY"
		if kInst.Exchange == "NFO" {
			im.underlyingMap[kInst.Name] = append(im.underlyingMap[kInst.Name], inst)
		}
	}

	return nil
}

// FindOption selects the best option based on criteria
// underlying: "NIFTY"
// side: "CE" or "PE"
// targetPremium: The price we want to pay (e.g., 250)
// quoteFetcher: A function to get current market price for a list of tokens
func (im *InstrumentManager) FindOption(underlying string, side string, targetPremium float64, quoteFetcher func([]uint32) (map[uint32]float64, error)) (*Instrument, error) {
	candidates := im.underlyingMap[underlying]
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no instruments found for underlying %s", underlying)
	}

	// 1. Filter by Expiry (Current Week/Month)
	// For simplicity, let's pick the nearest expiry that is today or in the future
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	var nearestExpiry time.Time
	foundExpiry := false

	// Sort candidates by expiry
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Expiry.Before(candidates[j].Expiry)
	})

	for _, inst := range candidates {
		if !inst.Expiry.Before(today) {
			nearestExpiry = inst.Expiry
			foundExpiry = true
			break
		}
	}

	if !foundExpiry {
		return nil, fmt.Errorf("no future expiry found for %s", underlying)
	}

	// 2. Filter by Type (CE/PE) and Expiry
	var filtered []Instrument
	for _, inst := range candidates {
		if inst.Expiry.Equal(nearestExpiry) && inst.InstrumentType == side {
			filtered = append(filtered, inst)
		}
	}

	if len(filtered) == 0 {
		return nil, fmt.Errorf("no %s options found for expiry %v", side, nearestExpiry)
	}

	// 3. We need to find the one closest to targetPremium.
	// Since we don't know the ATM strike, we can't just pick "ATM".
	// We have to fetch quotes for ALL strikes in this expiry? That's too many (hundreds).
	// Optimization: We need the underlying price (Spot) to narrow down the search range.
	// But the interface doesn't provide Spot Price.
	// Let's assume the caller can provide Spot Price, or we fetch it.
	// Actually, the prompt said "FindOption(underlying, side, spotPrice)".
	// Wait, the user prompt said "FindOption(underlying, side, spotPrice)" in the plan.
	// I should update the signature.

	// BUT, even with Spot Price, we want to target a Premium.
	// A 250 Rs premium might be deep ITM or ATM depending on volatility.
	// Strategy:
	// 1. Get Spot Price (passed in arg).
	// 2. Select a range of strikes around Spot (e.g., +/- 5%).
	// 3. Fetch quotes for these ~20-30 instruments.
	// 4. Pick the one closest to targetPremium.

	return nil, fmt.Errorf("FindOption requires spot price to filter strikes efficiently")
}

// FindOptionWithSpot is the actual implementation
func (im *InstrumentManager) FindOptionWithSpot(underlying string, side string, spotPrice float64, targetPremium float64, quoteFetcher func([]uint32) (map[uint32]float64, error)) (*Instrument, error) {
	candidates := im.underlyingMap[underlying]

	// 1. Find Nearest Expiry
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	// Sort by expiry
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Expiry.Before(candidates[j].Expiry)
	})

	var targetExpiry time.Time
	found := false
	for _, inst := range candidates {
		if !inst.Expiry.Before(today) {
			targetExpiry = inst.Expiry
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("no expiry found")
	}

	// 2. Filter candidates by Expiry, Side, and Strike Range
	// We only look at strikes within +/- 3% of Spot to reduce API calls
	lowerBound := spotPrice * 0.97
	upperBound := spotPrice * 1.03

	var potentialTokens []uint32
	var potentialInstruments []*Instrument

	for i := range candidates {
		inst := &candidates[i]
		if inst.Expiry.Equal(targetExpiry) && inst.InstrumentType == side {
			if inst.Strike >= lowerBound && inst.Strike <= upperBound {
				potentialTokens = append(potentialTokens, inst.Token)
				potentialInstruments = append(potentialInstruments, inst)
			}
		}
	}

	if len(potentialTokens) == 0 {
		return nil, fmt.Errorf("no strikes found near spot %f", spotPrice)
	}

	// 3. Fetch Quotes
	quotes, err := quoteFetcher(potentialTokens)
	if err != nil {
		return nil, err
	}

	// 4. Find closest to Target Premium
	var bestInst *Instrument
	minDiff := math.MaxFloat64

	for _, inst := range potentialInstruments {
		if price, ok := quotes[inst.Token]; ok {
			diff := math.Abs(price - targetPremium)
			if diff < minDiff {
				minDiff = diff
				bestInst = inst
			}
		}
	}

	if bestInst == nil {
		return nil, fmt.Errorf("could not determine best option")
	}

	return bestInst, nil
}

// GetSymbol returns the trading symbol for a given token
func (im *InstrumentManager) GetSymbol(token uint32) (string, error) {
	inst, ok := im.tokenToInstrument[token]
	if !ok {
		return "", fmt.Errorf("token %d not found", token)
	}
	return inst.Tradingsymbol, nil
}
