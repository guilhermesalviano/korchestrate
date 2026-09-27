// Command kor is the korchestrate orchestrator: it plans with
// claude, executes with codex and reviews with opencode, all inside an isolated
// git worktree.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.1.0"

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "kor: "+err.Error())
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		configPath   string
		repo         string
		artifactsDir string
	)

	root := &cobra.Command{
		Use:           "kor",
		Short:         "korchestrate — plan/execute/review agent orchestrator",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDashboardDefault(cmd, configPath, repo, artifactsDir, false, false, false, false)
		},
	}

	root.PersistentFlags().StringVar(&configPath, "config", "", "path to JSON or YAML config (searches repo, then ~/.config/kor; YAML takes precedence)")
	root.PersistentFlags().StringVar(&repo, "repo", ".", "target git repository")
	root.PersistentFlags().StringVar(&artifactsDir, "artifacts-dir", "", "override the run artifacts directory")

	root.AddCommand(
		newRunCmd(&configPath, &repo, &artifactsDir),
		newDashboardCmd(&configPath, &repo, &artifactsDir),
		newWebCmd(&configPath, &repo, &artifactsDir),
		newResumeCmd(&configPath, &artifactsDir),
		newListCmd(&artifactsDir),
		newStatusCmd(&artifactsDir),
		newLogsCmd(&artifactsDir),
		newCleanCmd(&artifactsDir),
		newDoctorCmd(&configPath, &repo),
		newVersionCmd(),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print the kor version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "kor "+version)
		},
	}
}
