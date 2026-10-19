package memory

import "time"

// timeNow is a tiny indirection so tests can freeze time without
// dragging in a clock abstraction. It is set to time.Now by default.
var timeNow = time.Now
