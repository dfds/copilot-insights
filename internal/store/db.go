package store

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
)

type Options struct {
	Host         string
	Port         int
	Name         string
	User         string
	Password     string
	SSLMode      string
	MaxOpenConns int
	// Params are extra runtime parameters, e.g. search_path for tests.
	Params map[string]string
}

// DSN renders a postgres:// URL. url.UserPassword escapes credentials, so
// passwords containing '@', ':' or spaces survive.
func (o Options) DSN() string {
	q := url.Values{}
	q.Set("sslmode", o.SSLMode)
	for k, v := range o.Params {
		q.Set(k, v)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(o.User, o.Password),
		Host:     net.JoinHostPort(o.Host, strconv.Itoa(o.Port)),
		Path:     "/" + o.Name,
		RawQuery: q.Encode(),
	}
	return u.String()
}

func Open(ctx context.Context, o Options) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(o.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}
	// pgx scans timestamptz into time.Local by default, so API timestamps
	// would carry the host's offset.
	db := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(_ context.Context, conn *pgx.Conn) error {
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name:  "timestamptz",
			OID:   pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
		})
		return nil
	}))
	if o.MaxOpenConns > 0 {
		db.SetMaxOpenConns(o.MaxOpenConns)
		db.SetMaxIdleConns(o.MaxOpenConns)
	}
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database %s:%d/%s: %w", o.Host, o.Port, o.Name, err)
	}
	return db, nil
}
