package fake

import "time"

// Fixed numbers, spelled as arithmetic: the convention of the core's example module
// matter-clock, so the core's settings source check (modulekit.CheckSource, AC-030) can
// tell a fixed rule from a tunable. Each mirrors a rule of the QuickBooks Online API that
// the fake stands in for.
var (
	zero = int(0)
	one  = zero + 1
	hour = time.Duration(60 * 60 * 1000 * 1000 * 1000 * one)
	// firstEntityID is the id the fake gives a company's first entity.
	firstEntityID = 1000 * one
	// minJournalLines: a journal entry has at least a debit and a credit line.
	minJournalLines = 2 * one
)

// Lifetimes of issued tokens on the fake's clock: Intuit's one-hour access token and
// hundred-day refresh token.
var (
	AccessLifetime  = hour
	RefreshLifetime = 100 * 24 * hour
)
