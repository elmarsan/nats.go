// Copyright 2020-2022 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"errors"

	"github.com/nats-io/nats.go"
)

type (

	// Consumer contains methods for fetching/processing messages from a stream, as well as fetching consumer info
	Consumer interface {
		// Next is used to retrieve a single message from the stream
		Next(context.Context, ...ConsumerNextOpt) (JetStreamMsg, error)
		// Stream can be used to continuously receive messages and handle them with the provided callback function
		Stream(context.Context, MessageHandler, ...ConsumerStreamOpt) error

		// Info returns Consumer details
		Info(context.Context) (*ConsumerInfo, error)
		// CachedInfo returns *ConsumerInfo cached on a consumer struct
		CachedInfo() *ConsumerInfo
	}

	// ConsumerNextOpt is used to configure `Next()` method with additional parameters
	ConsumerNextOpt func(*pullRequest) error

	// ConsumerStreamOpt represent additional options used in `Stream()` for pull consumers
	ConsumerStreamOpt func(*pullRequest) error

	// MessageHandler is a handler function used as callback in `Stream()`
	MessageHandler func(msg JetStreamMsg, err error)

	consumer struct {
		jetStream    *jetStream
		stream       string
		durable      bool
		name         string
		subscription *nats.Subscription
		info         *ConsumerInfo
		sync.Mutex
	}
	pullConsumer struct {
		consumer
		heartbeat   chan struct{}
		isStreaming uint32
	}

	pullRequest struct {
		Expires   time.Duration `json:"expires,omitempty"`
		Batch     int           `json:"batch,omitempty"`
		MaxBytes  int           `json:"max_bytes,omitempty"`
		NoWait    bool          `json:"no_wait,omitempty"`
		Heartbeat time.Duration `json:"idle_heartbeat,omitempty"`
	}
)

// Next fetches an individual message from a consumer.
// Timeout for this operation is handled using `context.Deadline()`, so it should always be set to avoid getting stuck
//
// Available options:
// WithNoWait() - when set to true, `Next()` request does not wait for a message if no message is available at the time of request
func (p *pullConsumer) Next(ctx context.Context, opts ...ConsumerNextOpt) (JetStreamMsg, error) {
	p.Lock()
	if atomic.LoadUint32(&p.isStreaming) == 1 {
		p.Unlock()
		return nil, ErrConsumerHasActiveSubscription
	}
	timeout := 30 * time.Second
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		timeout = time.Until(deadline)
	}
	req := &pullRequest{
		Batch: 1,
	}
	// Make expiry a little bit shorter than timeout
	if timeout >= 20*time.Millisecond {
		req.Expires = timeout - 10*time.Millisecond
	}
	for _, opt := range opts {
		if err := opt(req); err != nil {
			p.Unlock()
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msgChan := make(chan *jetStreamMsg, 1)
	p.heartbeat = make(chan struct{})
	errs := make(chan error)
	p.Unlock()

	go func() {
		err := p.fetch(ctx, *req, msgChan)
		if err != nil {
			if errors.Is(err, ErrNoMessages) || errors.Is(err, nats.ErrTimeout) {
				errs <- ErrNoMessages
			}
			errs <- err
		}
	}()

	for {
		if req.Heartbeat != 0 {
			select {
			case msg := <-msgChan:
				return msg, nil
			case err := <-errs:
				if errors.Is(err, ErrNoMessages) {
					return nil, nil
				}
				return nil, err
			case <-p.heartbeat:
			case <-time.After(2 * req.Heartbeat):
				p.Lock()
				p.subscription.Unsubscribe()
				p.subscription = nil
				p.Unlock()
				return nil, ErrNoHeartbeat
			}
			continue
		}
		select {
		case msg := <-msgChan:
			return msg, nil
		case err := <-errs:
			if errors.Is(err, ErrNoMessages) {
				return nil, nil
			}
			return nil, err
		}
	}
}

// Stream continuously receives messages from a consumer and handles them with the provided callback function
// ctx is used to handle the whole operation, not individual messages batch, so to avoid cancellation, a context without Deadline should be provided
//
// Available options:
// WithBatchSize() - sets a single batch request messages limit, default is set to 100
// WithExpiry() - sets a timeout for individual batch request, default is set to 30 seconds
// WithStreamHeartbeat() - sets an idle heartbeat setting for a pull request, no heartbeat is set by default
func (p *pullConsumer) Stream(ctx context.Context, handler MessageHandler, opts ...ConsumerStreamOpt) error {
	if atomic.LoadUint32(&p.isStreaming) == 1 {
		return ErrConsumerHasActiveSubscription
	}
	if handler == nil {
		return ErrHandlerRequired
	}
	defaultTimeout := 30 * time.Second
	req := &pullRequest{
		Batch:   100,
		Expires: defaultTimeout,
	}
	for _, opt := range opts {
		if err := opt(req); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	pending := make(chan *jetStreamMsg, 2*req.Batch)
	p.heartbeat = make(chan struct{})
	errs := make(chan error, 1)
	atomic.StoreUint32(&p.isStreaming, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if len(pending) < req.Batch {
					fetchCtx, fetchCancel := context.WithTimeout(ctx, req.Expires+10*time.Millisecond)
					if err := p.fetch(fetchCtx, *req, pending); err != nil && !errors.Is(err, ErrNoMessages) && !errors.Is(err, nats.ErrTimeout) {
						errs <- err
					}
					fetchCancel()
				}
			}
		}
	}()

	go func() {
		for {
			if req.Heartbeat != 0 {
				select {
				case msg := <-pending:
					handler(msg, nil)
				case err := <-errs:
					handler(nil, err)
				case <-p.heartbeat:
				case <-time.After(2 * req.Heartbeat):
					handler(nil, ErrNoHeartbeat)
					cancel()
					p.Lock()
					p.subscription.Unsubscribe()
					p.subscription = nil
					p.Unlock()
					atomic.StoreUint32(&p.isStreaming, 0)
					return
				case <-ctx.Done():
					p.Lock()
					p.subscription.Unsubscribe()
					p.subscription = nil
					p.Unlock()
					atomic.StoreUint32(&p.isStreaming, 0)
					return
				}
				continue
			}
			select {
			case msg := <-pending:
				handler(msg, nil)
			case err := <-errs:
				handler(nil, err)
			case <-ctx.Done():
				p.Lock()
				p.subscription.Unsubscribe()
				p.subscription = nil
				p.Unlock()
				atomic.StoreUint32(&p.isStreaming, 0)
				return
			}
		}
	}()

	return nil
}

// fetch sends a pull request to the server and waits for messages using a subscription from `pullConsumer`
// messages will be fetched up to given batch_size or until there are no more messages or timeout is returned
func (c *pullConsumer) fetch(ctx context.Context, req pullRequest, target chan<- *jetStreamMsg) error {
	if req.Batch < 1 {
		return fmt.Errorf("%w: batch size must be at least 1", nats.ErrInvalidArg)
	}
	c.Lock()
	defer c.Unlock()
	// if there is no subscription for this consumer, create new inbox subject and subscribe
	if c.subscription == nil {
		inbox := nats.NewInbox()
		sub, err := c.jetStream.conn.SubscribeSync(inbox)
		if err != nil {
			return err
		}
		c.subscription = sub
	}

	reqJSON, err := json.Marshal(req)
	if err != nil {
		return err
	}

	subject := apiSubj(c.jetStream.apiPrefix, fmt.Sprintf(apiRequestNextT, c.stream, c.name))
	if err := c.jetStream.conn.PublishRequest(subject, c.subscription.Subject, reqJSON); err != nil {
		return err
	}
	var count int
	for count < req.Batch {
		msg, err := c.subscription.NextMsgWithContext(ctx)
		if err != nil {
			return err
		}
		userMsg, err := checkMsg(msg)
		if err != nil {
			return err
		}
		if !userMsg {
			if req.Heartbeat != 0 {
				c.heartbeat <- struct{}{}
			}
			continue
		}
		target <- c.jetStream.toJSMsg(msg)
		count++
	}
	return nil
}

// Info returns ConsumerInfo for a given consumer
func (p *pullConsumer) Info(ctx context.Context) (*ConsumerInfo, error) {
	infoSubject := apiSubj(p.jetStream.apiPrefix, fmt.Sprintf(apiConsumerInfoT, p.stream, p.name))
	var resp consumerInfoResponse

	if _, err := p.jetStream.apiRequestJSON(ctx, infoSubject, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		if resp.Error.ErrorCode == JSErrCodeConsumerNotFound {
			return nil, ErrConsumerNotFound
		}
		return nil, resp.Error
	}

	p.info = resp.ConsumerInfo
	return resp.ConsumerInfo, nil
}

// CachedInfo returns ConsumerInfo fetched when initializing/updating a consumer
//
// NOTE: The returned object might not be up to date with the most recent updates on the server
// For up-to-date information, use `Info()`
func (p *pullConsumer) CachedInfo() *ConsumerInfo {
	return p.info
}

func upsertConsumer(ctx context.Context, js *jetStream, stream string, cfg ConsumerConfig) (Consumer, error) {
	req := createConsumerRequest{
		Stream: stream,
		Config: &cfg,
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	var ccSubj string
	if cfg.Durable != "" {
		if err := validateDurableName(cfg.Durable); err != nil {
			return nil, err
		}
		ccSubj = apiSubj(js.apiPrefix, fmt.Sprintf(apiDurableCreateT, stream, cfg.Durable))
	} else {
		ccSubj = apiSubj(js.apiPrefix, fmt.Sprintf(apiConsumerCreateT, stream))
	}
	var resp consumerInfoResponse

	if _, err := js.apiRequestJSON(ctx, ccSubj, &resp, reqJSON); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		if resp.Error.ErrorCode == JSErrCodeStreamNotFound {
			return nil, ErrStreamNotFound
		}
		return nil, resp.Error
	}

	return &pullConsumer{
		consumer: consumer{
			jetStream: js,
			stream:    stream,
			name:      resp.Name,
			durable:   cfg.Durable != "",
			info:      resp.ConsumerInfo,
		},
	}, nil
}

func getConsumer(ctx context.Context, js *jetStream, stream, name string) (Consumer, error) {
	if err := validateDurableName(name); err != nil {
		return nil, err
	}
	infoSubject := apiSubj(js.apiPrefix, fmt.Sprintf(apiConsumerInfoT, stream, name))

	var resp consumerInfoResponse

	if _, err := js.apiRequestJSON(ctx, infoSubject, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		if resp.Error.ErrorCode == JSErrCodeConsumerNotFound {
			return nil, ErrConsumerNotFound
		}
		return nil, resp.Error
	}

	return &pullConsumer{
		consumer: consumer{
			jetStream: js,
			stream:    stream,
			name:      name,
			durable:   resp.Config.Durable != "",
			info:      resp.ConsumerInfo,
		},
	}, nil
}

func deleteConsumer(ctx context.Context, js *jetStream, stream, consumer string) error {
	if err := validateDurableName(consumer); err != nil {
		return err
	}
	deleteSubject := apiSubj(js.apiPrefix, fmt.Sprintf(apiConsumerDeleteT, stream, consumer))

	var resp consumerDeleteResponse

	if _, err := js.apiRequestJSON(ctx, deleteSubject, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		if resp.Error.ErrorCode == JSErrCodeConsumerNotFound {
			return ErrConsumerNotFound
		}
		return resp.Error
	}
	return nil
}

func validateDurableName(dur string) error {
	if strings.Contains(dur, ".") {
		return fmt.Errorf("%w: '%s'", ErrInvalidConsumerName, dur)
	}
	return nil
}

func compareConsumerConfig(s, u *ConsumerConfig) error {
	makeErr := func(fieldName string, usrVal, srvVal interface{}) error {
		return fmt.Errorf("configuration requests %s to be %v, but consumer's value is %v", fieldName, usrVal, srvVal)
	}

	if u.Durable != s.Durable {
		return makeErr("durable", u.Durable, s.Durable)
	}
	if u.Description != s.Description {
		return makeErr("description", u.Description, s.Description)
	}
	if u.DeliverPolicy != s.DeliverPolicy {
		return makeErr("deliver policy", u.DeliverPolicy, s.DeliverPolicy)
	}
	if u.OptStartSeq != s.OptStartSeq {
		return makeErr("optional start sequence", u.OptStartSeq, s.OptStartSeq)
	}
	if u.OptStartTime != nil && !u.OptStartTime.IsZero() && !(*u.OptStartTime).Equal(*s.OptStartTime) {
		return makeErr("optional start time", u.OptStartTime, s.OptStartTime)
	}
	if u.AckPolicy != s.AckPolicy {
		return makeErr("ack policy", u.AckPolicy, s.AckPolicy)
	}
	if u.AckWait != 0 && u.AckWait != s.AckWait {
		return makeErr("ack wait", u.AckWait.String(), s.AckWait.String())
	}
	if !(u.MaxDeliver == 0 && s.MaxDeliver == -1) && u.MaxDeliver != s.MaxDeliver {
		return makeErr("max deliver", u.MaxDeliver, s.MaxDeliver)
	}
	if len(u.BackOff) != len(s.BackOff) {
		return makeErr("backoff", u.BackOff, s.BackOff)
	}
	for i, val := range u.BackOff {
		if val != s.BackOff[i] {
			return makeErr("backoff", u.BackOff, s.BackOff)
		}
	}
	if u.FilterSubject != s.FilterSubject {
		return makeErr("filter subject", u.FilterSubject, s.FilterSubject)
	}
	if u.ReplayPolicy != s.ReplayPolicy {
		return makeErr("replay policy", u.ReplayPolicy, s.ReplayPolicy)
	}
	if u.RateLimit != s.RateLimit {
		return makeErr("rate limit", u.RateLimit, s.RateLimit)
	}
	if u.SampleFrequency != s.SampleFrequency {
		return makeErr("sample frequency", u.SampleFrequency, s.SampleFrequency)
	}
	if u.MaxWaiting != 0 && u.MaxWaiting != s.MaxWaiting {
		return makeErr("max waiting", u.MaxWaiting, s.MaxWaiting)
	}
	if u.MaxAckPending != 0 && u.MaxAckPending != s.MaxAckPending {
		return makeErr("max ack pending", u.MaxAckPending, s.MaxAckPending)
	}
	if u.FlowControl != s.FlowControl {
		return makeErr("flow control", u.FlowControl, s.FlowControl)
	}
	if u.Heartbeat != s.Heartbeat {
		return makeErr("heartbeat", u.Heartbeat, s.Heartbeat)
	}
	if u.HeadersOnly != s.HeadersOnly {
		return makeErr("headers only", u.HeadersOnly, s.HeadersOnly)
	}
	if u.MaxRequestBatch != s.MaxRequestBatch {
		return makeErr("max request batch", u.MaxRequestBatch, s.MaxRequestBatch)
	}
	if u.MaxRequestExpires != s.MaxRequestExpires {
		return makeErr("max request expires", u.MaxRequestExpires.String(), s.MaxRequestExpires.String())
	}
	if u.DeliverSubject != s.DeliverSubject {
		return makeErr("deliver subject", u.DeliverSubject, s.DeliverSubject)
	}
	if u.DeliverGroup != s.DeliverGroup {
		return makeErr("deliver group", u.DeliverSubject, s.DeliverSubject)
	}
	if u.InactiveThreshold != s.InactiveThreshold {
		return makeErr("inactive threshhold", u.InactiveThreshold.String(), s.InactiveThreshold.String())
	}
	if u.Replicas != s.Replicas {
		return makeErr("replicas", u.Replicas, s.Replicas)
	}
	if u.MemoryStorage != s.MemoryStorage {
		return makeErr("memory storage", u.MemoryStorage, s.MemoryStorage)
	}
	return nil
}