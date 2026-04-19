package ingest

import "context"

// Chain runs a fixed sequence of processors against every claimed job.
// The chain fails fast: the first processor to return an error aborts
// the run, and the pool marks the document as failed with that error.
// A nil processor in the slice is skipped — handy when a stage is
// feature-flagged off but the caller still wants a single pipeline
// definition.
type Chain []Processor

// Process implements Processor.
func (c Chain) Process(ctx context.Context, job Job) error {
	for _, p := range c {
		if p == nil {
			continue
		}
		if err := p.Process(ctx, job); err != nil {
			return err
		}
	}
	return nil
}
