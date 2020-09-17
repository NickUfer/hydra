package driver

import (
	"context"
	"strings"
	"time"

	"github.com/ory/hydra/driver/configuration"

	"github.com/ory/x/resilience"

	"github.com/gobuffalo/pop/v5"
	"github.com/pkg/errors"

	_ "github.com/jackc/pgx/v4/stdlib"

	"github.com/ory/hydra/persistence/sql"

	"github.com/jmoiron/sqlx"

	"github.com/ory/x/dbal"
	"github.com/ory/x/sqlcon"

	"github.com/ory/hydra/client"
	"github.com/ory/hydra/consent"
	"github.com/ory/hydra/jwk"
	"github.com/ory/hydra/x"
)

type RegistrySQL struct {
	*RegistryBase
	db          *sqlx.DB
	dbalOptions []sqlcon.OptionModifier
}

var _ Registry = new(RegistrySQL)

func init() {
	dbal.RegisterDriver(func() dbal.Driver {
		return NewRegistrySQL()
	})
}

func NewRegistrySQL() *RegistrySQL {
	r := &RegistrySQL{
		RegistryBase: new(RegistryBase),
	}
	r.RegistryBase.with(r)
	return r
}

func (m *RegistrySQL) WithDB(db *sqlx.DB) Registry {
	m.db = db
	return m
}

func (m *RegistrySQL) Init() error {
	//if m.db == nil {
	//	// old db connection
	//	options := append([]sqlcon.OptionModifier{}, m.dbalOptions...)
	//	if m.Tracer().IsLoaded() {
	//		options = append(options, sqlcon.WithDistributedTracing(), sqlcon.WithOmitArgsFromTraceSpans())
	//	}
	//
	//	connection, err := sqlcon.NewSQLConnection(m.C.DSN(), m.Logger(), options...)
	//	if err != nil {
	//		return err
	//	}
	//
	//	m.db, err = connection.GetDatabaseRetry(time.Second*5, time.Minute*5)
	//	if err != nil {
	//		return err
	//	}
	//}

	if m.persister == nil {
		// new db connection
		pool, idlePool, connMaxLifetime, cleanedDSN := sqlcon.ParseConnectionOptions(m.l, m.C.DSN())
		c, err := pop.NewConnection(&pop.ConnectionDetails{
			URL:             sqlcon.FinalizeDSN(m.l, cleanedDSN),
			IdlePool:        idlePool,
			ConnMaxLifetime: connMaxLifetime,
			Pool:            pool,
		})
		if err != nil {
			return errors.WithStack(err)
		}
		if err := resilience.Retry(m.l, 5*time.Second, 5*time.Minute, c.Open); err != nil {
			return errors.WithStack(err)
		}
		m.persister, err = sql.NewPersister(c, m, m.C, m.l)
		if err != nil {
			return err
		}

		// if dsn is memory we have to run the migrations on every start
		if m.C.DSN() == configuration.DefaultSQLiteMemoryDSN {
			m.Logger().Print("Kratos is running migrations on every startup as DSN is memory.\n")
			m.Logger().Print("This means your data is lost when Kratos terminates.\n")
			if err := m.persister.MigrateUp(context.Background()); err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *RegistrySQL) DB() *sqlx.DB {
	if m.db == nil {
		if err := m.Init(); err != nil {
			m.Logger().WithError(err).Fatalf("Unable to initialize database.")
		}
	}

	return m.db
}

func (m *RegistrySQL) alwaysCanHandle(dsn string) bool {
	scheme := strings.Split(dsn, "://")[0]
	s := dbal.Canonicalize(scheme)
	return s == dbal.DriverMySQL || s == dbal.DriverPostgreSQL || s == dbal.DriverCockroachDB
}

func (m *RegistrySQL) Ping() error {
	return m.Persister().Connection(context.Background()).Open()
}

func (m *RegistrySQL) ClientManager() client.Manager {
	return m.Persister()
}

func (m *RegistrySQL) ConsentManager() consent.Manager {
	return m.Persister()
}

func (m *RegistrySQL) OAuth2Storage() x.FositeStorer {
	return m.Persister()
}

func (m *RegistrySQL) KeyManager() jwk.Manager {
	return m.Persister()
}
