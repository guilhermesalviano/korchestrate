package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/pipeline"
	"github.com/guilhermesalviano/korchestrate/internal/preflight"
)

func newListCmd(artifactsDir *string) *cobra.Command {
	var failed bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, err := artifact.List(artifactBase(*artifactsDir))
			if err != nil {
				return err
			}
			if failed {
				kept := runs[:0]
				for _, r := range runs {
					if r.State == artifact.StateFailed || r.State == artifact.StateAborted {
						kept = append(kept, r)
					}
				}
				runs = kept
			}
			if len(runs) == 0 {
				if failed {
					fmt.Fprintln(cmd.OutOrStdout(), "no failed or aborted runs")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "no runs")
				}
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTATE\tITER\tCREATED\tERROR\tPROMPT")
			for _, r := range runs {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
					r.ID, r.State, r.Iteration,
					r.CreatedAt.Format("2006-01-02 15:04"),
					truncate(errorSummary(r), 40),
					truncate(r.Prompt, 50))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&failed, "failed", false, "show only failed or aborted runs")
	return cmd
}

// errorSummary is the run's most recent error, or "-" when it has none.
func errorSummary(r *artifact.Run) string {
	switch {
	case len(r.Errors) > 0:
		return firstLineOf(r.Errors[len(r.Errors)-1].Message)
	case r.Error != "":
		return firstLineOf(r.Error)
	}
	return "-"
}

// firstLineOf collapses multi-line errors to their first line.
func firstLineOf(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

func newStatusCmd(artifactsDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status <run-id>",
		Short: "show details of a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := artifact.LoadByID(artifactBase(*artifactsDir), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "id:        %s\n", r.ID)
			fmt.Fprintf(out, "state:     %s\n", r.State)
			fmt.Fprintf(out, "repo:      %s\n", r.Repo)
			fmt.Fprintf(out, "branch:    %s\n", r.Branch)
			fmt.Fprintf(out, "worktree:  %s\n", r.Worktree)
			fmt.Fprintf(out, "dir:       %s\n", r.Dir)
			fmt.Fprintf(out, "iteration: %d\n", r.Iteration)
			fmt.Fprintf(out, "commit:    %s\n", r.Commit)
			fmt.Fprintf(out, "created:   %s\n", r.CreatedAt.Format("2006-01-02 15:04:05"))
			fmt.Fprintf(out, "usage:     in=%d out=%d cost=$%.4f\n",
				r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CostUSD)
			if len(r.Errors) == 0 && r.Error != "" {
				fmt.Fprintf(out, "error:     %s\n", r.Error)
			}
			if len(r.Errors) > 0 {
				fmt.Fprintf(out, "errors:    %d\n", len(r.Errors))
				for _, e := range r.Errors {
					stage := e.Stage
					if stage == "" {
						stage = "run"
					}
					fmt.Fprintf(out, "  %s  %-9s %s\n",
						e.At.Format("2006-01-02 15:04:05"), stage, firstLineOf(e.Message))
				}
			}
			fmt.Fprintf(out, "prompt:    %s\n", r.Prompt)
			if _, err := os.Stat(r.Path(artifact.LogFile)); err == nil {
				fmt.Fprintf(out, "log:       %s\n", r.Path(artifact.LogFile))
				fmt.Fprintf(out, "           kor logs %s --errors\n", r.ID)
			}

			if data, err := r.Read("review.json"); err == nil {
				var v any
				if json.Unmarshal(data, &v) == nil {
					pretty, _ := json.MarshalIndent(v, "", "  ")
					fmt.Fprintf(out, "\nreview.json:\n%s\n", pretty)
				}
			}
			return nil
		},
	}
}

func newCleanCmd(artifactsDir *string) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "clean [run-id]",
		Short: "remove a run's artifacts, and its worktree/branch if present",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base := artifactBase(*artifactsDir)
			var runs []*artifact.Run
			switch {
			case all:
				r, err := artifact.List(base)
				if err != nil {
					return err
				}
				runs = r
			case len(args) == 1:
				r, err := artifact.LoadByID(base, args[0])
				if err != nil {
					return err
				}
				runs = []*artifact.Run{r}
			default:
				return fmt.Errorf("provide a run-id or --all")
			}
			for _, r := range runs {
				warn, err := pipeline.Discard(r)
				if warn != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warn: %v\n", warn)
				}
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", r.ID)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove every run")
	return cmd
}

func newDoctorCmd(configPath, repo *string) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "check CLIs, credentials and git state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig(*configPath, *repo, "")
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "repo:      %s\n", cfg.Repo)
			fmt.Fprintf(out, "artifacts: %s\n", cfg.ArtifactsDir)
			fmt.Fprintf(out, "planner:   %s (%s)\n", cfg.Models.Planner.Model, cfg.Models.Planner.Agent)
			fmt.Fprintf(out, "executor:  %s (%s, sandbox=%s)\n", cfg.Models.Executor.Model, cfg.Models.Executor.Agent, cfg.Models.Executor.Sandbox)
			fmt.Fprintf(out, "reviewer:  %s (%s)\n\n", cfg.Models.Reviewer.Model, cfg.Models.Reviewer.Agent)

			checks := preflight.Checks(cfg.Repo,
				cfg.Models.Planner.Agent, cfg.Models.Executor.Agent, cfg.Models.Reviewer.Agent)
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "STATUS\tCHECK\tDETAIL")
			for _, c := range checks {
				status := "ok"
				if !c.OK {
					if c.Fatal {
						status = "FAIL"
					} else {
						status = "warn"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", status, c.Name, c.Detail)
			}
			_ = w.Flush()
			return preflight.Fatal(checks)
		},
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}
