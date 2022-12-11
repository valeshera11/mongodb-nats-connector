package mongo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/exp/slog"
)

type CollectionCreator struct {
	wrapped *Client
	logger  *slog.Logger
}

func NewCollectionCreator(client *Client, logger *slog.Logger) *CollectionCreator {
	return &CollectionCreator{
		wrapped: client,
		logger:  logger,
	}
}

func (c *CollectionCreator) CreateCollection(ctx context.Context, opts *CreateCollectionOptions) error {
	db := c.wrapped.client.Database(opts.DbName)
	collNames, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: opts.CollName}})
	if err != nil {
		return fmt.Errorf("could not list mongo collection names: %v", err)
	}

	// creates the collection if it does not exist
	if len(collNames) == 0 {
		mongoOpt := options.CreateCollection()
		if opts.Capped {
			mongoOpt.SetCapped(true).SetSizeInBytes(opts.SizeInBytes)
		}
		if err := db.CreateCollection(ctx, opts.CollName, mongoOpt); err != nil {
			return fmt.Errorf("could not create mongo collection %v: %v", opts.CollName, err)
		}
		c.logger.Debug("created mongodb collection", "collName", opts.CollName, "dbName", opts.DbName)
	}

	// enables change stream pre and post images
	if opts.ChangeStreamPreAndPostImages {
		err = db.RunCommand(ctx, bson.D{{Key: "collMod", Value: opts.CollName},
			{Key: "changeStreamPreAndPostImages", Value: bson.D{{Key: "enabled", Value: true}}}}).Err()
		if err != nil {
			return fmt.Errorf("could not enable changeStreamPreAndPostImages on mongo collection %v: %v",
				opts.CollName, err)
		}
	}
	return nil
}

type CreateCollectionOptions struct {
	DbName                       string
	CollName                     string
	Capped                       bool
	SizeInBytes                  int64
	ChangeStreamPreAndPostImages bool
}

type CollectionWatcher struct {
	wrapped *Client
	logger  *slog.Logger

	changeStreamHandler ChangeStreamHandler
}

func NewCollectionWatcher(client *Client, logger *slog.Logger, opts ...CollectionWatcherOption) *CollectionWatcher {
	w := &CollectionWatcher{
		wrapped: client,
		logger:  logger,
	}

	for _, opt := range opts {
		opt(w)
	}

	return w
}

func (w *CollectionWatcher) WatchCollection(ctx context.Context, opts *WatchCollectionOptions) error {
	resumeTokensDb := w.wrapped.client.Database(opts.ResumeTokensDbName)
	resumeTokensColl := resumeTokensDb.Collection(opts.ResumeTokensCollName)

	findOneOpts := options.FindOne().SetSort(bson.D{{Key: "$natural", Value: -1}})
	resumeToken := resumeTokensColl.FindOne(ctx, bson.D{}, findOneOpts)
	previousChangeEvent := &changeEvent{}
	if err := resumeToken.Decode(previousChangeEvent); err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return fmt.Errorf("could not fetch or decode resume token: %v", err)
	}

	changeStreamOpts := options.ChangeStream().
		SetFullDocument(options.UpdateLookup).
		SetFullDocumentBeforeChange(options.WhenAvailable)

	if previousChangeEvent.Id.Data != "" {
		w.logger.Debug("resuming after token", "token", previousChangeEvent.Id.Data)
		changeStreamOpts.SetResumeAfter(bson.D{{Key: "_data", Value: previousChangeEvent.Id.Data}})
	}

	watchedDb := w.wrapped.client.Database(opts.WatchedDbName)
	watchedColl := watchedDb.Collection(opts.WatchedCollName)

	cs, err := watchedColl.Watch(ctx, mongo.Pipeline{}, changeStreamOpts)
	if err != nil {
		return fmt.Errorf("could not watch mongo collection %v: %v", watchedColl.Name(), err)
	}
	w.logger.Info("watching mongodb collection", "collName", watchedColl.Name())

	for cs.Next(ctx) {
		event := &changeEvent{}
		if err = cs.Decode(event); err != nil {
			return fmt.Errorf("could not decode mongo change stream: %v", err)
		}

		json, err := bson.MarshalExtJSON(cs.Current, false, false)
		if err != nil {
			return fmt.Errorf("could not marshal mongo change stream from bson: %v", err)
		}
		w.logger.Debug("received change stream", "changeStream", string(json))

		subj := fmt.Sprintf("%s.%s", strings.ToUpper(watchedColl.Name()), event.OperationType)
		if err = w.changeStreamHandler(subj, event.Id.Data, json); err != nil {
			// nats error: current change stream must be retried.
			// does not save current resume token, stops the connector.
			// connector will resume from the previous token upon restart.
			return fmt.Errorf("could not publish to nats stream: %v", err)
		}

		if _, err := resumeTokensColl.InsertOne(ctx, event); err != nil {
			// change event has been published but token insertion failed.
			// connector will resume from the previous token upon restart publishing a duplicate change event.
			// the duplicate change event will be discarded by consumers because of the nats msg id.
			return fmt.Errorf("could not insert resume token: %v", err)
		}
	}

	w.logger.Info("stopped watching mongodb collection", "collName", watchedColl.Name())
	return cs.Close(context.Background())
}

type CollectionWatcherOption func(*CollectionWatcher)

type ChangeStreamHandler func(subj, msgId string, data []byte) error

func WithChangeStreamHandler(csHandler ChangeStreamHandler) CollectionWatcherOption {
	return func(w *CollectionWatcher) {
		w.changeStreamHandler = csHandler
	}
}

type WatchCollectionOptions struct {
	WatchedDbName        string
	WatchedCollName      string
	ResumeTokensDbName   string
	ResumeTokensCollName string
}

type changeEvent struct {
	Id            changeEventId `bson:"_id"`
	OperationType string        `bson:"operationType"`
}

type changeEventId struct {
	Data string `bson:"_data"`
}
