package scripts

import (
	"path/filepath"
	"regexp"

	"github.com/hasura/graphql-engine/cli/internal/metadataobject"

	"github.com/hasura/graphql-engine/cli/internal/metadatautil"

	"github.com/fatih/color"

	"github.com/hasura/graphql-engine/cli/internal/statestore"
	"github.com/hasura/graphql-engine/cli/internal/statestore/migrations"
	"github.com/hasura/graphql-engine/cli/internal/statestore/settings"

	"github.com/hasura/graphql-engine/cli"

	"fmt"

	"github.com/hasura/graphql-engine/cli/util"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
)

type UpdateProjectV3Opts struct {
	EC *cli.ExecutionContext
	Fs afero.Fs
	// Path to project directory
	ProjectDirectory string
	// Directory in which migrations are stored
	MigrationsAbsDirectoryPath string
	SeedsAbsDirectoryPath      string
	TargetDatabase             string
	Force                      bool
	MoveStateOnly              bool
	Logger                     *logrus.Logger
}

// UpdateProjectV3 will help a project directory move from a single
// The project is expected to be in Config V2
func UpdateProjectV3(opts UpdateProjectV3Opts) error {
	/* New flow
		Config V2 -> Config V3
		- Warn user about creating a backup
		- Ask user for the name of database to migrate to
	  	- copy state from hdb_tables to catalog state
		- Move current migration directories to a new source directory
		- Move seeds belonging to the source to a new directory
		- Update config file and version
	*/

	// pre checks
	if opts.EC.Config.Version != cli.V2 && !opts.MoveStateOnly {
		return fmt.Errorf("project should be using config V2 to be able to update to V3")
	}
	if !opts.EC.HasMetadataV3 {
		return fmt.Errorf("unsupported server version %v, config V3 is supported only on server with metadata version >= 3", opts.EC.Version.Server)
	}
	if r, err := opts.EC.APIClient.V1Metadata.GetInconsistentMetadata(); err != nil {
		return fmt.Errorf("determing server metadata inconsistency: %w", err)
	} else {
		if !r.IsConsistent {
			return fmt.Errorf("cannot continue: metadata is inconsistent on the server")
		}
	}

	opts.Logger.Infof("The upgrade process will make some changes to your project directory, It is advised to create a backup project directory before continuing")
	opts.Logger.Warn(`Config V3 is expected to be used with servers >=v2.0.0-alpha.1`)
	opts.Logger.Warn(`During the update process CLI uses the server as the source of truth, so make sure your server is upto date`)
	opts.Logger.Warn(`The update process replaces project metadata with metadata on the server`)

	if !opts.Force {
		response, err := util.GetYesNoPrompt("continue?")
		if err != nil {
			return err
		}
		if response == "n" {
			return nil
		}
	}

	// if database name is set using --database-name flag, copy it to this variable
	targetDatabase := opts.TargetDatabase

	// if targetDatabase is not set, get list of databases connected from hasura
	sources, err := metadatautil.GetSources(opts.EC.APIClient.V1Metadata.ExportMetadata)
	if err != nil {
		return err
	}
	if len(targetDatabase) == 0 {
		if len(sources) == 1 && sources[0] == "default" {
			targetDatabase = sources[0]
		} else if len(sources) > 0 {
			targetDatabase, err = util.GetSelectPrompt("what database does this current migrations / seeds belong to?", sources)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("cannot determine name of database for which current migrations / seed belong to, found 0 connected databases on hasura %v", sources)
		}
	}
	opts.EC.Spinner.Start()
	opts.EC.Spin("updating project... ")

	defer opts.EC.Spinner.Stop()
	if len(sources) >= 1 {
		opts.EC.Logger.Debug("start: copying state from from hdb_catalog.schema_migrations")
		opts.EC.Spin("Moving state from hdb_catalog.schema_migrations ")
		if err := CopyState(opts.EC, targetDatabase, targetDatabase); err != nil {
			return err
		}
		if opts.MoveStateOnly {
			opts.EC.Logger.Debug("move state only is set, copied state and returning early")
			return nil
		}
		opts.EC.Logger.Debug("completed: copying state from from hdb_catalog.schema_migrations")
	}

	opts.EC.Logger.Debug("start: copy old migrations to new directory structure")
	opts.EC.Spin("Moving migrations and seeds to new directories ")
	// move migration child directories
	// get directory names to move
	migrationDirectoriesToMove, err := getMigrationDirectoryNames(opts.Fs, opts.MigrationsAbsDirectoryPath)
	if err != nil {
		return errors.Wrap(err, "getting list of migrations to move")
	}
	// move seed child directories
	// get directory names to move
	seedFilesToMove, err := getSeedFiles(opts.Fs, opts.SeedsAbsDirectoryPath)
	if err != nil {
		return errors.Wrap(err, "getting list of seed files to move")
	}

	// create a new directory for TargetDatabase
	targetMigrationsDirectoryName := filepath.Join(opts.MigrationsAbsDirectoryPath, targetDatabase)
	if err = opts.Fs.Mkdir(targetMigrationsDirectoryName, 0755); err != nil {
		errors.Wrap(err, "creating target migrations directory")
	}

	// create a new directory for TargetDatabase
	targetSeedsDirectoryName := filepath.Join(opts.SeedsAbsDirectoryPath, targetDatabase)
	if err = opts.Fs.Mkdir(targetSeedsDirectoryName, 0755); err != nil {
		errors.Wrap(err, "creating target seeds directory")
	}

	// move migration directories to target database directory
	if err := copyMigrations(opts.Fs, migrationDirectoriesToMove, opts.MigrationsAbsDirectoryPath, targetMigrationsDirectoryName); err != nil {
		return errors.Wrap(err, "moving migrations to target database directory")
	}
	// move seed directories to target database directory
	if err := copyFiles(opts.Fs, seedFilesToMove, opts.SeedsAbsDirectoryPath, targetSeedsDirectoryName); err != nil {
		return errors.Wrap(err, "moving seeds to target database directory")
	}
	opts.EC.Logger.Debug("completed: copy old migrations to new directory structure")

	opts.EC.Logger.Debug("start: generate new config file")
	opts.EC.Spin("Generating new config file ")
	// write new config file
	newConfig := *opts.EC.Config
	newConfig.Version = cli.V3
	if err := opts.EC.WriteConfig(&newConfig); err != nil {
		return err
	}
	opts.EC.Config = &newConfig
	opts.EC.Logger.Debug("completed: generate new config file")

	opts.EC.Logger.Debug("start: delete old migrations and seeds")
	opts.EC.Spin("Cleaning project directory ")
	// delete original migrations
	if err := removeDirectories(opts.Fs, opts.MigrationsAbsDirectoryPath, migrationDirectoriesToMove); err != nil {
		return errors.Wrap(err, "removing up original migrations")
	}
	// delete original seeds
	if err := removeDirectories(opts.Fs, opts.SeedsAbsDirectoryPath, seedFilesToMove); err != nil {
		return errors.Wrap(err, "removing up original migrations")
	}
	// remove functions.yaml and tables.yaml files
	metadataFiles := []string{"functions.yaml", "tables.yaml"}
	if err := removeDirectories(opts.Fs, opts.EC.MetadataDir, metadataFiles); err != nil {
		return err
	}
	opts.EC.Logger.Debug("completed: delete old migrations and seeds")

	opts.EC.Logger.Debug("start: export metadata from server")
	opts.EC.Spin("Exporting metadata from server ")
	var files map[string][]byte
	mdHandler := metadataobject.NewHandlerFromEC(opts.EC)
	files, err = mdHandler.ExportMetadata()
	if err != nil {
		return err
	}
	if err := mdHandler.WriteMetadata(files); err != nil {
		return err
	}
	opts.EC.Spinner.Stop()
	opts.EC.Logger.Debug("completed: export metadata from server")
	opts.EC.Logger.Info("Operation completed")
	return nil
}

func removeDirectories(fs afero.Fs, parentDirectory string, dirNames []string) error {
	for _, d := range dirNames {
		if err := fs.RemoveAll(filepath.Join(parentDirectory, d)); err != nil {
			return err
		}
	}
	return nil
}

func copyMigrations(fs afero.Fs, dirs []string, parentDir, target string) error {
	for _, dir := range dirs {
		f, _ := fs.Stat(filepath.Join(parentDir, dir))
		if f != nil {
			if f.IsDir() {
				err := util.CopyDirAfero(fs, filepath.Join(parentDir, dir), filepath.Join(target, dir))
				if err != nil {
					return errors.Wrapf(err, "moving %s to %s", dir, target)
				}
			} else {
				err := util.CopyFileAfero(fs, filepath.Join(parentDir, dir), filepath.Join(target, dir))
				if err != nil {
					return errors.Wrapf(err, "moving %s to %s", dir, target)
				}
			}
		}

	}
	return nil
}

func copyFiles(fs afero.Fs, files []string, parentDir, target string) error {
	for _, dir := range files {
		err := util.CopyFileAfero(fs, filepath.Join(parentDir, dir), filepath.Join(target, dir))
		if err != nil {
			return errors.Wrapf(err, "moving %s to %s", dir, target)
		}
	}
	return nil
}

func getMigrationDirectoryNames(fs afero.Fs, rootMigrationsDir string) ([]string, error) {
	return getMatchingFilesAndDirs(fs, rootMigrationsDir, isHasuraCLIGeneratedMigration)
}

func getSeedFiles(fs afero.Fs, rootSeedDir string) ([]string, error) {
	// find migrations which are in the format <timestamp>_name
	var seedFiles []string
	dirs, err := afero.ReadDir(fs, rootSeedDir)
	if err != nil {
		return nil, err
	}
	for _, info := range dirs {
		if !info.IsDir() {
			seedFiles = append(seedFiles, filepath.Join(info.Name()))
		}

	}
	return seedFiles, nil
}

func getMatchingFilesAndDirs(fs afero.Fs, parentDir string, matcher func(string) (bool, error)) ([]string, error) {
	// find migrations which are in the format <timestamp>_name
	var migs []string
	dirs, err := afero.ReadDir(fs, parentDir)
	if err != nil {
		return nil, err
	}
	for _, info := range dirs {
		if ok, err := matcher(info.Name()); !ok || err != nil {
			if err != nil {
				return nil, err
			}
			continue
		}
		migs = append(migs, filepath.Join(info.Name()))

	}
	return migs, nil
}

func isHasuraCLIGeneratedMigration(dirPath string) (bool, error) {
	const regex = `^([0-9]{13})_(.*)$`
	return regexp.MatchString(regex, filepath.Base(dirPath))
}

func CopyState(ec *cli.ExecutionContext, sourceDatabase, destDatabase string) error {
	// copy migrations state
	src := migrations.NewMigrationStateStoreHdbTable(ec.APIClient.V2Query, migrations.DefaultSchema, migrations.DefaultMigrationsTable)
	if err := src.PrepareMigrationsStateStore(sourceDatabase); err != nil {
		return err
	}
	dst := migrations.NewCatalogStateStore(statestore.NewCLICatalogState(ec.APIClient.V1Metadata))
	if err := dst.PrepareMigrationsStateStore(destDatabase); err != nil {
		return err
	}
	err := statestore.CopyMigrationState(src, dst, sourceDatabase, destDatabase)
	if err != nil {
		return err
	}
	// copy settings state
	srcSettingsStore := cli.GetSettingsStateStore(ec, sourceDatabase)
	if err := srcSettingsStore.PrepareSettingsDriver(); err != nil {
		return err
	}
	dstSettingsStore := settings.NewStateStoreCatalog(statestore.NewCLICatalogState(ec.APIClient.V1Metadata))
	if err := dstSettingsStore.PrepareSettingsDriver(); err != nil {
		return err
	}
	err = statestore.CopySettingsState(srcSettingsStore, dstSettingsStore)
	if err != nil {
		return err
	}
	cliState, err := statestore.NewCLICatalogState(ec.APIClient.V1Metadata).Get()
	if err != nil {
		return fmt.Errorf("error while fetching catalog state: %v", err)
	}
	cliState.IsStateCopyCompleted = true
	if _, err := statestore.NewCLICatalogState(ec.APIClient.V1Metadata).Set(*cliState); err != nil {
		return fmt.Errorf("cannot set catalog state: %v", err)
	}
	return nil
}

func CheckIfUpdateToConfigV3IsRequired(ec *cli.ExecutionContext) error {
	// see if an update to config V3 is necessary
	if ec.Config.Version <= cli.V1 && ec.HasMetadataV3 {
		ec.Logger.Info("config v1 is deprecated from v1.4")
		return errors.New("please upgrade your project to a newer version.\nuse " + color.New(color.FgCyan).SprintFunc()("hasura scripts update-project-v2") + " to upgrade your project to config v2")
	}
	if ec.Config.Version < cli.V3 && ec.HasMetadataV3 {
		sources, err := metadatautil.GetSources(ec.APIClient.V1Metadata.ExportMetadata)
		if err != nil {
			return err
		}
		upgrade := func() error {
			ec.Logger.Info("Looks like you are trying to use hasura with multiple databases, which requires some changes on your project directory\n")
			ec.Logger.Info("please use " + color.New(color.FgCyan).SprintFunc()("hasura scripts update-project-v3") + " to make this change")
			return errors.New("update to config V3")
		}
		if len(sources) == 0 {
			return fmt.Errorf("no connected databases found on hasura")
		}
		// if no sources are configured prompt and upgrade
		if len(sources) != 1 {
			return upgrade()
		}
		// if 1 source is configured and it is not "default" then it's a custom database
		// then also prompt an upgrade
		if len(sources) == 1 {
			if sources[0] != "default" {
				return upgrade()
			}
		}
	}
	return nil
}
