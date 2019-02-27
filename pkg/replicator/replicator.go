package replicator

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx"
	"github.com/kshvakov/clickhouse"

	"github.com/ikitiki/pg2ch/pkg/config"
	"github.com/ikitiki/pg2ch/pkg/consumer"
	"github.com/ikitiki/pg2ch/pkg/message"
	"github.com/ikitiki/pg2ch/pkg/tableengines"
	"github.com/ikitiki/pg2ch/pkg/utils"
)

type CHTable interface {
	Insert(lsn utils.LSN, new message.Row) error
	Update(lsn utils.LSN, old message.Row, new message.Row) error
	Delete(lsn utils.LSN, old message.Row) error
	Sync(*pgx.Tx) error
	Close() error
	Begin() error
	Commit() error
}

type Replicator struct {
	ctx      context.Context
	cancel   context.CancelFunc
	consumer consumer.Interface
	cfg      config.Config
	errCh    chan error

	pgConn *pgx.Conn
	pgTx   *pgx.Tx
	chConn *sql.DB

	tables       map[string]CHTable
	oidName      map[utils.OID]string
	tempSlotName string

	finalLSN utils.LSN
	startLSN utils.LSN

	txTables map[string]struct{} // touched in the tx tables
}

func New(cfg config.Config) *Replicator {
	r := Replicator{
		cfg:      cfg,
		tables:   make(map[string]CHTable),
		oidName:  make(map[utils.OID]string),
		errCh:    make(chan error),
		txTables: make(map[string]struct{}),
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())

	return &r
}

func (r *Replicator) newTable(tableName string) (CHTable, error) {
	tbl := r.cfg.Tables[tableName]
	switch tbl.Engine {
	//case config.VersionedCollapsingMergeTree:
	//	if tbl.SignColumn == "" {
	//		return nil, fmt.Errorf("VersionedCollapsingMergeTree requires sign column to be set")
	//	}
	//	if tbl.VerColumn == "" {
	//		return nil, fmt.Errorf("VersionedCollapsingMergeTree requires version column to be set")
	//	}
	//
	//	return tableengines.NewVersionedCollapsingMergeTree(r.chConn, tableName, tbl), nil
	case config.ReplacingMergeTree:
		if tbl.VerColumn == "" {
			return nil, fmt.Errorf("ReplacingMergeTree requires version column to be set")
		}

		return tableengines.NewReplacingMergeTree(r.chConn, tableName, tbl), nil
	case config.MergeTree:
		return tableengines.NewMergeTree(r.chConn, tableName, tbl), nil
	case config.CollapsingMergeTree:
		if tbl.SignColumn == "" {
			return nil, fmt.Errorf("CollapsingMergeTree requires sign column to be set")
		}

		return tableengines.NewCollapsingMergeTree(r.chConn, tableName, tbl), nil
	}

	return nil, fmt.Errorf("%s table engine is not implemented", tbl.Engine)
}

func (r *Replicator) Run() error {
	if err := r.chConnect(); err != nil {
		return fmt.Errorf("could not connect to clickhouse: %v", err)
	}
	defer r.chDisconnect()

	if err := r.pgConnect(); err != nil {
		return fmt.Errorf("could not connecto postgresql: %v", err)
	}

	if err := r.pgCreateRepSlot(); err != nil {
		return fmt.Errorf("could not create replication slot: %v", err)
	}
	r.consumer = consumer.New(r.ctx, r.errCh, r.cfg.Pg.ConnConfig, r.cfg.Pg.ReplicationSlotName, r.cfg.Pg.PublicationName, r.startLSN)

	if err := r.fetchTableOIDs(); err != nil {
		return fmt.Errorf("table check failed: %v", err)
	}

	for tableName := range r.cfg.Tables {
		tbl, err := r.newTable(tableName)
		if err != nil {
			return fmt.Errorf("could not instantiate table: %v", err)
		}

		r.tables[tableName] = tbl
		if err := tbl.Sync(r.pgTx); err != nil {
			return fmt.Errorf("could not sync %q: %v", tableName, err)
		}
	}

	if err := r.pgDropRepSlot(); err != nil {
		return fmt.Errorf("could not drop replication slot: %v", err)
	}

	if err := r.pgConn.Close(); err != nil { // logical replication consumer uses it's own connection
		return fmt.Errorf("could not close pg connection: %v", err)
	}

	if err := r.consumer.Run(r); err != nil {
		return err
	}

	go r.logErrCh()

	r.waitForShutdown()
	r.cancel()
	r.consumer.Wait()

	for tblName, tbl := range r.tables {
		if err := tbl.Close(); err != nil {
			log.Printf("could not close %s: %v", tblName, err)
		}
	}

	return nil
}

func (r *Replicator) logErrCh() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case err := <-r.errCh:
			log.Println(err)
		}
	}
}

func (r *Replicator) fetchTableOIDs() error {
	rows, err := r.pgTx.Query(`
			select c.oid,
				   c.relname,
				   c.relreplident
			from pg_class c
				   join pg_namespace n on n.oid = c.relnamespace
       			   join pg_publication_tables pub on (c.relname = pub.tablename and n.nspname = pub.schemaname)
			where 
				c.relkind = 'r'
				and pub.pubname = $1`, r.cfg.Pg.PublicationName)

	if err != nil {
		return fmt.Errorf("could not exec: %v", err)
	}

	for rows.Next() {
		var (
			oid             utils.OID
			name            string
			replicaIdentity message.ReplicaIdentity
		)

		err = rows.Scan(&oid, &name, &replicaIdentity)
		if err != nil {
			return fmt.Errorf("could not scan: %v", err)
		}

		if _, ok := r.cfg.Tables[name]; ok && replicaIdentity != message.ReplicaIdentityFull {
			return fmt.Errorf("table %s must have FULL replica identity(currently it is %q)", name, replicaIdentity)
		}

		r.oidName[oid] = name
	}

	return nil
}

func (r *Replicator) chConnect() error {
	var err error

	r.chConn, err = sql.Open("clickhouse", r.cfg.CHConnectionString)
	if err != nil {
		log.Fatal(err)
	}
	if err := r.chConn.Ping(); err != nil {
		if exception, ok := err.(*clickhouse.Exception); ok {
			return fmt.Errorf("[%d] %s %s", exception.Code, exception.Message, exception.StackTrace)
		}

		return fmt.Errorf("could not ping: %v", err)
	}

	return nil
}

func (r *Replicator) chDisconnect() {
	if err := r.chConn.Close(); err != nil {
		log.Printf("could not close connection to clickhouse: %v", err)
	}
}

func (r *Replicator) pgConnect() error {
	var err error

	r.pgConn, err = pgx.Connect(r.cfg.Pg.Merge(pgx.ConnConfig{
		RuntimeParams:        map[string]string{"replication": "database"},
		PreferSimpleProtocol: true}))
	if err != nil {
		return fmt.Errorf("could not rep connect to pg: %v", err)
	}

	connInfo, err := initPostgresql(r.pgConn)
	if err != nil {
		return fmt.Errorf("could not fetch conn info: %v", err)
	}
	r.pgConn.ConnInfo = connInfo

	return nil
}

func (r *Replicator) pgDropRepSlot() error {
	_, err := r.pgTx.Exec(fmt.Sprintf("DROP_REPLICATION_SLOT %s", r.tempSlotName))

	return err
}

func (r *Replicator) pgCreateRepSlot() error {
	var basebackupLSN, snapshotName, plugin sql.NullString

	tx, err := r.pgConn.BeginEx(r.ctx, &pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("could not start pg transaction: %v", err)
	}

	row := tx.QueryRow(fmt.Sprintf("CREATE_REPLICATION_SLOT %s TEMPORARY LOGICAL %s USE_SNAPSHOT",
		fmt.Sprintf("tempslot_%d", r.pgConn.PID()), utils.OutputPlugin))

	if err := row.Scan(&r.tempSlotName, &basebackupLSN, &snapshotName, &plugin); err != nil {
		return fmt.Errorf("could not scan: %v", err)
	}

	if err := r.startLSN.Parse(basebackupLSN.String); err != nil {
		return fmt.Errorf("could not parse LSN: %v", err)
	}

	r.pgTx = tx

	return nil
}

func (r *Replicator) waitForShutdown() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)

loop:
	for {
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGABRT:
				fallthrough
			case syscall.SIGINT:
				fallthrough
			case syscall.SIGQUIT:
				fallthrough
			case syscall.SIGTERM:
				break loop
			default:
				log.Printf("unhandled signal: %v", sig)
			}
		}
	}
}

func (r *Replicator) HandleMessage(msg message.Message, lsn utils.LSN) error {
	if lsn < r.startLSN {
		return nil
	}

	switch v := msg.(type) {
	case message.Begin:
		r.txTables = make(map[string]struct{})
	case message.Commit:
		r.finalLSN = v.LSN
		r.consumer.AdvanceLSN(lsn)
		for tblName := range r.txTables {
			if err := r.tables[tblName].Commit(); err != nil {
				return fmt.Errorf("could not commit %q table: %v", tblName, err)
			}
		}
	case message.Insert:
		tblName, ok := r.oidName[v.RelationOID]
		if !ok {
			break
		}
		tbl, ok := r.tables[tblName]
		if !ok {
			break
		}
		if _, ok := r.txTables[tblName]; !ok {
			r.txTables[tblName] = struct{}{}
			if err := tbl.Begin(); err != nil {
				return fmt.Errorf("could not begin tx for table %q: %v", tblName, err)
			}
		}

		return tbl.Insert(lsn, v.NewRow)
	case message.Update:
		tblName, ok := r.oidName[v.RelationOID]
		if !ok {
			break
		}
		tbl, ok := r.tables[tblName]
		if !ok {
			break
		}
		if _, ok := r.txTables[tblName]; !ok {
			r.txTables[tblName] = struct{}{}
			if err := tbl.Begin(); err != nil {
				return fmt.Errorf("could not begin tx for table %q: %v", tblName, err)
			}
		}

		return tbl.Update(lsn, v.OldRow, v.NewRow)
	case message.Delete:
		tblName, ok := r.oidName[v.RelationOID]
		if !ok {
			break
		}
		tbl, ok := r.tables[tblName]
		if !ok {
			break
		}
		if _, ok := r.txTables[tblName]; !ok {
			r.txTables[tblName] = struct{}{}
			if err := tbl.Begin(); err != nil {
				return fmt.Errorf("could not begin tx for table %q: %v", tblName, err)
			}
		}

		return tbl.Delete(lsn, v.OldRow)
	}

	return nil
}
