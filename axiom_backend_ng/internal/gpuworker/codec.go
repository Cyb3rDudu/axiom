package gpuworker

import "github.com/vmihailenco/msgpack/v5"

// reencode and unmarshal are split into a tiny file so tests can swap
// the msgpack library without touching client logic.
func reencode(v any) ([]byte, error) { return msgpack.Marshal(v) }

func unmarshal(b []byte, v any) error { return msgpack.Unmarshal(b, v) }
