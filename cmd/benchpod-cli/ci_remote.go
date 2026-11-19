package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/spf13/cobra"
)

// ── ci list, ci show, ci diff: runs here or on embeddedci.com ───────────────

// isLocalRun says arg names a run folder or a run.json on this machine (else it is a run id on
// embeddedci.com).
func isLocalRun(arg string) bool {
	_, err := os.Stat(arg)
	return err == nil
}

func remoteErr(what string, err error) error {
	return &ciExit{code: ci.ExitSetup, msg: what + ": " + err.Error()}
}

func newCIListCmd() *cobra.Command {
	var f serverapi.RunFilter
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the runs uploaded to embeddedci.com, newest first",
		Long: "List the organization's runs on embeddedci.com, newest first, filtered by suite, DUT,\n" +
			"build, pod or branch. --before takes the next_before of the previous page.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			api, cred, err := runsAPI(ctx)
			if err != nil {
				return remoteErr("list", err)
			}
			list, err := api.ListRuns(ctx, cred, f)
			if err != nil {
				return remoteErr("list", err)
			}
			if asJSON {
				data, _ := json.MarshalIndent(list, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return nil
			}
			printRunList(cmd.OutOrStdout(), list)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.Suite, "suite", "", "only this suite")
	fl.StringVar(&f.DUT, "dut", "", "only this DUT")
	fl.StringVar(&f.Build, "build", "", "only the runs of this build (its id on embeddedci.com)")
	fl.StringVar(&f.Device, "pod", "", "only the runs on this pod (its name or id)")
	fl.StringVar(&f.Ref, "ref", "", "only the runs of tests on this branch")
	fl.StringVar(&f.Before, "before", "", "the page after this time (next_before of the previous page)")
	fl.IntVar(&f.Limit, "limit", 20, "how many runs (at most 200)")
	fl.BoolVar(&asJSON, "json", false, "print the server's reply as JSON")
	return cmd
}

func printRunList(w io.Writer, l *serverapi.RunList) {
	if len(l.Runs) == 0 {
		fmt.Fprintln(w, "No runs.")
		return
	}
	pods := l.OrgPods > 1
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := "STARTED\tSUITE\tDUT\tOUTCOME\tPASSED\tFAILED\tREF"
	if pods {
		head += "\tPOD"
	}
	fmt.Fprintln(tw, head+"\tID")
	for _, r := range l.Runs {
		started := r.Started
		if t, err := time.Parse(time.RFC3339Nano, r.Started); err == nil {
			started = t.Local().Format("2006-01-02 15:04")
		}
		outcome := r.Outcome
		if !r.Valid {
			outcome = "invalid"
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%d\t%s", started, r.Suite, r.DUT, outcome, r.Counts["passed"],
			r.Counts["failed"]+r.Counts["error"], valueOrDash(r.TestsRef))
		if pods {
			line += "\t" + valueOrDash(r.DeviceName)
		}
		fmt.Fprintln(tw, line+"\t"+r.ID)
	}
	tw.Flush()
	if l.NextBefore != "" {
		fmt.Fprintf(w, "More: --before %s\n", l.NextBefore)
	}
}

func newCIShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <run id|run folder>",
		Short: "Show one run: its outcome, inputs, failed cases and measurements",
		Long: "Show one run, from embeddedci.com (its run id) or from a local run folder or run.json:\n" +
			"suite, DUT, outcome and counts, the pod, firmware and tests commit, the failed cases\n" +
			"with their messages, and the measurements.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw []byte
			if isLocalRun(args[0]) {
				r, err := ci.ReadRun(args[0])
				if err != nil {
					return &ciExit{code: ci.ExitSetup, msg: err.Error()}
				}
				raw, _ = json.Marshal(r)
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				api, cred, err := runsAPI(ctx)
				if err != nil {
					return remoteErr("show", err)
				}
				raw, err = api.GetRun(ctx, cred, args[0])
				if err != nil {
					return remoteErr("show", err)
				}
			}
			if asJSON {
				var v any
				_ = json.Unmarshal(raw, &v)
				data, _ := json.MarshalIndent(v, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return nil
			}
			var r ci.Run
			if err := json.Unmarshal(raw, &r); err != nil {
				return &ciExit{code: ci.ExitSetup, msg: "show: " + err.Error()}
			}
			var extra struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			}
			_ = json.Unmarshal(raw, &extra)
			printRun(cmd.OutOrStdout(), &r, extra.ID, extra.URL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the run as JSON")
	return cmd
}

func printRun(w io.Writer, r *ci.Run, id, url string) {
	if url == "" && r.Uploaded != nil {
		id, url = r.Uploaded.ID, r.Uploaded.URL
	}
	fmt.Fprintf(w, "%s (dut %s): %s\n", r.Key.Suite, r.Key.DUT, strings.ToUpper(r.Results.Outcome))
	c := r.Results.Counts
	fmt.Fprintf(w, "  %d passed, %d failed, %d error, %d skipped in %s\n", c.Passed, c.Failed, c.Error, c.Skipped,
		(time.Duration(r.Results.DurationMS) * time.Millisecond).Round(100*time.Millisecond))
	fmt.Fprintf(w, "  run %s, started %s (%s)\n", firstNonEmptyStr(id, r.RunID), r.Started, r.Trigger)
	if url != "" {
		fmt.Fprintf(w, "  %s\n", url)
	}
	in := r.Inputs
	if p := in.Pod; p.Name != "" || p.Firmware != "" {
		fmt.Fprintf(w, "  pod %s: %s, firmware %s, gateware %d, %s\n", valueOrDash(p.Name), valueOrDash(p.Board), valueOrDash(p.Firmware), p.Gateware, valueOrDash(p.Connection))
	}
	if f := in.DUTFirmware; f.File != "" || f.BuildID != "" {
		sha := f.SHA256
		if len(sha) > 12 {
			sha = sha[:12]
		}
		fmt.Fprintf(w, "  firmware %s %s %s\n", valueOrDash(f.File), sha, f.BuildID)
	}
	if t := in.Tests; t.SHA != "" {
		dirty := ""
		if t.Dirty {
			dirty = " (with changes)"
		}
		sha := t.SHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		fmt.Fprintf(w, "  tests %s %s %s%s\n", valueOrDash(t.Repo), valueOrDash(t.Ref), sha, dirty)
	}
	if in.Wiring.Name != "" {
		fmt.Fprintf(w, "  wiring %s\n", in.Wiring.Name)
	}
	if !r.Health.Valid {
		fmt.Fprintf(w, "  not valid: %s\n", strings.Join(r.Health.Reasons, "; "))
	}
	if failed := r.FailedCases(); len(failed) > 0 {
		fmt.Fprintln(w, "\nFailed:")
		for _, fc := range failed {
			att := ""
			if fc.Attempt > 1 {
				att = fmt.Sprintf(" [attempt %d]", fc.Attempt)
			}
			fmt.Fprintf(w, "  %s%s (%s)\n", fc.ID, att, fc.Outcome)
			for _, l := range strings.Split(strings.TrimSpace(ci.Cap(fc.Message, 2000)), "\n") {
				fmt.Fprintf(w, "      %s\n", l)
			}
		}
	}
	var rows [][]string
	for _, cs := range r.Results.Cases {
		for _, m := range cs.Measurements {
			var lim []string
			if m.Min != nil {
				lim = append(lim, fmt.Sprintf("min %g", *m.Min))
			}
			if m.Max != nil {
				lim = append(lim, fmt.Sprintf("max %g", *m.Max))
			}
			if m.Drift != nil {
				lim = append(lim, fmt.Sprintf("drift %g%%", *m.Drift*100))
			}
			rows = append(rows, []string{cs.ID, m.Name, strings.TrimSpace(fmt.Sprintf("%g %s", m.Value, m.Unit)), valueOrDash(strings.Join(lim, ", ")), m.Outcome})
		}
	}
	if len(rows) > 0 {
		fmt.Fprintln(w, "\nMeasurements:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  CASE\tNAME\tVALUE\tLIMITS\tOUTCOME")
		for _, row := range rows {
			fmt.Fprintln(tw, "  "+strings.Join(row, "\t"))
		}
		tw.Flush()
	}
}

func newCIDiffCmd() *cobra.Command {
	var asJSON bool
	var baseline string
	cmd := &cobra.Command{
		Use:   "diff <a> [<b>]",
		Short: "Compare two runs (run folders, run.json files or run ids on embeddedci.com)",
		Long: "Compare run a (the older) with run b: which cases newly fail, were fixed, still fail,\n" +
			"are new, removed or newly skipped; each measurement in both with its change and whether\n" +
			"it drifted past its drift or failed its limit; cases and pod commands that got slower;\n" +
			"and the inputs that changed (firmware, pod, tools, tests commit).\n\n" +
			"Two run folders (or run.json files) are compared here. Run ids are compared by\n" +
			"embeddedci.com: `ci diff <id>` compares that run with the previous valid run of its\n" +
			"suite and DUT (--baseline main: the last one on that branch), `ci diff <a> <b>` the two.\n\n" +
			"Runs compare when they have the same suite and dut, are both valid and used the same\n" +
			"wiring; otherwise the diff says why they are incomparable. A different pod, board or\n" +
			"connection is compared and flagged.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			local := 0
			for _, a := range args {
				if isLocalRun(a) {
					local++
				}
			}
			if local == 2 {
				if baseline != "" {
					return &ciExit{code: ci.ExitSetup, msg: "--baseline compares runs on embeddedci.com: give one run id"}
				}
				a, err := ci.ReadRun(args[0])
				if err != nil {
					return &ciExit{code: ci.ExitSetup, msg: err.Error()}
				}
				b, err := ci.ReadRun(args[1])
				if err != nil {
					return &ciExit{code: ci.ExitSetup, msg: err.Error()}
				}
				d := ci.Compare(a, b)
				if asJSON {
					data, _ := json.MarshalIndent(d, "", "  ")
					fmt.Fprintln(out, string(data))
					return nil
				}
				d.WriteText(out)
				return nil
			}
			if local > 0 {
				msg := "give two run folders (compared here) or run ids on embeddedci.com, not one of each"
				if len(args) == 1 {
					msg = "one run folder has nothing to compare with: give two run folders, or a run id on embeddedci.com (upload it with `benchpod ci upload`)"
				}
				return &ciExit{code: ci.ExitSetup, msg: msg}
			}
			id, with := args[0], "previous"
			switch {
			case len(args) == 2:
				id, with = args[1], args[0]
				if baseline != "" {
					return &ciExit{code: ci.ExitSetup, msg: "--baseline takes one run id"}
				}
			case baseline != "":
				with = "baseline:" + baseline
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			api, cred, err := runsAPI(ctx)
			if err != nil {
				return remoteErr("diff", err)
			}
			raw, err := api.CompareRun(ctx, cred, id, with)
			if err != nil {
				return remoteErr("diff", err)
			}
			if asJSON {
				var v any
				_ = json.Unmarshal(raw, &v)
				data, _ := json.MarshalIndent(v, "", "  ")
				fmt.Fprintln(out, string(data))
				return nil
			}
			var d ci.Diff
			if err := json.Unmarshal(raw, &d); err != nil {
				return remoteErr("diff", err)
			}
			if !d.Found {
				var m struct {
					Message string `json:"message"`
				}
				_ = json.Unmarshal(raw, &m)
				fmt.Fprintln(out, firstNonEmptyStr(m.Message, "no run to compare with"))
				return nil
			}
			d.WriteText(out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the diff as JSON")
	cmd.Flags().StringVar(&baseline, "baseline", "", "with one run id: compare with the last valid run of its suite and DUT on this branch")
	return cmd
}
