// Command exit-chain-verify exports and verifies the MMR exit-drill
// evidence chain (I21): resource_plan_id → model_route_decision_id →
// entrypoint/recipe → pre/post outcomes → findings/remediation → audit
// trail, with the ARR/Agent zero-change proofs left for the operator's
// documented diff commands. Writes the JSON chain to stdout (the
// Go/No-Go pack input) and exits non-zero when the chain is broken.
//
// Usage:
//
//	exit-chain-verify -dsn <postgres-dsn> -drill <drill-key> [-arr-zero] [-agent-zero]
//
// The -arr-zero/-agent-zero flags record the operator's sign-off AFTER
// running the export-diff commands in the runbook — never before.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/exitdrillverify"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("SAOAF_DB_DSN"), "PostgreSQL DSN (or SAOAF_DB_DSN)")
	drill := flag.String("drill", "", "exit drill key to verify")
	plans := flag.String("plan", "", "comma-separated resource plan ids this drill exercised (EXACT attribution; empty = provider-scope fallback — see the runbook)")
	arrZero := flag.Bool("arr-zero", false, "ARR config zero-change proof signed off (run the runbook diff first)")
	agentZero := flag.Bool("agent-zero", false, "Agent code zero-change proof signed off (run the runbook diff first)")
	flag.Parse()
	if *dsn == "" || *drill == "" {
		fmt.Fprintln(os.Stderr, "usage: exit-chain-verify -dsn <postgres-dsn> -drill <drill-key> [-arr-zero] [-agent-zero]")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	var planList []string
	if *plans != "" {
		for _, p := range strings.Split(*plans, ",") {
			if p = strings.TrimSpace(p); p != "" {
				planList = append(planList, p)
			}
		}
	}
	ch, err := (exitdrillverify.Checker{Pool: pool}).VerifyDrillChainScoped(ctx, *drill, planList)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chain verify failed: %v\n", err)
		os.Exit(1)
	}
	ch.MarkARRZeroChange(*arrZero)
	ch.MarkAgentZeroChange(*agentZero)

	b, err := json.MarshalIndent(ch, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(b))

	verdict, reasons := ch.GoNoGo()
	if verdict != "GO" {
		fmt.Fprintln(os.Stderr, "VERDICT: NO-GO")
		for _, r := range reasons {
			fmt.Fprintf(os.Stderr, "  - %s\n", r)
		}
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "VERDICT: GO")
}
