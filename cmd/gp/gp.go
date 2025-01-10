package gp

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/wal-g/tracelog"

	"github.com/wal-g/wal-g/cmd/common"
	"github.com/wal-g/wal-g/cmd/pg"
	"github.com/wal-g/wal-g/internal"
	conf "github.com/wal-g/wal-g/internal/config"
	"github.com/wal-g/wal-g/internal/databases/greenplum"
	"github.com/wal-g/wal-g/internal/databases/postgres"
	"github.com/wal-g/wal-g/internal/multistorage"
	"github.com/wal-g/wal-g/internal/multistorage/policies"
	"github.com/wal-g/wal-g/pkg/storages/storage"
)

// These variables are here only to show current version. They are set in makefile during build process
var (
	dbShortDescription = "GreenplumDB backup tool"
	walgVersion        = "devel"
	gitRevision        = "devel"
	buildDate          = "devel"

	targetStorage            string
	targetStorageDescription = `Name of the storage to execute the command only for. Use "default" to select the primary one.`

	cmd = &cobra.Command{
		Use:     "wal-g",
		Short:   dbShortDescription, // TODO : improve description
		Version: strings.Join([]string{walgVersion, gitRevision, buildDate, "GreenplumDB"}, "\t"),
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Greenplum uses the 64MB WAL segment size by default
			postgres.SetWalSize(viper.GetUint64(conf.PgWalSize))
			err := internal.AssertRequiredSettingsSet()
			tracelog.ErrorLogger.FatalOnError(err)
			err = conf.ConfigureAndRunDefaultWebServer()
			tracelog.ErrorLogger.FatalOnError(err)

			// In case the --target-storage flag isn't specified (the variable is set in commands' init() funcs),
			// we take the value from the config.
			if targetStorage == "" {
				targetStorage = viper.GetString(conf.PgTargetStorage)
			}
		},
	}
)

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main().
func Execute() {
	if err := cmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func GetCmd() *cobra.Command {
	return cmd
}

var SegContentID string

func init() {
	common.Init(cmd, conf.GP)

	_ = cmd.MarkFlagRequired("config") // config is required for Greenplum WAL-G
	// wrap the Postgres command so it can be used in the same binary
	wrappedPgCmd := pg.Cmd
	wrappedPgCmd.Use = "seg"
	wrappedPgCmd.Short = "PostgreSQL command series to run on segments (use with caution)"
	wrappedPreRun := wrappedPgCmd.PersistentPreRun
	wrappedPgCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		// segment content ID is required in order to get the corresponding segment subfolder
		contentID, err := greenplum.ConfigureSegContentID(SegContentID)
		tracelog.ErrorLogger.FatalOnError(err)
		greenplum.SetSegmentStoragePrefix(contentID)
		wrappedPreRun(cmd, args)
	}
	wrappedPgCmd.PersistentFlags().StringVar(&SegContentID, "content-id", "", "segment content ID")
	cmd.AddCommand(wrappedPgCmd)

	// Add the hidden prefetch command to the root command
	// since WAL-G prefetch fork logic does not know anything about the "wal-g seg" subcommand
	pg.WalPrefetchCmd.PreRun = func(cmd *cobra.Command, args []string) {
		conf.RequiredSettings[conf.StoragePrefixSetting] = true
		tracelog.ErrorLogger.FatalOnError(internal.AssertRequiredSettingsSet())
	}
	cmd.AddCommand(pg.WalPrefetchCmd)
}

func getMultistorageRootFolder(checkWrite bool, policy policies.Policies) (storage.Folder, error) {
	storage, err := internal.ConfigureMultiStorage(checkWrite)
	if err != nil {
		return nil, err
	}

	rootFolder := multistorage.SetPolicies(storage.RootFolder(), policy)
	if targetStorage == "" {
		rootFolder, err = multistorage.UseAllAliveStorages(rootFolder)
	} else {
		rootFolder, err = multistorage.UseSpecificStorage(targetStorage, rootFolder)
	}
	if err != nil {
		return nil, err
	}
	if policy == policies.TakeFirstStorage {
		tracelog.InfoLogger.Printf("Using storages: %v", multistorage.UsedStorages(rootFolder)[0])
	} else if policy == policies.UniteAllStorages {
		tracelog.InfoLogger.Printf("Using storages: %v", multistorage.UsedStorages(rootFolder))
	}
	return rootFolder, nil
}
