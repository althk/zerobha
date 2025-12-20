package broker

import (
	"testing"
	"time"
)

func TestInstrumentManager_FindOptionWithSpot(t *testing.T) {
	im := NewInstrumentManager()

	// Mock Instruments
	expiry := time.Now().AddDate(0, 0, 5) // 5 days from now
	im.underlyingMap["NIFTY"] = []Instrument{
		{Token: 1, Tradingsymbol: "NIFTY24DEC24000CE", Expiry: expiry, Strike: 24000, InstrumentType: "CE", Exchange: "NFO"},
		{Token: 2, Tradingsymbol: "NIFTY24DEC24100CE", Expiry: expiry, Strike: 24100, InstrumentType: "CE", Exchange: "NFO"},
		{Token: 3, Tradingsymbol: "NIFTY24DEC24200CE", Expiry: expiry, Strike: 24200, InstrumentType: "CE", Exchange: "NFO"},
		{Token: 4, Tradingsymbol: "NIFTY24DEC24300CE", Expiry: expiry, Strike: 24300, InstrumentType: "CE", Exchange: "NFO"},
	}

	// Mock Fetcher
	mockFetcher := func(tokens []uint32) (map[uint32]float64, error) {
		return map[uint32]float64{
			1: 400.0, // Deep ITM
			2: 300.0, // ITM
			3: 240.0, // Target ~250
			4: 150.0, // OTM
		}, nil
	}

	// Test Case: Target 250 Premium
	opt, err := im.FindOptionWithSpot("NIFTY", "CE", 24150, 250.0, mockFetcher)
	if err != nil {
		t.Fatalf("FindOptionWithSpot failed: %v", err)
	}

	if opt.Token != 3 {
		t.Errorf("Expected Token 3 (Price 240), got Token %d", opt.Token)
	}
}
