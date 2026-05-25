package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const threadDeliveryPollInterval = 30 * time.Second

func (r *workspaceRuntime) wakeThreadDelivery() {
	if r == nil {
		return
	}
	wake := r.ensureThreadDeliveryWake()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (r *workspaceRuntime) ensureThreadDeliveryWake() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.threadDeliveryWake == nil {
		r.threadDeliveryWake = make(chan struct{}, 1)
	}
	return r.threadDeliveryWake
}

func (r *workspaceRuntime) threadDeliveryLoop(ctx context.Context) {
	wake := r.ensureThreadDeliveryWake()
	for {
		if err := r.deliverDueThreadIntents(ctx); err != nil && ctx.Err() == nil {
			log.Printf("%s thread delivery error: %v", r.actorID(), err)
		}
		delay := r.nextThreadDeliveryDelay(time.Now().UTC())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (r *workspaceRuntime) nextThreadDeliveryDelay(now time.Time) time.Duration {
	if r == nil || r.docCache == nil {
		return threadDeliveryPollInterval
	}
	next, ok, err := r.docCache.nextThreadIntentAttemptAt(now)
	if err != nil || !ok {
		return threadDeliveryPollInterval
	}
	if next.After(now) {
		delay := next.Sub(now)
		if delay < threadDeliveryPollInterval {
			return delay
		}
		return threadDeliveryPollInterval
	}
	return 0
}

func (r *workspaceRuntime) deliverDueThreadIntents(ctx context.Context) error {
	if r == nil || r.docCache == nil {
		return nil
	}
	intents, err := r.docCache.loadDueReadyThreadIntents(time.Now().UTC())
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.deliverThreadIntent(ctx, intent); err != nil {
			return err
		}
	}
	return nil
}

func (r *workspaceRuntime) deliverThreadIntent(ctx context.Context, intent threadOutboxIntent) error {
	if intent.Resolved == nil {
		return nil
	}
	payload := *intent.Resolved
	if strings.TrimSpace(payload.ClientOperationID) == "" {
		payload.ClientOperationID = intent.IdempotencyKey
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := r.newBackendRequest(ctx, http.MethodPost, "/api/threads", bytes.NewReader(body))
	if err != nil {
		return err
	}
	if strings.TrimSpace(intent.IdempotencyKey) != "" {
		req.Header.Set("X-Notty-Idempotency-Key", strings.TrimSpace(intent.IdempotencyKey))
	}
	req.Header.Set("Content-Type", "application/json")
	if intent.ActorType == "agent" && strings.TrimSpace(intent.ActorID) != "" {
		applyBackendAuth(req.Header, r.cfg, intent.ActorID)
	}
	if strings.TrimSpace(r.cfg.DaemonToken) == "" {
		query := req.URL.Query()
		query.Set("actor", firstNonEmptyText(intent.ActorID, r.cfg.AgentID))
		query.Set("actor_type", firstNonEmptyText(intent.ActorType, "daemon"))
		req.URL.RawQuery = query.Encode()
	}
	now := time.Now().UTC()
	res, err := r.client.Do(req)
	if err != nil {
		intent.Attempts++
		intent.LastAttemptAt = now
		intent.LastError = err.Error()
		intent.LastStatusCode = 0
		intent.NextAttemptAt = now.Add(threadIntentBackoff(intent.Attempts))
		return r.docCache.updateThreadIntent(intent)
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return r.docCache.deleteThreadIntent(intent)
	}
	bodyText, _ := io.ReadAll(io.LimitReader(res.Body, backendErrorBodyLimit))
	intent.Attempts++
	intent.LastAttemptAt = now
	intent.LastStatusCode = res.StatusCode
	intent.LastError = strings.TrimSpace(string(bodyText))
	if intent.LastError == "" {
		intent.LastError = res.Status
	}
	if retryableThreadCreateStatus(res.StatusCode) {
		intent.NextAttemptAt = now.Add(threadIntentBackoff(intent.Attempts))
		intent.Status = threadIntentReady
		return r.docCache.updateThreadIntent(intent)
	}
	intent.Status = threadIntentFailed
	intent.NextAttemptAt = time.Time{}
	return r.docCache.updateThreadIntent(intent)
}

func retryableThreadCreateStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

func threadIntentBackoff(attempts int) time.Duration {
	if attempts <= 0 {
		return time.Second
	}
	delay := time.Second << minInt(attempts-1, 6)
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
