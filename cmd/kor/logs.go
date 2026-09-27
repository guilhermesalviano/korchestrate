package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/guilhermesalviano/korchestrate/internal/artifact"
)

func newLogsCmd(artifactsDir *string) *cobra.Command {
	var (
		errorsOnly bool
		raw        bool
		follow     bool
	)
	cmd := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "show a run's audit log, with --errors to find failures",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := artifact.LoadByID(artifactBase(*artifactsDir), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			printed, shown := 0, 0
			for {
				entries, err := artifact.ReadLog(run.Dir)
				if err != nil {
					return err
				}
				for _, e := range entries[printed:] {
					if errorsOnly && e.Level == artifact.LevelInfo {
						continue
					}
					shown++
					if raw {
						data, _ := json.Marshal(e)
						fmt.Fprintln(out, string(data))
					} else {
						fmt.Fprintln(out, formatLogEntry(e))
					}
				}
				printed = len(entries)
				if !follow || terminalState(run.State) {
					break
				}
				time.Sleep(500 * time.Millisecond)
				if updated, err := artifact.Load(run.Dir); err == nil {
					run = updated
				}
			}
			if shown == 0 {
				switch {
				case printed == 0:
					fmt.Fprintln(out, "no log entries for this run")
				case errorsOnly:
					fmt.Fprintln(out, "no warnings or errors for this run")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&errorsOnly, "errors", "e", false, "show only warnings and errors, including recovered failures")
	cmd.Flags().BoolVar(&raw, "json", false, "print raw JSONL entries for jq and friends")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new entries until the run finishes")
	return cmd
}

func terminalState(s artifact.State) bool {
	switch s {
	case artifact.StateDone, artifact.StateFailed, artifact.StateAborted:
		return true
	}
	return false
}

// formatLogEntry renders one timeline entry as a single grep-able line.
func formatLogEntry(e artifact.LogEntry) string {
	stage := e.Stage
	if stage == "" {
		stage = "-"
	}
	msg := strings.NewReplacer("\r", " ", "\n", " ⏎ ").Replace(strings.TrimSpace(e.Message))
	line := fmt.Sprintf("%s  %-5s  %-9s  %s",
		e.Time.Local().Format("2006-01-02 15:04:05"), strings.ToUpper(e.Level), stage, msg)

	var extra []string
	if e.Event != "" {
		extra = append(extra, "event="+e.Event)
	}
	if e.Agent != "" {
		extra = append(extra, "agent="+e.Agent)
	}
	if e.Model != "" {
		extra = append(extra, "model="+e.Model)
	}
	if e.Iter > 0 {
		extra = append(extra, fmt.Sprintf("iter=%d", e.Iter))
	}
	if e.DurationMS > 0 {
		extra = append(extra, "dur="+(time.Duration(e.DurationMS)*time.Millisecond).Round(time.Millisecond).String())
	}
	if e.ExitCode != nil {
		extra = append(extra, fmt.Sprintf("exit=%d", *e.ExitCode))
	}
	if u := e.Usage; u != nil && (u.InputTokens > 0 || u.OutputTokens > 0 || u.CostUSD > 0) {
		extra = append(extra, fmt.Sprintf("tokens=%d/%d", u.InputTokens, u.OutputTokens))
		if u.CostUSD > 0 {
			extra = append(extra, fmt.Sprintf("cost=$%.4f", u.CostUSD))
		}
	}
	if len(extra) > 0 {
		line += "  · " + strings.Join(extra, " ")
	}
	return line
}
