package utils

import "time"

// EAT is East Africa Time (UTC+3, no daylight saving). The server runs on UTC
// and stores UTC; anything shown to customers or staff is converted with
// Kenya(t) so it reads in their local time.
var EAT = time.FixedZone("EAT", 3*60*60)

// Kenya converts t to East Africa Time.
func Kenya(t time.Time) time.Time { return t.In(EAT) }
