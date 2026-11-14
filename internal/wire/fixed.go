package wire

// Fixed numbers, spelled as arithmetic: the convention of the core's example module
// matter-clock, so the core's settings source check (modulekit.CheckSource, AC-030) can
// tell a fixed rule from a tunable.
var (
	zero = int(0)
	one  = zero + 1
	// decimal is the base of the signature header's Unix timestamp.
	decimal = 10 * one
)
