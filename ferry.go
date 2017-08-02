package ghostferry

import (
	"crypto/tls"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/sirupsen/logrus"
)

const (
	StateStarting          = "starting"
	StateCopying           = "copying"
	StateWaitingForCutover = "wait-for-cutover"
	StateCutover           = "cutover"
	StateDone              = "done"
)

type Ferry struct {
	*Config

	SourceDB *sql.DB
	TargetDB *sql.DB

	BinlogStreamer *BinlogStreamer
	DataIterator   *DataIterator
	ErrorHandler   *ErrorHandler
	Throttler      *Throttler
	ControlServer  *ControlServer
	Verifier       Verifier

	Tables TableSchemaCache

	StartTime    time.Time
	DoneTime     time.Time
	OverallState string

	logger *logrus.Entry

	coreServicesWg       *sync.WaitGroup
	supportingServicesWg *sync.WaitGroup
	controlServerWg      *sync.WaitGroup
	rowCopyCompleteCh    chan struct{}
}

// Initialize all the components of Ghostferry and connect to the Database
func (f *Ferry) Initialize() (err error) {
	f.StartTime = time.Now().Truncate(time.Second)
	f.OverallState = StateStarting

	f.coreServicesWg = &sync.WaitGroup{}
	f.supportingServicesWg = &sync.WaitGroup{}
	f.controlServerWg = &sync.WaitGroup{}
	f.logger = logrus.WithField("tag", "ferry")
	f.rowCopyCompleteCh = make(chan struct{})

	sourceConfig := &mysql.Config{
		User:   f.SourceUser,
		Passwd: f.SourcePass,
		Net:    "tcp",
		Addr:   fmt.Sprintf("%s:%d", f.SourceHost, f.SourcePort),
	}

	targetConfig := &mysql.Config{
		User:   f.TargetUser,
		Passwd: f.TargetPass,
		Net:    "tcp",
		Addr:   fmt.Sprintf("%s:%d", f.TargetHost, f.TargetPort),
	}

	// TLS Config
	var sourceTLSConfig, targetTLSConfig *tls.Config
	if f.SourceTLS != nil {
		sourceTLSConfig, err = f.SourceTLS.RealTLSConfig()
		if err != nil {
			f.logger.WithError(err).Error("failed to get source TLS config")
			return err
		}

		err = mysql.RegisterTLSConfig("source", sourceTLSConfig)
		if err != nil {
			f.logger.WithError(err).Error("failed to register source TLS config")
			return err
		}

		sourceConfig.TLSConfig = "source"
	}

	if f.TargetTLS != nil {
		targetTLSConfig, err = f.SourceTLS.RealTLSConfig()
		if err != nil {
			f.logger.WithError(err).Error("failed to get target TLS config")
			return err
		}

		err = mysql.RegisterTLSConfig("target", targetTLSConfig)
		if err != nil {
			f.logger.WithError(err).Error("failed to register target TLS config")
			return err
		}

		targetConfig.TLSConfig = "target"
	}

	// Connect to the database
	sourceDSN := sourceConfig.FormatDSN()
	f.logger.WithField("dsn", sourceDSN).Info("connecting to the source database")
	f.SourceDB, err = sql.Open("mysql", sourceDSN)
	if err != nil {
		f.logger.WithError(err).Error("failed to connect to source database")
		return err
	}

	err = checkConnection(f.logger, sourceDSN, f.SourceDB)
	if err != nil {
		f.logger.WithError(err).Error("source connection checking failed")
		return err
	}

	targetDSN := targetConfig.FormatDSN()
	f.logger.WithField("dsn", targetDSN).Info("connecting to the target database")
	f.TargetDB, err = sql.Open("mysql", targetDSN)
	if err != nil {
		f.logger.WithError(err).Error("failed to connect to target database")
		return err
	}

	err = checkConnection(f.logger, targetDSN, f.TargetDB)
	if err != nil {
		f.logger.WithError(err).Error("target connection checking failed")
		return err
	}

	// Initialize the ErrorHandler
	f.ErrorHandler = &ErrorHandler{
		Ferry: f,
	}
	f.ErrorHandler.Initialize()

	f.Throttler = &Throttler{
		Db:           f.SourceDB,
		Config:       f.Config,
		ErrorHandler: f.ErrorHandler,
	}
	f.Throttler.Initialize()

	// Initialize binlog streamer
	f.BinlogStreamer = &BinlogStreamer{
		Db:           f.SourceDB,
		Config:       f.Config,
		ErrorHandler: f.ErrorHandler,
		Throttler:    f.Throttler,
	}
	err = f.BinlogStreamer.Initialize()
	if err != nil {
		return err
	}

	// Initialize the DataIterator
	f.DataIterator = &DataIterator{
		Db:           f.SourceDB,
		Config:       f.Config,
		ErrorHandler: f.ErrorHandler,
		Throttler:    f.Throttler,
	}

	err = f.DataIterator.Initialize()
	if err != nil {
		return err
	}

	// Initialize the ControlServer
	f.ControlServer = &ControlServer{
		F:       f,
		Addr:    f.Config.ServerBindAddr,
		Basedir: f.Config.WebBasedir,
	}

	err = f.ControlServer.Initialize()
	if err != nil {
		return err
	}

	f.logger.Info("ferry initialized")

	return nil
}

// Determine the binlog coordinates, table mapping for the pending
// Ghostferry run.
func (f *Ferry) Start() error {
	// Event listeners for the BinlogStreamer and DataIterator are called
	// in the order they are registered.
	// The builtin event listeners are to write the events to the target
	// database.
	// Registering the builtin event listeners in Start allows the consumer
	// of the library to register event listeners that gets called before
	// and after the data gets written to the target database.
	f.BinlogStreamer.AddEventListener(f.writeEventsToTargetWithRetries)
	f.DataIterator.AddEventListener(f.writeEventsToTargetWithRetries)
	f.DataIterator.AddDoneListener(f.onFinishedIterations)

	// The starting binlog coordinates must be determined first. If it is
	// determined after the DataIterator starts, the DataIterator might
	// miss some records that are inserted between the time the
	// DataIterator determines the range of IDs to copy and the time that
	// the starting binlog coordinates are determined.
	err := f.BinlogStreamer.ConnectBinlogStreamerToMysql()
	if err != nil {
		return err
	}

	// Loads the schema of the tables that are applicable.
	// We need to do this at the beginning of the run as this is required
	// in order to determine the PrimaryKey of each table as well as finding
	// which value in the binlog event correspond to which field in the
	// table.
	f.Tables, err = loadTables(f.SourceDB, f.ApplicableDatabases, f.ApplicableTables)
	if err != nil {
		return err
	}

	f.DataIterator.Tables = f.Tables.AsSlice()

	return nil
}

// Spawns the background tasks that actually perform the run.
// Wait for the background tasks to finish.
func (f *Ferry) Run() {
	f.logger.Info("starting ferry run")
	f.OverallState = StateCopying

	f.controlServerWg.Add(1)
	go f.ControlServer.Run(f.controlServerWg)

	f.coreServicesWg.Add(2)
	go f.BinlogStreamer.Run(f.coreServicesWg)
	go f.DataIterator.Run(f.coreServicesWg)

	f.supportingServicesWg.Add(2)
	go f.ErrorHandler.Run(f.supportingServicesWg)
	go f.Throttler.Run(f.supportingServicesWg)

	f.coreServicesWg.Wait()

	f.OverallState = StateDone
	f.DoneTime = time.Now()

	// Need to wait to ensure that the ErrorHandler does not get
	// interrupted if it is received some errors, have not printed it
	// out, but all other threads (including the main thread) has quit.
	// Without some sort of waiting on the main thread for the
	// ErrorHandler to exit first, the program could exit without ever
	// printing out the error and panicking.
	//
	// Furthermore, in a normal run without errors we need to ensure this
	// shuts down and does not block forever.
	f.ErrorHandler.Stop()
	f.Throttler.Stop()
	f.supportingServicesWg.Wait()
}

// Call this method and perform the cutover after this method returns.
func (f *Ferry) WaitUntilRowCopyIsComplete() {
	<-f.rowCopyCompleteCh
}

// After you stop writing to the source and made sure that all inflight
// transactions to the source are completed, call this method to ensure
// that the binlog streaming has caught up and stop the binlog streaming.
//
// This method will actually not shutdown the BinlogStreamer immediately.
// You will know that the BinlogStreamer finished when .Run() returns.
func (f *Ferry) FlushBinlogAndStopStreaming() {
	f.BinlogStreamer.FlushAndStop()
}

// This method waits for the ControlServer to exit. By default,
// Ghostferry will not shutdown the control server nor will it wait for
// its finish.
func (f *Ferry) WaitForControlServer() {
	f.logger.Warn("waiting for control server...")
	f.controlServerWg.Wait()
}

// This method is used to shutdown the ControlServer. By default,
// Ghostferry will not call this for you.
func (f *Ferry) ShutdownControlServer() error {
	f.logger.Info("shutting down control server")
	err := f.ControlServer.Shutdown()
	if err != nil {
		f.logger.WithError(err).Warn("control server shutdown failure")
	}

	return err
}

func (f *Ferry) onFinishedIterations() error {
	f.logger.Info("finished iterations")
	f.OverallState = StateWaitingForCutover

	for !f.AutomaticCutover {
		time.Sleep(1 * time.Second)
		f.logger.Debug("waiting for AutomaticCutover to become true before signaling for row copy complete")
	}

	f.logger.Info("entering cutover phase")

	f.OverallState = StateCutover
	// TODO: make it so that this is non-blocking
	f.rowCopyCompleteCh <- struct{}{}
	return nil
}

func (f *Ferry) writeEventsToTargetWithRetries(events []DMLEvent) error {
	var err error
	for i := 0; i < f.MaxWriteRetriesOnTargetDBError; i++ {
		err = f.writeEventsToTarget(events)
		if err == nil {
			return nil
		} else {
			f.logger.WithError(err).Error("failed to write event to target")
		}
	}

	f.logger.Error("failed to write events to target even after retries")
	return err
}

func (f *Ferry) writeEventsToTarget(events []DMLEvent) error {
	tx, err := f.TargetDB.Begin()
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		tx.Rollback()
		return err
	}

	sessionQuery := `
		SET SESSION time_zone = '+00:00',
		sql_mode = CONCAT(@@session.sql_mode, ',STRICT_ALL_TABLES')
	`

	_, err = tx.Exec(sessionQuery)
	if err != nil {
		err = fmt.Errorf("during setting session: %v", err)
		return rollback(err)
	}

	for _, ev := range events {
		sql, args, err := ev.AsSQLQuery(f.Tables)
		if err != nil {
			err = fmt.Errorf("during generating sql query: %v", err)
			return rollback(err)
		}

		_, err = tx.Exec(sql, args...)
		if err != nil {
			err = fmt.Errorf("during exec query (%s %v): %v", sql, args, err)
			return rollback(err)
		}
	}

	return tx.Commit()
}

func checkConnection(logger *logrus.Entry, dsn string, db *sql.DB) error {
	row := db.QueryRow("SHOW STATUS LIKE 'Ssl_cipher'")
	var name, cipher string
	err := row.Scan(&name, &cipher)
	if err != nil {
		return err
	}

	hasSSL := cipher != ""

	logger.WithFields(logrus.Fields{
		"hasSSL":     hasSSL,
		"ssl_cipher": cipher,
		"dsn":        dsn,
	}).Infof("connected to %s", dsn)

	return nil
}
