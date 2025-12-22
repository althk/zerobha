package broker

import "strings"

func toZerodhaTimeframe(tf string) string {
	switch strings.ToLower(tf) {
	case "1m", "1minute", "minute":
		return "minute"
	case "3m", "3minute":
		return "3minute"
	case "5m", "5minute":
		return "5minute"
	case "10m", "10minute":
		return "10minute"
	case "15m", "15minute":
		return "15minute"
	case "30m", "30minute":
		return "30minute"
	case "60m", "1h", "60minute":
		return "60minute"
	case "1d", "day":
		return "day"
	default:
		return tf
	}
}
