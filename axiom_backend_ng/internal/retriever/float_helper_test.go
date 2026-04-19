package retriever_test

import "strconv"

// strconvFormatFloat64 is a trivial wrapper kept in its own file so
// integration_test.go can stay focused on the retriever scenarios.
func strconvFormatFloat64(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
