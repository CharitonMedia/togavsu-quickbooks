package qbo

import "time"

// Fixed numbers, spelled as arithmetic: the convention of the core's example module
// matter-clock, so the core's settings source check (modulekit.CheckSource, AC-030) can
// tell a fixed rule from a tunable. Each is a rule of the QuickBooks Online API or of the
// money format. Intuit's request ceiling itself comes from limits.json.
var (
	zero   = int(0)
	one    = zero + 1
	zero64 = int64(zero)
)

var (
	second = time.Duration(1000 * 1000 * 1000 * one)
	// rateWindow is the span of Intuit's per-minute request ceiling.
	rateWindow = 60 * second
	// pageSize is QBO's largest query page, and firstPosition its first startposition.
	pageSize      = 1000 * one
	firstPosition = one
	// centDigits: QBO amounts carry at most two decimals.
	centDigits = 2 * one
)
