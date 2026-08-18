package main

// Trace + step-dump instrumentation for the mREBEL perf campaign (#171 / 12-mrebel-perf.md).
// MRBEL_TRACE=1      -> /tmp/mrebel_trace.jsonl : one line per ORT call {chunk,kind,shape,ms}
// MRBEL_DUMP_STEPS=1 -> /tmp/mrebel_steps.jsonl : per beam-step {chunk,path,step,beams:[{ids,score,top6}]}
//                       (first-divergence evidence for the cached-vs-nocache parity diff)

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	traceEnabled bool
	traceMu      sync.Mutex
	curChunk     int
)

var traceFile *os.File

func traceInit() {
	if os.Getenv("MRBEL_TRACE") == "1" {
		f, err := os.OpenFile("/tmp/mrebel_trace.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			traceFile = f
			traceEnabled = true
			fmt.Fprintln(os.Stderr, "[trace] -> /tmp/mrebel_trace.jsonl")
		}
	}
}

// traceT records one ORT call. kind: enc|step1|dec|stepN. shape: "e317" / "L8" / "d8e317".
func traceT(kind, shape string, d time.Duration) {
	if !traceEnabled {
		return
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	fmt.Fprintf(traceFile, "{\"chunk\":%d,\"kind\":%q,\"shape\":%q,\"ms\":%.3f}\n",
		curChunk, kind, shape, float64(d.Microseconds())/1000.0)
}

// ---- step dump ----

type dumpBeam struct {
	Ids   []int64   `json:"ids"`
	Score float64   `json:"score"`
	Top6  [][2]any  `json:"top6,omitempty"` // [token, logp] of the candidates this beam expanded to
}

func dumpStepsOn() bool { return os.Getenv("MRBEL_DUMP_STEPS") == "1" }

func dumpStep(path string, step int, beams []dumpBeam) {
	if !dumpStepsOn() {
		return
	}
	f, err := os.OpenFile("/tmp/mrebel_steps.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]any{"chunk": curChunk, "path": path, "step": step, "beams": beams})
	f.Write(append(b, '\n'))
}
