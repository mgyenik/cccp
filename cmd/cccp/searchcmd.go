package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"cccp/internal/present"
	"cccp/internal/search"
)

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print hits as JSON")
	limit := fs.Int("n", 30, "maximum sessions to list")
	_ = fs.Parse(args)
	q := search.ParseQuery(strings.Join(fs.Args(), " "))
	if len(q.Terms) == 0 {
		return errors.New("usage: cccp search [--json] [-n N] TERMS…")
	}
	if _, err := search.Update(); err != nil {
		return err
	}
	hits, err := search.Run(q, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(hits)
	}
	now := time.Now()
	for _, h := range hits {
		fmt.Printf("%s %-5s %-44s %s  (%d)\n", present.AgentBadge(h.Doc.Agent), present.Since(now, h.At), clipStr(h.Doc.Title, 44), h.Doc.Cwd, h.Matches)
		fmt.Printf("      %s: %s\n", h.Role, clipStr(h.Snippet, 140))
		fmt.Printf("      %s\n", h.Doc.Key())
	}
	if len(hits) == 0 {
		fmt.Println("no matches")
	}
	return nil
}
