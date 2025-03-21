// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package sqlstore

import (
	"context"
	"database/sql"
	dbsql "database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	mysqlmigrate "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	bindata "github.com/golang-migrate/migrate/v4/source/go_bindata"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	_ "github.com/lib/pq"
	"github.com/mattermost/gorp"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/v6/db/migrations"
	"github.com/mattermost/mattermost-server/v6/einterfaces"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/shared/mlog"
	"github.com/mattermost/mattermost-server/v6/store"
)

type migrationDirection string

const (
	IndexTypeFullText                 = "full_text"
	IndexTypeFullTextFunc             = "full_text_func"
	IndexTypeDefault                  = "default"
	PGDupTableErrorCode               = "42P07"      // see https://github.com/lib/pq/blob/master/error.go#L268
	MySQLDupTableErrorCode            = uint16(1050) // see https://dev.mysql.com/doc/mysql-errors/5.7/en/server-error-reference.html#error_er_table_exists_error
	PGForeignKeyViolationErrorCode    = "23503"
	MySQLForeignKeyViolationErrorCode = 1452
	PGDuplicateObjectErrorCode        = "42710"
	MySQLDuplicateObjectErrorCode     = 1022
	DBPingAttempts                    = 18
	DBPingTimeoutSecs                 = 10
	// This is a numerical version string by postgres. The format is
	// 2 characters for major, minor, and patch version prior to 10.
	// After 10, it's major and minor only.
	// 10.1 would be 100001.
	// 9.6.3 would be 90603.
	minimumRequiredPostgresVersion = 100000
	// major*1000 + minor*100 + patch
	minimumRequiredMySQLVersion = 5712

	migrationsDirectionUp   migrationDirection = "up"
	migrationsDirectionDown migrationDirection = "down"

	replicaLagPrefix = "replica-lag"
)

type SqlStoreStores struct {
	team                 store.TeamStore
	channel              store.ChannelStore
	post                 store.PostStore
	retentionPolicy      store.RetentionPolicyStore
	thread               store.ThreadStore
	user                 store.UserStore
	bot                  store.BotStore
	audit                store.AuditStore
	cluster              store.ClusterDiscoveryStore
	remoteCluster        store.RemoteClusterStore
	compliance           store.ComplianceStore
	session              store.SessionStore
	oauth                store.OAuthStore
	system               store.SystemStore
	webhook              store.WebhookStore
	command              store.CommandStore
	commandWebhook       store.CommandWebhookStore
	preference           store.PreferenceStore
	license              store.LicenseStore
	token                store.TokenStore
	emoji                store.EmojiStore
	status               store.StatusStore
	fileInfo             store.FileInfoStore
	uploadSession        store.UploadSessionStore
	reaction             store.ReactionStore
	job                  store.JobStore
	userAccessToken      store.UserAccessTokenStore
	plugin               store.PluginStore
	channelMemberHistory store.ChannelMemberHistoryStore
	role                 store.RoleStore
	scheme               store.SchemeStore
	TermsOfService       store.TermsOfServiceStore
	productNotices       store.ProductNoticesStore
	group                store.GroupStore
	UserTermsOfService   store.UserTermsOfServiceStore
	linkMetadata         store.LinkMetadataStore
	sharedchannel        store.SharedChannelStore
}

type SqlStore struct {
	// rrCounter and srCounter should be kept first.
	// See https://github.com/mattermost/mattermost-server/v6/pull/7281
	rrCounter int64
	srCounter int64

	master  *gorp.DbMap
	masterX *sqlxDBWrapper

	Replicas  []*gorp.DbMap
	ReplicaXs []*sqlxDBWrapper

	searchReplicas  []*gorp.DbMap
	searchReplicaXs []*sqlxDBWrapper

	replicaLagHandles []*dbsql.DB
	stores            SqlStoreStores
	settings          *model.SqlSettings
	lockedToMaster    bool
	context           context.Context
	license           *model.License
	licenseMutex      sync.RWMutex
	metrics           einterfaces.MetricsInterface
}

// ColumnInfo holds information about a column.
type ColumnInfo struct {
	DataType          string
	DefaultValue      string
	CharMaximumLength int
}

func New(settings model.SqlSettings, metrics einterfaces.MetricsInterface) *SqlStore {
	store := &SqlStore{
		rrCounter: 0,
		srCounter: 0,
		settings:  &settings,
		metrics:   metrics,
	}

	store.initConnection()

	ver, err := store.GetDbVersion(true)
	if err != nil {
		mlog.Fatal("Error while getting DB version.", mlog.Err(err))
	}

	ok, err := store.ensureMinimumDBVersion(ver)
	if !ok {
		mlog.Fatal("Error while checking DB version.", mlog.Err(err))
	}

	err = store.migrate(migrationsDirectionUp)
	if err != nil {
		mlog.Fatal("Failed to apply database migrations.", mlog.Err(err))
	}

	store.stores.team = newSqlTeamStore(store)
	store.stores.channel = newSqlChannelStore(store, metrics)
	store.stores.post = newSqlPostStore(store, metrics)
	store.stores.retentionPolicy = newSqlRetentionPolicyStore(store, metrics)
	store.stores.user = newSqlUserStore(store, metrics)
	store.stores.bot = newSqlBotStore(store, metrics)
	store.stores.audit = newSqlAuditStore(store)
	store.stores.cluster = newSqlClusterDiscoveryStore(store)
	store.stores.remoteCluster = newSqlRemoteClusterStore(store)
	store.stores.compliance = newSqlComplianceStore(store)
	store.stores.session = newSqlSessionStore(store)
	store.stores.oauth = newSqlOAuthStore(store)
	store.stores.system = newSqlSystemStore(store)
	store.stores.webhook = newSqlWebhookStore(store, metrics)
	store.stores.command = newSqlCommandStore(store)
	store.stores.commandWebhook = newSqlCommandWebhookStore(store)
	store.stores.preference = newSqlPreferenceStore(store)
	store.stores.license = newSqlLicenseStore(store)
	store.stores.token = newSqlTokenStore(store)
	store.stores.emoji = newSqlEmojiStore(store, metrics)
	store.stores.status = newSqlStatusStore(store)
	store.stores.fileInfo = newSqlFileInfoStore(store, metrics)
	store.stores.uploadSession = newSqlUploadSessionStore(store)
	store.stores.thread = newSqlThreadStore(store)
	store.stores.job = newSqlJobStore(store)
	store.stores.userAccessToken = newSqlUserAccessTokenStore(store)
	store.stores.channelMemberHistory = newSqlChannelMemberHistoryStore(store)
	store.stores.plugin = newSqlPluginStore(store)
	store.stores.TermsOfService = newSqlTermsOfServiceStore(store, metrics)
	store.stores.UserTermsOfService = newSqlUserTermsOfServiceStore(store)
	store.stores.linkMetadata = newSqlLinkMetadataStore(store)
	store.stores.sharedchannel = newSqlSharedChannelStore(store)
	store.stores.reaction = newSqlReactionStore(store)
	store.stores.role = newSqlRoleStore(store)
	store.stores.scheme = newSqlSchemeStore(store)
	store.stores.group = newSqlGroupStore(store)
	store.stores.productNotices = newSqlProductNoticesStore(store)
	err = store.GetMaster().CreateTablesIfNotExists()

	if err != nil {
		if IsDuplicate(err) {
			mlog.Warn("Duplicate key error occurred; assuming table already created and proceeding.", mlog.Err(err))
		} else {
			mlog.Fatal("Error creating database tables.", mlog.Err(err))
		}
	}

	err = upgradeDatabase(store, model.CurrentVersion)
	if err != nil {
		mlog.Fatal("Failed to upgrade database.", mlog.Err(err))
	}

	store.stores.channel.(*SqlChannelStore).createIndexesIfNotExists()
	store.stores.post.(*SqlPostStore).createIndexesIfNotExists()
	store.stores.retentionPolicy.(*SqlRetentionPolicyStore).createIndexesIfNotExists()
	store.stores.thread.(*SqlThreadStore).createIndexesIfNotExists()
	store.stores.user.(*SqlUserStore).createIndexesIfNotExists()
	store.stores.bot.(*SqlBotStore).createIndexesIfNotExists()
	store.stores.audit.(*SqlAuditStore).createIndexesIfNotExists()
	store.stores.compliance.(*SqlComplianceStore).createIndexesIfNotExists()
	store.stores.session.(*SqlSessionStore).createIndexesIfNotExists()
	store.stores.oauth.(*SqlOAuthStore).createIndexesIfNotExists()
	store.stores.system.(*SqlSystemStore).createIndexesIfNotExists()
	store.stores.webhook.(*SqlWebhookStore).createIndexesIfNotExists()
	store.stores.command.(*SqlCommandStore).createIndexesIfNotExists()
	store.stores.commandWebhook.(*SqlCommandWebhookStore).createIndexesIfNotExists()
	store.stores.preference.(*SqlPreferenceStore).createIndexesIfNotExists()
	store.stores.license.(*SqlLicenseStore).createIndexesIfNotExists()
	store.stores.token.(*SqlTokenStore).createIndexesIfNotExists()
	store.stores.emoji.(*SqlEmojiStore).createIndexesIfNotExists()
	store.stores.status.(*SqlStatusStore).createIndexesIfNotExists()
	store.stores.fileInfo.(*SqlFileInfoStore).createIndexesIfNotExists()
	store.stores.uploadSession.(*SqlUploadSessionStore).createIndexesIfNotExists()
	store.stores.job.(*SqlJobStore).createIndexesIfNotExists()
	store.stores.userAccessToken.(*SqlUserAccessTokenStore).createIndexesIfNotExists()
	store.stores.plugin.(*SqlPluginStore).createIndexesIfNotExists()
	store.stores.TermsOfService.(SqlTermsOfServiceStore).createIndexesIfNotExists()
	store.stores.productNotices.(SqlProductNoticesStore).createIndexesIfNotExists()
	store.stores.UserTermsOfService.(SqlUserTermsOfServiceStore).createIndexesIfNotExists()
	store.stores.linkMetadata.(*SqlLinkMetadataStore).createIndexesIfNotExists()
	store.stores.sharedchannel.(*SqlSharedChannelStore).createIndexesIfNotExists()
	store.stores.group.(*SqlGroupStore).createIndexesIfNotExists()
	store.stores.scheme.(*SqlSchemeStore).createIndexesIfNotExists()
	store.stores.remoteCluster.(*sqlRemoteClusterStore).createIndexesIfNotExists()
	store.stores.preference.(*SqlPreferenceStore).deleteUnusedFeatures()

	return store
}

func setupConnection(connType string, dataSource string, settings *model.SqlSettings) *dbsql.DB {
	db, err := dbsql.Open(*settings.DriverName, dataSource)
	if err != nil {
		mlog.Fatal("Failed to open SQL connection to err.", mlog.Err(err))
	}

	for i := 0; i < DBPingAttempts; i++ {
		mlog.Info("Pinging SQL", mlog.String("database", connType))
		ctx, cancel := context.WithTimeout(context.Background(), DBPingTimeoutSecs*time.Second)
		defer cancel()
		err = db.PingContext(ctx)
		if err == nil {
			break
		} else {
			if i == DBPingAttempts-1 {
				mlog.Fatal("Failed to ping DB, server will exit.", mlog.Err(err))
			} else {
				mlog.Error("Failed to ping DB", mlog.Err(err), mlog.Int("retrying in seconds", DBPingTimeoutSecs))
				time.Sleep(DBPingTimeoutSecs * time.Second)
			}
		}
	}

	if strings.HasPrefix(connType, replicaLagPrefix) {
		// If this is a replica lag connection, we just open one connection.
		//
		// Arguably, if the query doesn't require a special credential, it does take up
		// one extra connection from the replica DB. But falling back to the replica
		// data source when the replica lag data source is null implies an ordering constraint
		// which makes things brittle and is not a good design.
		// If connections are an overhead, it is advised to use a connection pool.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxIdleConns(*settings.MaxIdleConns)
		db.SetMaxOpenConns(*settings.MaxOpenConns)
	}
	db.SetConnMaxLifetime(time.Duration(*settings.ConnMaxLifetimeMilliseconds) * time.Millisecond)
	db.SetConnMaxIdleTime(time.Duration(*settings.ConnMaxIdleTimeMilliseconds) * time.Millisecond)

	return db
}

func getDBMap(settings *model.SqlSettings, db *dbsql.DB) *gorp.DbMap {
	connectionTimeout := time.Duration(*settings.QueryTimeout) * time.Second
	var dbMap *gorp.DbMap
	switch *settings.DriverName {
	case model.DatabaseDriverMysql:
		dbMap = &gorp.DbMap{
			Db:            db,
			TypeConverter: mattermConverter{},
			Dialect:       gorp.MySQLDialect{Engine: "InnoDB", Encoding: "UTF8MB4"},
			QueryTimeout:  connectionTimeout,
		}
	case model.DatabaseDriverPostgres:
		dbMap = &gorp.DbMap{
			Db:            db,
			TypeConverter: mattermConverter{},
			Dialect:       gorp.PostgresDialect{},
			QueryTimeout:  connectionTimeout,
		}
	default:
		mlog.Fatal("Failed to create dialect specific driver")
		return nil
	}
	if settings.Trace != nil && *settings.Trace {
		dbMap.TraceOn("sql-trace:", &TraceOnAdapter{})
	}
	return dbMap
}

func (ss *SqlStore) SetContext(context context.Context) {
	ss.context = context
}

func (ss *SqlStore) Context() context.Context {
	return ss.context
}

func noOpMapper(s string) string { return s }

func (ss *SqlStore) initConnection() {
	dataSource := *ss.settings.DataSource
	if ss.DriverName() == model.DatabaseDriverMysql {
		// TODO: We ignore the readTimeout datasource parameter for MySQL since QueryTimeout
		// covers that already. Ideally we'd like to do this only for the upgrade
		// step. To be reviewed in MM-35789.
		var err error
		dataSource, err = resetReadTimeout(dataSource)
		if err != nil {
			mlog.Fatal("Failed to reset read timeout from datasource.", mlog.Err(err), mlog.String("src", dataSource))
		}
	}

	handle := setupConnection("master", dataSource, ss.settings)
	ss.master = getDBMap(ss.settings, handle)
	ss.masterX = newSqlxDBWrapper(sqlx.NewDb(handle, ss.DriverName()),
		time.Duration(*ss.settings.QueryTimeout)*time.Second,
		*ss.settings.Trace)
	if ss.DriverName() == model.DatabaseDriverMysql {
		ss.masterX.MapperFunc(noOpMapper)
	}

	if len(ss.settings.DataSourceReplicas) > 0 {
		ss.Replicas = make([]*gorp.DbMap, len(ss.settings.DataSourceReplicas))
		ss.ReplicaXs = make([]*sqlxDBWrapper, len(ss.settings.DataSourceReplicas))
		for i, replica := range ss.settings.DataSourceReplicas {
			handle := setupConnection(fmt.Sprintf("replica-%v", i), replica, ss.settings)
			ss.Replicas[i] = getDBMap(ss.settings, handle)
			ss.ReplicaXs[i] = newSqlxDBWrapper(sqlx.NewDb(handle, ss.DriverName()),
				time.Duration(*ss.settings.QueryTimeout)*time.Second,
				*ss.settings.Trace)
			if ss.DriverName() == model.DatabaseDriverMysql {
				ss.ReplicaXs[i].MapperFunc(noOpMapper)
			}
		}
	}

	if len(ss.settings.DataSourceSearchReplicas) > 0 {
		ss.searchReplicas = make([]*gorp.DbMap, len(ss.settings.DataSourceSearchReplicas))
		ss.searchReplicaXs = make([]*sqlxDBWrapper, len(ss.settings.DataSourceSearchReplicas))
		for i, replica := range ss.settings.DataSourceSearchReplicas {
			handle := setupConnection(fmt.Sprintf("search-replica-%v", i), replica, ss.settings)
			ss.searchReplicas[i] = getDBMap(ss.settings, handle)
			ss.searchReplicaXs[i] = newSqlxDBWrapper(sqlx.NewDb(handle, ss.DriverName()),
				time.Duration(*ss.settings.QueryTimeout)*time.Second,
				*ss.settings.Trace)
			if ss.DriverName() == model.DatabaseDriverMysql {
				ss.searchReplicaXs[i].MapperFunc(noOpMapper)
			}
		}
	}

	if len(ss.settings.ReplicaLagSettings) > 0 {
		ss.replicaLagHandles = make([]*dbsql.DB, len(ss.settings.ReplicaLagSettings))
		for i, src := range ss.settings.ReplicaLagSettings {
			if src.DataSource == nil {
				continue
			}
			ss.replicaLagHandles[i] = setupConnection(fmt.Sprintf(replicaLagPrefix+"-%d", i), *src.DataSource, ss.settings)
		}
	}
}

func (ss *SqlStore) DriverName() string {
	return *ss.settings.DriverName
}

func (ss *SqlStore) GetCurrentSchemaVersion() string {
	version, _ := ss.GetMaster().SelectStr("SELECT Value FROM Systems WHERE Name='Version'")
	return version
}

// GetDbVersion returns the version of the database being used.
// If numerical is set to true, it attempts to return a numerical version string
// that can be parsed by callers.
func (ss *SqlStore) GetDbVersion(numerical bool) (string, error) {
	var sqlVersion string
	if ss.DriverName() == model.DatabaseDriverPostgres {
		if numerical {
			sqlVersion = `SHOW server_version_num`
		} else {
			sqlVersion = `SHOW server_version`
		}
	} else if ss.DriverName() == model.DatabaseDriverMysql {
		sqlVersion = `SELECT version()`
	} else {
		return "", errors.New("Not supported driver")
	}

	version, err := ss.GetReplica().SelectStr(sqlVersion)
	if err != nil {
		return "", err
	}

	return version, nil

}

func (ss *SqlStore) GetMaster() *gorp.DbMap {
	return ss.master
}

func (ss *SqlStore) GetMasterX() *sqlxDBWrapper {
	return ss.masterX
}

func (ss *SqlStore) SetMasterX(db *sql.DB) {
	ss.masterX = newSqlxDBWrapper(sqlx.NewDb(db, ss.DriverName()),
		time.Duration(*ss.settings.QueryTimeout)*time.Second,
		*ss.settings.Trace)
	if ss.DriverName() == model.DatabaseDriverMysql {
		ss.masterX.MapperFunc(noOpMapper)
	}
}

func (ss *SqlStore) GetSearchReplica() *gorp.DbMap {
	ss.licenseMutex.RLock()
	license := ss.license
	ss.licenseMutex.RUnlock()
	if license == nil {
		return ss.GetMaster()
	}

	if len(ss.settings.DataSourceSearchReplicas) == 0 {
		return ss.GetReplica()
	}

	rrNum := atomic.AddInt64(&ss.srCounter, 1) % int64(len(ss.searchReplicas))
	return ss.searchReplicas[rrNum]
}

func (ss *SqlStore) GetSearchReplicaX() *sqlxDBWrapper {
	ss.licenseMutex.RLock()
	license := ss.license
	ss.licenseMutex.RUnlock()
	if license == nil {
		return ss.GetMasterX()
	}

	if len(ss.settings.DataSourceSearchReplicas) == 0 {
		return ss.GetReplicaX()
	}

	rrNum := atomic.AddInt64(&ss.srCounter, 1) % int64(len(ss.searchReplicaXs))
	return ss.searchReplicaXs[rrNum]
}

func (ss *SqlStore) GetReplica() *gorp.DbMap {
	ss.licenseMutex.RLock()
	license := ss.license
	ss.licenseMutex.RUnlock()
	if len(ss.settings.DataSourceReplicas) == 0 || ss.lockedToMaster || license == nil {
		return ss.GetMaster()
	}

	rrNum := atomic.AddInt64(&ss.rrCounter, 1) % int64(len(ss.Replicas))
	return ss.Replicas[rrNum]
}

func (ss *SqlStore) GetReplicaX() *sqlxDBWrapper {
	ss.licenseMutex.RLock()
	license := ss.license
	ss.licenseMutex.RUnlock()
	if len(ss.settings.DataSourceReplicas) == 0 || ss.lockedToMaster || license == nil {
		return ss.GetMasterX()
	}

	rrNum := atomic.AddInt64(&ss.rrCounter, 1) % int64(len(ss.Replicas))
	return ss.ReplicaXs[rrNum]
}

func (ss *SqlStore) TotalMasterDbConnections() int {
	return ss.GetMaster().Db.Stats().OpenConnections
}

// ReplicaLagAbs queries all the replica databases to get the absolute replica lag value
// and updates the Prometheus metric with it.
func (ss *SqlStore) ReplicaLagAbs() error {
	for i, item := range ss.settings.ReplicaLagSettings {
		if item.QueryAbsoluteLag == nil || *item.QueryAbsoluteLag == "" {
			continue
		}
		var binDiff float64
		var node string
		err := ss.replicaLagHandles[i].QueryRow(*item.QueryAbsoluteLag).Scan(&node, &binDiff)
		if err != nil {
			return err
		}
		// There is no nil check needed here because it's called from the metrics store.
		ss.metrics.SetReplicaLagAbsolute(node, binDiff)
	}
	return nil
}

// ReplicaLagAbs queries all the replica databases to get the time-based replica lag value
// and updates the Prometheus metric with it.
func (ss *SqlStore) ReplicaLagTime() error {
	for i, item := range ss.settings.ReplicaLagSettings {
		if item.QueryTimeLag == nil || *item.QueryTimeLag == "" {
			continue
		}
		var timeDiff float64
		var node string
		err := ss.replicaLagHandles[i].QueryRow(*item.QueryTimeLag).Scan(&node, &timeDiff)
		if err != nil {
			return err
		}
		// There is no nil check needed here because it's called from the metrics store.
		ss.metrics.SetReplicaLagTime(node, timeDiff)
	}
	return nil
}

func (ss *SqlStore) TotalReadDbConnections() int {
	if len(ss.settings.DataSourceReplicas) == 0 {
		return 0
	}

	count := 0
	for _, db := range ss.Replicas {
		count = count + db.Db.Stats().OpenConnections
	}

	return count
}

func (ss *SqlStore) TotalSearchDbConnections() int {
	if len(ss.settings.DataSourceSearchReplicas) == 0 {
		return 0
	}

	count := 0
	for _, db := range ss.searchReplicas {
		count = count + db.Db.Stats().OpenConnections
	}

	return count
}

func (ss *SqlStore) MarkSystemRanUnitTests() {
	props, err := ss.System().Get()
	if err != nil {
		return
	}

	unitTests := props[model.SystemRanUnitTests]
	if unitTests == "" {
		systemTests := &model.System{Name: model.SystemRanUnitTests, Value: "1"}
		ss.System().Save(systemTests)
	}
}

func (ss *SqlStore) DoesTableExist(tableName string) bool {
	if ss.DriverName() == model.DatabaseDriverPostgres {
		count, err := ss.GetMaster().SelectInt(
			`SELECT count(relname) FROM pg_class WHERE relname=$1`,
			strings.ToLower(tableName),
		)

		if err != nil {
			mlog.Fatal("Failed to check if table exists", mlog.Err(err))
		}

		return count > 0

	} else if ss.DriverName() == model.DatabaseDriverMysql {

		count, err := ss.GetMaster().SelectInt(
			`SELECT
		    COUNT(0) AS table_exists
			FROM
			    information_schema.TABLES
			WHERE
			    TABLE_SCHEMA = DATABASE()
			        AND TABLE_NAME = ?
		    `,
			tableName,
		)

		if err != nil {
			mlog.Fatal("Failed to check if table exists", mlog.Err(err))
		}

		return count > 0

	} else {
		mlog.Fatal("Failed to check if column exists because of missing driver")
		return false
	}
}

func (ss *SqlStore) DoesColumnExist(tableName string, columnName string) bool {
	if ss.DriverName() == model.DatabaseDriverPostgres {
		count, err := ss.GetMaster().SelectInt(
			`SELECT COUNT(0)
			FROM   pg_attribute
			WHERE  attrelid = $1::regclass
			AND    attname = $2
			AND    NOT attisdropped`,
			strings.ToLower(tableName),
			strings.ToLower(columnName),
		)

		if err != nil {
			if err.Error() == "pq: relation \""+strings.ToLower(tableName)+"\" does not exist" {
				return false
			}

			mlog.Fatal("Failed to check if column exists", mlog.Err(err))
		}

		return count > 0

	} else if ss.DriverName() == model.DatabaseDriverMysql {

		count, err := ss.GetMaster().SelectInt(
			`SELECT
		    COUNT(0) AS column_exists
		FROM
		    information_schema.COLUMNS
		WHERE
		    TABLE_SCHEMA = DATABASE()
		        AND TABLE_NAME = ?
		        AND COLUMN_NAME = ?`,
			tableName,
			columnName,
		)

		if err != nil {
			mlog.Fatal("Failed to check if column exists", mlog.Err(err))
		}

		return count > 0

	} else {
		mlog.Fatal("Failed to check if column exists because of missing driver")
		return false
	}
}

// GetColumnInfo returns data type information about the given column.
func (ss *SqlStore) GetColumnInfo(tableName, columnName string) (*ColumnInfo, error) {
	var columnInfo ColumnInfo
	if ss.DriverName() == model.DatabaseDriverPostgres {
		err := ss.GetMaster().SelectOne(&columnInfo,
			`SELECT data_type as DataType, COALESCE(column_default, '') as DefaultValue,
					COALESCE(character_maximum_length, 0) as CharMaximumLength
			 FROM information_schema.columns
			 WHERE lower(table_name) = lower($1)
			 AND lower(column_name) = lower($2)`,
			tableName, columnName)
		if err != nil {
			return nil, err
		}
		return &columnInfo, nil
	} else if ss.DriverName() == model.DatabaseDriverMysql {
		err := ss.GetMaster().SelectOne(&columnInfo,
			`SELECT data_type as DataType, COALESCE(column_default, '') as DefaultValue,
					COALESCE(character_maximum_length, 0) as CharMaximumLength
			 FROM information_schema.columns
			 WHERE table_schema = DATABASE()
			 AND lower(table_name) = lower(?)
			 AND lower(column_name) = lower(?)`,
			tableName, columnName)
		if err != nil {
			return nil, err
		}
		return &columnInfo, nil
	}
	return nil, errors.New("Driver not supported for this method")
}

// IsVarchar returns true if the column type matches one of the varchar types
// either in MySQL or PostgreSQL.
func (ss *SqlStore) IsVarchar(columnType string) bool {
	if ss.DriverName() == model.DatabaseDriverPostgres && columnType == "character varying" {
		return true
	}

	if ss.DriverName() == model.DatabaseDriverMysql && columnType == "varchar" {
		return true
	}

	return false
}

func (ss *SqlStore) DoesTriggerExist(triggerName string) bool {
	if ss.DriverName() == model.DatabaseDriverPostgres {
		count, err := ss.GetMaster().SelectInt(`
			SELECT
				COUNT(0)
			FROM
				pg_trigger
			WHERE
				tgname = $1
		`, triggerName)

		if err != nil {
			mlog.Fatal("Failed to check if trigger exists", mlog.Err(err))
		}

		return count > 0

	} else if ss.DriverName() == model.DatabaseDriverMysql {
		count, err := ss.GetMaster().SelectInt(`
			SELECT
				COUNT(0)
			FROM
				information_schema.triggers
			WHERE
				trigger_schema = DATABASE()
			AND	trigger_name = ?
		`, triggerName)

		if err != nil {
			mlog.Fatal("Failed to check if trigger exists", mlog.Err(err))
		}

		return count > 0

	} else {
		mlog.Fatal("Failed to check if column exists because of missing driver")
		return false
	}
}

func (ss *SqlStore) CreateColumnIfNotExists(tableName string, columnName string, mySqlColType string, postgresColType string, defaultValue string) bool {

	if ss.DoesColumnExist(tableName, columnName) {
		return false
	}

	if ss.DriverName() == model.DatabaseDriverPostgres {
		_, err := ss.GetMaster().ExecNoTimeout("ALTER TABLE " + tableName + " ADD " + columnName + " " + postgresColType + " DEFAULT '" + defaultValue + "'")
		if err != nil {
			mlog.Fatal("Failed to create column", mlog.Err(err))
		}

		return true

	} else if ss.DriverName() == model.DatabaseDriverMysql {
		_, err := ss.GetMaster().ExecNoTimeout("ALTER TABLE " + tableName + " ADD " + columnName + " " + mySqlColType + " DEFAULT '" + defaultValue + "'")
		if err != nil {
			mlog.Fatal("Failed to create column", mlog.Err(err))
		}

		return true

	} else {
		mlog.Fatal("Failed to create column because of missing driver")
		return false
	}
}

func (ss *SqlStore) CreateColumnIfNotExistsNoDefault(tableName string, columnName string, mySqlColType string, postgresColType string) bool {

	if ss.DoesColumnExist(tableName, columnName) {
		return false
	}

	if ss.DriverName() == model.DatabaseDriverPostgres {
		_, err := ss.GetMaster().ExecNoTimeout("ALTER TABLE " + tableName + " ADD " + columnName + " " + postgresColType)
		if err != nil {
			mlog.Fatal("Failed to create column", mlog.Err(err))
		}

		return true

	} else if ss.DriverName() == model.DatabaseDriverMysql {
		_, err := ss.GetMaster().ExecNoTimeout("ALTER TABLE " + tableName + " ADD " + columnName + " " + mySqlColType)
		if err != nil {
			mlog.Fatal("Failed to create column", mlog.Err(err))
		}

		return true

	} else {
		mlog.Fatal("Failed to create column because of missing driver")
		return false
	}
}

func (ss *SqlStore) RemoveColumnIfExists(tableName string, columnName string) bool {

	if !ss.DoesColumnExist(tableName, columnName) {
		return false
	}

	_, err := ss.GetMaster().ExecNoTimeout("ALTER TABLE " + tableName + " DROP COLUMN " + columnName)
	if err != nil {
		mlog.Fatal("Failed to drop column", mlog.Err(err))
	}

	return true
}

func (ss *SqlStore) RemoveTableIfExists(tableName string) bool {
	if !ss.DoesTableExist(tableName) {
		return false
	}

	_, err := ss.GetMaster().ExecNoTimeout("DROP TABLE " + tableName)
	if err != nil {
		mlog.Fatal("Failed to drop table", mlog.Err(err))
	}

	return true
}

func (ss *SqlStore) RenameColumnIfExists(tableName string, oldColumnName string, newColumnName string, colType string) bool {
	if !ss.DoesColumnExist(tableName, oldColumnName) {
		return false
	}

	var err error
	if ss.DriverName() == model.DatabaseDriverMysql {
		_, err = ss.GetMaster().ExecNoTimeout("ALTER TABLE " + tableName + " CHANGE " + oldColumnName + " " + newColumnName + " " + colType)
	} else if ss.DriverName() == model.DatabaseDriverPostgres {
		_, err = ss.GetMaster().ExecNoTimeout("ALTER TABLE " + tableName + " RENAME COLUMN " + oldColumnName + " TO " + newColumnName)
	}

	if err != nil {
		mlog.Fatal("Failed to rename column", mlog.Err(err))
	}

	return true
}

func (ss *SqlStore) GetMaxLengthOfColumnIfExists(tableName string, columnName string) string {
	if !ss.DoesColumnExist(tableName, columnName) {
		return ""
	}

	var result string
	var err error
	if ss.DriverName() == model.DatabaseDriverMysql {
		result, err = ss.GetMaster().SelectStr("SELECT CHARACTER_MAXIMUM_LENGTH FROM information_schema.columns WHERE table_name = '" + tableName + "' AND COLUMN_NAME = '" + columnName + "'")
	} else if ss.DriverName() == model.DatabaseDriverPostgres {
		result, err = ss.GetMaster().SelectStr("SELECT character_maximum_length FROM information_schema.columns WHERE table_name = '" + strings.ToLower(tableName) + "' AND column_name = '" + strings.ToLower(columnName) + "'")
	}

	if err != nil {
		mlog.Fatal("Failed to get max length of column", mlog.Err(err))
	}

	return result
}

func (ss *SqlStore) AlterColumnTypeIfExists(tableName string, columnName string, mySqlColType string, postgresColType string) bool {
	if !ss.DoesColumnExist(tableName, columnName) {
		return false
	}

	var err error
	if ss.DriverName() == model.DatabaseDriverMysql {
		_, err = ss.GetMaster().ExecNoTimeout("ALTER TABLE " + tableName + " MODIFY " + columnName + " " + mySqlColType)
	} else if ss.DriverName() == model.DatabaseDriverPostgres {
		query := "ALTER TABLE " + strings.ToLower(tableName) + " ALTER COLUMN " + strings.ToLower(columnName) + " TYPE " + postgresColType
		// We need to explicitly cast when moving from text based to jsonb datatypes.
		if postgresColType == "jsonb" {
			query += " USING " + strings.ToLower(columnName) + "::jsonb"
		}
		_, err = ss.GetMaster().ExecNoTimeout(query)
	}

	if err != nil {
		msg := "Failed to alter column type."
		fields := []mlog.Field{mlog.Err(err)}
		if mySqlColType == "JSON" && postgresColType == "jsonb" {
			msg += " It is likely you have invalid JSON values in the column. Please fix the values manually and run the migration again."
			fields = append(fields,
				mlog.String("tableName", tableName),
				mlog.String("columnName", columnName),
			)
		}
		mlog.Fatal(msg, fields...)
	}

	return true
}

func (ss *SqlStore) RemoveDefaultIfColumnExists(tableName, columnName string) bool {
	if !ss.DoesColumnExist(tableName, columnName) {
		return false
	}

	_, err := ss.GetMaster().ExecNoTimeout("ALTER TABLE " + tableName + " ALTER COLUMN " + columnName + " DROP DEFAULT")
	if err != nil {
		mlog.Fatal("Failed to drop column default", mlog.String("table", tableName), mlog.String("column", columnName), mlog.Err(err))
		return false
	}

	return true
}

func (ss *SqlStore) AlterDefaultIfColumnExists(tableName string, columnName string, mySqlColDefault *string, postgresColDefault *string) bool {
	if !ss.DoesColumnExist(tableName, columnName) {
		return false
	}

	var defaultValue string
	if ss.DriverName() == model.DatabaseDriverMysql {
		// Some column types in MySQL cannot have defaults, so don't try to configure anything.
		if mySqlColDefault == nil {
			return true
		}

		defaultValue = *mySqlColDefault
	} else if ss.DriverName() == model.DatabaseDriverPostgres {
		// Postgres doesn't have the same limitation, but preserve the interface.
		if postgresColDefault == nil {
			return true
		}

		tableName = strings.ToLower(tableName)
		columnName = strings.ToLower(columnName)
		defaultValue = *postgresColDefault
	} else {
		mlog.Fatal("Failed to alter column default because of missing driver")
		return false
	}

	if defaultValue == "" {
		defaultValue = "''"
	}

	query := "ALTER TABLE " + tableName + " ALTER COLUMN " + columnName + " SET DEFAULT " + defaultValue
	_, err := ss.GetMaster().ExecNoTimeout(query)
	if err != nil {
		mlog.Fatal("Failed to alter column default", mlog.String("table", tableName), mlog.String("column", columnName), mlog.String("default value", defaultValue), mlog.Err(err))
		return false
	}

	return true
}

func (ss *SqlStore) AlterPrimaryKey(tableName string, columnNames []string) bool {
	var currentPrimaryKey string
	var err error
	// get the current primary key as a comma separated list of columns
	switch ss.DriverName() {
	case model.DatabaseDriverMysql:
		query := `
			SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index) AS PK
		FROM
			information_schema.statistics
		WHERE
			table_schema = DATABASE()
		AND table_name = ?
		AND index_name = 'PRIMARY'
		GROUP BY
			index_name`
		currentPrimaryKey, err = ss.GetMaster().SelectStr(query, tableName)
	case model.DatabaseDriverPostgres:
		query := `
			SELECT string_agg(a.attname, ',') AS pk
		FROM
			pg_constraint AS c
		CROSS JOIN
			(SELECT unnest(conkey) FROM pg_constraint WHERE conrelid='` + strings.ToLower(tableName) + `'::REGCLASS AND contype='p') AS cols(colnum)
		INNER JOIN
			pg_attribute AS a ON a.attrelid = c.conrelid
		AND cols.colnum = a.attnum
		WHERE
			c.contype = 'p'
		AND c.conrelid = '` + strings.ToLower(tableName) + `'::REGCLASS`
		currentPrimaryKey, err = ss.GetMaster().SelectStr(query)
	}
	if err != nil {
		mlog.Fatal("Failed to get current primary key", mlog.String("table", tableName), mlog.Err(err))
	}

	primaryKey := strings.Join(columnNames, ",")
	if strings.EqualFold(currentPrimaryKey, primaryKey) {
		return false
	}
	// alter primary key
	var alterQuery string
	if ss.DriverName() == model.DatabaseDriverMysql {
		alterQuery = "ALTER TABLE " + tableName + " DROP PRIMARY KEY, ADD PRIMARY KEY (" + primaryKey + ")"
	} else if ss.DriverName() == model.DatabaseDriverPostgres {
		alterQuery = "ALTER TABLE " + tableName + " DROP CONSTRAINT " + strings.ToLower(tableName) + "_pkey, ADD PRIMARY KEY (" + strings.ToLower(primaryKey) + ")"
	}
	_, err = ss.GetMaster().ExecNoTimeout(alterQuery)
	if err != nil {
		mlog.Fatal("Failed to alter primary key", mlog.String("table", tableName), mlog.Err(err))
	}
	return true
}

func (ss *SqlStore) CreateUniqueIndexIfNotExists(indexName string, tableName string, columnName string) bool {
	return ss.createIndexIfNotExists(indexName, tableName, []string{columnName}, IndexTypeDefault, true)
}

func (ss *SqlStore) CreateIndexIfNotExists(indexName string, tableName string, columnName string) bool {
	return ss.createIndexIfNotExists(indexName, tableName, []string{columnName}, IndexTypeDefault, false)
}

func (ss *SqlStore) CreateCompositeIndexIfNotExists(indexName string, tableName string, columnNames []string) bool {
	return ss.createIndexIfNotExists(indexName, tableName, columnNames, IndexTypeDefault, false)
}

func (ss *SqlStore) CreateUniqueCompositeIndexIfNotExists(indexName string, tableName string, columnNames []string) bool {
	return ss.createIndexIfNotExists(indexName, tableName, columnNames, IndexTypeDefault, true)
}

func (ss *SqlStore) CreateFullTextIndexIfNotExists(indexName string, tableName string, columnName string) bool {
	return ss.createIndexIfNotExists(indexName, tableName, []string{columnName}, IndexTypeFullText, false)
}

func (ss *SqlStore) CreateFullTextFuncIndexIfNotExists(indexName string, tableName string, function string) bool {
	return ss.createIndexIfNotExists(indexName, tableName, []string{function}, IndexTypeFullTextFunc, false)
}

func (ss *SqlStore) createIndexIfNotExists(indexName string, tableName string, columnNames []string, indexType string, unique bool) bool {

	uniqueStr := ""
	if unique {
		uniqueStr = "UNIQUE "
	}

	if ss.DriverName() == model.DatabaseDriverPostgres {
		_, errExists := ss.GetMaster().SelectStr("SELECT $1::regclass", indexName)
		// It should fail if the index does not exist
		if errExists == nil {
			return false
		}

		query := ""
		if indexType == IndexTypeFullText {
			if len(columnNames) != 1 {
				mlog.Fatal("Unable to create multi column full text index")
			}
			columnName := columnNames[0]
			postgresColumnNames := convertMySQLFullTextColumnsToPostgres(columnName)
			query = "CREATE INDEX " + indexName + " ON " + tableName + " USING gin(to_tsvector('english', " + postgresColumnNames + "))"
		} else if indexType == IndexTypeFullTextFunc {
			if len(columnNames) != 1 {
				mlog.Fatal("Unable to create multi column full text index")
			}
			columnName := columnNames[0]
			query = "CREATE INDEX " + indexName + " ON " + tableName + " USING gin(to_tsvector('english', " + columnName + "))"
		} else {
			query = "CREATE " + uniqueStr + "INDEX " + indexName + " ON " + tableName + " (" + strings.Join(columnNames, ", ") + ")"
		}

		_, err := ss.GetMaster().ExecNoTimeout(query)
		if err != nil {
			mlog.Fatal("Failed to create index", mlog.Err(errExists), mlog.Err(err))
		}
	} else if ss.DriverName() == model.DatabaseDriverMysql {

		count, err := ss.GetMaster().SelectInt("SELECT COUNT(0) AS index_exists FROM information_schema.statistics WHERE TABLE_SCHEMA = DATABASE() and table_name = ? AND index_name = ?", tableName, indexName)
		if err != nil {
			mlog.Fatal("Failed to check index", mlog.Err(err))
		}

		if count > 0 {
			return false
		}

		fullTextIndex := ""
		if indexType == IndexTypeFullText {
			fullTextIndex = " FULLTEXT "
		}

		_, err = ss.GetMaster().ExecNoTimeout("CREATE  " + uniqueStr + fullTextIndex + " INDEX " + indexName + " ON " + tableName + " (" + strings.Join(columnNames, ", ") + ")")
		if err != nil {
			mlog.Fatal("Failed to create index", mlog.String("table", tableName), mlog.String("index_name", indexName), mlog.Err(err))
		}
	} else {
		mlog.Fatal("Failed to create index because of missing driver")
	}

	return true
}

func (ss *SqlStore) CreateForeignKeyIfNotExists(
	tableName, columnName, refTableName, refColumnName string,
	onDeleteCascade bool,
) (err error) {
	deleteClause := ""
	if onDeleteCascade {
		deleteClause = "ON DELETE CASCADE"
	}
	constraintName := "FK_" + tableName + "_" + refTableName
	sQuery := `
	ALTER TABLE ` + tableName + `
	ADD CONSTRAINT ` + constraintName + `
	FOREIGN KEY (` + columnName + `) REFERENCES ` + refTableName + ` (` + refColumnName + `)
	` + deleteClause + `;`
	_, err = ss.GetMaster().ExecNoTimeout(sQuery)
	if IsConstraintAlreadyExistsError(err) {
		err = nil
	}
	if err != nil {
		mlog.Warn("Could not create foreign key: " + err.Error())
	}
	return
}

func (ss *SqlStore) RemoveIndexIfExists(indexName string, tableName string) bool {

	if ss.DriverName() == model.DatabaseDriverPostgres {
		_, err := ss.GetMaster().SelectStr("SELECT $1::regclass", indexName)
		// It should fail if the index does not exist
		if err != nil {
			return false
		}

		_, err = ss.GetMaster().ExecNoTimeout("DROP INDEX " + indexName)
		if err != nil {
			mlog.Fatal("Failed to remove index", mlog.Err(err))
		}

		return true
	} else if ss.DriverName() == model.DatabaseDriverMysql {

		count, err := ss.GetMaster().SelectInt("SELECT COUNT(0) AS index_exists FROM information_schema.statistics WHERE TABLE_SCHEMA = DATABASE() and table_name = ? AND index_name = ?", tableName, indexName)
		if err != nil {
			mlog.Fatal("Failed to check index", mlog.Err(err))
		}

		if count <= 0 {
			return false
		}

		_, err = ss.GetMaster().ExecNoTimeout("DROP INDEX " + indexName + " ON " + tableName)
		if err != nil {
			mlog.Fatal("Failed to remove index", mlog.Err(err))
		}
	} else {
		mlog.Fatal("Failed to create index because of missing driver")
	}

	return true
}

func IsConstraintAlreadyExistsError(err error) bool {
	switch dbErr := err.(type) {
	case *pq.Error:
		if dbErr.Code == PGDuplicateObjectErrorCode {
			return true
		}
	case *mysql.MySQLError:
		if dbErr.Number == MySQLDuplicateObjectErrorCode {
			return true
		}
	}
	return false
}

func IsUniqueConstraintError(err error, indexName []string) bool {
	unique := false
	if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
		unique = true
	}

	if mysqlErr, ok := err.(*mysql.MySQLError); ok && mysqlErr.Number == 1062 {
		unique = true
	}

	field := false
	for _, contain := range indexName {
		if strings.Contains(err.Error(), contain) {
			field = true
			break
		}
	}

	return unique && field
}

func (ss *SqlStore) GetAllConns() []*gorp.DbMap {
	all := make([]*gorp.DbMap, len(ss.Replicas)+1)
	copy(all, ss.Replicas)
	all[len(ss.Replicas)] = ss.master
	return all
}

// RecycleDBConnections closes active connections by setting the max conn lifetime
// to d, and then resets them back to their original duration.
func (ss *SqlStore) RecycleDBConnections(d time.Duration) {
	// Get old time.
	originalDuration := time.Duration(*ss.settings.ConnMaxLifetimeMilliseconds) * time.Millisecond
	// Set the max lifetimes for all connections.
	for _, conn := range ss.GetAllConns() {
		conn.Db.SetConnMaxLifetime(d)
	}
	// Wait for that period with an additional 2 seconds of scheduling delay.
	time.Sleep(d + 2*time.Second)
	// Reset max lifetime back to original value.
	for _, conn := range ss.GetAllConns() {
		conn.Db.SetConnMaxLifetime(originalDuration)
	}
}

func (ss *SqlStore) Close() {
	ss.master.Db.Close()
	for _, replica := range ss.Replicas {
		replica.Db.Close()
	}

	for _, replica := range ss.searchReplicas {
		replica.Db.Close()
	}
}

func (ss *SqlStore) LockToMaster() {
	ss.lockedToMaster = true
}

func (ss *SqlStore) UnlockFromMaster() {
	ss.lockedToMaster = false
}

func (ss *SqlStore) Team() store.TeamStore {
	return ss.stores.team
}

func (ss *SqlStore) Channel() store.ChannelStore {
	return ss.stores.channel
}

func (ss *SqlStore) Post() store.PostStore {
	return ss.stores.post
}

func (ss *SqlStore) RetentionPolicy() store.RetentionPolicyStore {
	return ss.stores.retentionPolicy
}

func (ss *SqlStore) User() store.UserStore {
	return ss.stores.user
}

func (ss *SqlStore) Bot() store.BotStore {
	return ss.stores.bot
}

func (ss *SqlStore) Session() store.SessionStore {
	return ss.stores.session
}

func (ss *SqlStore) Audit() store.AuditStore {
	return ss.stores.audit
}

func (ss *SqlStore) ClusterDiscovery() store.ClusterDiscoveryStore {
	return ss.stores.cluster
}

func (ss *SqlStore) RemoteCluster() store.RemoteClusterStore {
	return ss.stores.remoteCluster
}

func (ss *SqlStore) Compliance() store.ComplianceStore {
	return ss.stores.compliance
}

func (ss *SqlStore) OAuth() store.OAuthStore {
	return ss.stores.oauth
}

func (ss *SqlStore) System() store.SystemStore {
	return ss.stores.system
}

func (ss *SqlStore) Webhook() store.WebhookStore {
	return ss.stores.webhook
}

func (ss *SqlStore) Command() store.CommandStore {
	return ss.stores.command
}

func (ss *SqlStore) CommandWebhook() store.CommandWebhookStore {
	return ss.stores.commandWebhook
}

func (ss *SqlStore) Preference() store.PreferenceStore {
	return ss.stores.preference
}

func (ss *SqlStore) License() store.LicenseStore {
	return ss.stores.license
}

func (ss *SqlStore) Token() store.TokenStore {
	return ss.stores.token
}

func (ss *SqlStore) Emoji() store.EmojiStore {
	return ss.stores.emoji
}

func (ss *SqlStore) Status() store.StatusStore {
	return ss.stores.status
}

func (ss *SqlStore) FileInfo() store.FileInfoStore {
	return ss.stores.fileInfo
}

func (ss *SqlStore) UploadSession() store.UploadSessionStore {
	return ss.stores.uploadSession
}

func (ss *SqlStore) Reaction() store.ReactionStore {
	return ss.stores.reaction
}

func (ss *SqlStore) Job() store.JobStore {
	return ss.stores.job
}

func (ss *SqlStore) UserAccessToken() store.UserAccessTokenStore {
	return ss.stores.userAccessToken
}

func (ss *SqlStore) ChannelMemberHistory() store.ChannelMemberHistoryStore {
	return ss.stores.channelMemberHistory
}

func (ss *SqlStore) Plugin() store.PluginStore {
	return ss.stores.plugin
}

func (ss *SqlStore) Thread() store.ThreadStore {
	return ss.stores.thread
}

func (ss *SqlStore) Role() store.RoleStore {
	return ss.stores.role
}

func (ss *SqlStore) TermsOfService() store.TermsOfServiceStore {
	return ss.stores.TermsOfService
}

func (ss *SqlStore) ProductNotices() store.ProductNoticesStore {
	return ss.stores.productNotices
}

func (ss *SqlStore) UserTermsOfService() store.UserTermsOfServiceStore {
	return ss.stores.UserTermsOfService
}

func (ss *SqlStore) Scheme() store.SchemeStore {
	return ss.stores.scheme
}

func (ss *SqlStore) Group() store.GroupStore {
	return ss.stores.group
}

func (ss *SqlStore) LinkMetadata() store.LinkMetadataStore {
	return ss.stores.linkMetadata
}

func (ss *SqlStore) SharedChannel() store.SharedChannelStore {
	return ss.stores.sharedchannel
}

func (ss *SqlStore) DropAllTables() {
	ss.master.TruncateTables()
}

func (ss *SqlStore) getQueryBuilder() sq.StatementBuilderType {
	builder := sq.StatementBuilder.PlaceholderFormat(sq.Question)
	if ss.DriverName() == model.DatabaseDriverPostgres {
		builder = builder.PlaceholderFormat(sq.Dollar)
	}
	return builder
}

func (ss *SqlStore) CheckIntegrity() <-chan model.IntegrityCheckResult {
	results := make(chan model.IntegrityCheckResult)
	go CheckRelationalIntegrity(ss, results)
	return results
}

func (ss *SqlStore) UpdateLicense(license *model.License) {
	ss.licenseMutex.Lock()
	defer ss.licenseMutex.Unlock()
	ss.license = license
}

func (ss *SqlStore) GetLicense() *model.License {
	return ss.license
}

func (ss *SqlStore) migrate(direction migrationDirection) error {
	var driver database.Driver
	var err error

	// golang-migrate doesn't have a way to reuse an existing connection in a migration.
	// The current API will always close the current connection _as well as_ the *sql.DB
	// instance along with it. Which means that we cannot pass an existing DB instance,
	// because it will be closed. Therefore, we always have to create a new instance,
	// and therefore a new connection.
	dataSource, err := ss.appendMultipleStatementsFlag(*ss.settings.DataSource)
	if err != nil {
		return err
	}
	db := setupConnection("migrations", dataSource, ss.settings)
	defer db.Close()

	if ss.DriverName() == model.DatabaseDriverMysql {
		driver, err = mysqlmigrate.WithInstance(db, &mysqlmigrate.Config{})
		if err != nil {
			return err
		}
	} else {
		driver, err = postgres.WithInstance(db, &postgres.Config{})
		if err != nil {
			return err
		}
	}

	var assetNamesForDriver []string
	for _, assetName := range migrations.AssetNames() {
		if strings.HasPrefix(assetName, ss.DriverName()) {
			assetNamesForDriver = append(assetNamesForDriver, filepath.Base(assetName))
		}
	}

	source := bindata.Resource(assetNamesForDriver, func(name string) ([]byte, error) {
		return migrations.Asset(filepath.Join(ss.DriverName(), name))
	})

	sourceDriver, err := bindata.WithInstance(source)
	if err != nil {
		return err
	}

	migrations, err := migrate.NewWithInstance("go-bindata",
		sourceDriver,
		ss.DriverName(),
		driver)

	if err != nil {
		return err
	}
	defer migrations.Close()

	switch direction {
	case migrationsDirectionUp:
		err = migrations.Up()
	case migrationsDirectionDown:
		err = migrations.Down()
	default:
		return errors.New(fmt.Sprintf("unsupported migration direction %s", direction))
	}

	if err != nil && err != migrate.ErrNoChange && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func (ss *SqlStore) appendMultipleStatementsFlag(dataSource string) (string, error) {
	// We need to tell the MySQL driver that we want to use multiStatements
	// in order to make migrations work.
	if ss.DriverName() == model.DatabaseDriverMysql {
		config, err := mysql.ParseDSN(dataSource)
		if err != nil {
			return "", err
		}

		if config.Params == nil {
			config.Params = map[string]string{}
		}

		config.Params["multiStatements"] = "true"
		return config.FormatDSN(), nil
	}

	return dataSource, nil
}

func resetReadTimeout(dataSource string) (string, error) {
	config, err := mysql.ParseDSN(dataSource)
	if err != nil {
		return "", err
	}
	config.ReadTimeout = 0
	return config.FormatDSN(), nil
}

func convertMySQLFullTextColumnsToPostgres(columnNames string) string {
	columns := strings.Split(columnNames, ", ")
	concatenatedColumnNames := ""
	for i, c := range columns {
		concatenatedColumnNames += c
		if i < len(columns)-1 {
			concatenatedColumnNames += " || ' ' || "
		}
	}

	return concatenatedColumnNames
}

// IsDuplicate checks whether an error is a duplicate key error, which comes when processes are competing on creating the same
// tables in the database.
func IsDuplicate(err error) bool {
	var pqErr *pq.Error
	var mysqlErr *mysql.MySQLError
	switch {
	case errors.As(errors.Cause(err), &pqErr):
		if pqErr.Code == PGDupTableErrorCode {
			return true
		}
	case errors.As(errors.Cause(err), &mysqlErr):
		if mysqlErr.Number == MySQLDupTableErrorCode {
			return true
		}
	}

	return false
}

// ensureMinimumDBVersion gets the DB version and ensures it is
// above the required minimum version requirements.
func (ss *SqlStore) ensureMinimumDBVersion(ver string) (bool, error) {
	switch *ss.settings.DriverName {
	case model.DatabaseDriverPostgres:
		intVer, err2 := strconv.Atoi(ver)
		if err2 != nil {
			return false, fmt.Errorf("cannot parse DB version: %v", err2)
		}
		if intVer < minimumRequiredPostgresVersion {
			return false, fmt.Errorf("minimum Postgres version requirements not met. Found: %s, Wanted: %s", versionString(intVer, *ss.settings.DriverName), versionString(minimumRequiredPostgresVersion, *ss.settings.DriverName))
		}
	case model.DatabaseDriverMysql:
		// Usually a version string is of the form 5.6.49-log, 10.4.5-MariaDB etc.
		if strings.Contains(strings.ToLower(ver), "maria") {
			mlog.Debug("MariaDB detected. Skipping version check.")
			return true, nil
		}
		parts := strings.Split(ver, "-")
		if len(parts) < 1 {
			return false, fmt.Errorf("cannot parse MySQL DB version: %s", ver)
		}
		// Get the major and minor versions.
		versions := strings.Split(parts[0], ".")
		if len(versions) < 3 {
			return false, fmt.Errorf("cannot parse MySQL DB version: %s", ver)
		}
		majorVer, err2 := strconv.Atoi(versions[0])
		if err2 != nil {
			return false, fmt.Errorf("cannot parse MySQL DB version: %s", err2)
		}
		minorVer, err2 := strconv.Atoi(versions[1])
		if err2 != nil {
			return false, fmt.Errorf("cannot parse MySQL DB version: %s", err2)
		}
		patchVer, err2 := strconv.Atoi(versions[2])
		if err2 != nil {
			return false, fmt.Errorf("cannot parse MySQL DB version: %s", err2)
		}
		intVer := majorVer*1000 + minorVer*100 + patchVer
		if intVer < minimumRequiredMySQLVersion {
			return false, fmt.Errorf("minimum MySQL version requirements not met. Found: %s, Wanted: %s", versionString(intVer, *ss.settings.DriverName), versionString(minimumRequiredMySQLVersion, *ss.settings.DriverName))
		}
	}
	return true, nil
}

// versionString converts an integer representation of a DB version
// to a pretty-printed string.
// Postgres doesn't follow three-part version numbers from 10.0 onwards:
// https://www.postgresql.org/docs/13/libpq-status.html#LIBPQ-PQSERVERVERSION.
// For MySQL, we consider a major*1000 + minor*100 + patch format.
func versionString(v int, driver string) string {
	switch driver {
	case model.DatabaseDriverPostgres:
		minor := v % 10000
		major := v / 10000
		return strconv.Itoa(major) + "." + strconv.Itoa(minor)
	case model.DatabaseDriverMysql:
		minor := v % 1000
		major := v / 1000
		patch := minor % 100
		minor = minor / 100
		return strconv.Itoa(major) + "." + strconv.Itoa(minor) + "." + strconv.Itoa(patch)
	}
	return ""
}

func (ss *SqlStore) jsonDataType() string {
	if ss.DriverName() == model.DatabaseDriverPostgres {
		return "jsonb"
	}
	return "json"
}

func (ss *SqlStore) toReserveCase(str string) string {
	if ss.DriverName() == model.DatabaseDriverPostgres {
		return fmt.Sprintf("%q", str)
	}

	return fmt.Sprintf("`%s`", strings.Title(str))
}
