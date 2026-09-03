package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/pkg/contracts"
	"github.com/redis/go-redis/v9"
)

const (
	StreamKey   = "gateway:events:v1"
	GroupName   = "control"
	DLQKey      = "gateway:events:dlq:v1"
	MaxRetries  = 5
	ReclaimIdle = 60 * time.Second
	Retention   = 7 * 24 * time.Hour
)

type Producer struct {
	Redis      redis.UniversalClient
	ProducerID string
	Metrics    *observability.Registry
}

func (p Producer) Add(ctx context.Context, event contracts.StreamEvent) (string, error) {
	if p.Redis == nil {
		p.recordRelease(event, "error")
		return "", errors.New("redis client is nil")
	}
	if err := event.Validate(); err != nil {
		p.recordRelease(event, "error")
		return "", err
	}
	id, err := p.Redis.XAdd(ctx, &redis.XAddArgs{Stream: StreamKey, Values: map[string]any{
		"event_type": event.EventType, "schema_version": event.SchemaVersion,
		"event_id": event.EventID, "request_id": event.RequestID,
		"attempt_id": event.AttemptID, "producer_id": event.ProducerID,
		"occurred_at": event.OccurredAt.UTC().Format(time.RFC3339Nano),
		"payload":     string(event.Payload),
	}}).Result()
	if err != nil {
		p.recordRelease(event, "error")
		return "", err
	}
	p.recordRelease(event, "success")
	return id, nil
}

func (p Producer) recordRelease(event contracts.StreamEvent, result string) {
	if event.EventType != contracts.EventTypeRelease {
		return
	}
	metrics := p.Metrics
	if metrics == nil {
		metrics = observability.Default
	}
	metrics.AddCounter("release_xadd_total", 1, "result", result)
	if result == "success" {
		var release contracts.Release
		if json.Unmarshal(event.Payload, &release) == nil && release.UsageSource == contracts.UsageSourceMissing {
			metrics.AddCounter("release_usage_missing_total", 1)
		}
	}
}

type Handler interface {
	HandleAttemptStarted(context.Context, contracts.AttemptStarted) error
	HandleRelease(context.Context, contracts.Release) error
	RecoverAttempts(context.Context, time.Time) (int, error)
}

type Consumer struct {
	Redis    redis.UniversalClient
	Consumer string
	Handler  Handler
	Now      func() time.Time
	Metrics  *observability.Registry
}

func (c Consumer) ensureGroup(ctx context.Context) error {
	err := c.Redis.XGroupCreateMkStream(ctx, StreamKey, GroupName, "0").Err()
	if err != nil && !isBusyGroup(err) {
		return err
	}
	return nil
}

func isBusyGroup(err error) bool {
	return err != nil && (err.Error() == "BUSYGROUP Consumer Group name already exists" || len(err.Error()) >= 9 && err.Error()[:9] == "BUSYGROUP")
}

func (c Consumer) RunOnce(ctx context.Context) error {
	if c.Redis == nil || c.Handler == nil {
		return errors.New("consumer redis and handler are required")
	}
	if c.Consumer == "" {
		c.Consumer = "control-1"
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	defer c.recordPending(ctx)
	if err := c.ensureGroup(ctx); err != nil {
		return err
	}
	if err := c.reclaim(ctx); err != nil {
		return err
	}
	streams, err := c.Redis.XReadGroup(ctx, &redis.XReadGroupArgs{Group: GroupName, Consumer: c.Consumer, Streams: []string{StreamKey, ">"}, Count: 32, Block: time.Millisecond}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	for _, stream := range streams {
		for _, message := range stream.Messages {
			if err := c.process(ctx, message); err != nil {
				return err
			}
		}
	}
	recovered, err := c.Handler.RecoverAttempts(ctx, c.Now().Add(-5*time.Minute))
	if err == nil && recovered > 0 {
		c.metrics().AddCounter("request_attempt_recovered_total", float64(recovered), "source", "synthetic")
	}
	return err
}

func (c Consumer) reclaim(ctx context.Context) error {
	pending, err := c.Redis.XPendingExt(ctx, &redis.XPendingExtArgs{Stream: StreamKey, Group: GroupName, Start: "-", End: "+", Count: 32, Idle: ReclaimIdle}).Result()
	if err != nil || len(pending) == 0 {
		return err
	}
	ids := make([]string, 0, len(pending))
	for _, item := range pending {
		ids = append(ids, item.ID)
	}
	claimed, err := c.Redis.XClaim(ctx, &redis.XClaimArgs{Stream: StreamKey, Group: GroupName, Consumer: c.Consumer, MinIdle: ReclaimIdle, Messages: ids}).Result()
	if err != nil {
		return err
	}
	c.metrics().AddCounter("control_stream_reclaim_total", float64(len(claimed)))
	for _, message := range claimed {
		if err := c.process(ctx, message); err != nil {
			return err
		}
	}
	return nil
}

// Trim removes entries older than the retention window without crossing the
// oldest pending ID, so unacknowledged work is never silently discarded.
func (c Consumer) Trim(ctx context.Context) error {
	if c.Redis == nil {
		return errors.New("consumer redis is required")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	threshold := fmt.Sprintf("%d-0", now().Add(-Retention).UnixMilli())
	pending, err := c.Redis.XPendingExt(ctx, &redis.XPendingExtArgs{Stream: StreamKey, Group: GroupName, Start: "-", End: "+", Count: 1}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	if len(pending) == 1 && streamIDBefore(pending[0].ID, threshold) {
		threshold = pending[0].ID
	}
	return c.Redis.XTrimMinID(ctx, StreamKey, threshold).Err()
}

func streamIDBefore(left, right string) bool {
	parse := func(id string) (int64, int64, bool) {
		parts := strings.SplitN(id, "-", 2)
		if len(parts) != 2 {
			return 0, 0, false
		}
		milliseconds, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		sequence, err := strconv.ParseInt(parts[1], 10, 64)
		return milliseconds, sequence, err == nil
	}
	leftMS, leftSeq, leftOK := parse(left)
	rightMS, rightSeq, rightOK := parse(right)
	if !leftOK || !rightOK {
		return false
	}
	return leftMS < rightMS || leftMS == rightMS && leftSeq < rightSeq
}

func (c Consumer) process(ctx context.Context, message redis.XMessage) error {
	event, err := decode(message.Values)
	if err != nil {
		return c.deadLetter(ctx, message, "invalid_envelope", err)
	}
	if err := event.Validate(); err != nil {
		return c.deadLetter(ctx, message, "invalid_contract", err)
	}
	var attemptPayload *contracts.AttemptStarted
	var releasePayload *contracts.Release
	switch event.EventType {
	case contracts.EventTypeAttemptStarted:
		var payload contracts.AttemptStarted
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return c.deadLetter(ctx, message, "invalid_json", err)
		}
		if err := validateAttemptIdentity(event, payload); err != nil {
			return c.deadLetter(ctx, message, "invalid_contract", err)
		}
		if err := payload.Validate(); err != nil {
			return c.deadLetter(ctx, message, "invalid_contract", err)
		}
		attemptPayload = &payload
	case contracts.EventTypeRelease:
		var payload contracts.Release
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return c.deadLetter(ctx, message, "invalid_json", err)
		}
		if err := validateReleaseIdentity(event, payload); err != nil {
			return c.deadLetter(ctx, message, "invalid_contract", err)
		}
		if err := payload.Validate(); err != nil {
			return c.deadLetter(ctx, message, "invalid_contract", err)
		}
		releasePayload = &payload
	}
	retryKey := "gateway:events:retry:" + message.ID
	attempt, err := c.Redis.Incr(ctx, retryKey).Result()
	if err != nil {
		return err
	}
	_ = c.Redis.Expire(ctx, retryKey, 7*24*time.Hour).Err()
	if attempt > MaxRetries {
		return c.deadLetter(ctx, message, "retry_exhausted", fmt.Errorf("retry limit exceeded (%d)", attempt))
	}
	var handleErr error
	if attemptPayload != nil {
		handleErr = c.Handler.HandleAttemptStarted(ctx, *attemptPayload)
	} else {
		handleErr = c.Handler.HandleRelease(ctx, *releasePayload)
	}
	if handleErr != nil {
		return handleErr
	}
	if err := c.Redis.XAck(ctx, StreamKey, GroupName, message.ID).Err(); err != nil {
		return err
	}
	_ = c.Redis.Del(ctx, retryKey).Err()
	return nil
}

func validateAttemptIdentity(event contracts.StreamEvent, payload contracts.AttemptStarted) error {
	if event.EventID != payload.EventID || event.RequestID != payload.RequestID || event.AttemptID != payload.AttemptID || event.ProducerID != payload.ProducerID {
		return fmt.Errorf("stream envelope and attempt payload identifiers differ")
	}
	return nil
}

func validateReleaseIdentity(event contracts.StreamEvent, payload contracts.Release) error {
	if event.EventID != payload.EventID || event.RequestID != payload.RequestID || event.AttemptID != payload.AttemptID || event.ProducerID != payload.ProducerID {
		return fmt.Errorf("stream envelope and release payload identifiers differ")
	}
	return nil
}

func decode(values map[string]any) (contracts.StreamEvent, error) {
	get := func(key string) (string, error) {
		value, ok := values[key]
		if !ok {
			return "", fmt.Errorf("missing stream field %s", key)
		}
		return fmt.Sprint(value), nil
	}
	type eventFields struct{ EventType, Schema, EventID, RequestID, AttemptID, ProducerID, OccurredAt, Payload string }
	var f eventFields
	var err error
	if f.EventType, err = get("event_type"); err != nil {
		return contracts.StreamEvent{}, err
	}
	if f.Schema, err = get("schema_version"); err != nil {
		return contracts.StreamEvent{}, err
	}
	if f.EventID, err = get("event_id"); err != nil {
		return contracts.StreamEvent{}, err
	}
	if f.RequestID, err = get("request_id"); err != nil {
		return contracts.StreamEvent{}, err
	}
	if f.AttemptID, err = get("attempt_id"); err != nil {
		return contracts.StreamEvent{}, err
	}
	if f.ProducerID, err = get("producer_id"); err != nil {
		return contracts.StreamEvent{}, err
	}
	if f.OccurredAt, err = get("occurred_at"); err != nil {
		return contracts.StreamEvent{}, err
	}
	if f.Payload, err = get("payload"); err != nil {
		return contracts.StreamEvent{}, err
	}
	schema, err := strconv.Atoi(f.Schema)
	if err != nil {
		return contracts.StreamEvent{}, err
	}
	occurred, err := time.Parse(time.RFC3339Nano, f.OccurredAt)
	if err != nil {
		return contracts.StreamEvent{}, err
	}
	return contracts.StreamEvent{EventType: f.EventType, SchemaVersion: schema, EventID: f.EventID, RequestID: f.RequestID, AttemptID: f.AttemptID, ProducerID: f.ProducerID, OccurredAt: occurred, Payload: json.RawMessage(f.Payload)}, nil
}

func (c Consumer) deadLetter(ctx context.Context, message redis.XMessage, category string, reason error) error {
	retries, _ := c.Redis.Get(ctx, "gateway:events:retry:"+message.ID).Result()
	original, marshalErr := json.Marshal(message.Values)
	if marshalErr != nil {
		original = []byte(fmt.Sprint(message.Values))
	}
	_, err := c.Redis.XAdd(ctx, &redis.XAddArgs{Stream: DLQKey, Values: map[string]any{
		"original_id": message.ID, "category": category, "reason": reason.Error(), "attempts": retries, "failed_at": c.Now().UTC().Format(time.RFC3339Nano), "values": string(original),
	}}).Result()
	if err != nil {
		return err
	}
	c.metrics().AddCounter("control_stream_dlq_total", 1, "reason", category)
	if err := c.Redis.XAck(ctx, StreamKey, GroupName, message.ID).Err(); err != nil {
		return err
	}
	_ = c.Redis.Del(ctx, "gateway:events:retry:"+message.ID).Err()
	return nil
}

func (c Consumer) metrics() *observability.Registry {
	if c.Metrics != nil {
		return c.Metrics
	}
	return observability.Default
}

func (c Consumer) recordPending(ctx context.Context) {
	metricCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	pending, err := c.Redis.XPending(metricCtx, StreamKey, GroupName).Result()
	if err == nil {
		c.metrics().SetGauge("control_stream_pending", float64(pending.Count))
	}
}
