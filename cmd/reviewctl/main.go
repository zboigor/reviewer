package main

import (
	"log/slog"
	"os"

	"reviewsrv/pkg/reviewer/ctl"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	cfg := &ctl.Config{}

	rootCmd := &cobra.Command{
		Use:          "reviewctl",
		Short:        "AI code review orchestrator",
		SilenceUsage: true,
	}

	pf := rootCmd.PersistentFlags()
	pf.StringVar(&cfg.Key, "key", os.Getenv("PROJECT_KEY"), "project key (UUID)")
	pf.StringVar(&cfg.URL, "url", os.Getenv("REVIEWSRV_URL"), "reviewsrv server URL")
	pf.StringVar(&cfg.PublicURL, "public-url", os.Getenv("REVIEWSRV_PUBLIC_URL"), "browser-facing base URL for links (defaults to --url)")
	pf.StringVar(&cfg.Model, "model", envDefault("REVIEW_MODEL", "opus"), "Claude model")
	pf.StringVar(&cfg.Dir, "dir", envDefault("REVIEW_DIR", "."), "working directory with review files")
	pf.BoolVar(&cfg.Verbose, "verbose", os.Getenv("REVIEW_VERBOSE") == "true", "verbose output")
	pf.StringVar(&cfg.SourceBranch, "source-branch", "", "source branch")
	pf.StringVar(&cfg.TargetBranch, "target-branch", "", "target branch")
	pf.StringVar(&cfg.Commit, "commit", "", "commit SHA")
	pf.StringVar(&cfg.Author, "author", "", "PR author")
	pf.StringVar(&cfg.MRTitle, "pr-title", "", "PR title")
	pf.StringVar(&cfg.ExternalID, "external-id", "", "external ID")
	pf.StringVar(&cfg.SessionID, "session", "", "Claude session ID for --resume (reuses prompt cache)")
	pf.BoolVar(&cfg.ContinueSession, "continue", false, "continue last Claude session (auto-detect)")

	reviewCmd := &cobra.Command{
		Use:   "review",
		Short: "Full review cycle: prompt → Claude → upload → comment → HTML",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.Validate("review"); err != nil {
				return err
			}
			log := slog.Default()
			runner := &ctl.ExecClaudeRunner{Model: cfg.Model, Dir: cfg.Dir, SessionID: cfg.SessionID, ContinueSession: cfg.ContinueSession, Log: log}
			c := ctl.NewController(cfg, runner, log)
			return c.Review(cmd.Context())
		},
	}

	uploadCmd := &cobra.Command{
		Use:   "upload",
		Short: "Upload local review.json + R*.md to server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.Validate("upload"); err != nil {
				return err
			}
			c := ctl.NewController(cfg, nil, slog.Default())
			return c.Upload(cmd.Context())
		},
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write([]byte("reviewctl " + version + "\n"))
			return err
		},
	}

	rootCmd.AddCommand(reviewCmd, uploadCmd, versionCmd)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
