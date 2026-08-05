package gtw

import "time"

// timeNow is the production clock. Inlined behind a package-level
// variable so tests can swap it via deps.Now.
var timeNow = time.Now
