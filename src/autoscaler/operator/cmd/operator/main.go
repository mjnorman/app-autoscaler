package main

import (
	"autoscaler/db"
	"autoscaler/db/sqldb"
	"autoscaler/helpers"
	"autoscaler/operator"
	"autoscaler/operator/config"
	sync "autoscaler/sync"
	"flag"
	"fmt"
	"os"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/consuladapter"
	"code.cloudfoundry.org/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"
)

func main() {
	var path string
	flag.StringVar(&path, "c", "", "config file")
	flag.Parse()
	if path == "" {
		fmt.Fprintln(os.Stderr, "missing config file")
		os.Exit(1)
	}

	configFile, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stdout, "failed to open config file '%s' : %s\n", path, err.Error())
		os.Exit(1)
	}

	var conf *config.Config
	conf, err = config.LoadConfig(configFile)
	if err != nil {
		fmt.Fprintf(os.Stdout, "failed to read config file '%s' : %s\n", path, err.Error())
		os.Exit(1)
	}
	configFile.Close()

	err = conf.Validate()
	if err != nil {
		fmt.Fprintf(os.Stdout, "failed to validate configuration : %s\n", err.Error())
		os.Exit(1)
	}

	logger := helpers.InitLoggerFromConfig(&conf.Logging, "operator")
	prClock := clock.NewClock()

	instanceMetricsDB, err := sqldb.NewInstanceMetricsSQLDB(conf.InstanceMetricsDB.DB, logger.Session("instancemetrics-db"))
	if err != nil {
		logger.Error("failed to connect instancemetrics db", err, lager.Data{"dbConfig": conf.InstanceMetricsDB.DB})
		os.Exit(1)
	}
	defer instanceMetricsDB.Close()

	appMetricsDB, err := sqldb.NewAppMetricSQLDB(conf.AppMetricsDB.DB, logger.Session("appmetrics-db"))
	if err != nil {
		logger.Error("failed to connect appmetrics db", err, lager.Data{"dbConfig": conf.AppMetricsDB.DB})
		os.Exit(1)
	}
	defer appMetricsDB.Close()

	scalingEngineDB, err := sqldb.NewScalingEngineSQLDB(conf.ScalingEngineDB.DB, logger.Session("scalingengine-db"))
	if err != nil {
		logger.Error("failed to connect scalingengine db", err, lager.Data{"dbConfig": conf.ScalingEngineDB.DB})
		os.Exit(1)
	}
	defer scalingEngineDB.Close()

	scalingEngineHttpclient, err := helpers.CreateHTTPClient(&conf.ScalingEngine.TLSClientCerts)
	if err != nil {
		logger.Error("failed to create http client for scalingengine", err, lager.Data{"scalingengineTLS": conf.ScalingEngine.TLSClientCerts})
		os.Exit(1)
	}
	schedulerHttpclient, err := helpers.CreateHTTPClient(&conf.Scheduler.TLSClientCerts)
	if err != nil {
		logger.Error("failed to create http client for scheduler", err, lager.Data{"schedulerTLS": conf.Scheduler.TLSClientCerts})
		os.Exit(1)
	}

	loggerSessionName := "instancemetrics-dbpruner"
	instanceMetricDBPruner := operator.NewInstanceMetricsDbPruner(instanceMetricsDB, conf.InstanceMetricsDB.CutoffDays, prClock, logger.Session(loggerSessionName))
	instanceMetricsDBOperatorRunner := operator.NewOperatorRunner(instanceMetricDBPruner, conf.InstanceMetricsDB.RefreshInterval, prClock, logger.Session(loggerSessionName))

	loggerSessionName = "appmetrics-dbpruner"
	appMetricsDBPruner := operator.NewAppMetricsDbPruner(appMetricsDB, conf.AppMetricsDB.CutoffDays, prClock, logger.Session(loggerSessionName))
	appMetricsDBOperatorRunner := operator.NewOperatorRunner(appMetricsDBPruner, conf.AppMetricsDB.RefreshInterval, prClock, logger.Session(loggerSessionName))

	loggerSessionName = "scalingengine-dbpruner"
	scalingEngineDBPruner := operator.NewScalingEngineDbPruner(scalingEngineDB, conf.ScalingEngineDB.CutoffDays, prClock, logger.Session(loggerSessionName))
	scalingEngineDBOperatorRunner := operator.NewOperatorRunner(scalingEngineDBPruner, conf.ScalingEngineDB.RefreshInterval, prClock, logger.Session(loggerSessionName))
	loggerSessionName = "scalingengine-sync"
	scalingEngineSync := operator.NewScheduleSynchronizer(scalingEngineHttpclient, conf.ScalingEngine.URL, prClock, logger.Session(loggerSessionName))
	scalingEngineSyncRunner := operator.NewOperatorRunner(scalingEngineSync, conf.ScalingEngine.SyncInterval, prClock, logger.Session(loggerSessionName))

	loggerSessionName = "scheduler-sync"
	schedulerSync := operator.NewScheduleSynchronizer(schedulerHttpclient, conf.Scheduler.URL, prClock, logger.Session(loggerSessionName))
	schedulerSyncRunner := operator.NewOperatorRunner(schedulerSync, conf.Scheduler.SyncInterval, prClock, logger.Session(loggerSessionName))

	members := grouper.Members{
		{"instancemetrics-dbpruner", instanceMetricsDBOperatorRunner},
		{"appmetrics-dbpruner", appMetricsDBOperatorRunner},
		{"scalingEngine-dbpruner", scalingEngineDBOperatorRunner},
		{"scalingEngine-sync", scalingEngineSyncRunner},
		{"scheduler-sync", schedulerSyncRunner},
	}

	guid, err := helpers.GenerateGUID(logger)
	if err != nil {
		logger.Error("failed-to-generate-guid", err)
	}
	const lockTableName = "operator_lock"
	if conf.EnableDBLock {
		logger.Debug("database-lock-feature-enabled")
		var lockDB db.LockDB
		lockDB, err = sqldb.NewLockSQLDB(conf.DBLock.DB, lockTableName, logger.Session("lock-db"))
		if err != nil {
			logger.Error("failed-to-connect-lock-database", err, lager.Data{"dbConfig": conf.DBLock.DB})
			os.Exit(1)
		}
		defer lockDB.Close()
		prdl := sync.NewDatabaseLock(logger)
		dbLockMaintainer := prdl.InitDBLockRunner(conf.DBLock.LockRetryInterval, conf.DBLock.LockTTL, guid, lockDB)
		members = append(grouper.Members{{"db-lock-maintainer", dbLockMaintainer}}, members...)
	}

	if conf.Lock.ConsulClusterConfig != "" {
		consulClient, err := consuladapter.NewClientFromUrl(conf.Lock.ConsulClusterConfig)
		if err != nil {
			logger.Fatal("new consul client failed", err)
		}

		serviceClient := operator.NewServiceClient(consulClient, prClock)

		guid, err := helpers.GenerateGUID(logger)
		if err != nil {
			logger.Error("failed-to-generate-guid", err)
			os.Exit(1)
		}
		if !conf.EnableDBLock {
			lockMaintainer := serviceClient.NewOperatorLockRunner(
				logger,
				guid,
				conf.Lock.LockRetryInterval,
				conf.Lock.LockTTL,
			)
			members = append(grouper.Members{{"lock-maintainer", lockMaintainer}}, members...)
		}
	}

	monitor := ifrit.Invoke(sigmon.New(grouper.NewOrdered(os.Interrupt, members)))

	logger.Info("started")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")

}
