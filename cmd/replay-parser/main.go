// Command replay-parser decodes Artifact (Source 2 DCG) replay files and emits
// a turn-grouped JSON state log and/or an obtrace-style phase trace.
//
// Supports Artifact 2.0 (Foundry, .dem) fully. Artifact 1.0 (Classic, .dcg)
// headers are parsed but its record stream is not yet decoded (see the package
// docs / repo README).
//
// Usage:
//
//	replay-parser [flags] <replay-file> [<replay-file> ...]
//
// Flags:
//
//	-json <path>     write turn-grouped JSON (default: stdout; "-" for stdout)
//	-trace <path>    write obtrace-style phase text
//	-out <dir>       batch mode: write <dir>/<name>.json (+ .trace.txt) per input
//	-payloads        embed base64 of opaque board-state payloads in JSON
//	-chat            include venchatoquist (chat/gesture) events
//	-summary         print a one-line summary per file to stderr
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"artifact/replay-parser/internal/dcg"
)

func main() {
	jsonOut := flag.String("json", "", "write turn-grouped JSON to this path (\"-\" = stdout)")
	traceOut := flag.String("trace", "", "write obtrace-style phase text to this path (\"-\" = stdout)")
	demoInfoOut := flag.String("demoinfo", "", "write demo-info summary JSON to this path (\"-\" = stdout)")
	packetsOut := flag.String("packets", "", "write per-packet JSONL (one record per line) to this path (\"-\" = stdout)")
	outDir := flag.String("out", "", "batch mode: write <dir>/<name>.{json,trace.txt,info.json,packets.jsonl} per input file")
	traces := flag.Bool("traces", true, "include the per-turn decoded trace log in JSON")
	cards := flag.String("cards", "", "path to a card-name table (JSON {\"<id>\":\"<name>\"}) for naming cards")
	traceTypes := flag.String("tracetypes", "", "path to a trace-type-id table (JSON {\"<typeId>\":\"<CTraceMsg name>\"})")
	gcDir := flag.String("gc", "", "directory of GC blob responses (<matchId>.json) to join match metadata into .info.json")
	reportOut := flag.String("report", "", "write a trace-type coverage report (JSON) aggregated over all inputs")
	templateOut := flag.String("tracetemplate", "", "write a -tracetypes metadata template (JSON) to fill in")
	summary := flag.Bool("summary", false, "print a one-line summary per file to stderr")
	flag.Parse()

	files := flag.Args()
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "usage: replay-parser [flags] <replay-file> ...")
		flag.PrintDefaults()
		os.Exit(2)
	}

	cardsPath := *cards
	if cardsPath == "" {
		cardsPath = os.Getenv("DCG_CARDS")
	}
	if cardsPath != "" {
		if err := dcg.LoadCardNames(cardsPath); err != nil {
			fmt.Fprintf(os.Stderr, "cards: %v\n", err)
		}
	}
	if *traceTypes != "" {
		if err := dcg.LoadTraceTypes(*traceTypes); err != nil {
			fmt.Fprintf(os.Stderr, "tracetypes: %v\n", err)
		}
	}
	dcg.SetGCDir(*gcDir)

	reportMode := *reportOut != "" || *templateOut != ""

	// Default to JSON on stdout when nothing else is requested and not batching.
	if *jsonOut == "" && *traceOut == "" && *demoInfoOut == "" && *packetsOut == "" && *outDir == "" && !reportMode {
		*jsonOut = "-"
	}

	opt := dcg.BuildOptions{IncludeTraces: *traces}
	reportReaders := map[string]*dcg.Reader{}

	exit := 0
	for _, f := range files {
		buf, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
			exit = 1
			continue
		}
		r, err := dcg.Parse(f, buf)
		if reportMode && r != nil {
			reportReaders[filepath.Base(f)] = r
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: parse error: %v\n", f, err)
			exit = 1
			if r == nil {
				continue
			}
		}
		if reportMode {
			continue // report is built once, after all files are parsed
		}

		game := dcg.BuildGame(filepath.Base(f), r, opt)

		if *summary {
			fmt.Fprintf(os.Stderr, "%-16s %-14s match=%-9d records=%-5d turns=%-3d decoded=%v\n",
				filepath.Base(f), r.Version, r.MatchID, len(r.Records), len(game.Turns), r.ContainerDecoded)
		}

		if *outDir != "" {
			if err := writeBatch(*outDir, f, game, r); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
				exit = 1
			}
			continue
		}
		if *jsonOut != "" {
			if err := writeJSON(*jsonOut, game, len(files) > 1, f); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
				exit = 1
			}
		}
		if *traceOut != "" {
			if err := writeText(*traceOut, dcg.WriteTrace(r)); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
				exit = 1
			}
		}
		if *demoInfoOut != "" {
			if err := writeAny(*demoInfoOut, dcg.BuildDemoInfo(filepath.Base(f), r)); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
				exit = 1
			}
		}
		if *packetsOut != "" {
			if err := writePackets(*packetsOut, r); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
				exit = 1
			}
		}
	}

	if reportMode && len(reportReaders) > 0 {
		rep := dcg.BuildTraceReport(reportReaders)
		if *reportOut != "" {
			if err := writeAny(*reportOut, rep); err != nil {
				fmt.Fprintf(os.Stderr, "report: %v\n", err)
				exit = 1
			}
		}
		if *templateOut != "" {
			if err := writeAny(*templateOut, rep.TraceTypeTemplate()); err != nil {
				fmt.Fprintf(os.Stderr, "tracetemplate: %v\n", err)
				exit = 1
			}
		}
	}
	os.Exit(exit)
}

func writeBatch(dir, in string, game *dcg.Game, r *dcg.Reader) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.Base(in), filepath.Ext(in))
	jb, err := json.MarshalIndent(game, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, base+".json"), append(jb, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, base+".trace.txt"), []byte(dcg.WriteTrace(r)), 0o644); err != nil {
		return err
	}
	ib, err := json.MarshalIndent(dcg.BuildDemoInfo(filepath.Base(in), r), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, base+".info.json"), append(ib, '\n'), 0o644); err != nil {
		return err
	}
	pf, err := os.Create(filepath.Join(dir, base+".packets.jsonl"))
	if err != nil {
		return err
	}
	defer pf.Close()
	return dcg.WritePacketsJSONL(pf, r)
}

func writeAny(path string, v any) error {
	jb, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	jb = append(jb, '\n')
	if path == "-" {
		_, err = os.Stdout.Write(jb)
		return err
	}
	return os.WriteFile(path, jb, 0o644)
}

func writePackets(path string, r *dcg.Reader) error {
	if path == "-" {
		return dcg.WritePacketsJSONL(os.Stdout, r)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return dcg.WritePacketsJSONL(f, r)
}

func writeJSON(path string, game *dcg.Game, multi bool, name string) error {
	jb, err := json.MarshalIndent(game, "", "  ")
	if err != nil {
		return err
	}
	jb = append(jb, '\n')
	if path == "-" {
		if multi {
			fmt.Printf("// %s\n", name)
		}
		_, err = os.Stdout.Write(jb)
		return err
	}
	return os.WriteFile(path, jb, 0o644)
}

func writeText(path, s string) error {
	if path == "-" {
		_, err := os.Stdout.WriteString(s)
		return err
	}
	return os.WriteFile(path, []byte(s), 0o644)
}
