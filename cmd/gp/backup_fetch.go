package gp

import (
	"fmt"

	"github.com/wal-g/wal-g/internal/databases/greenplum"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	conf "github.com/wal-g/wal-g/internal/config"
	"github.com/wal-g/wal-g/internal/multistorage/policies"
)

const (
	backupFetchShortDescription  = "Fetches a backup from storage"
	targetUserDataDescription    = "Fetch storage backup which has the specified user data"
	restorePointDescription      = "Fetch storage backup w/ restore point specified by name"
	restorePointTSDescription    = "Fetch storage backup w/ restore point time less or equal to the provided timestamp"
	restoreConfigPathDescription = "Path to the cluster restore configuration"
	fetchContentIDsDescription   = "If set, WAL-G will fetch only the specified segments"
	fetchModeDescription         = "Backup fetch mode. default: do the backup unpacking " +
		"and prepare the configs [unpack+prepare], unpack: backup unpacking only, prepare: config preparation only."
	inPlaceFlagDescription = "Perform the backup fetch in-place (without the restore config)"
	restoreOnlyDescription = `[Experimental] Downloads only databases specified by passed names from default tablespace.
Always downloads system databases.`
)

var fetchTargetUserData string
var restorePointTS string
var restorePoint string
var restoreConfigPath string
var fetchContentIDs *[]int
var fetchModeStr string
var inPlaceRestore bool
var partialRestoreArgs []string

var backupFetchCmd = &cobra.Command{
	Use:   "backup-fetch [backup_name | --target-user-data <data> | --restore-point <name>]",
	Short: backupFetchShortDescription, // TODO : improve description
	Args:  cobra.RangeArgs(0, 1),
	Run: func(cmd *cobra.Command, args []string) {
		internal.ConfigureLimiters()

		if !inPlaceRestore && restoreConfigPath == "" {
			tracelog.ErrorLogger.Fatalf(
				"No restore config was specified. Either specify one via the --restore-config flag or add the --in-place flag to restore in-place.")
		}

		if fetchTargetUserData == "" {
			fetchTargetUserData = viper.GetString(conf.FetchTargetUserDataSetting)
		}

		rootFolder, err := getMultistorageRootFolder(false, policies.UniteAllStorages)
		tracelog.ErrorLogger.FatalOnError(err)

		if restorePoint != "" && restorePointTS != "" {
			tracelog.ErrorLogger.Fatalf("can't use both --restore-point and --restore-point-ts")
		}

		if restorePointTS != "" {
			restorePoints, err := greenplum.FetchAllRestorePoints(rootFolder)
			tracelog.ErrorLogger.FatalOnError(err)
			restorePoint, err = greenplum.FindRestorePointBeforeTS(restorePointTS, restorePoints)
			tracelog.ErrorLogger.FatalOnError(err)
		}

		targetBackupSelector, err := createTargetFetchBackupSelector(cmd, args, fetchTargetUserData, restorePoint)
		tracelog.ErrorLogger.FatalOnError(err)

		logsDir := viper.GetString(conf.GPLogsDirectory)

		if len(*fetchContentIDs) > 0 {
			tracelog.InfoLogger.Printf("Will perform fetch operations only on the specified segments: %v", *fetchContentIDs)
		}

		fetchMode, err := greenplum.NewBackupFetchMode(fetchModeStr)
		tracelog.ErrorLogger.FatalOnError(err)

		internal.HandleBackupFetch(rootFolder, targetBackupSelector,
			greenplum.NewGreenplumBackupFetcher(restoreConfigPath, inPlaceRestore, logsDir, *fetchContentIDs, fetchMode, restorePoint,
				partialRestoreArgs))
	},
}

// create the BackupSelector to select the backup to fetch
func createTargetFetchBackupSelector(cmd *cobra.Command,
	args []string, targetUserData, restorePoint string) (internal.BackupSelector, error) {
	targetName := ""
	if len(args) >= 1 {
		targetName = args[0]
	}

	// if target restore point is provided without the backup name, then
	// choose the latest backup up to the specified restore point name
	if restorePoint != "" && targetUserData == "" && targetName == "" {
		tracelog.InfoLogger.Printf("Restore point %s is specified without the backup name or target user data, "+
			"will search for a matching backup", restorePoint)
		return greenplum.NewRestorePointBackupSelector(restorePoint), nil
	}

	backupSelector, err := internal.NewTargetBackupSelector(targetUserData, targetName, greenplum.NewGenericMetaFetcher())
	if err != nil {
		fmt.Println(cmd.UsageString())
		return nil, err
	}
	return backupSelector, nil
}

func init() {
	backupFetchCmd.Flags().StringVar(&fetchTargetUserData, "target-user-data",
		"", targetUserDataDescription)
	backupFetchCmd.Flags().StringVar(&restorePointTS, "restore-point-ts", "", restorePointTSDescription)
	backupFetchCmd.Flags().StringVar(&restorePoint, "restore-point", "", restorePointDescription)
	backupFetchCmd.Flags().StringVar(&restoreConfigPath, "restore-config",
		"", restoreConfigPathDescription)
	backupFetchCmd.Flags().BoolVar(&inPlaceRestore, "in-place", false, inPlaceFlagDescription)
	fetchContentIDs = backupFetchCmd.Flags().IntSlice("content-ids", []int{}, fetchContentIDsDescription)
	backupFetchCmd.Flags().StringSliceVar(&partialRestoreArgs, "restore-only", nil, restoreOnlyDescription)

	backupFetchCmd.Flags().StringVar(&fetchModeStr, "mode", "default", fetchModeDescription)
	cmd.AddCommand(backupFetchCmd)
}
