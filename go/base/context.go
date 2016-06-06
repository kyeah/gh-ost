/*
   Copyright 2016 GitHub Inc.
	 See https://github.com/github/gh-ost/blob/master/LICENSE
*/

package base

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/github/gh-ost/go/mysql"
	"github.com/github/gh-ost/go/sql"

	"gopkg.in/gcfg.v1"
)

// RowsEstimateMethod is the type of row number estimation
type RowsEstimateMethod string

const (
	TableStatusRowsEstimate RowsEstimateMethod = "TableStatusRowsEstimate"
	ExplainRowsEstimate                        = "ExplainRowsEstimate"
	CountRowsEstimate                          = "CountRowsEstimate"
)

type CutOver int

const (
	CutOverTwoStep CutOver = 1
	CutOverVoluntaryLock
	CutOverUdfWait
)

const (
	maxRetries = 10
)

// MigrationContext has the general, global state of migration. It is used by
// all components throughout the migration process.
type MigrationContext struct {
	DatabaseName      string
	OriginalTableName string
	AlterStatement    string

	CountTableRows           bool
	AllowedRunningOnMaster   bool
	SwitchToRowBinlogFormat  bool
	NullableUniqueKeyAllowed bool

	config      ContextConfig
	configMutex *sync.Mutex
	ConfigFile  string
	CliUser     string
	CliPassword string

	ChunkSize                           int64
	MaxLagMillisecondsThrottleThreshold int64
	ReplictionLagQuery                  string
	ThrottleControlReplicaKeys          *mysql.InstanceKeyMap
	ThrottleFlagFile                    string
	ThrottleAdditionalFlagFile          string
	MaxLoad                             map[string]int64
	PostponeSwapTablesFlagFile          string
	SwapTablesTimeoutSeconds            int64

	Noop                    bool
	TestOnReplica           bool
	OkToDropTable           bool
	InitiallyDropOldTable   bool
	InitiallyDropGhostTable bool
	CutOverType             CutOver

	TableEngine               string
	RowsEstimate              int64
	UsedRowsEstimateMethod    RowsEstimateMethod
	OriginalBinlogFormat      string
	OriginalBinlogRowImage    string
	InspectorConnectionConfig *mysql.ConnectionConfig
	ApplierConnectionConfig   *mysql.ConnectionConfig
	StartTime                 time.Time
	RowCopyStartTime          time.Time
	LockTablesStartTime       time.Time
	RenameTablesStartTime     time.Time
	RenameTablesEndTime       time.Time
	pointOfInterestTime       time.Time
	pointOfInterestTimeMutex  *sync.Mutex
	CurrentLag                int64
	TotalRowsCopied           int64
	TotalDMLEventsApplied     int64
	isThrottled               bool
	throttleReason            string
	throttleMutex             *sync.Mutex

	OriginalTableColumns             *sql.ColumnList
	OriginalTableUniqueKeys          [](*sql.UniqueKey)
	GhostTableColumns                *sql.ColumnList
	GhostTableUniqueKeys             [](*sql.UniqueKey)
	UniqueKey                        *sql.UniqueKey
	SharedColumns                    *sql.ColumnList
	MigrationRangeMinValues          *sql.ColumnValues
	MigrationRangeMaxValues          *sql.ColumnValues
	Iteration                        int64
	MigrationIterationRangeMinValues *sql.ColumnValues
	MigrationIterationRangeMaxValues *sql.ColumnValues

	CanStopStreaming func() bool
}

type ContextConfig struct {
	Client struct {
		User     string
		Password string
	}
	Osc struct {
		Chunk_Size            int64
		Max_Lag_Millis        int64
		Replication_Lag_Query string
		Max_Load              string
	}
}

var context *MigrationContext

func init() {
	context = newMigrationContext()
}

func newMigrationContext() *MigrationContext {
	return &MigrationContext{
		ChunkSize:                           1000,
		InspectorConnectionConfig:           mysql.NewConnectionConfig(),
		ApplierConnectionConfig:             mysql.NewConnectionConfig(),
		MaxLagMillisecondsThrottleThreshold: 1000,
		SwapTablesTimeoutSeconds:            3,
		MaxLoad:                             make(map[string]int64),
		throttleMutex:                       &sync.Mutex{},
		ThrottleControlReplicaKeys:          mysql.NewInstanceKeyMap(),
		configMutex:                         &sync.Mutex{},
		pointOfInterestTimeMutex:            &sync.Mutex{},
	}
}

// GetMigrationContext
func GetMigrationContext() *MigrationContext {
	return context
}

// GetGhostTableName generates the name of ghost table, based on original table name
func (this *MigrationContext) GetGhostTableName() string {
	return fmt.Sprintf("_%s_gst", this.OriginalTableName)
}

// GetOldTableName generates the name of the "old" table, into which the original table is renamed.
func (this *MigrationContext) GetOldTableName() string {
	return fmt.Sprintf("_%s_old", this.OriginalTableName)
}

// GetChangelogTableName generates the name of changelog table, based on original table name
func (this *MigrationContext) GetChangelogTableName() string {
	return fmt.Sprintf("_%s_osc", this.OriginalTableName)
}

// GetVoluntaryLockName returns a name of a voluntary lock to be used throughout
// the swap-tables process.
func (this *MigrationContext) GetVoluntaryLockName() string {
	return fmt.Sprintf("%s.%s.lock", this.DatabaseName, this.OriginalTableName)
}

// RequiresBinlogFormatChange is `true` when the original binlog format isn't `ROW`
func (this *MigrationContext) RequiresBinlogFormatChange() bool {
	return this.OriginalBinlogFormat != "ROW"
}

// InspectorIsAlsoApplier is `true` when the both inspector and applier are the
// same database instance. This would be true when running directly on master or when
// testing on replica.
func (this *MigrationContext) InspectorIsAlsoApplier() bool {
	return this.InspectorConnectionConfig.Equals(this.ApplierConnectionConfig)
}

// HasMigrationRange tells us whether there's a range to iterate for copying rows.
// It will be `false` if the table is initially empty
func (this *MigrationContext) HasMigrationRange() bool {
	return this.MigrationRangeMinValues != nil && this.MigrationRangeMaxValues != nil
}

func (this *MigrationContext) MaxRetries() int {
	return maxRetries
}

func (this *MigrationContext) IsTransactionalTable() bool {
	switch strings.ToLower(this.TableEngine) {
	case "innodb":
		{
			return true
		}
	case "tokudb":
		{
			return true
		}
	}
	return false
}

// ElapsedTime returns time since very beginning of the process
func (this *MigrationContext) ElapsedTime() time.Duration {
	return time.Now().Sub(this.StartTime)
}

// ElapsedRowCopyTime returns time since starting to copy chunks of rows
func (this *MigrationContext) ElapsedRowCopyTime() time.Duration {
	return time.Now().Sub(this.RowCopyStartTime)
}

// GetTotalRowsCopied returns the accurate number of rows being copied (affected)
// This is not exactly the same as the rows being iterated via chunks, but potentially close enough
func (this *MigrationContext) GetTotalRowsCopied() int64 {
	return atomic.LoadInt64(&this.TotalRowsCopied)
}

func (this *MigrationContext) GetIteration() int64 {
	return atomic.LoadInt64(&this.Iteration)
}

func (this *MigrationContext) MarkPointOfInterest() int64 {
	this.pointOfInterestTimeMutex.Lock()
	defer this.pointOfInterestTimeMutex.Unlock()

	this.pointOfInterestTime = time.Now()
	return atomic.LoadInt64(&this.Iteration)
}

func (this *MigrationContext) TimeSincePointOfInterest() time.Duration {
	this.pointOfInterestTimeMutex.Lock()
	defer this.pointOfInterestTimeMutex.Unlock()

	return time.Now().Sub(this.pointOfInterestTime)
}

func (this *MigrationContext) SetThrottled(throttle bool, reason string) {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()
	this.isThrottled = throttle
	this.throttleReason = reason
}

func (this *MigrationContext) IsThrottled() (bool, string) {
	this.throttleMutex.Lock()
	defer this.throttleMutex.Unlock()
	return this.isThrottled, this.throttleReason
}

// ReadMaxLoad parses the `--max-load` flag, which is in multiple key-value format,
// such as: 'Threads_running=100,Threads_connected=500'
func (this *MigrationContext) ReadMaxLoad(maxLoadList string) error {
	if maxLoadList == "" {
		return nil
	}
	maxLoadConditions := strings.Split(maxLoadList, ",")
	for _, maxLoadCondition := range maxLoadConditions {
		maxLoadTokens := strings.Split(maxLoadCondition, "=")
		if len(maxLoadTokens) != 2 {
			return fmt.Errorf("Error parsing max-load condition: %s", maxLoadCondition)
		}
		if maxLoadTokens[0] == "" {
			return fmt.Errorf("Error parsing status variable in max-load condition: %s", maxLoadCondition)
		}
		if n, err := strconv.ParseInt(maxLoadTokens[1], 10, 0); err != nil {
			return fmt.Errorf("Error parsing numeric value in max-load condition: %s", maxLoadCondition)
		} else {
			this.MaxLoad[maxLoadTokens[0]] = n
		}
	}
	return nil
}

// ApplyCredentials sorts out the credentials between the config file and the CLI flags
func (this *MigrationContext) ApplyCredentials() {
	this.configMutex.Lock()
	defer this.configMutex.Unlock()

	if this.config.Client.User != "" {
		this.InspectorConnectionConfig.User = this.config.Client.User
	}
	if this.CliUser != "" {
		// Override
		this.InspectorConnectionConfig.User = this.CliUser
	}
	if this.config.Client.Password != "" {
		this.InspectorConnectionConfig.Password = this.config.Client.Password
	}
	if this.CliPassword != "" {
		// Override
		this.InspectorConnectionConfig.Password = this.CliPassword
	}
}

// ReadConfigFile attempts to read the config file, if it exists
func (this *MigrationContext) ReadConfigFile() error {
	this.configMutex.Lock()
	defer this.configMutex.Unlock()

	if this.ConfigFile == "" {
		return nil
	}
	if err := gcfg.ReadFileInto(&this.config, this.ConfigFile); err != nil {
		return err
	}
	return nil
}
