package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	dsn := os.Getenv("AXIOM_TEST_DATABASE_URL")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil { fmt.Println("CONNECT ERR:", err); return }
	defer conn.Close(ctx)
	// columns of processing_chunks
	rows, err := conn.Query(ctx, "select column_name, data_type from information_schema.columns where table_name='processing_chunks' order by ordinal_position")
	if err != nil { fmt.Println("cols err:", err); return }
	defer rows.Close()
	for rows.Next() {
		var n, t string
		rows.Scan(&n, &t)
		fmt.Printf("%-28s %s\n", n, t)
	}
	fmt.Println("--- count ---")
	var c int
	conn.QueryRow(ctx, "select count(*) from processing_chunks").Scan(&c)
	fmt.Println("processing_chunks count:", c)
	// Block-5 gold-delta sync numbers (#171): DB source-metadata vs OS index.
	conn.QueryRow(ctx, "select count(*) from zotero_documents").Scan(&c)
	fmt.Println("zotero_documents count:", c)
	fmt.Println("OS doc count (separate store, run manually):")
	fmt.Println(`  curl -s $OS_URL/axiom-ng-chunks-v1/_count | jq .count`)
}
