package main
import (
    "fmt"
    "os"
    x "github.com/xberg-io/xberg/packages/go"
)
func main() {
    in := x.ExtractInputFromURI(os.Args[1])
    var cfg x.ExtractionConfig
    res, err := x.Extract(*in, cfg)
    if err != nil { fmt.Println("EXTRACT ERR:", err); os.Exit(1) }
    r := res.Results[0]
    fmt.Printf("pages=%d tables=%d images=%d markdown_len=%d\n", r.Counts.Pages, r.Counts.Tables, r.Counts.Images, len(r.Content))
    for i, pg := range r.Pages {
        fmt.Printf("  page#%d len=%d\n", pg.PageNumber, len(pg.Content))
        if i>5 { break }
    }
    fmt.Printf("languages=%v method=%v\n", r.DetectedLanguages, r.ExtractionMethod)
    os.WriteFile("/tmp/gd/xberg_allpages.md", []byte(r.Content), 0o644)
}
