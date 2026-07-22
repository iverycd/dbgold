package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := run(os.Args[1:]); err != nil {
		log.Printf("cdcstress: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printHelp()
		return nil
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", "cdcstress.json", "path to non-secret JSON configuration")
	runID := flags.String("run-id", "", "existing or new run ID")
	mode := flags.String("mode", "both", "full_then_cdc, incremental_only, or both")
	target := flags.Bool("target", false, "also create target tables (required for a fresh incremental_only run)")
	confirm := flags.String("confirm-run-id", "", "required cleanup confirmation")
	manualRestart := flags.Bool("manual-restart", false, "pause for a manual dbgold restart scenario")
	startTPS := flags.Int("start-tps", 0, "skip configured TPS steps below this value when resuming")
	resumeFrom := flags.String("resume-from", "", "legacy state recovery point (pause-resume)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	switch command {
	case "precheck":
		return precheck(ctx, cfg)
	case "prepare":
		id := *runID
		if id == "" {
			id = makeRunID()
		}
		state := newRunState(cfg, id)
		if _, statErr := os.Stat(statePath(cfg, id)); statErr == nil {
			state, err = loadState(cfg, id)
			if err != nil {
				return err
			}
		}
		if err = prepare(ctx, cfg, &state, *target); err != nil {
			return err
		}
		fmt.Printf("prepared run_id=%s state=%s\n", id, statePath(cfg, id))
		return nil
	case "run":
		if *runID == "" {
			return errors.New("run requires --run-id from prepare")
		}
		if *mode != "full_then_cdc" && *mode != "incremental_only" && *mode != "both" {
			return fmt.Errorf("invalid --mode %q", *mode)
		}
		state, loadErr := loadState(cfg, *runID)
		if loadErr != nil {
			return loadErr
		}
		return executeRun(ctx, cfg, &state, *mode, *manualRestart, *startTPS, *resumeFrom)
	case "verify":
		if *runID == "" {
			return errors.New("verify requires --run-id")
		}
		state, loadErr := loadState(cfg, *runID)
		if loadErr != nil {
			return loadErr
		}
		_, err = verifyAndReport(ctx, cfg, &state)
		return err
	case "cleanup":
		if *runID == "" || *confirm == "" || *runID != *confirm {
			return errors.New("cleanup requires matching --run-id and --confirm-run-id")
		}
		state, loadErr := loadState(cfg, *runID)
		if loadErr != nil {
			return loadErr
		}
		return cleanup(ctx, cfg, state, *confirm)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func makeRunID() string { return "run_" + time.Now().UTC().Format("20060102T150405Z") }

func printHelp() {
	fmt.Print(strings.TrimSpace(`
MySQL -> GaussDB CDC production-scale test harness

Usage:
  go run ./cmd/cdcstress precheck -config cdcstress.json
  go run ./cmd/cdcstress prepare  -config cdcstress.json [-run-id ID] [-target]
  go run ./cmd/cdcstress run      -config cdcstress.json -run-id ID [-mode both] [-manual-restart] [-resume-from pause-resume]
  go run ./cmd/cdcstress verify   -config cdcstress.json -run-id ID
  go run ./cmd/cdcstress cleanup  -config cdcstress.json -run-id ID -confirm-run-id ID

Secrets are read only from CDCSTRESS_MYSQL_PASSWORD, CDCSTRESS_GAUSSDB_PASSWORD,
and either CDCSTRESS_DBGOLD_TOKEN or CDCSTRESS_DBGOLD_USERNAME plus
CDCSTRESS_DBGOLD_PASSWORD.
`))
	fmt.Println()
}
