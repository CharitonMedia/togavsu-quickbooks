package quickbooks

import "time"

// Fixed numbers, spelled as arithmetic: the convention of the core's example module
// matter-clock, so the core's settings source check (modulekit.CheckSource, AC-030) can
// tell a fixed rule from a tunable. Each is a rule of the spec, of QuickBooks or of a
// format the module reads or writes. A value a firm might change is a module setting in
// package/togavsu-module.yaml or a limit in limits.json, never one of these.
var (
	zero   = int(0)
	one    = zero + 1
	two    = one + one
	zero64 = int64(zero) // no cents, no microseconds, no rows
)

// Units of time, built from the second.
var (
	second = time.Duration(1000 * 1000 * 1000 * one)
	minute = 60 * second
	hour   = 60 * minute
	day    = 24 * hour
)

var (
	// firstGroup is the first capture group of a regexp submatch.
	firstGroup = one
	// hoursPerDay bounds reconcile_hour_utc, an hour from 0 to 23.
	hoursPerDay = 24 * one
	// suffixChars: a customer whose name a vendor or employee holds takes the first
	// eight characters of its contact id as a suffix (MOD-QBO-27's id suffix).
	suffixChars = 8 * one
)

// UUID layout (RFC 9562): the version byte, the variant byte, the six bytes of a v7
// millisecond timestamp, and where the hex form takes its dashes.
var (
	uuidVersionByte = 6 * one
	uuidVariantByte = 8 * one
	uuidMillisHigh  = 2 * one // bytes 0 and 1 hold the top 16 bits of the timestamp
	uuidMillisEnd   = 6 * one // bytes 2 to 5 hold the low 32 bits
	uuidDashes      = []int{8 * one, 12 * one, 16 * one, 20 * one}
)
