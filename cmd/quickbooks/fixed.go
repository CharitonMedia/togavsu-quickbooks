package main

import "os"

// Fixed numbers, spelled as arithmetic: the convention of the core's example module
// matter-clock, so the core's settings source check (modulekit.CheckSource, AC-030) can
// tell a fixed rule from a tunable.
var (
	zero = int(0)
	one  = zero + 1
	// usageExit is the exit status of a command line the program can't read.
	usageExit = 2 * one
	// packageMode: the signed package file is readable by all and writable by its owner.
	packageMode = os.FileMode(zero) | 0o644
)
