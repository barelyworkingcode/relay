package main

import "time"

var screenJourneys = []journey{
	{staleID, []string{"projects", "grants"}, phaseScreen, 30 * time.Second, runStaleDerivedEdit},
	{"context-number-resave", []string{"projects", "grants", "audit"}, phaseScreen, 30 * time.Second, runContextNumberResave},
}
