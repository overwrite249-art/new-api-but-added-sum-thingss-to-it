package model

import "errors"

// RunMigrations creates or updates the schema for both the main and the log
// database, using the same code path a master node runs on boot.
//
// New API normally migrates as a side effect of startup: InitDB() calls
// migrateDB(), but only when common.IsMasterNode. The serverless target forces
// NODE_TYPE=slave, so it never migrates implicitly -- which is correct for a
// request path, but left migration with nowhere to happen at all, since a
// function bundle cannot run a one-off command.
//
// Callers are expected to invoke this from an authenticated admin/cron endpoint,
// once, before first use and after upgrades.
//
// Safe to call repeatedly: GORM's AutoMigrate only adds missing tables, columns
// and indexes, and the bespoke migrations it delegates to each re-check the
// current column type before touching anything.
//
// Deliberately a thin re-export rather than a reimplementation. migrateDB and
// migrateLOGDB are unexported and therefore unreachable from package
// serverless; duplicating their contents would create a second, silently
// drifting definition of the schema.
func RunMigrations() error {
	if DB == nil {
		return errors.New("model: RunMigrations called before InitDB")
	}

	if err := migrateDB(); err != nil {
		return err
	}

	// LOG_DB is an alias of DB unless LOG_SQL_DSN is set, in which case it is a
	// separate database that needs its own logs table. migrateLOGDB also covers
	// the ClickHouse variant, which is created with explicit DDL rather than
	// AutoMigrate.
	if LOG_DB == nil {
		return errors.New("model: RunMigrations called before InitLogDB")
	}

	return migrateLOGDB()
}
