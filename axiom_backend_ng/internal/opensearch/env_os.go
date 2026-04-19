package opensearch

import "os"

// osGetenv is indirected so tests can override the fallback lookup.
var osGetenv = os.Getenv
