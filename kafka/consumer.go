// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	apmqueue "github.com/elastic/apm-queue"
	"github.com/elastic/apm-queue/queuecontext"
)

var (
	// ErrCommitFailed may be returned by `consumer.Run` when DeliveryType is
	// apmqueue.AtMostOnceDelivery.
	ErrCommitFailed = errors.New("kafka: failed to commit offsets")
)

// ConsumerConfig defines the configuration for the Kafka consumer.
type ConsumerConfig struct {
	CommonConfig
	// Topics that the consumer will consume messages from
	Topics []apmqueue.Topic
	// GroupID to join as part of the consumer group.
	GroupID string
	// MaxPollRecords defines an upper bound to the number of records that can
	// be polled on a single fetch. If MaxPollRecords <= 0, defaults to 100.
	//
	// It is best to keep the number of polled records small or the consumer
	// risks being forced out of the group if it exceeds rebalance.timeout.ms.
	MaxPollRecords int
	// MaxPollWait defines the maximum amount of time a broker will wait for a
	// fetch response to hit the minimum number of required bytes before
	// returning, overriding the default 5s.
	MaxPollWait time.Duration
	// ShutdownGracePeriod defines the maximum amount of time to wait for the
	// partition consumers to process events before the underlying kgo.Client
	// is closed, overriding the default 5s.
	ShutdownGracePeriod time.Duration
	// Delivery mechanism to use to acknowledge the messages.
	// AtMostOnceDeliveryType and AtLeastOnceDeliveryType are supported.
	// If not set, it defaults to apmqueue.AtMostOnceDeliveryType.
	Delivery apmqueue.DeliveryType
	// Processor that will be used to process each event individually.
	// It is recommended to keep the synchronous processing fast and below the
	// rebalance.timeout.ms setting in Kafka.
	//
	// The processing time of each processing cycle can be calculated as:
	// record.process.time * MaxPollRecords.
	Processor apmqueue.Processor
}

// finalize ensures the configuration is valid, setting default values from
// environment variables as described in doc comments, returning an error if
// any configuration is invalid.
func (cfg *ConsumerConfig) finalize() error {
	var errs []error
	if err := cfg.CommonConfig.finalize(); err != nil {
		errs = append(errs, err)
	}
	if len(cfg.Topics) == 0 {
		errs = append(errs, errors.New("kafka: at least one topic must be set"))
	}
	if cfg.GroupID == "" {
		errs = append(errs, errors.New("kafka: consumer GroupID must be set"))
	}
	if cfg.Processor == nil {
		errs = append(errs, errors.New("kafka: processor must be set"))
	}
	return errors.Join(errs...)
}

var _ apmqueue.Consumer = &Consumer{}

// Consumer wraps a Kafka consumer and the consumption implementation details.
// Consumes each partition in a dedicated goroutine.
type Consumer struct {
	mu       sync.RWMutex
	client   *kgo.Client
	cfg      ConsumerConfig
	consumer *consumer
	running  chan struct{}
	closed   chan struct{}

	forceClose context.CancelCauseFunc
	stopPoll   context.CancelFunc

	tracer trace.Tracer
}

// NewConsumer creates a new instance of a Consumer. The consumer will read from
// each partition concurrently by using a dedicated goroutine per partition.
func NewConsumer(cfg ConsumerConfig) (*Consumer, error) {
	if err := cfg.finalize(); err != nil {
		return nil, fmt.Errorf("kafka: invalid consumer config: %w", err)
	}
	// `forceClose` is called by `Consumer.Close()` if / when the
	// `cfg.ShutdownGracePeriod` is exceeded.
	processingCtx, forceClose := context.WithCancelCause(context.Background())
	consumer := &consumer{
		consumers: make(map[topicPartition]partitionConsumer),
		processor: cfg.Processor,
		logger:    cfg.Logger.Named("partition"),
		delivery:  cfg.Delivery,
		ctx:       processingCtx,
	}
	topics := make([]string, 0, len(cfg.Topics))
	for _, t := range cfg.Topics {
		topics = append(topics, string(t))
	}
	opts := []kgo.Opt{
		// Injects the kgo.Client context as the record.Context.
		kgo.WithHooks(consumer),
		kgo.ConsumerGroup(cfg.GroupID),
		kgo.ConsumeTopics(topics...),
		// If a rebalance happens while the client is polling, the consumed
		// records may belong to a partition which has been reassigned to a
		// different consumer int he group. To avoid this scenario, Polls will
		// block rebalances of partitions which would be lost, and the consumer
		// MUST manually call `AllowRebalance`.
		kgo.BlockRebalanceOnPoll(),
		kgo.DisableAutoCommit(),
		// Assign concurrent consumer callbacks to ensure consuming starts
		// for newly assigned partitions, and consuming ceases from lost or
		// revoked partitions.
		kgo.OnPartitionsAssigned(consumer.assigned),
		kgo.OnPartitionsLost(consumer.lost),
		kgo.OnPartitionsRevoked(consumer.lost),
	}
	if cfg.MaxPollWait > 0 {
		opts = append(opts, kgo.FetchMaxWait(cfg.MaxPollWait))
	}
	if cfg.ShutdownGracePeriod <= 0 {
		cfg.ShutdownGracePeriod = 5 * time.Second
	}
	client, err := cfg.newClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka: failed creating kafka consumer: %w", err)
	}
	if cfg.MaxPollRecords <= 0 {
		cfg.MaxPollRecords = 100
	}
	return &Consumer{
		cfg:        cfg,
		client:     client,
		consumer:   consumer,
		closed:     make(chan struct{}),
		running:    make(chan struct{}),
		forceClose: forceClose,
		stopPoll:   func() {},
		tracer:     cfg.tracerProvider().Tracer("kafka"),
	}, nil
}

// Close the consumer, blocking until all partition consumers are stopped.
func (c *Consumer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.consumer.wg.Wait() // Wait for consumer goroutines to exit.
	select {
	case <-c.closed:
	default:
		close(c.closed)
		defer c.client.Close() // Last, close the `kgo.Client`
		// Cancel the context used in client.PollRecords, triggering graceful
		// cancellation.
		c.stopPoll()
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			// Close all partition consumers first to ensure there aren't any
			// records being processed while the kgo.Client is being closed.
			// Also ensures that commits can be issued after the records are
			// processed when AtLeastOnceDelivery is configured.
			c.consumer.close()
		}()
		// Wait for the consumers to process any in-flight records, or cancel
		// the underlying processing context if they aren't stopped in time.
		select {
		case <-time.After(c.cfg.ShutdownGracePeriod): // Timeout
			c.forceClose(fmt.Errorf(
				"consumer: close: timeout waiting for consumers to stop (%s)",
				c.cfg.ShutdownGracePeriod.String(),
			))
		case <-stopped: // Stopped within c.cfg.ShutdownGracePeriod.
		}
	}
	return nil
}

// Run the consumer until a non recoverable error is found:
//   - ErrCommitFailed.
//
// To shut down the consumer, call consumer.Close() or cancel the context.
// Calling `consumer.Close` is advisable to ensure graceful shutdown and
// avoid any records from being lost (AMOD), or processed twice (ALOD).
// To ensure that all polled records are processed. Close() must be called,
// even when the context is canceled.
//
// If called more than once, returns `apmqueue.ErrConsumerAlreadyRunning`.
func (c *Consumer) Run(ctx context.Context) error {
	c.mu.Lock()
	select {
	case <-c.running:
		c.mu.Unlock()
		return apmqueue.ErrConsumerAlreadyRunning
	default:
		close(c.running)
	}
	// Create a new context from the passed context, used exclusively for
	// kgo.Client.* calls. c.stopFetch is called by consumer.Close() to
	// cancel this context as part of the graceful shutdown sequence.
	var clientCtx context.Context
	clientCtx, c.stopPoll = context.WithCancel(ctx)
	c.mu.Unlock()
	for {
		if err := c.fetch(ctx, clientCtx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil // Return no error if err == context.Canceled.
			}
			return err
		}
	}
}

// fetch polls the Kafka broker for new records up to cfg.MaxPollRecords.
// Any errors returned by fetch should be considered fatal.
func (c *Consumer) fetch(ctx, clientCtx context.Context) error {
	fetches := c.client.PollRecords(clientCtx, c.cfg.MaxPollRecords)
	defer c.client.AllowRebalance()

	if fetches.IsClientClosed() ||
		errors.Is(fetches.Err0(), context.Canceled) ||
		errors.Is(fetches.Err0(), context.DeadlineExceeded) {
		return context.Canceled
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch c.cfg.Delivery {
	case apmqueue.AtLeastOnceDeliveryType:
		// Committing the processed records happens on each partition consumer.
	case apmqueue.AtMostOnceDeliveryType:
		// Commit the fetched record offsets as soon as we've polled them.
		if err := c.client.CommitUncommittedOffsets(clientCtx); err != nil {
			// NOTE(marclop): If the commit fails with an unrecoverable error,
			// return it and terminate the consumer. This will avoid potentially
			// processing records twice, and it's up to the consumer to re-start
			// the consumer.
			return ErrCommitFailed
		}
		// Allow re-balancing now that we have committed offsets, preventing
		// another consumer from reprocessing the records.
		c.client.AllowRebalance()
	}
	fetches.EachError(func(t string, p int32, err error) {
		c.cfg.Logger.Error("consumer fetches returned error",
			zap.Error(err), zap.String("topic", t), zap.Int32("partition", p),
		)
	})
	c.consumer.processFetch(ctx, fetches)
	return nil
}

// Healthy returns an error if the Kafka client fails to reach a discovered
// broker.
func (c *Consumer) Healthy(ctx context.Context) error {
	if err := c.client.Ping(ctx); err != nil {
		return fmt.Errorf("health probe: %w", err)
	}
	return nil
}

// consumer wraps partitionConsumers and exposes the necessary callbacks
// to use when partitions are reassigned.
type consumer struct {
	mu        sync.RWMutex
	wg        sync.WaitGroup
	consumers map[topicPartition]partitionConsumer
	processor apmqueue.Processor
	logger    *zap.Logger
	delivery  apmqueue.DeliveryType
	// ctx contains the graceful cancellation context that is passed to the
	// partition consumers.
	ctx context.Context
}

type topicPartition struct {
	topic     string
	partition int32
}

// assigned must be set as a kgo.OnPartitionsAssigned callback. Ensuring all
// assigned partitions to this consumer process received records.
func (c *consumer) assigned(_ context.Context, client *kgo.Client, assigned map[string][]int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for topic, partitions := range assigned {
		for _, partition := range partitions {
			c.wg.Add(1)
			pc := partitionConsumer{
				records:   make(chan []*kgo.Record),
				processor: c.processor,
				logger:    c.logger,
				client:    client,
				delivery:  c.delivery,
			}
			go func(topic string, partition int32) {
				defer c.wg.Done()
				pc.consume(c.ctx, topic, partition)
			}(topic, partition)
			tp := topicPartition{partition: partition, topic: topic}
			c.consumers[tp] = pc
		}
	}
}

// lost must be set as a kgo.OnPartitionsLost and kgo.OnPartitionsReassigned
// callbacks. Ensures that partitions that are lost (see kgo.OnPartitionsLost
// for more details) or reassigned (see kgo.OnPartitionsReassigned for more
// details) have their partition consumer stopped.
// This callback must finish within the re-balance timeout.
func (c *consumer) lost(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.consumers) == 0 {
		return // When consumer.close() has been called.
	}
	for topic, partitions := range lost {
		for _, partition := range partitions {
			tp := topicPartition{topic: topic, partition: partition}
			if pc, ok := c.consumers[tp]; ok {
				delete(c.consumers, tp)
				close(pc.records)
			}
		}
	}
}

// close is used on initiate clean shutdown. This call blocks until all the
// partition consumers have processed their records and stopped.
//
// It holds the write lock, which cannot be acquired until the last fetch of
// records has been sent to all the partition consumers.
func (c *consumer) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for tp, pc := range c.consumers {
		delete(c.consumers, tp)
		close(pc.records)
	}
	c.wg.Wait()
}

// processFetch sends the received records for a partition to the corresponding
// partition consumer. If topic/partition combination can't be found in the
// consumer map, the consumer has been closed.
//
// It holds the consumer read lock, which cannot be acquired if the consumer is
// closing.
func (c *consumer) processFetch(ctx context.Context, fetches kgo.Fetches) {
	if fetches.NumRecords() == 0 {
		return
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	fetches.EachPartition(func(ftp kgo.FetchTopicPartition) {
		if len(ftp.Records) == 0 {
			return
		}
		tp := topicPartition{topic: ftp.Topic, partition: ftp.Partition}
		pc, ok := c.consumers[tp]
		if !ok {
			// NOTE(marclop) While possible, this is unlikely to happen given the
			// locking that's in place in the caller.
			if c.delivery == apmqueue.AtMostOnceDeliveryType {
				c.logWarn(errors.New(
					"attempted to process records for revoked partition",
				), ftp)
			}
			return
		}
		var err error
		select {
		// Send partition records to be processed by its dedicated goroutine.
		case pc.records <- ftp.Records:
			// Success.
		case <-ctx.Done(): // Consumer.Run(ctx) context.
			err = ctx.Err()
		case <-c.ctx.Done(): // Forceful cancellation context.
			err = c.ctx.Err()
		}
		// When using AtMostOnceDelivery, if the context is cancelled between
		// PollRecords and this line, records will be lost.
		if err != nil && c.delivery == apmqueue.AtMostOnceDeliveryType {
			c.logWarn(err, ftp)
		}
	})
}

func (c *consumer) logWarn(err error, ftp kgo.FetchTopicPartition) {
	c.logger.Warn(
		"data loss: failed to send records to process after commit",
		zap.Error(err),
		zap.String("topic", ftp.Topic),
		zap.Int32("partition", ftp.Partition),
		zap.Int64("offset", ftp.HighWatermark),
		zap.Int("records", len(ftp.Records)),
	)
}

// OnFetchRecordBuffered Implements the kgo.Hook that injects the processCtx
// context that is canceled by `Consumer.Close()`.
func (c *consumer) OnFetchRecordBuffered(r *kgo.Record) {
	if r.Context == nil {
		r.Context = c.ctx
	}
}

type partitionConsumer struct {
	client    *kgo.Client
	records   chan []*kgo.Record
	processor apmqueue.Processor
	logger    *zap.Logger
	delivery  apmqueue.DeliveryType
}

// consume processed the records from a topic and partition. Calling consume
// more than once will cause a panic.
// The received context which is only canceled when the kgo.Client is closed.
func (pc partitionConsumer) consume(ctx context.Context, topic string, partition int32) {
	logger := pc.logger.With(
		zap.String("topic", topic),
		zap.Int32("partition", partition),
	)
	for records := range pc.records {
		// Store the last processed record. Default to -1 for cases where
		// only the first record is received.
		last := -1
		for i, msg := range records {
			meta := make(map[string]string)
			for _, h := range msg.Headers {
				meta[h.Key] = string(h.Value)
			}
			processCtx := queuecontext.WithMetadata(msg.Context, meta)
			record := apmqueue.Record{Topic: apmqueue.Topic(topic), Value: msg.Value}
			// If a record can't be processed, no retries are attempted and it
			// may be lost. https://github.com/elastic/apm-queue/issues/118.
			if err := pc.processor.Process(processCtx, record); err != nil {
				logger.Error("data loss: unable to process event",
					zap.Error(err),
					zap.Int64("offset", msg.Offset),
					zap.Any("headers", meta),
				)
				switch pc.delivery {
				case apmqueue.AtLeastOnceDeliveryType:
					continue
				}
			}
			last = i
		}
		// Only commit the records when at least a record has been processed when
		// AtLeastOnceDeliveryType is set.
		if pc.delivery == apmqueue.AtLeastOnceDeliveryType && last >= 0 {
			lastRecord := records[last]
			if err := pc.client.CommitRecords(ctx, lastRecord); err != nil {
				logger.Error("unable to commit records",
					zap.Error(err),
					zap.Int64("offset", lastRecord.Offset),
				)
			} else if len(records) > 0 {
				logger.Info("committed",
					zap.Int64("offset", lastRecord.Offset),
				)
			}
		}
	}
}
