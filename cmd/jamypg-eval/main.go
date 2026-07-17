// jamypg-eval runs the golden-query evaluation from the command line so CI and
// operators can track accuracy without an MCP client.
//
// Usage:
//
//	go run ./cmd/jamypg-eval -data ./data/metadb [-golden path] [-top-k 5] [-verbose]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"sqlon/internal/catalog"
	"sqlon/internal/dbconn"
)

func main() {
	var (
		dataDir    string
		goldenPath string
		topK       int
		verbose    bool
		profile    string
	)
	flag.StringVar(&dataDir, "data", filepath.Join("data", "metadb"), "Path to metadata dataset directory")
	flag.StringVar(&goldenPath, "golden", "", "Path to golden_queries.json (default <data>/golden_queries.json)")
	flag.IntVar(&topK, "top-k", 5, "Search depth for table-selection accuracy")
	flag.BoolVar(&verbose, "verbose", false, "Print per-case results including misses")
	flag.StringVar(&profile, "profile", "", "DB profile id for execution-based checks (postgres/mysql/mariadb)")
	flag.Parse()

	cat, err := catalog.Load(dataDir)
	if err != nil {
		log.Fatalf("load catalog: %v", err)
	}
	var counter catalog.RowCounter
	if profile != "" {
		mgr := dbconn.NewManager(dataDir)
		defer mgr.Close()
		counter = func(ctx context.Context, sql string) (int64, error) {
			return mgr.CountRows(ctx, profile, sql)
		}
	}
	res, err := cat.RunEvaluationExec(context.Background(), goldenPath, topK, counter)
	if err != nil {
		log.Fatalf("run evaluation: %v", err)
	}
	results := res["results"].([]catalog.EvalCaseResult)
	summary := map[string]any{}
	for k, v := range res {
		if k != "results" {
			summary[k] = v
		}
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(b))
	if verbose {
		for _, r := range results {
			status := "ok"
			if !r.TableHit || !r.JoinPathOK || !r.MetricHit {
				status = "MISS"
			}
			fmt.Printf("[%s] table_hit=%t rank=%d col_recall=%.2f join=%t %.70s\n",
				status, r.TableHit, r.TableRank, r.ColumnRecall, r.JoinPathOK, r.Question)
			for _, m := range r.Missing {
				fmt.Printf("       missing: %s\n", m)
			}
		}
	}
	if acc, ok := summary["table_selection_acc"].(float64); ok && acc < 0.7 {
		fmt.Fprintln(os.Stderr, "table_selection_acc below 0.7")
		os.Exit(1)
	}
}
