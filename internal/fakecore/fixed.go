package fakecore

// Fixed numbers, spelled as arithmetic: the convention of the core's example module
// matter-clock, so the core's settings source check (modulekit.CheckSource, AC-030) can
// tell a fixed rule from a tunable. Each mirrors a rule of the core the fake stands in
// for, or a format the fake writes.
var (
	zero   = int(0)
	one    = zero + 1
	two    = one + one
	three  = two + one
	zero64 = int64(zero)
)

var (
	// tokenBytes and eventIDBytes: random bytes in a fake token and a fake event id.
	tokenBytes   = 16 * one
	eventIDBytes = 8 * one
	// pageDefault and pageMax are the core's list page size and its ceiling.
	pageDefault = 50 * one
	pageMax     = 200 * one
)

// UUID layout (RFC 9562): the version byte, the variant byte, the six bytes of a v7
// millisecond timestamp, and where the hex form takes its dashes.
var (
	uuidVersionByte = 6 * one
	uuidVariantByte = 8 * one
	uuidMillisHigh  = 2 * one
	uuidMillisEnd   = 6 * one
	uuidDashes      = []int{8 * one, 12 * one, 16 * one, 20 * one}
)
