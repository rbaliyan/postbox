// Package mbxstore provides factory functions for creating durable mailbox
// message store backends (postgres, mongo) from a DSN string.
package mbxstore

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
	"github.com/rbaliyan/mailbox"
	mailboxmemory "github.com/rbaliyan/mailbox/store/memory"
	mailboxmongo "github.com/rbaliyan/mailbox/store/mongo"
	mailboxpg "github.com/rbaliyan/mailbox/store/postgres"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Config selects a mailbox message store backend.
type Config struct {
	// Backend is "memory" (default), "postgres", or "mongo".
	Backend string
	// DSN is the connection string. Required for postgres and mongo.
	DSN string
	// Database is the database/schema name. Used for mongo (default "mailbox").
	Database string
}

// NewMailboxFactory returns a server.MailboxFactory that creates a mailbox.Service
// backed by the store described in cfg. The factory opens and verifies the
// connection, then hands the connected store to the mailbox service constructor.
func NewMailboxFactory(cfg Config, plugins []mailbox.Plugin) func(ctx context.Context, resolver mailbox.UserResolver) (mailbox.Service, error) {
	return func(ctx context.Context, resolver mailbox.UserResolver) (mailbox.Service, error) {
		opts, err := buildOptions(ctx, cfg, plugins, resolver)
		if err != nil {
			return nil, err
		}
		svc, err := mailbox.New(mailbox.Config{}, opts...)
		if err != nil {
			return nil, fmt.Errorf("mbxstore: mailbox.New: %w", err)
		}
		if err := svc.Connect(ctx); err != nil {
			return nil, fmt.Errorf("mbxstore: connect: %w", err)
		}
		return svc, nil
	}
}

func buildOptions(ctx context.Context, cfg Config, plugins []mailbox.Plugin, resolver mailbox.UserResolver) ([]mailbox.Option, error) {
	var opts []mailbox.Option

	switch cfg.Backend {
	case "", "memory":
		opts = append(opts, mailbox.WithStore(mailboxmemory.New()))

	case "postgres":
		if cfg.DSN == "" {
			return nil, fmt.Errorf("mbxstore: postgres backend requires --mailbox-dsn")
		}
		db, err := sql.Open("postgres", cfg.DSN)
		if err != nil {
			return nil, fmt.Errorf("mbxstore: open postgres: %w", err)
		}
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("mbxstore: ping postgres: %w", err)
		}
		// db is owned by the store; store.Close() will close it via the mailbox
		// service Close chain.
		opts = append(opts, mailbox.WithStore(mailboxpg.NewFromDB(db)))

	case "mongo":
		if cfg.DSN == "" {
			return nil, fmt.Errorf("mbxstore: mongo backend requires --mailbox-dsn")
		}
		client, err := mongo.Connect(options.Client().ApplyURI(cfg.DSN))
		if err != nil {
			return nil, fmt.Errorf("mbxstore: connect mongo: %w", err)
		}
		if err := client.Ping(ctx, nil); err != nil {
			_ = client.Disconnect(ctx)
			return nil, fmt.Errorf("mbxstore: ping mongo: %w", err)
		}
		storeOpts := []mailboxmongo.Option{}
		if cfg.Database != "" {
			storeOpts = append(storeOpts, mailboxmongo.WithDatabase(cfg.Database))
		}
		// client is owned by the store; store.Close() disconnects it.
		opts = append(opts, mailbox.WithStore(mailboxmongo.New(client, storeOpts...)))

	default:
		return nil, fmt.Errorf("mbxstore: unknown backend %q; want memory|postgres|mongo", cfg.Backend)
	}

	if resolver != nil {
		opts = append(opts, mailbox.WithUserResolver(resolver))
	}
	if len(plugins) > 0 {
		opts = append(opts, mailbox.WithPlugins(plugins...))
	}
	return opts, nil
}
