package cli

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

func newDaemonIndexCmd() *cobra.Command {
	var configFile string
	cmd := &cobra.Command{Use: "index", Short: "Index operations"}
	cmd.PersistentFlags().StringVar(&configFile, "config", "", "path to config file")

	var rescanNew bool
	var rescanSkipBackup bool
	rescanCmd := &cobra.Command{
		Use:   "rescan",
		Short: "Force full rescan",
		RunE: func(_ *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("linux only")
			}
			cfg, err := loadConfig(configFile)
			if err != nil {
				return err
			}
			if !rescanNew && rescanSkipBackup {
				return fmt.Errorf("--skip-backup requires --new")
			}
			if rescanNew {
				backup := !rescanSkipBackup
				backupPath, err := rebuildIndexPath(cfg.Storage.IndexPath, backup)
				if err != nil {
					return err
				}
				if backup && backupPath != "" {
					fmt.Printf("new index prepared\nbackup created: %s\n", backupPath)
				} else if backup {
					fmt.Println("new index prepared (no existing index dir to backup)")
				} else {
					fmt.Println("new index prepared (backup skipped)")
				}
			}
			idx, _, err := buildCore(cfg)
			if err != nil {
				return err
			}
			defer idx.Close()
			// buildCore performs a full rescan as part of service bootstrap.
			fmt.Println("rescan completed")
			return nil
		},
	}
	rescanCmd.Flags().BoolVar(&rescanNew, "new", false, "recreate index directory before rescanning (daemon must be stopped)")
	rescanCmd.Flags().BoolVar(&rescanSkipBackup, "skip-backup", false, "with --new, do not create index backup")
	cmd.AddCommand(rescanCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "stats",
		Short: "Print index entry count",
		RunE: func(_ *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return fmt.Errorf("linux only")
			}
			cfg, err := loadConfig(configFile)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(cfg.Storage.IndexPath, 0o755); err != nil {
				return err
			}
			idx, err := indexpebble.Open(cfg.Storage.IndexPath, 128<<20)
			if err != nil {
				return err
			}
			defer idx.Close()
			entities, err := idx.ListEntities()
			if err != nil {
				return err
			}
			fmt.Printf("entities=%d\n", len(entities))
			return nil
		},
	})

	return cmd
}
