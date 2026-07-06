// Package mongodb owns the MongoDB connection and the index catalog.
// The connection string comes exclusively from configuration (env) — never
// from code.
package mongodb

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// Connect dials MongoDB, verifies the connection with a ping, and returns the
// client plus the application database handle. Pool sizing targets peak
// pour-time write bursts (twice-daily shift spikes, blueprint §16).
func Connect(ctx context.Context, uri, dbName string) (*mongo.Client, *mongo.Database, error) {
	opts := options.Client().
		ApplyURI(uri).
		SetMaxPoolSize(200).
		SetMinPoolSize(8).
		SetMaxConnIdleTime(5 * time.Minute).
		SetServerSelectionTimeout(5 * time.Second).
		SetTimeout(15 * time.Second)

	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("mongo connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		return nil, nil, fmt.Errorf("mongo ping (%s): %w", uri, err)
	}

	return client, client.Database(dbName), nil
}
