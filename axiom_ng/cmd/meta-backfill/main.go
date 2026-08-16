// Retroactive metadata backfill (#159): re-derives title/year/publisher for
// statute and report documents from the canonical raw_data using the SAME
// zotero.Normalize mapping the sync projection uses. Delta syncs never pick
// these up (zotero_version unchanged), so this heals the 4 title-less statutes
// (nameOfAct) and the phantom publisher-less reports (institution).
//
// Updates only rows where a value actually differs (IS DISTINCT FROM), so a
// second run reports 0 changes — that idempotence is the success criterion.
// Chunks/index are untouched (metadata-only; search hydrates titles from DB).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
)

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func yearArg(y *int) any {
	if y == nil {
		return nil
	}
	return *y
}

func main() {
	dry := flag.Bool("dry", false, "print would-be changes without writing")
	dsn := flag.String("dsn", os.Getenv("AXIOM_DATABASE_URL"), "database DSN (default AXIOM_DATABASE_URL)")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "no DSN (set AXIOM_DATABASE_URL or -dsn)")
		os.Exit(1)
	}

	ctx := context.Background()
	d, err := db.Open(ctx, *dsn)
	if err != nil {
		fatal("connect: %v", err)
	}
	defer d.Close()

	rows, err := d.Pool().Query(ctx, `
		SELECT source_id::text, zotero_key, raw_data::text
		FROM zotero_items
		WHERE item_type IN ('statute','report') AND NOT deleted
		ORDER BY zotero_key`)
	if err != nil {
		fatal("select items: %v", err)
	}
	defer rows.Close()

	type item struct {
		src, key, raw string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.src, &it.key, &it.raw); err != nil {
			fatal("scan: %v", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		fatal("rows: %v", err)
	}

	changed := 0
	for _, it := range items {
		nm := zotero.Normalize([]byte(it.raw))
		if nm.ItemType != "statute" && nm.ItemType != "report" {
			continue
		}
		title, year, date, publisher := nullIfEmpty(nm.Title), yearArg(nm.PublicationYear), nullIfEmpty(nm.Date), nullIfEmpty(nm.Publisher)
		if *dry {
			var exists bool
			if err := d.Pool().QueryRow(ctx, `
				SELECT (title IS DISTINCT FROM $3 OR publication_year IS DISTINCT FROM $4
				     OR publication_date IS DISTINCT FROM $5 OR publisher IS DISTINCT FROM $6)
				FROM zotero_documents WHERE source_id=$1 AND zotero_key=$2`,
				it.src, it.key, title, year, date, publisher).Scan(&exists); err != nil {
				fatal("dry check %s: %v", it.key, err)
			}
			if exists {
				changed++
				fmt.Printf("WOULD UPDATE %-10s %s -> title=%v year=%v date=%v publisher=%v\n", nm.ItemType, it.key, title, year, date, publisher)
			}
			continue
		}
		tag, err := d.Pool().Exec(ctx, `
			UPDATE zotero_documents SET
				title=$3, publication_year=$4, publication_date=$5, publisher=$6, updated_at=now()
			WHERE source_id=$1 AND zotero_key=$2
			  AND (title IS DISTINCT FROM $3 OR publication_year IS DISTINCT FROM $4
			   OR publication_date IS DISTINCT FROM $5 OR publisher IS DISTINCT FROM $6)`,
			it.src, it.key, title, year, date, publisher)
		if err != nil {
			fatal("update %s: %v", it.key, err)
		}
		if tag.RowsAffected() > 0 {
			changed++
			fmt.Printf("updated %-10s %s -> title=%v year=%v date=%v publisher=%v\n", nm.ItemType, it.key, title, year, date, publisher)
		}
	}
	fmt.Printf("\n%d/%d statutes+reports changed (re-run must report 0)\n", changed, len(items))
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "meta-backfill: "+f+"\n", a...)
	os.Exit(1)
}
